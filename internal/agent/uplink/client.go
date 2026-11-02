package uplink

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// Errors a caller can test with errors.Is.
var (
	// ErrPathLeak: the body failed api.AssertPathFree and was not sent.
	ErrPathLeak = errors.New("uplink: body is not path-free; not sent")
	// ErrPollInFlight: a command long poll is already running (one at a time, §5.5).
	ErrPollInFlight = errors.New("uplink: a command poll is already in flight")
	// ErrTooLarge: the encoded body exceeds the route's cap; split the batch.
	ErrTooLarge = errors.New("uplink: body exceeds the route's size cap")
	// ErrNoToken: the device token is not available.
	ErrNoToken = errors.New("uplink: no device token")
)

// Error is the failure of one call. Its text is fixed vocabulary only: the
// problem title, the request id and any wire text are never repeated in it
// (the wire code is repeated only when it is in the api table).
type Error struct {
	Op         string
	Status     int // HTTP status, 0 for a transport failure
	Code       string
	RetryAfter time.Duration
	RequestID  string
	Network    bool
	TLS        bool
	cause      error
}

func (e *Error) Error() string {
	switch {
	case e.TLS:
		return "uplink: " + e.Op + ": tls error"
	case e.Network:
		return "uplink: " + e.Op + ": network error"
	}
	var b strings.Builder
	b.WriteString("uplink: " + e.Op + ": http " + strconv.Itoa(e.Status))
	if e.Code != "" {
		b.WriteString(" " + e.Code)
	}
	return b.String()
}

// Unwrap exposes the transport error for device-local logging.
func (e *Error) Unwrap() error { return e.cause }

// Class maps a failure to the error class the heartbeat reports as
// last_error_class: tls, network, auth (401 and 403), or "" when the failure
// says nothing about connectivity (a 4xx about the request itself).
func (e *Error) Class() restic.ErrorClass {
	switch {
	case e.TLS:
		return restic.ClassTLS
	case e.Network:
		return restic.ClassNetwork
	case e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden:
		return restic.ClassAuth
	case e.Status >= 500:
		return restic.ClassNetwork
	}
	return ""
}

// ErrBadCommandID: the command id is not a command id (api validation); the
// result is not posted, because the route built from it would not name a
// command (an empty, "." or ".." id is redirected by the console's mux).
var ErrBadCommandID = errors.New("uplink: not a valid command id; not sent")

// Refused reports whether err is the final answer to a command-result post
// that no retry can change: the request itself was refused (400, 404, 409,
// 410, 413, 422 and any other 4xx except the ones below), was answered with a
// redirect (the client never follows one, so the same post gets the same
// answer), or can never be sent (ErrTooLarge, ErrPathLeak, ErrBadCommandID).
// It is false for success (nil), for 401 and 403 (an auth problem: the caller
// backs off and re-authenticates), 407, 408, 425 and 429 (try again later),
// for every 5xx and for transport failures. A caller that keeps retrying a
// refused body wedges forever (ADR 0010).
func Refused(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrTooLarge) || errors.Is(err, ErrPathLeak) || errors.Is(err, ErrBadCommandID) {
		return true
	}
	var ue *Error
	if !errors.As(err, &ue) || ue.Network || ue.TLS {
		return false
	}
	switch s := ue.Status; {
	case s >= 300 && s < 400:
		return true
	case s < 400 || s >= 500:
		return false
	case s == http.StatusUnauthorized, s == http.StatusForbidden, s == http.StatusProxyAuthRequired,
		s == http.StatusRequestTimeout, s == http.StatusTooEarly, s == http.StatusTooManyRequests:
		return false
	}
	return true
}

// Options configures New.
type Options struct {
	// BaseURL is the console's agent listener, https://host[:port].
	BaseURL string
	// Token returns the device token. It is read on every request so a rotated
	// token takes effect immediately; nil is allowed for an enroll-only client.
	Token func() string
	// AgentVersion is sent as X-CryoBackup-Agent.
	AgentVersion string
	// InsecureLab allows an http:// base URL and skips TLS verification.
	InsecureLab bool
	// SPKIPin, when set, is the base64 SHA-256 of the console's
	// SubjectPublicKeyInfo; a connection to any other key is refused (also with
	// InsecureLab).
	SPKIPin string
	// Proxy is an optional proxy URL; empty means the environment's proxy.
	Proxy string
	// Clock is the device clock; nil means time.Now.
	Clock func() time.Time
	// HTTPClient replaces the built client (tests).
	HTTPClient *http.Client
	// StartLeaseTimeout bounds the start-lease call; zero means
	// api.StartLeaseTimeout (tests set it longer, so that only the caller's
	// context can end a hung call).
	StartLeaseTimeout time.Duration
	// RequestTimeout bounds every call except the long poll; zero means 60 s.
	RequestTimeout time.Duration
}

