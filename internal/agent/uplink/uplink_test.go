package uplink

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

var now = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

const (
	devToken   = "device-token-value"
	enrollTok  = "enrollment-token-value"
	deviceULID = "01JB0000000000000000000001"
)

func clientFor(t *testing.T, srv *httptest.Server, mutate func(*Options)) *Client {
	t.Helper()
	o := Options{
		BaseURL: srv.URL, Token: func() string { return devToken }, AgentVersion: "test-1.0",
		InsecureLab: true, Clock: func() time.Time { return now },
	}
	if mutate != nil {
		mutate(&o)
	}
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func stamp(w http.ResponseWriter, server time.Time) {
	w.Header().Set(api.HeaderServerTime, api.FormatServerTime(server))
}

func problem(w http.ResponseWriter, code string, retryAfter string) {
	e := api.NewError(code)
	e.Title = `attacker text C:\Users\PHI-sentinel\x.docx` // wire text the client must never repeat
	e.RequestID = "req-123"
	stamp(w, now)
	w.Header().Set("Content-Type", api.ContentTypeProblem)
	if retryAfter != "" {
		w.Header().Set(api.HeaderRetryAfter, retryAfter)
	}
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(e)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	stamp(w, now)
	w.Header().Set("Content-Type", api.ContentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestEnrollHappyPathAndNonceRetry(t *testing.T) {
	var mu sync.Mutex
	devices := map[string]string{} // nonce -> device id (idempotent on the nonce)
	tokens := 0
	var lastAuth, lastAgent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != api.RouteEnroll {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		lastAuth, lastAgent = r.Header.Get("Authorization"), r.Header.Get(api.HeaderAgentVersion)
		if lastAuth != "Bearer "+enrollTok {
			problem(w, api.CodeUnauthorized, "")
			return
		}
		var req api.EnrollRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Validate() != nil {
			problem(w, api.CodeInvalidRequest, "")
			return
		}
		id, ok := devices[req.Nonce]
		if !ok {
			id = deviceULID
			devices[req.Nonce] = id
		}
		tokens++ // a retry gets a fresh token; the earlier one is revoked
		writeJSON(w, 200, api.EnrollResponse{
			DeviceID: id, DeviceToken: api.Secret("tok-" + string(rune('0'+tokens))),
			Gateway:      api.Gateway{URL: "https://gw.example", Username: id, Password: "gwpw"},
			RepoPassword: "repopw", Custody: api.CustodyEscrowed,
			Policy:     api.PolicyResponse{Policy: policy.Default(policy.ClassDesktop), Version: 1, ETag: `"e1"`, ServerTime: now},
			ServerTime: now,
		})
	}))
	defer srv.Close()
	c := clientFor(t, srv, func(o *Options) { o.Token = nil })

	req := api.EnrollRequest{
		Nonce: strings.Repeat("ab", 16), Hostname: "pc1", OS: "windows", OSVersion: "10.0.26300", Arch: api.ArchAMD64,
		DeviceClass: api.DeviceClassDesktop, HWFingerprint: strings.Repeat("cd", 16), AgentVersion: "test-1.0", ResticVersion: "0.19.1",
	}
	first, err := c.Enroll(context.Background(), enrollTok, req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Enroll(context.Background(), enrollTok, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.DeviceID != second.DeviceID {
		t.Errorf("a retry with the same nonce created another device: %s then %s", first.DeviceID, second.DeviceID)
	}
	if first.DeviceToken == second.DeviceToken {
		t.Error("the retry should have returned a fresh device token")
	}
	if string(second.DeviceToken) != "tok-2" || string(second.Gateway.Password) != "gwpw" || string(second.RepoPassword) != "repopw" {
		t.Errorf("secrets did not decode: %q %q %q", second.DeviceToken, second.Gateway.Password, second.RepoPassword)
	}
	if lastAgent != "test-1.0" {
		t.Errorf("agent version header = %q", lastAgent)
	}

	// A wrong enrollment token is an auth failure, not a network failure.
	_, err = c.Enroll(context.Background(), "wrong", req)
	var ue *Error
	if !errors.As(err, &ue) || ue.Status != 401 || ue.Class() != restic.ClassAuth {
		t.Fatalf("err = %v", err)
	}
}

func TestPolicy200Then304(t *testing.T) {
	var gotINM []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+devToken {
			problem(w, api.CodeUnauthorized, "")
			return
		}
		inm := r.Header.Get(api.HeaderIfNoneMatch)
		gotINM = append(gotINM, inm)
		if inm == `"e1"` {
			stamp(w, now)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set(api.HeaderETag, `"e1"`)
		writeJSON(w, 200, api.PolicyResponse{Policy: policy.Default(policy.ClassLaptop), Version: 4, ETag: `"e1"`, ServerTime: now})
	}))
	defer srv.Close()
	c := clientFor(t, srv, nil)

	resp, changed, err := c.PullPolicy(context.Background(), "")
	if err != nil || !changed || resp.Version != 4 || resp.ETag != `"e1"` {
		t.Fatalf("first pull: %+v changed=%v err=%v", resp, changed, err)
	}
	if resp.Policy.Conditions.BatteryFloorPct != 30 {
		t.Error("the policy body did not decode")
	}
	_, changed, err = c.PullPolicy(context.Background(), resp.ETag)
	if err != nil || changed {
		t.Fatalf("second pull: changed=%v err=%v, want a 304", changed, err)
	}
	if len(gotINM) != 2 || gotINM[0] != "" || gotINM[1] != `"e1"` {
		t.Errorf("If-None-Match sequence = %q", gotINM)
	}
}

func TestHeartbeatRetryAfterAndClockOffset(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			problem(w, api.CodeRateLimited, "37")
		case 2:
			problem(w, api.CodeUnavailable, now.Add(90*time.Second).UTC().Format(http.TimeFormat))
		default:
			stamp(w, now.Add(-3*time.Second)) // the console is 3 s behind the device
			writeJSON2(w, api.HeartbeatResponse{ServerTime: now.Add(-3 * time.Second), PolicyETag: `"e9"`, CommandsPending: true})
		}
	}))
	defer srv.Close()
	c := clientFor(t, srv, nil)
	hb := api.HeartbeatRequest{AgentVersion: "t", DeviceTime: now, ServiceState: api.ServiceIdle, PolicyETag: `"e1"`}

	_, err := c.Heartbeat(context.Background(), hb)
	var ue *Error
	if !errors.As(err, &ue) || ue.Status != 429 || ue.Code != api.CodeRateLimited || ue.RetryAfter != 37*time.Second {
		t.Fatalf("429: %+v (%v)", ue, err)
	}
	if strings.Contains(err.Error(), "PHI-sentinel") || strings.Contains(err.Error(), "attacker") || strings.Contains(err.Error(), "req-123") {
		t.Errorf("wire text leaked into the error string: %q", err.Error())
	}
	bo := NewBackoff()
	if d := bo.Delay(err); d != 37*time.Second {
		t.Errorf("Delay = %v, want Retry-After honoured exactly (37 s)", d)
	}

	_, err = c.Heartbeat(context.Background(), hb)
	if !errors.As(err, &ue) || ue.Status != 503 || ue.RetryAfter != 90*time.Second {
		t.Fatalf("503 with an HTTP date: %+v (%v)", ue, err)
	}

	resp, err := c.Heartbeat(context.Background(), hb)
	if err != nil || !resp.CommandsPending || resp.PolicyETag != `"e9"` {
		t.Fatalf("heartbeat: %+v %v", resp, err)
	}
	if off, ok := c.OffsetMS(); !ok || off != 3000 {
		t.Errorf("clock offset = %d ms (known %v), want device minus server = +3000", off, ok)
	}
}

