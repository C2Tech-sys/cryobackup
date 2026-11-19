package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/buildinfo"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// The test URL carries a secret in its path and its query. No log line, error, job row, fact or
// banner word may carry either.
const (
	hbSecretPath  = "/ping/s3cr3t-token-4f9a"
	hbSecretQuery = "key=q9w8e7r6"
)

type hbRig struct {
	st  *store.Store
	now *time.Time
}

func newHBRig(t *testing.T) *hbRig {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	r := &hbRig{now: &now}
	st, err := store.OpenMemory(context.Background(), func() time.Time { return *r.now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	r.st = st
	return r
}

// evaluator records the alarm-evaluator's newest run, as the runner would.
func (r *hbRig) evaluator(t *testing.T, status string, finishedAgo time.Duration) {
	t.Helper()
	at := r.now.Add(-finishedAgo)
	if err := r.st.RecordJobRun(context.Background(), NameAlarmEvaluator, store.JobRun{Status: status, StartedAt: at, FinishedAt: at}); err != nil {
		t.Fatal(err)
	}
}

// job builds a ping that started an hour ago (past the start-up wait) against url.
func (r *hbRig) job(url string) *HeartbeatPing {
	return NewHeartbeatPing(r.st, url, "", 0, r.now.Add(-time.Hour))
}

func classOfErr(err error) string {
	var ce *ClassError
	if errors.As(err, &ce) {
		return ce.Class
	}
	return ""
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func hbServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func secretURL(base string) string { return base + hbSecretPath + "?" + hbSecretQuery }

// leaks reports whether s carries the secret path or query.
func leaks(s string) bool {
	return strings.Contains(s, "s3cr3t") || strings.Contains(s, "q9w8e7r6")
}

func TestM4HeartbeatPingRequestShape(t *testing.T) {
	rig := newHBRig(t)
	rig.evaluator(t, "ok", 30*time.Second)
	type seen struct {
		method, ua, cookie, auth, path, query string
		body                                  string
		length                                int64
	}
	var got atomic.Value
	srv, hits := hbServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.Store(seen{r.Method, r.UserAgent(), r.Header.Get("Cookie"), r.Header.Get("Authorization"), r.URL.Path, r.URL.RawQuery, string(b), r.ContentLength})
	})
	for _, tc := range []struct{ method, want string }{{"", "GET"}, {"GET", "GET"}, {"HEAD", "HEAD"}, {"POST", "POST"}} {
		before := hits.Load()
		j := NewHeartbeatPing(rig.st, secretURL(srv.URL), tc.method, 0, rig.now.Add(-time.Hour))
		if err := j.Run(context.Background()); err != nil {
			t.Fatalf("method %q: class %q", tc.method, classOfErr(err))
		}
		if hits.Load() != before+1 {
			t.Fatalf("method %q: %d requests", tc.method, hits.Load()-before)
		}
		s := got.Load().(seen)
		if s.method != tc.want || s.path != hbSecretPath || s.query != hbSecretQuery {
			t.Fatalf("method %q: sent %s to the wrong place", tc.method, s.method)
		}
		if s.ua != "CryoBackup/"+buildinfo.Version {
			t.Fatalf("user agent %q", s.ua)
		}
		if s.cookie != "" || s.auth != "" || s.body != "" || s.length > 0 {
			t.Fatalf("method %q: the request carried a cookie, credentials or a body", tc.method)
		}
	}
}

// A 2xx is success whatever the body, and no more than 4 KiB of it is read.
func TestM4HeartbeatPingBodyIsDiscardedAndBounded(t *testing.T) {
	rig := newHBRig(t)
	rig.evaluator(t, "ok", 0)
	var read atomic.Int64
	j := rig.job("https://monitor.example.test" + hbSecretPath)
	j.Transport = rtFunc(func(r *http.Request) (*http.Response, error) {
		body := io.NopCloser(readerFunc(func(p []byte) (int, error) {
			read.Add(int64(len(p)))
			for i := range p {
				p[i] = 'x'
			}
			return len(p), nil
		}))
		return &http.Response{StatusCode: 200, Body: body, Header: http.Header{}, Request: r}, nil
	})
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("class %q", classOfErr(err))
	}
	if n := read.Load(); n > 8<<10 { // the 4 KiB cap, plus what one buffered read may hand over
		t.Fatalf("read %d bytes of the body", n)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestM4HeartbeatPingFailureClasses(t *testing.T) {
	ctx := context.Background()
	status := func(code int) func(*testing.T, *hbRig) (*HeartbeatPing, func()) {
		return func(t *testing.T, r *hbRig) (*HeartbeatPing, func()) {
			srv, _ := hbServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
			return r.job(secretURL(srv.URL)), nil
		}
	}
	cases := []struct {
		name  string
		want  string
		build func(*testing.T, *hbRig) (*HeartbeatPing, func())
	}{
		{"status_4xx_404", "status_4xx", status(404)},
		{"status_4xx_429", "status_4xx", status(429)},
		{"status_5xx_500", "status_5xx", status(500)},
		{"status_5xx_503", "status_5xx", status(503)},
		{"status_outside_every_class_is_other_not_success", "other", func(t *testing.T, r *hbRig) (*HeartbeatPing, func()) {
			j := r.job(secretURL("https://monitor.example.test"))
			j.Transport = rtFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 600, Body: http.NoBody, Header: http.Header{}, Request: req}, nil
			})
			return j, nil
		}},
		{"timeout", "timeout", func(t *testing.T, r *hbRig) (*HeartbeatPing, func()) {
			srv, _ := hbServer(t, func(w http.ResponseWriter, req *http.Request) { <-req.Context().Done() })
			j := r.job(secretURL(srv.URL))
			j.Timeout = 150 * time.Millisecond
			return j, nil
		}},
		{"refused", "refused", func(t *testing.T, r *hbRig) (*HeartbeatPing, func()) {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := l.Addr().String()
			_ = l.Close()
			return r.job(secretURL("http://" + addr)), nil
		}},
		{"tls", "tls", func(t *testing.T, r *hbRig) (*HeartbeatPing, func()) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			t.Cleanup(srv.Close)
			return r.job(secretURL(srv.URL)), nil // the default transport does not trust the test certificate
		}},
		{"dns", "dns", func(t *testing.T, r *hbRig) (*HeartbeatPing, func()) {
			j := r.job(secretURL("https://monitor.example.test"))
			j.Transport = rtFunc(func(*http.Request) (*http.Response, error) {
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "monitor.example.test", IsNotFound: true}}
			})
			return j, nil
		}},
		{"other", "other", func(t *testing.T, r *hbRig) (*HeartbeatPing, func()) {
			j := r.job(secretURL("https://monitor.example.test"))
			j.Transport = rtFunc(func(req *http.Request) (*http.Response, error) {
				return nil, errors.New("boom " + req.URL.String()) // an error that quotes the URL, as some do
			})
			return j, nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newHBRig(t)
			rig.evaluator(t, "ok", 0)
			j, _ := tc.build(t, rig)
			err := j.Run(ctx)
			if got := classOfErr(err); got != tc.want {
				t.Fatalf("class %q, want %q", got, tc.want)
			}
			f := j.Facts()
			if f.ConsecutiveFailures != 1 || f.LastFailureClass != tc.want || f.LastSuccessAt != 0 || f.Withheld || !f.Configured {
				t.Fatalf("facts %+v", f)
			}
			assertNoSecret(t, "the error", err)
		})
	}
}