// Client speaks the agent API (architecture §5.1). It is safe for concurrent
// use; only one command poll runs at a time.
type Client struct {
	base    *url.URL
	hc      *http.Client
	token   func() string
	version string
	clock   func() time.Time
	timeout time.Duration
	leaseTO time.Duration // StartLease's own bound (Options.StartLeaseTimeout)

	offsetMS   atomic.Int64
	haveOffset atomic.Bool
	polling    atomic.Bool
}

// New builds a Client.
func New(o Options) (*Client, error) {
	u, err := url.Parse(o.BaseURL)
	if err != nil || u.Host == "" {
		return nil, errors.New("uplink: base URL must have a host")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !o.InsecureLab {
			return nil, errors.New("uplink: http is refused unless insecure lab mode is set")
		}
	default:
		return nil, errors.New("uplink: base URL scheme must be https")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("uplink: base URL must not carry userinfo, query or fragment")
	}
	c := &Client{base: u, token: o.Token, version: o.AgentVersion, clock: o.Clock, timeout: o.RequestTimeout, leaseTO: o.StartLeaseTimeout}
	if c.leaseTO <= 0 {
		c.leaseTO = api.StartLeaseTimeout
	}
	if c.clock == nil {
		c.clock = time.Now
	}
	if c.timeout <= 0 {
		c.timeout = 60 * time.Second
	}
	c.hc = o.HTTPClient
	if c.hc == nil {
		hc, err := buildHTTPClient(o)
		if err != nil {
			return nil, err
		}
		c.hc = hc
	}
	return c, nil
}

