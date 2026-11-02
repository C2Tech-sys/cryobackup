package adminpath

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// maxResponseBytes bounds what Client reads back (64 MiB: a snapshot list of a
// long-lived repository).
const maxResponseBytes = 64 << 20

// DefaultTimeouts returns the per-method timeouts Client uses unless
// ClientOptions overrides them (architecture §1.1 maintenance worker: prune
// and check are long, interactive browsing is short). The map is a copy.
func DefaultTimeouts() map[string]time.Duration {
	return map[string]time.Duration{
		MethodPing:      5 * time.Second,
		MethodCatConfig: 60 * time.Second,
		MethodLs:        120 * time.Second,
		MethodFind:      120 * time.Second,
		MethodSnapshots: 120 * time.Second,
		MethodStats:     120 * time.Second,
		MethodInit:      120 * time.Second,
		MethodDump:      DumpTimeout,
		MethodUnlock:    60 * time.Second,
		MethodForget:    10 * time.Minute,
		MethodPrune:     6 * time.Hour,
		MethodCheck:     6 * time.Hour,
	}
}

// DumpTimeout bounds one dump stream end to end. A stream runs at the speed of
// the technician's download (ADR 0008: restores stream, nothing is staged), so
// it is long; a dropped download cancels it at once, and a stalled one is cut
// by the console's per-write deadline.
const DumpTimeout = 12 * time.Hour

// ClientOptions configures NewClient.
type ClientOptions struct {
	// Timeouts overrides the per-method timeout, keyed by method name (the
	// Method constants). A missing key or a value of zero or less keeps the
	// default from DefaultTimeouts.
	Timeouts map[string]time.Duration
}

// Client calls a maintenance worker over its unix socket. It is safe for
// concurrent use and satisfies Backend. Create it with NewClient and release it
// with Close.
//
// Errors a method returns:
//
//   - *Error, with the wire Code and Class and the fixed Message, when the
//     worker answered with an error (also for a request that failed Validate
//     before it was sent: CodeInvalidRequest);
//   - an error for which errors.Is(err, ErrUnavailable) holds, when the worker
//     could not be reached; its text never contains the socket path;
//   - context.DeadlineExceeded when the method's timeout or the caller's
//     deadline passed, context.Canceled when the caller cancelled;
//   - an *Error with CodeInternal when the worker's answer could not be decoded.
type Client struct {
	http     *http.Client
	tr       *http.Transport
	timeouts map[string]time.Duration
}

var _ Backend = (*Client)(nil)

// NewClient returns a Client that dials socketPath for every connection. No
// connection is made until the first call.
func NewClient(socketPath string, opts ClientOptions) *Client {
	timeouts := DefaultTimeouts()
	for k, v := range opts.Timeouts {
		if v > 0 {
			timeouts[k] = v
		}
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
		MaxIdleConns:        2,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
		DisableCompression:  true,
		ForceAttemptHTTP2:   false,
	}
	return &Client{http: &http.Client{Transport: tr}, tr: tr, timeouts: timeouts}
}

// Close closes idle connections. The Client stays usable.
func (c *Client) Close() { c.tr.CloseIdleConnections() }

// Ping calls Backend.Ping on the worker.
func (c *Client) Ping(ctx context.Context) (PingResult, error) {
	return invoke[PingResult](c, ctx, MethodPing, struct{}{})
}

// Init calls Backend.Init on the worker.
func (c *Client) Init(ctx context.Context, req InitRequest) (InitResult, error) {
	return call[InitResult](c, ctx, MethodInit, &req)
}

// CatConfig calls Backend.CatConfig on the worker.
func (c *Client) CatConfig(ctx context.Context, req DeviceRequest) (restic.Config, error) {
	return call[restic.Config](c, ctx, MethodCatConfig, &req)
}

// Ls calls Backend.Ls on the worker.
func (c *Client) Ls(ctx context.Context, req LsRequest) (LsResult, error) {
	return call[LsResult](c, ctx, MethodLs, &req)
}

// Find calls Backend.Find on the worker.
func (c *Client) Find(ctx context.Context, req FindRequest) ([]restic.FindResult, error) {
	return call[[]restic.FindResult](c, ctx, MethodFind, &req)
}