func assertNoSecret(t *testing.T, what string, err error) {
	t.Helper()
	for e := err; e != nil; e = errors.Unwrap(e) {
		if leaks(e.Error()) {
			t.Fatalf("%s carries the secret in its text", what)
		}
	}
	if leaks(fmt.Sprintf("%v|%+v|%#v", err, err, err)) {
		t.Fatalf("%s carries the secret in a formatted form", what)
	}
}

// A redirect is not followed: the target is never requested and the class says redirect.
func TestM4HeartbeatPingRedirectIsNotFollowed(t *testing.T) {
	rig := newHBRig(t)
	rig.evaluator(t, "ok", 0)
	target, targetHits := hbServer(t, func(http.ResponseWriter, *http.Request) {})
	for _, code := range []int{301, 302, 307, 308} {
		first, _ := hbServer(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/elsewhere", code)
		})
		err := rig.job(secretURL(first.URL)).Run(context.Background())
		if classOfErr(err) != "redirect" {
			t.Fatalf("%d: class %q", code, classOfErr(err))
		}
	}
	if targetHits.Load() != 0 {
		t.Fatalf("the redirect was followed %d times", targetHits.Load())
	}
}

func TestM4HeartbeatPingTotalTimeoutIsTenSeconds(t *testing.T) {
	if HeartbeatPingTimeout != 10*time.Second {
		t.Fatalf("timeout %v", HeartbeatPingTimeout)
	}
	rig := newHBRig(t)
	j := rig.job("https://monitor.example.test/x")
	var deadline time.Duration
	j.Transport = rtFunc(func(r *http.Request) (*http.Response, error) {
		if d, ok := r.Context().Deadline(); ok {
			deadline = time.Until(d)
		}
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}, Request: r}, nil
	})
	rig.evaluator(t, "ok", 0)
	if err := j.Run(context.Background()); err != nil {
		t.Fatal(classOfErr(err))
	}
	if deadline <= 9*time.Second || deadline > 10*time.Second {
		t.Fatalf("request deadline %v from now", deadline)
	}
}