func writeJSON2(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", api.ContentTypeJSON)
	_ = json.NewEncoder(w).Encode(v)
}

func TestNoOffsetBeforeFirstResponse(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c := clientFor(t, srv, nil)
	if _, ok := c.OffsetMS(); ok {
		t.Error("an offset is reported before any response")
	}
}

func TestRunsUpload(t *testing.T) {
	var got api.RunsRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		out := api.RunsResponse{ServerTime: now}
		for i, run := range got.Runs {
			st := []api.RunStatus{api.RunCreated, api.RunUpdated, api.RunDuplicate, api.RunRejected}[i%4]
			res := api.RunResult{ID: run.ID, Status: st}
			if st == api.RunRejected {
				res.Reason = api.ReasonInvalid
			}
			out.Results = append(out.Results, res)
		}
		writeJSON(w, 200, out)
	}))
	defer srv.Close()
	c := clientFor(t, srv, nil)
	var req api.RunsRequest
	for i := 0; i < 4; i++ {
		req.Runs = append(req.Runs, api.RunRecord{ID: "01JB000000000000000000000" + string(rune('A'+i)), Kind: restic.KindBackup,
			Trigger: api.TriggerSchedule, StartedAt: now, Attempt: 1})
	}
	resp, err := c.UploadRuns(context.Background(), req)
	if err != nil || len(resp.Results) != 4 {
		t.Fatalf("%+v %v", resp, err)
	}
	if resp.Results[3].Status != api.RunRejected || resp.Results[3].Reason != api.ReasonInvalid || resp.Results[2].Status != api.RunDuplicate {
		t.Errorf("results = %+v", resp.Results)
	}
}

