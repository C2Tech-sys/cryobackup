package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
	"github.com/c2tech-sys/cryobackup/internal/agent/uplink"
	"github.com/c2tech-sys/cryobackup/internal/api"
)

// enrollAnswer is one scripted answer of the enroll front: a problem document
// with status (and Retry-After seconds when retryAfter > 0), or a cut
// connection when drop is set. A zero answer lets the real fake console reply.
type enrollAnswer struct {
	status     int
	code       string
	retryAfter int
	drop       bool
}

// enrollFront stands in front of the fake console and answers POST /enroll from
// a script (the last answer repeats when repeat is set), recording every
// request's nonce; everything else, and an unscripted enroll, goes through.
type enrollFront struct {
	srv *httptest.Server

	mu     sync.Mutex
	script []enrollAnswer
	repeat bool
	nonces []string
}

func newEnrollFront(t *testing.T, fc *fakeConsole, repeat bool, script ...enrollAnswer) *enrollFront {
	t.Helper()
	f := &enrollFront{script: script, repeat: repeat}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != api.RouteEnroll {
			fc.serve(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var req api.EnrollRequest
		_ = json.Unmarshal(body, &req)
		f.mu.Lock()
		f.nonces = append(f.nonces, req.Nonce)
		var a enrollAnswer
		if len(f.script) > 0 {
			a = f.script[0]
			if len(f.script) > 1 || !f.repeat {
				f.script = f.script[1:]
			}
		}
		f.mu.Unlock()
		switch {
		case a.drop:
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
		case a.status != 0:
			e := api.NewError(a.code)
			e.Status = a.status
			w.Header().Set("Content-Type", api.ContentTypeProblem)
			if a.retryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(a.retryAfter))
			}
			w.WriteHeader(a.status)
			_ = json.NewEncoder(w).Encode(e)
		default:
			fc.serve(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *enrollFront) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.nonces)
}

func (f *enrollFront) seenNonces() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.nonces...)
}

// retryClock is the fake device clock: the sleep seam advances it, so a retry
// budget of minutes runs in microseconds.
type retryClock struct {
	mu     sync.Mutex
	now    time.Time
	slept  []time.Duration
	cancel context.CancelFunc // when set, the first sleep cancels the context
}

func newRetryClock() *retryClock { return &retryClock{now: time.Now()} }

func (c *retryClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *retryClock) sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.slept = append(c.slept, d)
	c.now = c.now.Add(d)
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return ctx.Err()
}

func (c *retryClock) waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}

func (c *retryClock) total() time.Duration {
	var t time.Duration
	for _, d := range c.waits() {
		t += d
	}
	return t
}

func retryOpts(t *testing.T, serverURL string, clk *retryClock, log io.Writer) (EnrollOptions, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	return EnrollOptions{
		StateDir: dir, ServerURL: serverURL, Token: testEnrollToken, InsecureLab: true, InsecurePlaintextSecrets: plainSecrets(),
		AllowUnprotectedState: true, Clock: clk.Now, Log: log,
		sleep: clk.sleep, rand: func() float64 { return 0 },
	}, dir
}

func asUplink(t *testing.T, err error) *uplink.Error {
	t.Helper()
	var ue *uplink.Error
	if !errors.As(err, &ue) {
		t.Fatalf("error %v is not an *uplink.Error (the class must survive the retry wrapper)", err)
	}
	return ue
}