// https only, unless the host is loopback. A refused URL sends nothing and says bad_config.
func TestM4HeartbeatPingSchemeRule(t *testing.T) {
	srv, hits := hbServer(t, func(http.ResponseWriter, *http.Request) {})
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	for _, tc := range []struct {
		url   string
		sends bool
	}{
		{fmt.Sprintf("http://127.0.0.1:%d/p", port), true},
		{fmt.Sprintf("http://localhost:%d/p", port), true},
		{"http://monitor.example.test/p", false},
		{"http://10.0.0.5/p", false},
		{"ftp://127.0.0.1/p", false},
		{"monitor.example.test/p", false},
		{"https:///p", false},
		{"", false},
	} {
		rig := newHBRig(t)
		rig.evaluator(t, "ok", 0)
		j := rig.job(tc.url)
		j.Transport = http.DefaultTransport
		before := hits.Load()
		err := j.Run(context.Background())
		switch {
		case tc.url == "":
			if !errors.Is(err, errDidNotRun) {
				t.Fatalf("empty URL: %v", err)
			}
		case tc.sends && err != nil:
			t.Fatalf("%q refused: %q", tc.url, classOfErr(err))
		case !tc.sends && classOfErr(err) != "bad_config":
			t.Fatalf("%q: class %q", tc.url, classOfErr(err))
		}
		if sent := hits.Load() - before; (sent == 1) != tc.sends {
			t.Fatalf("%q: %d requests", tc.url, sent)
		}
	}
}

// The ping goes out only while the evaluator's newest pass succeeded and is not older than three
// of its periods; the withheld pass is an error row so the System page shows why; the ping resumes.
func TestM4HeartbeatPingWithheldWhileEvaluatorUnhealthyThenResumes(t *testing.T) {
	rig := newHBRig(t)
	ctx := context.Background()
	srv, hits := hbServer(t, func(http.ResponseWriter, *http.Request) {})
	j := rig.job(secretURL(srv.URL))

	// Never ran, an hour after this console started: withheld, and that is the alarm.
	if err := j.Run(ctx); classOfErr(err) != ClassEvaluatorUnhealthy || hits.Load() != 0 {
		t.Fatalf("no evaluator row: %q, %d requests", classOfErr(err), hits.Load())
	}
	if f := j.Facts(); !f.Withheld || f.ConsecutiveFailures != 0 {
		t.Fatalf("facts %+v", f)
	}
	rig.evaluator(t, "ok", 30*time.Second)
	if err := j.Run(ctx); err != nil || hits.Load() != 1 {
		t.Fatalf("healthy: %q, %d requests", classOfErr(err), hits.Load())
	}
	if f := j.Facts(); f.Withheld || f.LastSuccessAt != state.Millis(rig.now.UnixMilli()) {
		t.Fatalf("facts after a ping %+v", f)
	}
	rig.evaluator(t, "error", 10*time.Second)
	if err := j.Run(ctx); classOfErr(err) != ClassEvaluatorUnhealthy || hits.Load() != 1 {
		t.Fatalf("failed pass: %q, %d requests", classOfErr(err), hits.Load())
	}
	if f := j.Facts(); !f.Withheld {
		t.Fatalf("facts %+v", f)
	}
	rig.evaluator(t, "ok", 4*time.Minute) // succeeded, but older than three one-minute periods
	if err := j.Run(ctx); classOfErr(err) != ClassEvaluatorUnhealthy || hits.Load() != 1 {
		t.Fatalf("stale pass: %q, %d requests", classOfErr(err), hits.Load())
	}
	rig.evaluator(t, "ok", 3*time.Minute) // exactly three periods is still healthy
	if err := j.Run(ctx); err != nil || hits.Load() != 2 {
		t.Fatalf("three periods old: %q, %d requests", classOfErr(err), hits.Load())
	}
	rig.evaluator(t, "ok", 0)
	if err := j.Run(ctx); err != nil || hits.Load() != 3 {
		t.Fatalf("resumed: %q, %d requests", classOfErr(err), hits.Load())
	}
	if f := j.Facts(); f.Withheld || f.ConsecutiveFailures != 0 {
		t.Fatalf("facts after resuming %+v", f)
	}
}

