package adminpath

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/restic"
)

const (
	// drainLimit bounds how much of an oversized request body Handler reads
	// and discards before it answers, so the client reliably sees the 413
	// instead of a reset connection.
	drainLimit = 8 << 20
	// shutdownGrace is how long Serve lets in-flight requests finish after its
	// context is done, before it closes their connections (which cancels the
	// requests' contexts).
	shutdownGrace = 10 * time.Second
	// socketMode is the mode of the socket file (architecture DH1): owner and
	// group may connect, nobody else.
	socketMode os.FileMode = 0o660
)

// Handler returns the http.Handler of the admin path: routes under /v1/,
// request validation, JSON in and out, panic recovery and error translation. It
// panics if b is nil.
//
// Behaviour (architecture DH1, §6.3):
//
//   - POST /v1/<method> with a JSON body (Content-Type application/json, at
//     most MaxRequestBytes, unknown fields rejected) answers 200 with the JSON
//     result. GET /v1/ping is an alias of POST /v1/ping for shell tools.
//   - Every failure answers {"error": {code, class, message}} with
//     HTTPStatus(code) (400 for a bad body, 404 for an unknown route, 405 for
//     the wrong verb, 413 for a body over the cap, 415 for the wrong media type).
//   - A request that fails Validate is invalid_request and never reaches b.
//   - A *Error from b passes through with its Message replaced by the fixed
//     sentence; context.DeadlineExceeded is timeout; context.Canceled is
//     unavailable; a *ValidationError is invalid_request; a panic and any other
//     error are internal. The underlying error or panic value is never
//     serialised.
//   - The result's RequestID is set to the request's.
//   - An Ls result is cut to the request's Limit (DefaultLsLimit when zero) and
//     marked Truncated, as a backstop to the backend's own cap.
//   - Dump answers 200 with the zip archive as a chunked body and its outcome
//     in the TrailerDumpResult trailer (see stream.go); a failure before the
//     first byte is an ordinary error answer.
func Handler(b Backend) http.Handler {
	if b == nil {
		panic("adminpath: nil Backend")
	}
	h := &handler{routes: map[string]route{}, backend: b}
	h.routes[MethodPing] = route{verbs: "GET, POST", fn: func(ctx context.Context, _ []byte) (any, error) {
		res, err := b.Ping(ctx)
		return res, err
	}}
	add(h, MethodInit, b.Init)
	add(h, MethodCatConfig, b.CatConfig)
	add(h, MethodLs, func(ctx context.Context, req LsRequest) (LsResult, error) {
		res, err := b.Ls(ctx, req)
		limit := req.Limit
		if limit == 0 {
			limit = DefaultLsLimit
		}
		if err == nil && len(res.Nodes) > limit {
			res.Nodes = res.Nodes[:limit]
			res.Truncated = true
		}
		return res, err
	})
	add(h, MethodFind, func(ctx context.Context, req FindRequest) ([]restic.FindResult, error) {
		res, err := b.Find(ctx, req)
		if res == nil {
			res = []restic.FindResult{}
		}
		return res, err
	})
	add(h, MethodSnapshots, func(ctx context.Context, req DeviceRequest) ([]restic.Snapshot, error) {
		res, err := b.Snapshots(ctx, req)
		if res == nil {
			res = []restic.Snapshot{}
		}
		return res, err
	})
	h.routes[MethodDump] = route{verbs: "POST"} // streamed: serveDump
	add(h, MethodStats, b.Stats)
	add(h, MethodUnlock, b.Unlock)
	add(h, MethodForget, b.Forget)
	add(h, MethodPrune, b.Prune)
	add(h, MethodCheck, b.Check)
	return h
}

// route is one method of the admin path. fn gets the raw body, decodes and
// validates it, calls the backend and returns the result to serialise.
type route struct {
	verbs string
	fn    func(ctx context.Context, body []byte) (any, error)
}

type handler struct {
	routes  map[string]route
	backend Backend
}

// requestIDer is implemented by the result types that embed ResultMeta.
type requestIDer interface{ setRequestID(string) }

// add registers a method whose request type is Req and whose result is Res.
func add[Req any, Res any, P requestPtr[Req]](h *handler, method string, fn func(context.Context, Req) (Res, error)) {
	h.routes[method] = route{verbs: "POST", fn: func(ctx context.Context, body []byte) (any, error) {
		var req Req
		if err := decodeStrict(body, &req); err != nil {
			return nil, NewError(CodeInvalidRequest, "")
		}
		if err := P(&req).Validate(); err != nil {
			return nil, NewError(CodeInvalidRequest, "")
		}
		res, err := fn(ctx, req)
		if err != nil {
			return nil, err
		}
		if m, ok := any(&res).(requestIDer); ok {
			m.setRequestID(P(&req).requestID())
		}
		return res, nil
	}}
}

func (r DeviceRequest) requestID() string { return r.RequestID }