// A 429 with Retry-After 1 s (a second enrollment on one token while the first
// is in flight) is retried and enrolls exactly one device (F4).
func TestEnrollRetries429ThenEnrollsOnce(t *testing.T) {
	fc := newFakeConsole(t)
	front := newEnrollFront(t, fc, false, enrollAnswer{status: 429, code: api.CodeRateLimited, retryAfter: 1})
	clk := newRetryClock()
	var log bytes.Buffer
	o, dir := retryOpts(t, front.srv.URL, clk, &log)

	res, err := Enroll(context.Background(), o)
	if err != nil {
		t.Fatalf("Enroll after a 429: %v", err)
	}
	if res.DeviceID != testDeviceID {
		t.Errorf("device id = %q", res.DeviceID)
	}
	if front.requests() != 2 {
		t.Errorf("enroll requests = %d, want 2 (the 429, then the success)", front.requests())
	}
	fc.mu.Lock()
	devices, issued := len(fc.nonces), fc.tokensIssued
	fc.mu.Unlock()
	if devices != 1 || issued != 1 {
		t.Errorf("console made %d devices and issued %d device tokens, want exactly one of each", devices, issued)
	}
	// Retry-After is honoured: never less than it, and (rand 0) exactly it.
	if w := clk.waits(); len(w) != 1 || w[0] != time.Second {
		t.Errorf("waits = %v, want [1s]", w)
	}
	cfg, err := config.Load(config.Layout{Root: dir}.ConfigFile())
	if err != nil || !cfg.Enrolled() {
		t.Fatalf("not enrolled after the retry: %v", err)
	}
	// The retry is logged, class and wait, with nothing that must not be there.
	out := log.String()
	if !strings.Contains(out, "http 429") || !strings.Contains(out, "retrying in 1s") {
		t.Errorf("log = %q, want the class (http 429) and the wait (retrying in 1s)", out)
	}
	for _, bad := range []string{testEnrollToken, dir, cfg.EnrollNonce, "Bearer"} {
		if strings.Contains(out, bad) {
			t.Errorf("the retry log contains %q: %q", bad, out)
		}
	}
}

// 503 (repository-init failure, master key unavailable) carries Retry-After and
// it is honoured, jittered upward and capped.
func TestEnrollHonoursRetryAfterWithJitterAndCap(t *testing.T) {
	cases := []struct {
		name       string
		code       string
		retryAfter int
		rand       float64
		want       time.Duration
	}{
		{"503 unavailable, no jitter", api.CodeUnavailable, 7, 0, 7 * time.Second},
		{"503 master key, jitter adds up to half", api.CodeMasterKeyUnavailable, 8, 0.5, 10 * time.Second},
		{"a huge Retry-After is capped", api.CodeUnavailable, 600, 0, enrollRetryAfterCap},
		{"the cap is jittered too", api.CodeUnavailable, 600, 0.5, enrollRetryAfterCap + enrollRetryAfterCap/4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fc := newFakeConsole(t)
			front := newEnrollFront(t, fc, false, enrollAnswer{status: 503, code: c.code, retryAfter: c.retryAfter})
			clk := newRetryClock()
			o, _ := retryOpts(t, front.srv.URL, clk, nil)
			o.rand = func() float64 { return c.rand }
			if _, err := Enroll(context.Background(), o); err != nil {
				t.Fatalf("Enroll: %v", err)
			}
			if w := clk.waits(); len(w) != 1 || w[0] != c.want {
				t.Errorf("waits = %v, want [%s]", w, c.want)
			}
		})
	}
}