// A longer evaluator period is honoured: the window is three of ITS periods.
func TestM4HeartbeatPingUsesTheEvaluatorPeriod(t *testing.T) {
	rig := newHBRig(t)
	srv, hits := hbServer(t, func(http.ResponseWriter, *http.Request) {})
	j := rig.job(secretURL(srv.URL))
	j.EvaluatorPeriod = 10 * time.Minute
	rig.evaluator(t, "ok", 25*time.Minute)
	if err := j.Run(context.Background()); err != nil || hits.Load() != 1 {
		t.Fatalf("%q %d", classOfErr(err), hits.Load())
	}
}

// Right after the console starts the evaluator has not finished yet: that is a skipped run, not a
// failure and not a ping, and once its three periods are over it is the failure it looks like.
func TestM4HeartbeatPingWaitsForTheFirstEvaluation(t *testing.T) {
	rig := newHBRig(t)
	ctx := context.Background()
	srv, hits := hbServer(t, func(http.ResponseWriter, *http.Request) {})
	j := NewHeartbeatPing(rig.st, secretURL(srv.URL), "", 0, rig.now.Add(-20*time.Second))
	if err := j.Run(ctx); !errors.Is(err, errSkipped) || hits.Load() != 0 {
		t.Fatalf("start-up: %v, %d requests", classOfErr(err), hits.Load())
	}
	if f := j.Facts(); !f.Withheld {
		t.Fatalf("facts %+v", f)
	}
	rig.evaluator(t, "ok", 0)
	if err := j.Run(ctx); err != nil || hits.Load() != 1 {
		t.Fatalf("after the first evaluation: %q, %d requests", classOfErr(err), hits.Load())
	}
	rig2 := newHBRig(t) // an empty store: no evaluation has ever finished
	late := NewHeartbeatPing(rig2.st, secretURL(srv.URL), "", 0, rig2.now.Add(-10*time.Minute))
	if err := late.Run(ctx); classOfErr(err) != ClassEvaluatorUnhealthy {
		t.Fatalf("no evaluation in ten minutes: %q", classOfErr(err))
	}
}

func TestM4HeartbeatPingStreakAndRecovery(t *testing.T) {
	rig := newHBRig(t)
	ctx := context.Background()
	rig.evaluator(t, "ok", 0)
	var code atomic.Int64
	code.Store(503)
	srv, _ := hbServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(int(code.Load())) })
	j := rig.job(secretURL(srv.URL))
	for i := 1; i <= 4; i++ {
		_ = j.Run(ctx)
		if f := j.Facts(); f.ConsecutiveFailures != i || f.LastFailureClass != "status_5xx" {
			t.Fatalf("pass %d: %+v", i, f)
		}
	}
	// A withheld pass is not a failure: the streak neither grows nor resets.
	rig.evaluator(t, "error", 0)
	_ = j.Run(ctx)
	if f := j.Facts(); f.ConsecutiveFailures != 4 || !f.Withheld {
		t.Fatalf("withheld: %+v", f)
	}
	rig.evaluator(t, "ok", 0)
	code.Store(404)
	_ = j.Run(ctx)
	if f := j.Facts(); f.ConsecutiveFailures != 5 || f.LastFailureClass != "status_4xx" || f.Withheld {
		t.Fatalf("class changed: %+v", f)
	}
	code.Store(204)
	if err := j.Run(ctx); err != nil {
		t.Fatal(classOfErr(err))
	}
	f := j.Facts()
	if f.ConsecutiveFailures != 0 || f.LastFailureClass != "" || f.LastSuccessAt == 0 || !f.Configured {
		t.Fatalf("recovered: %+v", f)
	}
}