// Snapshots calls Backend.Snapshots on the worker.
func (c *Client) Snapshots(ctx context.Context, req DeviceRequest) ([]restic.Snapshot, error) {
	return call[[]restic.Snapshot](c, ctx, MethodSnapshots, &req)
}

// Stats calls Backend.Stats on the worker.
func (c *Client) Stats(ctx context.Context, req StatsRequest) (restic.StatsResult, error) {
	return call[restic.StatsResult](c, ctx, MethodStats, &req)
}

// Unlock calls Backend.Unlock on the worker.
func (c *Client) Unlock(ctx context.Context, req UnlockRequest) (UnlockResult, error) {
	return call[UnlockResult](c, ctx, MethodUnlock, &req)
}

// Forget calls Backend.Forget on the worker.
func (c *Client) Forget(ctx context.Context, req ForgetRequest) (ForgetResult, error) {
	return call[ForgetResult](c, ctx, MethodForget, &req)
}

// Prune calls Backend.Prune on the worker.
func (c *Client) Prune(ctx context.Context, req PruneRequest) (PruneResult, error) {
	return call[PruneResult](c, ctx, MethodPrune, &req)
}

// Check calls Backend.Check on the worker.
func (c *Client) Check(ctx context.Context, req CheckRequest) (CheckResult, error) {
	return call[CheckResult](c, ctx, MethodCheck, &req)
}

// requestPtr is the constraint of a pointer to a request type: it can be
// validated and given a request id.
type requestPtr[Req any] interface {
	*Req
	Validate() error
	ensureRequestID() error
	requestID() string
}

// call fills the request id when it is empty, validates the request (before
// anything is sent) and posts it. req points to the method's own copy of the
// caller's request, so the caller's struct is untouched.
func call[Res any, Req any, P requestPtr[Req]](c *Client, ctx context.Context, method string, req P) (Res, error) {
	var zero Res
	if err := req.ensureRequestID(); err != nil {
		return zero, NewError(CodeInternal, "")
	}
	if err := req.Validate(); err != nil {
		return zero, NewError(CodeInvalidRequest, "")
	}
	return invoke[Res](c, ctx, method, req)
}

// ensureRequestID gives the request a random id when the caller set none.
func (r *DeviceRequest) ensureRequestID() error {
	if r.RequestID != "" {
		return nil
	}
	id, err := newRequestID()
	if err != nil {
		return err
	}
	r.RequestID = id
	return nil
}

// newRequestID returns 16 random bytes as hex.
func newRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// invoke posts body as JSON to /v1/<method> under the method's timeout and
// decodes the answer.
func invoke[Res any](c *Client, ctx context.Context, method string, body any) (Res, error) {
	var zero Res
	payload, err := json.Marshal(body)
	if err != nil {
		return zero, NewError(CodeInternal, "")
	}
	if len(payload) > MaxRequestBytes {
		return zero, NewError(CodeInvalidRequest, "")
	}
	if d := c.timeouts[method]; d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://adminpath/v1/"+method, bytes.NewReader(payload))
	if err != nil {
		return zero, NewError(CodeInternal, "")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return zero, transportError(ctx, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return zero, transportError(ctx, err)
	}
	if len(data) > maxResponseBytes {
		return zero, NewError(CodeInternal, "")
	}
	if resp.StatusCode != http.StatusOK {
		return zero, decodeError(data)
	}
	var out Res
	if err := json.Unmarshal(data, &out); err != nil {
		return zero, NewError(CodeInternal, "")
	}
	return out, nil
}

// transportError turns a failure of http.Client.Do or of reading the body into
// a context error (when the context is done) or a path-free unavailable error.
func transportError(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return &unavailableError{cause: stripAddr(err)}
}

// stripAddr peels the *url.Error and *net.OpError wrappers, which carry the
// request URL and the socket path, and returns the bare underlying error.
func stripAddr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		err = oe.Err
	}
	return err
}

// decodeError reads {"error": {...}} and returns the normalized *Error. A body
// that is not that (or has an unknown code) becomes CodeInternal: the client
// never passes on text from the wire.
func decodeError(data []byte) error {
	var env struct {
		Error *Error `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err != nil || env.Error == nil {
		return NewError(CodeInternal, "")
	}
	return env.Error.normalize()
}