// Without Retry-After the wait is the bounded exponential backoff.
func TestEnrollBacksOffWithoutRetryAfter(t *testing.T) {
	fc := newFakeConsole(t)
	front := newEnrollFront(t, fc, false,
		enrollAnswer{status: 502, code: api.CodeInternal},
		enrollAnswer{status: 504, code: api.CodeInternal},
		enrollAnswer{status: 503, code: api.CodeUnavailable},
	)
	clk := newRetryClock()
	o, _ := retryOpts(t, front.srv.URL, clk, nil)
	o.rand = func() float64 { return 0.5 } // the middle of the 20 % jitter: no change
	if _, err := Enroll(context.Background(), o); err != nil {
		t.Fatalf("Enroll through 502, 504, 503: %v", err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	got := clk.waits()
	if len(got) != len(want) {
		t.Fatalf("waits = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("wait %d = %s, want %s (all: %v)", i, got[i], want[i], got)
		}
	}
}

// Refusals a retry cannot change are tried once.
func TestEnrollDoesNotRetryRefusals(t *testing.T) {
	for _, s := range []struct {
		status int
		code   string
	}{
		{400, api.CodeInvalidRequest}, {401, api.CodeUnauthorized}, {403, api.CodeForbidden}, {404, api.CodeNotFound},
		{409, api.CodeConflict}, {410, api.CodeNotFound}, {413, api.CodePayloadTooLarge}, {422, api.CodeInvalidRequest},
		{500, api.CodeInternal},
	} {
		t.Run(strconv.Itoa(s.status), func(t *testing.T) {
			fc := newFakeConsole(t)
			front := newEnrollFront(t, fc, true, enrollAnswer{status: s.status, code: s.code, retryAfter: 5})
			clk := newRetryClock()
			var log bytes.Buffer
			o, _ := retryOpts(t, front.srv.URL, clk, &log)
			_, err := Enroll(context.Background(), o)
			if err == nil {
				t.Fatal("Enroll succeeded against a refusal")
			}
			if ue := asUplink(t, err); ue.Status != s.status {
				t.Errorf("status = %d, want %d", ue.Status, s.status)
			}
			if n := front.requests(); n != 1 {
				t.Errorf("requests = %d, want 1: a refusal is not retried", n)
			}
			if w := clk.waits(); len(w) != 0 {
				t.Errorf("slept %v before a refusal", w)
			}
			if log.Len() != 0 {
				t.Errorf("log = %q, want silence", log.String())
			}
		})
	}
}

// The retry budget is honoured on the fake clock: a console that never stops
// answering 429 ends the enrollment with its last error class after about
// three minutes of waiting, and not one retry past the budget.
func TestEnrollRetryDeadlineOnTheFakeClock(t *testing.T) {
	fc := newFakeConsole(t)
	front := newEnrollFront(t, fc, true, enrollAnswer{status: 429, code: api.CodeRateLimited, retryAfter: 20})
	clk := newRetryClock()
	var log bytes.Buffer
	o, dir := retryOpts(t, front.srv.URL, clk, &log)

	_, err := Enroll(context.Background(), o)
	if err == nil {
		t.Fatal("Enroll succeeded against a console that always answers 429")
	}
	if ue := asUplink(t, err); ue.Status != http.StatusTooManyRequests || ue.Class() != "" {
		t.Errorf("last error = %+v, want the 429 itself", ue)
	}
	if !strings.Contains(err.Error(), "gave up after") {
		t.Errorf("error %q does not say it gave up", err)
	}
	if total := clk.total(); total > EnrollRetryBudget || total < EnrollRetryBudget-20*time.Second {
		t.Errorf("waited %s in total, want just under the %s budget", total, EnrollRetryBudget)
	}
	// 20 s waits against 180 s: attempts at 0, 20, ..., 180 s.
	if n := front.requests(); n != 10 {
		t.Errorf("requests = %d, want 10", n)
	}
	if !strings.Contains(log.String(), "retry budget is spent") {
		t.Errorf("log = %q, want the budget message", log.String())
	}
	// The failed enrollment left the nonce, not a device.
	cfg, lerr := config.Load(config.Layout{Root: dir}.ConfigFile())
	if lerr != nil || cfg.Enrolled() || cfg.EnrollNonce == "" {
		t.Errorf("config after the failure: enrolled %v, nonce %q, err %v", cfg.Enrolled(), cfg.EnrollNonce, lerr)
	}
}

// A negative budget disables retries.
func TestEnrollRetriesCanBeDisabled(t *testing.T) {
	fc := newFakeConsole(t)
	front := newEnrollFront(t, fc, true, enrollAnswer{status: 429, code: api.CodeRateLimited, retryAfter: 1})
	clk := newRetryClock()
	o, _ := retryOpts(t, front.srv.URL, clk, nil)
	o.RetryBudget = -1
	if _, err := Enroll(context.Background(), o); err == nil {
		t.Fatal("Enroll succeeded")
	}
	if n := front.requests(); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

// Every attempt, whatever it was answered with, carries the nonce that was on
// disk before the first request, and a response lost after the console stored
// the enrollment is retried into the same device (a transport error retries).
func TestEnrollNonceIsIdenticalAcrossAttempts(t *testing.T) {
	fc := newFakeConsole(t)
	fc.dropEnrollOnce.Store(true) // the console stores the enrollment, the response is lost
	front := newEnrollFront(t, fc, false,
		enrollAnswer{status: 429, code: api.CodeRateLimited, retryAfter: 1},
		enrollAnswer{status: 502, code: api.CodeInternal},
		enrollAnswer{drop: true}, // a cut connection before any answer
	)
	clk := newRetryClock()
	o, dir := retryOpts(t, front.srv.URL, clk, nil)

	res, err := Enroll(context.Background(), o)
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	cfg, err := config.Load(config.Layout{Root: dir}.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	seen := front.seenNonces()
	if len(seen) != 5 { // 429, 502, cut, lost response, success
		t.Fatalf("attempts = %d (%v), want 5", len(seen), seen)
	}
	for i, n := range seen {
		if n != cfg.EnrollNonce || len(n) != 32 {
			t.Errorf("attempt %d sent nonce %q, want the persisted %q", i+1, n, cfg.EnrollNonce)
		}
	}
	fc.mu.Lock()
	devices, issued := len(fc.nonces), fc.tokensIssued
	fc.mu.Unlock()
	if devices != 1 || res.DeviceID != testDeviceID {
		t.Errorf("devices = %d, id = %q: retries must never create a second device", devices, res.DeviceID)
	}
	if issued != 2 { // the lost response and the success; the 429, 502 and cut never reached the console's enroll
		t.Errorf("device tokens issued = %d, want 2", issued)
	}
}

// Only the parent context ends a retry early, and it ends it at once.
func TestEnrollRetryStopsWhenTheContextEnds(t *testing.T) {
	fc := newFakeConsole(t)
	front := newEnrollFront(t, fc, true, enrollAnswer{status: 429, code: api.CodeRateLimited, retryAfter: 1})
	clk := newRetryClock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clk.cancel = cancel
	o, _ := retryOpts(t, front.srv.URL, clk, nil)

	_, err := Enroll(ctx, o)
	if err == nil {
		t.Fatal("Enroll succeeded")
	}
	if ue := asUplink(t, err); ue.Status != 429 {
		t.Errorf("error = %v, want the last console answer", err)
	}
	if !strings.Contains(err.Error(), "retry stopped") {
		t.Errorf("error %q does not say the retry was stopped", err)
	}
	if n := front.requests(); n != 1 {
		t.Errorf("requests = %d, want 1", n)
	}
}

func TestEnrollRetryClass(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"429", &uplink.Error{Status: 429}, true},
		{"503", &uplink.Error{Status: 503}, true},
		{"502", &uplink.Error{Status: 502}, true},
		{"504", &uplink.Error{Status: 504}, true},
		{"network", &uplink.Error{Network: true}, true},
		{"wrapped 429", errors.Join(errors.New("x"), &uplink.Error{Status: 429}), true},
		{"tls", &uplink.Error{TLS: true}, false},
		{"401", &uplink.Error{Status: 401}, false},
		{"403", &uplink.Error{Status: 403}, false},
		{"404", &uplink.Error{Status: 404}, false},
		{"409", &uplink.Error{Status: 409}, false},
		{"410", &uplink.Error{Status: 410}, false},
		{"422", &uplink.Error{Status: 422}, false},
		{"500", &uplink.Error{Status: 500}, false},
		{"malformed response", &uplink.Error{Status: 200, Code: "malformed_response"}, false},
		{"path leak", uplink.ErrPathLeak, false},
		{"too large", uplink.ErrTooLarge, false},
		{"context canceled", context.Canceled, false},
		{"plain", errors.New("boom"), false},
		{"nil", nil, false},
	} {
		if got := enrollRetryClass(c.err); got != c.want {
			t.Errorf("%s: retry = %v, want %v", c.name, got, c.want)
		}
	}
}