// decodeStrict decodes exactly one JSON value, rejecting unknown fields and
// trailing data.
func decodeStrict(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

// ServeHTTP implements http.Handler.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method, ok := strings.CutPrefix(r.URL.Path, "/v1/")
	rt, found := h.routes[method]
	if !ok || !found {
		writeError(w, http.StatusNotFound, NewError(CodeNotFound, ""))
		return
	}
	if !strings.Contains(", "+rt.verbs+",", ", "+r.Method+",") {
		w.Header().Set("Allow", rt.verbs)
		writeError(w, http.StatusMethodNotAllowed, NewError(CodeInvalidRequest, ""))
		return
	}
	var body []byte
	if r.Method == http.MethodPost {
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, NewError(CodeInvalidRequest, ""))
			return
		}
		var err error
		if body, err = io.ReadAll(io.LimitReader(r.Body, MaxRequestBytes+1)); err != nil {
			writeError(w, http.StatusBadRequest, NewError(CodeInvalidRequest, ""))
			return
		}
		if len(body) > MaxRequestBytes {
			_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, drainLimit))
			w.Header().Set("Connection", "close")
			writeError(w, http.StatusRequestEntityTooLarge, NewError(CodeInvalidRequest, ""))
			return
		}
	}
	if method == MethodDump {
		h.serveDump(w, r, body)
		return
	}
	res, err := h.call(r.Context(), rt, body)
	if err != nil {
		e := translate(err)
		writeError(w, HTTPStatus(e.Code), e)
		return
	}
	data, merr := json.Marshal(res)
	if merr != nil {
		writeError(w, http.StatusInternalServerError, NewError(CodeInternal, ""))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// call runs the route and turns a panic into an internal error. The panic value
// is dropped. http.ErrAbortHandler is passed on, as net/http expects.
func (h *handler) call(ctx context.Context, rt route, body []byte) (res any, err error) {
	defer func() {
		if p := recover(); p != nil {
			if p == http.ErrAbortHandler {
				panic(p)
			}
			res, err = nil, NewError(CodeInternal, "")
		}
	}()
	return rt.fn(ctx, body)
}

// translate maps a backend error to the *Error that goes on the wire. The
// result never carries text from err.
func translate(err error) *Error {
	var e *Error
	var v *ValidationError
	switch {
	case errors.As(err, &e):
		return e.normalize()
	case errors.As(err, &v):
		return NewError(CodeInvalidRequest, "")
	case errors.Is(err, context.DeadlineExceeded):
		return NewError(CodeTimeout, "")
	case errors.Is(err, context.Canceled):
		return NewError(CodeUnavailable, "")
	}
	return NewError(CodeInternal, "")
}

// writeError writes {"error": {...}} with status.
func writeError(w http.ResponseWriter, status int, e *Error) {
	data, _ := json.Marshal(struct {
		Error *Error `json:"error"`
	}{e})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// Listen creates the admin socket at socketPath and returns its listener. A
// stale socket file (one nothing is listening on) is removed first; a live one
// is an error, and so is any existing file that is not a socket. The socket is
// chmod 0660 before Listen returns (architecture DH1); the directory that holds
// it should already be restricted to the shared group, because the file exists
// with the process umask for the instant between bind and chmod. Closing the
// listener removes the file.
func Listen(socketPath string) (net.Listener, error) {
	if socketPath == "" {
		return nil, errors.New("adminpath: empty socket path")
	}
	if err := removeStale(socketPath); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("adminpath: listen: %w", err)
	}
	if err := os.Chmod(socketPath, socketMode); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("adminpath: chmod socket: %w", err)
	}
	return ln, nil
}

// removeStale deletes socketPath when it is a socket file nobody answers on.
func removeStale(socketPath string) error {
	fi, err := os.Lstat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("adminpath: stat socket: %w", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return errors.New("adminpath: socket path exists and is not a socket")
	}
	if conn, derr := (&net.Dialer{Timeout: time.Second}).Dial("unix", socketPath); derr == nil {
		_ = conn.Close()
		return errors.New("adminpath: socket is in use by another process")
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("adminpath: remove stale socket: %w", err)
	}
	return nil
}

// Serve listens on socketPath (see Listen) and serves h until ctx is done. It
// then shuts the server down, giving in-flight requests a short grace period
// before their connections are closed, removes the socket file and returns nil.
// It returns an error if the socket cannot be created or the server fails.
func Serve(ctx context.Context, socketPath string, h http.Handler) error {
	ln, err := Listen(socketPath)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	done, stop := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-stop:
			return
		case <-ctx.Done():
		}
		sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if srv.Shutdown(sctx) != nil {
			_ = srv.Close()
		}
	}()
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	close(stop)
	<-done
	_ = srv.Close()
	_ = ln.Close()
	if rerr := os.Remove(socketPath); rerr != nil && !errors.Is(rerr, os.ErrNotExist) && err == nil {
		err = fmt.Errorf("adminpath: remove socket: %w", rerr)
	}
	return err
}