func buildHTTPClient(o Options) (*http.Client, error) {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   20 * time.Second,
		ResponseHeaderTimeout: 0, // the long poll waits; per-call contexts bound everything
		MaxIdleConns:          2,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	if o.Proxy != "" {
		pu, err := url.Parse(o.Proxy)
		if err != nil || pu.Host == "" {
			return nil, errors.New("uplink: proxy must be a URL with a host")
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: o.InsecureLab} //nolint:gosec // lab flag, documented
	if o.SPKIPin != "" {
		want, err := base64.StdEncoding.DecodeString(o.SPKIPin)
		if err != nil || len(want) != sha256.Size {
			return nil, errors.New("uplink: SPKI pin must be the base64 of a SHA-256")
		}
		tc.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("tls: no peer certificate")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if subtle.ConstantTimeCompare(sum[:], want) != 1 {
				return errors.New("tls: server key does not match the pin")
			}
			return nil
		}
	}
	tr.TLSClientConfig = tc
	return &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// OffsetMS returns the device clock minus the server clock in milliseconds as
// last observed from X-CryoBackup-Server-Time, and whether one was observed.
func (c *Client) OffsetMS() (int64, bool) { return c.offsetMS.Load(), c.haveOffset.Load() }

func (c *Client) observe(h http.Header, receivedAt time.Time) {
	if st, err := api.ParseServerTime(h.Get(api.HeaderServerTime)); err == nil {
		c.offsetMS.Store(api.ClockOffset(receivedAt, st))
		c.haveOffset.Store(true)
	}
}

// call is the one place a request leaves the process.
type call struct {
	op      string
	method  string
	route   string
	query   url.Values
	bearer  string
	in      any // a typed api request; AssertPathFree is run on it
	maxBody int
	out     any
	etag    string
	timeout time.Duration
}

// do performs a call and returns the HTTP status (304 is a status, not an error).
func (c *Client) do(ctx context.Context, k call) (int, error) {
	var body io.Reader
	if k.in != nil {
		if err := api.AssertPathFree(k.in); err != nil {
			return 0, fmt.Errorf("%w: %v", ErrPathLeak, err)
		}
		b, err := json.Marshal(k.in)
		if err != nil {
			return 0, err
		}
		if k.maxBody > 0 && len(b) > k.maxBody {
			return 0, ErrTooLarge
		}
		body = bytes.NewReader(b)
	}
	to := k.timeout
	if to <= 0 {
		to = c.timeout
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()

	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + k.route
	if k.query != nil {
		u.RawQuery = k.query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, k.method, u.String(), body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", api.ContentTypeJSON)
	if k.in != nil {
		req.Header.Set("Content-Type", api.ContentTypeJSON)
	}
	if c.version != "" {
		req.Header.Set(api.HeaderAgentVersion, c.version)
	}
	if k.bearer != "" {
		req.Header.Set(api.HeaderAuthorization, api.BearerPrefix+k.bearer)
	}
	if k.etag != "" {
		req.Header.Set(api.HeaderIfNoneMatch, k.etag)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, transportError(k.op, err)
	}
	defer resp.Body.Close()
	c.observe(resp.Header, c.clock())

	switch {
	case resp.StatusCode == http.StatusNotModified:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return resp.StatusCode, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if k.out == nil || resp.StatusCode == http.StatusNoContent {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			return resp.StatusCode, nil
		}
		dec := json.NewDecoder(io.LimitReader(resp.Body, maxResponse))
		if err := dec.Decode(k.out); err != nil && !errors.Is(err, io.EOF) {
			return resp.StatusCode, &Error{Op: k.op, Status: resp.StatusCode, Code: "malformed_response"}
		}
		return resp.StatusCode, nil
	}
	return resp.StatusCode, c.problem(k.op, resp)
}

// maxResponse caps a successful body (a policy with long exclude lists is the
// largest legitimate one).
const maxResponse = 4 << 20

// problem decodes an RFC 9457 problem document into an *Error. The title and
// any other wire text are dropped.
func (c *Client) problem(op string, resp *http.Response) *Error {
	e := &Error{Op: op, Status: resp.StatusCode, RequestID: sanitizeID(resp.Header.Get(api.HeaderRequestID))}
	var p api.Error
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&p); err == nil {
		if knownCode(p.Code) {
			e.Code = p.Code
		}
		if p.RequestID != "" && e.RequestID == "" {
			e.RequestID = sanitizeID(p.RequestID)
		}
		if p.RetryAfterS > 0 {
			e.RetryAfter = time.Duration(p.RetryAfterS) * time.Second
		}
	}
	if ra := resp.Header.Get(api.HeaderRetryAfter); ra != "" {
		if d, ok := parseRetryAfter(ra, c.clock()); ok {
			e.RetryAfter = d
		}
	}
	if e.RetryAfter > maxRetryAfter {
		e.RetryAfter = maxRetryAfter
	}
	return e
}

func knownCode(code string) bool {
	for _, c := range api.ErrorCodes() {
		if c == code {
			return true
		}
	}
	return false
}

// maxRetryAfter bounds a server-supplied wait so a bad header cannot silence
// the agent for days.
const maxRetryAfter = time.Hour

// parseRetryAfter reads delta-seconds or an HTTP date.
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if n, err := strconv.Atoi(v); err == nil {
		if n < 0 {
			return 0, false
		}
		return time.Duration(n) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

func sanitizeID(s string) string {
	if len(s) > 64 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return ""
		}
	}
	return s
}

func transportError(op string, err error) *Error {
	e := &Error{Op: op, Network: true, cause: err}
	var (
		unknownAuth x509.UnknownAuthorityError
		hostErr     x509.HostnameError
		certInvalid x509.CertificateInvalidError
		recHdr      tls.RecordHeaderError
		verifyErr   *tls.CertificateVerificationError
	)
	if errors.As(err, &unknownAuth) || errors.As(err, &hostErr) || errors.As(err, &certInvalid) ||
		errors.As(err, &recHdr) || errors.As(err, &verifyErr) || strings.Contains(err.Error(), "tls:") {
		e.TLS, e.Network = true, false
	}
	return e
}

func (c *Client) deviceToken() (string, error) {
	if c.token == nil {
		return "", ErrNoToken
	}
	t := c.token()
	if t == "" {
		return "", ErrNoToken
	}
	return t, nil
}

// Enroll performs POST /enroll with the enrollment token (§5.1, DH17).
func (c *Client) Enroll(ctx context.Context, enrollmentToken string, req api.EnrollRequest) (api.EnrollResponse, error) {
	var out api.EnrollResponse
	_, err := c.do(ctx, call{op: "enroll", method: http.MethodPost, route: api.RouteEnroll, bearer: enrollmentToken,
		in: req, maxBody: api.MaxEnrollBody, out: &out})
	return out, err
}

// PullPolicy performs GET /policy with If-None-Match. changed is false for a 304.
func (c *Client) PullPolicy(ctx context.Context, etag string) (resp api.PolicyResponse, changed bool, err error) {
	tok, err := c.deviceToken()
	if err != nil {
		return resp, false, err
	}
	status, err := c.do(ctx, call{op: "policy", method: http.MethodGet, route: api.RoutePolicy, bearer: tok,
		etag: etag, out: &resp})
	if err != nil {
		return resp, false, err
	}
	return resp, status != http.StatusNotModified, nil
}

// Heartbeat performs POST /heartbeat.
func (c *Client) Heartbeat(ctx context.Context, req api.HeartbeatRequest) (api.HeartbeatResponse, error) {
	var out api.HeartbeatResponse
	tok, err := c.deviceToken()
	if err != nil {
		return out, err
	}
	_, err = c.do(ctx, call{op: "heartbeat", method: http.MethodPost, route: api.RouteHeartbeat, bearer: tok,
		in: req, maxBody: api.MaxHeartbeatBody, out: &out})
	return out, err
}

// UploadRuns performs POST /runs.
func (c *Client) UploadRuns(ctx context.Context, req api.RunsRequest) (api.RunsResponse, error) {
	var out api.RunsResponse
	tok, err := c.deviceToken()
	if err != nil {
		return out, err
	}
	_, err = c.do(ctx, call{op: "runs", method: http.MethodPost, route: api.RouteRuns, bearer: tok,
		in: req, maxBody: api.MaxRunsBody, out: &out})
	return out, err
}

// UploadSnapshots performs POST /snapshots.
func (c *Client) UploadSnapshots(ctx context.Context, req api.SnapshotsRequest) (api.SnapshotsResponse, error) {
	var out api.SnapshotsResponse
	tok, err := c.deviceToken()
	if err != nil {
		return out, err
	}
	_, err = c.do(ctx, call{op: "snapshots", method: http.MethodPost, route: api.RouteSnapshots, bearer: tok,
		in: req, maxBody: api.MaxSnapshotsBody, out: &out})
	return out, err
}

// StartLease performs POST /start-lease (ADR 0005) and waits at most
// api.StartLeaseTimeout (Options.StartLeaseTimeout) for the answer, or less
// when ctx ends first. The caller decides what a failure
// means (the agent fails open on a transport error, a timeout, 5xx and 429).
func (c *Client) StartLease(ctx context.Context, req api.StartLeaseRequest) (api.StartLeaseResponse, error) {
	var out api.StartLeaseResponse
	tok, err := c.deviceToken()
	if err != nil {
		return out, err
	}
	_, err = c.do(ctx, call{op: "start_lease", method: http.MethodPost, route: api.RouteStartLease, bearer: tok,
		in: req, maxBody: api.MaxStartLeaseBody, out: &out, timeout: c.leaseTO})
	return out, err
}

// PollCommands performs GET /commands?wait=N. Only one poll runs at a time: a
// second concurrent call returns ErrPollInFlight without touching the network.
func (c *Client) PollCommands(ctx context.Context, wait time.Duration) (api.CommandsResponse, error) {
	var out api.CommandsResponse
	if !c.polling.CompareAndSwap(false, true) {
		return out, ErrPollInFlight
	}
	defer c.polling.Store(false)
	tok, err := c.deviceToken()
	if err != nil {
		return out, err
	}
	if wait < 0 {
		wait = 0
	}
	if wait > api.CommandsLongPollMax {
		wait = api.CommandsLongPollMax
	}
	q := url.Values{api.QueryWait: {strconv.Itoa(int(wait / time.Second))}}
	_, err = c.do(ctx, call{op: "commands", method: http.MethodGet, route: api.RouteCommands, bearer: tok,
		query: q, out: &out, timeout: wait + 20*time.Second})
	return out, err
}

// CommandResult performs POST /commands/{id}/result.
func (c *Client) CommandResult(ctx context.Context, id string, req api.CommandResultRequest) error {
	if !validCommandID(id) {
		return ErrBadCommandID
	}
	tok, err := c.deviceToken()
	if err != nil {
		return err
	}
	_, err = c.do(ctx, call{op: "command_result", method: http.MethodPost, route: api.RouteCommandResult(id),
		bearer: tok, in: req, maxBody: api.MaxCommandResultBody})
	return err
}

// validCommandID applies the api's command-id rule (a canonical ULID) without
// needing the rest of a command.
func validCommandID(id string) bool {
	t := time.Unix(1, 0)
	return api.Command{ID: id, Type: api.CommandCheckIn, CreatedAt: t, ExpiresAt: t.Add(time.Second)}.Validate() == nil
}
