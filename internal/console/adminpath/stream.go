package adminpath

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"mime"
	"net/http"
)

// The dump stream (ADR 0008). POST /v1/dump takes a DumpRequest like every
// other method, but a 200 answer carries the zip archive itself as the body
// (Content-Type application/zip, chunked, no Content-Length) and the outcome in
// the HTTP trailer TrailerDumpResult: {"result": DumpResult} when the archive is
// complete, {"error": Error} when the run failed after the first byte. A
// failure before the first byte is an ordinary JSON error answer. A body that
// ends without the trailer (the worker died) is a failure too, so a cut stream
// is never taken for a complete archive. Nothing is buffered to disk on either
// side.

// TrailerDumpResult is the HTTP trailer that ends a dump stream.
const TrailerDumpResult = "Cryo-Dump-Result"

// ErrStreamWrite is returned by Client.Dump when the destination writer refused
// a write (the technician's browser went away). The worker's run is cancelled.
// The error carries no text from the destination.
var ErrStreamWrite = errors.New("adminpath: the stream destination stopped accepting data")

// maxErrorBody bounds what Client.Dump reads of a non-200 answer.
const maxErrorBody = 64 << 10

// dumpTrailer is the JSON of TrailerDumpResult.
type dumpTrailer struct {
	Result *DumpResult `json:"result,omitempty"`
	Error  *Error      `json:"error,omitempty"`
}

// Dump calls Backend.Dump on the worker and copies the archive into w as it
// arrives. It returns the worker's result once the trailer confirms the archive
// is complete and its size and SHA-256 match what this side received; anything
// else is an error and w holds a partial archive the caller must discard (the
// console aborts the HTTP response). Errors, in addition to those of the other
// methods: ErrStreamWrite when w failed; an *Error with CodeInternal when the
// trailer is missing, unreadable or disagrees with the bytes received.
func (c *Client) Dump(ctx context.Context, req DumpRequest, w io.Writer) (DumpResult, error) {
	var zero DumpResult
	if w == nil {
		return zero, NewError(CodeInternal, "")
	}
	if err := req.ensureRequestID(); err != nil {
		return zero, NewError(CodeInternal, "")
	}
	if err := req.Validate(); err != nil {
		return zero, NewError(CodeInvalidRequest, "")
	}
	payload, err := json.Marshal(&req)
	if err != nil {
		return zero, NewError(CodeInternal, "")
	}
	if d := c.timeouts[MethodDump]; d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	// Cancelling this context (on return) closes the connection, which cancels
	// the worker's request context and so its restic run.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://adminpath/v1/"+MethodDump, bytes.NewReader(payload))
	if err != nil {
		return zero, NewError(CodeInternal, "")
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "application/zip, application/json")
	resp, err := c.http.Do(hreq)
	if err != nil {
		return zero, transportError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		if err != nil {
			return zero, transportError(ctx, err)
		}
		return zero, decodeError(data)
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != "application/zip" {
		return zero, NewError(CodeInternal, "")
	}
	sink := &sinkWriter{w: w}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(sink, h), resp.Body)
	switch {
	case sink.failed:
		return zero, ErrStreamWrite
	case err != nil:
		return zero, transportError(ctx, err)
	}
	var tr dumpTrailer
	raw := resp.Trailer.Get(TrailerDumpResult)
	if raw == "" || json.Unmarshal([]byte(raw), &tr) != nil {
		return zero, NewError(CodeInternal, "")
	}
	if tr.Error != nil {
		return zero, tr.Error.normalize()
	}
	if tr.Result == nil || tr.Result.Bytes != n || tr.Result.SHA256 != hex.EncodeToString(h.Sum(nil)) {
		return zero, NewError(CodeInternal, "")
	}
	return *tr.Result, nil
}

// sinkWriter remembers that the destination failed, so Dump can tell a
// destination failure from a socket failure. The destination's error text is
// dropped: it can carry a client address.
type sinkWriter struct {
	w      io.Writer
	failed bool
}

func (s *sinkWriter) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	if err != nil {
		s.failed = true
	}
	return n, err
}

// serveDump is the server half of the dump stream.
func (h *handler) serveDump(w http.ResponseWriter, r *http.Request, body []byte) {
	var req DumpRequest
	if err := decodeStrict(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, NewError(CodeInvalidRequest, ""))
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, NewError(CodeInvalidRequest, ""))
		return
	}
	dw := &dumpWriter{w: w, h: sha256.New()}
	res, err := h.callDump(r.Context(), req, dw)
	if !dw.started {
		if err != nil {
			e := translate(err)
			writeError(w, HTTPStatus(e.Code), e)
			return
		}
		dw.start() // an empty archive is still an answer with a trailer
	}
	if dw.failed {
		return // the console went away: there is nobody to tell
	}
	var tr dumpTrailer
	if err != nil {
		tr.Error = translate(err)
	} else {
		res.RequestID = req.RequestID
		res.Bytes, res.SHA256 = dw.n, hex.EncodeToString(dw.h.Sum(nil))
		tr.Result = &res
	}
	data, merr := json.Marshal(tr)
	if merr != nil {
		data, _ = json.Marshal(dumpTrailer{Error: NewError(CodeInternal, "")})
	}
	w.Header().Set(TrailerDumpResult, string(data))
}

// callDump runs the backend's Dump and turns a panic into an internal error,
// like handler.call.
func (h *handler) callDump(ctx context.Context, req DumpRequest, w io.Writer) (res DumpResult, err error) {
	defer func() {
		if p := recover(); p != nil {
			if p == http.ErrAbortHandler {
				panic(p)
			}
			res, err = DumpResult{}, NewError(CodeInternal, "")
		}
	}()
	return h.backend.Dump(ctx, req, w)
}

// dumpWriter sends the 200 header on the first byte, counts and hashes what it
// writes, and remembers a failed write.
type dumpWriter struct {
	w       http.ResponseWriter
	h       hash.Hash
	n       int64
	started bool
	failed  bool
}

func (d *dumpWriter) start() {
	d.started = true
	hd := d.w.Header()
	hd.Set("Content-Type", "application/zip")
	hd.Set("Cache-Control", "no-store")
	hd.Set("Trailer", TrailerDumpResult)
	d.w.WriteHeader(http.StatusOK)
}

func (d *dumpWriter) Write(p []byte) (int, error) {
	if !d.started {
		d.start()
	}
	n, err := d.w.Write(p)
	d.h.Write(p[:n])
	d.n += int64(n)
	if err != nil {
		d.failed = true
	}
	return n, err
}