func TestM4HeartbeatPingNothingWithoutAURL(t *testing.T) {
	rig := newHBRig(t)
	var hits atomic.Int64
	j := NewHeartbeatPing(rig.st, "", "", 0, rig.now.Add(-time.Hour))
	j.Transport = rtFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return &http.Response{StatusCode: 200, Body: http.NoBody, Request: r}, nil
	})
	if j.Configured() || j.Facts() != (state.HeartbeatFacts{}) {
		t.Fatalf("configured %v facts %+v", j.Configured(), j.Facts())
	}
	r := NewRunner(rig.st, "t", nil)
	if RegisterHeartbeatPing(r, j) || len(r.Names()) != 0 {
		t.Fatalf("registered with no URL: %v", r.Names())
	}
	rig.evaluator(t, "ok", 0)
	r.Add(j.Job())
	if ran, err := r.RunOnce(context.Background(), NameHealthcheckPing); ran || err != nil {
		t.Fatalf("ran %v err %v", ran, err)
	}
	if hits.Load() != 0 {
		t.Fatalf("%d requests with no URL", hits.Load())
	}
}

func TestM4HeartbeatPingRegisters(t *testing.T) {
	rig := newHBRig(t)
	r := NewRunner(rig.st, "t", nil)
	j := NewHeartbeatPing(rig.st, "https://monitor.example.test"+hbSecretPath, "HEAD", 7*time.Minute, rig.now.Add(-time.Hour))
	if !RegisterHeartbeatPing(r, j) {
		t.Fatal("not registered")
	}
	if names := r.Names(); len(names) != 1 || names[0] != NameHealthcheckPing {
		t.Fatalf("names %v", names)
	}
	if got := j.Job(); got.Name != "healthcheck-ping" || got.Every != 7*time.Minute {
		t.Fatalf("job %q every %v", got.Name, got.Every)
	}
	if got := NewHeartbeatPing(rig.st, "https://monitor.example.test/x", "", 0, time.Time{}).Job(); got.Every != DefaultHealthcheckPingEvery {
		t.Fatalf("default period %v", got.Every)
	}
}

// Through the runner: the secret path and query reach no log line, no job row, no error and no
// fact, whichever way the ping fails.
func TestM4HeartbeatPingSecretNeverLeaks(t *testing.T) {
	rig := newHBRig(t)
	ctx := context.Background()
	rig.evaluator(t, "ok", 0)
	var logs bytes.Buffer
	var code atomic.Int64
	code.Store(503)
	srv, _ := hbServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(int(code.Load())) })
	j := rig.job(secretURL(srv.URL))
	r := NewRunner(rig.st, "t", logx.New(&logs, logx.LevelDebug))
	if !RegisterHeartbeatPing(r, j) {
		t.Fatal("not registered")
	}
	steps := []func(){
		func() {},                  // 503
		func() { code.Store(302) }, // redirect
		func() { code.Store(200) }, // success: the recovery is logged
		func() { j.Timeout = time.Nanosecond; code.Store(503) }, // timeout
		func() {
			j.Timeout = 0
			j.Transport = rtFunc(func(req *http.Request) (*http.Response, error) { return nil, errors.New(req.URL.String()) })
		},
		func() { rig.evaluator(t, "error", 0) }, // withheld
	}
	for i, step := range steps {
		step()
		ran, err := r.RunOnce(ctx, NameHealthcheckPing)
		if !ran {
			t.Fatalf("step %d did not run", i)
		}
		assertNoSecret(t, fmt.Sprintf("the error of step %d", i), err)
		row, gerr := rig.st.GetJobState(ctx, NameHealthcheckPing)
		if gerr != nil {
			t.Fatal(gerr)
		}
		if leaks(row.LastErrorClass) || leaks(row.LastStatus) {
			t.Fatalf("step %d: the job row carries the secret", i)
		}
		f := j.Facts()
		if leaks(f.LastFailureClass) {
			t.Fatalf("step %d: the facts carry the secret", i)
		}
	}
	if leaks(logs.String()) {
		t.Fatal("the log carries the secret path or query")
	}
	if strings.Contains(logs.String(), srv.Listener.Addr().String()) {
		t.Fatal("the log carries the ping host")
	}
	if !strings.Contains(logs.String(), "status_5xx") {
		t.Fatalf("the failure class is not in the log: %q", logs.String())
	}
}