func TestCommandPollIsSingleFlight(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if got := r.URL.Query().Get(api.QueryWait); n == 1 && got != "30" {
			t.Errorf("wait = %q, want 30", got)
		}
		<-release
		writeJSON(w, 200, api.CommandsResponse{ServerTime: now, Commands: []api.Command{{
			ID: "01JB0000000000000000000009", Type: api.CommandCheckIn, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}}})
	}))
	defer srv.Close()
	c := clientFor(t, srv, nil)

	type result struct {
		resp api.CommandsResponse
		err  error
	}
	first := make(chan result, 1)
	go func() {
		r, err := c.PollCommands(context.Background(), 30*time.Second)
		first <- result{r, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := c.PollCommands(context.Background(), 30*time.Second); !errors.Is(err, ErrPollInFlight) {
		t.Fatalf("second concurrent poll: %v, want ErrPollInFlight", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("the second poll reached the server (%d requests)", hits.Load())
	}
	close(release)
	r := <-first
	if r.err != nil || len(r.resp.Commands) != 1 || r.resp.Commands[0].Type != api.CommandCheckIn {
		t.Fatalf("first poll: %+v %v", r.resp, r.err)
	}
	// After it returns, a new poll is allowed again.
	if _, err := c.PollCommands(context.Background(), 0); err != nil {
		t.Fatalf("a poll after the first finished: %v", err)
	}
}

func TestCommandResult(t *testing.T) {
	var path string
	var body api.CommandResultRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		stamp(w, now)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := clientFor(t, srv, nil)
	err := c.CommandResult(context.Background(), "01JB0000000000000000000009", api.CommandResultRequest{
		Status: api.CommandFailed, ErrorClass: restic.ClassUnknown, Result: json.RawMessage(`{"reason":"not_implemented_m0"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if path != api.PathPrefix+"/commands/01JB0000000000000000000009/result" || body.Status != api.CommandFailed {
		t.Errorf("path %q body %+v", path, body)
	}
	// The result of a command is capped.
	huge := json.RawMessage(`{"a":"` + strings.Repeat("x", 70<<10) + `"}`)
	if err := c.CommandResult(context.Background(), "01JB0000000000000000000009", api.CommandResultRequest{Status: api.CommandSucceeded, Result: huge}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("an oversized body: %v, want ErrTooLarge", err)
	}
}

func TestAssertPathFreeStopsTheRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	c := clientFor(t, srv, nil)

	leaky := api.RunsRequest{Runs: []api.RunRecord{{
		ID: "01JB0000000000000000000001", Kind: restic.KindBackup, Trigger: api.TriggerSchedule, StartedAt: now, Attempt: 1,
		AgentVersion: `built at C:\Users\PHI-sentinel-Jane\secret.docx`,
	}}}
	_, err := c.UploadRuns(context.Background(), leaky)
	if !errors.Is(err, ErrPathLeak) {
		t.Fatalf("err = %v, want ErrPathLeak", err)
	}
	if strings.Contains(err.Error(), "PHI-sentinel") {
		t.Errorf("the refusal repeats the value: %q", err.Error())
	}
	if hits.Load() != 0 {
		t.Fatalf("the server received %d requests; a path-bearing body must never be sent", hits.Load())
	}
	// Every other body type is checked the same way.
	if _, err := c.Heartbeat(context.Background(), api.HeartbeatRequest{AgentVersion: `/home/jane/secret/notes`, DeviceTime: now, ServiceState: api.ServiceIdle}); !errors.Is(err, ErrPathLeak) {
		t.Errorf("heartbeat: %v", err)
	}
	if err := c.CommandResult(context.Background(), "01JB0000000000000000000009", api.CommandResultRequest{
		Status: api.CommandSucceeded, Result: json.RawMessage(`{"where":"C:\\Users\\jane"}`)}); !errors.Is(err, ErrPathLeak) {
		t.Errorf("command result: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("the server received %d requests", hits.Load())
	}
}

func TestProblemDocuments(t *testing.T) {
	var code string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code == "garbage" {
			w.WriteHeader(502)
			_, _ = w.Write([]byte("<html>bad gateway C:\\secret</html>"))
			return
		}
		e := api.NewError(code)
		e.Code = code
		w.Header().Set("Content-Type", api.ContentTypeProblem)
		w.WriteHeader(e.Status)
		_ = json.NewEncoder(w).Encode(e)
	}))
	defer srv.Close()
	c := clientFor(t, srv, nil)
	for _, tc := range []struct {
		code   string
		status int
		want   string
		class  restic.ErrorClass
	}{
		{api.CodeUnauthorized, 401, api.CodeUnauthorized, restic.ClassAuth},
		{api.CodeForbidden, 403, api.CodeForbidden, restic.ClassAuth},
		{api.CodeConflict, 409, api.CodeConflict, ""},
		{api.CodeUnavailable, 503, api.CodeUnavailable, restic.ClassNetwork},
		{"made_up_by_a_newer_console", 500, "", restic.ClassNetwork},
		{"garbage", 502, "", restic.ClassNetwork},
	} {
		code = tc.code
		_, err := c.Heartbeat(context.Background(), api.HeartbeatRequest{AgentVersion: "t", DeviceTime: now, ServiceState: api.ServiceIdle})
		var ue *Error
		if !errors.As(err, &ue) {
			t.Fatalf("%s: %v", tc.code, err)
		}
		if ue.Status != tc.status || ue.Code != tc.want || ue.Class() != tc.class {
			t.Errorf("%s: status %d code %q class %q, want %d %q %q", tc.code, ue.Status, ue.Code, ue.Class(), tc.status, tc.want, tc.class)
		}
		if strings.Contains(err.Error(), "made_up") || strings.Contains(err.Error(), "secret") {
			t.Errorf("wire text in the error string: %q", err.Error())
		}
	}
}

func TestNetworkAndTLSErrors(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		c := clientFor(t, srv, nil)
		srv.Close()
		_, err := c.Heartbeat(context.Background(), api.HeartbeatRequest{AgentVersion: "t", DeviceTime: now, ServiceState: api.ServiceIdle})
		var ue *Error
		if !errors.As(err, &ue) || !ue.Network || ue.Class() != restic.ClassNetwork || ue.Unwrap() == nil {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("untrusted certificate is a tls failure unless the lab flag is set", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, api.HeartbeatResponse{ServerTime: now})
		}))
		defer srv.Close()
		hb := api.HeartbeatRequest{AgentVersion: "t", DeviceTime: now, ServiceState: api.ServiceIdle}
		strict := clientFor(t, srv, func(o *Options) { o.InsecureLab = false })
		_, err := strict.Heartbeat(context.Background(), hb)
		var ue *Error
		if !errors.As(err, &ue) || !ue.TLS || ue.Class() != restic.ClassTLS {
			t.Fatalf("strict client: %v", err)
		}
		lab := clientFor(t, srv, nil)
		if _, err := lab.Heartbeat(context.Background(), hb); err != nil {
			t.Fatalf("lab client: %v", err)
		}
	})
	t.Run("an SPKI pin is enforced", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, api.HeartbeatResponse{ServerTime: now})
		}))
		defer srv.Close()
		sum := sha256.Sum256(srv.Certificate().RawSubjectPublicKeyInfo)
		good := base64.StdEncoding.EncodeToString(sum[:])
		bad := base64.StdEncoding.EncodeToString(make([]byte, sha256.Size))
		hb := api.HeartbeatRequest{AgentVersion: "t", DeviceTime: now, ServiceState: api.ServiceIdle}
		if _, err := clientFor(t, srv, func(o *Options) { o.SPKIPin = good }).Heartbeat(context.Background(), hb); err != nil {
			t.Fatalf("matching pin: %v", err)
		}
		_, err := clientFor(t, srv, func(o *Options) { o.SPKIPin = bad }).Heartbeat(context.Background(), hb)
		var ue *Error
		if !errors.As(err, &ue) || !ue.TLS {
			t.Fatalf("mismatching pin: %v", err)
		}
	})
	t.Run("a malformed pin is refused at construction", func(t *testing.T) {
		if _, err := New(Options{BaseURL: "https://x.example", SPKIPin: "not-base64!"}); err == nil {
			t.Error("accepted a malformed pin")
		}
	})
}

func TestConstructionRules(t *testing.T) {
	for _, tc := range []struct {
		url string
		lab bool
		ok  bool
	}{
		{"https://console.example:8443", false, true},
		{"http://127.0.0.1:1", false, false},
		{"http://127.0.0.1:1", true, true},
		{"ftp://x", true, false},
		{"https://user:pw@x.example", false, false},
		{"https://x.example/?a=b", false, false},
		{"", false, false},
	} {
		_, err := New(Options{BaseURL: tc.url, InsecureLab: tc.lab})
		if (err == nil) != tc.ok {
			t.Errorf("New(%q, lab=%v) err = %v", tc.url, tc.lab, err)
		}
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer srv.Close()
	_, err := clientFor(t, srv, nil).Heartbeat(context.Background(), api.HeartbeatRequest{AgentVersion: "t", DeviceTime: now, ServiceState: api.ServiceIdle})
	var ue *Error
	if !errors.As(err, &ue) || ue.Status != 302 || hits.Load() != 0 {
		t.Fatalf("err = %v, redirect target hits = %d (the bearer token must not follow a redirect)", err, hits.Load())
	}
}

func TestNoTokenIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c := clientFor(t, srv, func(o *Options) { o.Token = func() string { return "" } })
	if _, err := c.Heartbeat(context.Background(), api.HeartbeatRequest{}); !errors.Is(err, ErrNoToken) {
		t.Fatalf("err = %v", err)
	}
}

func TestBackoff(t *testing.T) {
	b := NewBackoff()
	b.Rand = func() float64 { return 0.5 } // the middle of the spread: factor 1.0
	var got []time.Duration
	for i := 0; i < 8; i++ {
		got = append(got, b.Next())
	}
	want := []time.Duration{1, 2, 4, 5, 5, 5, 5, 5} // capped at 5 min (ADR 0018)
	for i, w := range want {
		if got[i] != w*time.Minute {
			t.Fatalf("step %d = %v, want %v (sequence %v)", i, got[i], w*time.Minute, got)
		}
	}
	if b.Failures() != 8 {
		t.Errorf("failures = %d", b.Failures())
	}
	b.Reset()
	if d := b.Next(); d != time.Minute {
		t.Errorf("after Reset: %v, want 1m", d)
	}
	// Jitter stays inside +-20 % and never above the cap.
	lo := &Backoff{Min: BackoffMin, Max: BackoffMax, Jitter: 0.2, Rand: func() float64 { return 0 }}
	if d := lo.Next(); d != 48*time.Second {
		t.Errorf("low jitter = %v, want 48s", d)
	}
	hi := &Backoff{Min: BackoffMin, Max: BackoffMax, Jitter: 0.2, Rand: func() float64 { return 0.9999999 }}
	for i := 0; i < 12; i++ {
		if d := hi.Next(); d > BackoffMax {
			t.Fatalf("delay %v above the cap", d)
		}
	}
	if d := (&Backoff{Min: time.Minute, Max: time.Hour, Rand: func() float64 { return 0.5 }}).DelayFor(3); d != 8*time.Minute {
		t.Errorf("DelayFor(3) = %v, want 8m", d)
	}
}
