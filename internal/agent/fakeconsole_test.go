package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/policy"
)

const (
	testEnrollToken = "enrollment-token-for-tests"
	testDeviceID    = "01JB0000000000000000000001"
	testRepoSecret  = "repository-password-for-tests"
	testGatewaySec  = "gateway-password-for-tests"
	testCustomerID  = "01JB0000000000000000000C01"
	testPolicyID    = "01JB0000000000000000000P01"
)

// recorded is one request the fake console received, byte for byte.
type recorded struct {
	Method, Path string
	Body         []byte
}

// fakeConsole is an in-memory implementation of the agent API on httptest. It
// records every byte it receives so tests can grep for what must never arrive.
type fakeConsole struct {
	t   *testing.T
	srv *httptest.Server

	mu           sync.Mutex
	deviceToken  string
	tokensIssued int
	nonces       map[string]string // nonce -> device id
	enrollNonces []string
	policy       api.PolicyResponse
	bodies       []recorded
	heartbeats   []api.HeartbeatRequest
	runs         map[string]api.RunRecord // latest record by id
	runOrder     []string
	runPosts     []api.RunsRequest
	snapshots    []api.SnapshotsRequest
	queue        []api.Command
	results      map[string][]api.CommandResultRequest
	policyHits   int
	// resultAnswers: HTTP statuses the next result posts of a command id get,
	// in order, before it answers 204 again (M2-W3C). resultPosts counts every
	// post by command id, whatever the answer.
	resultAnswers map[string][]int
	resultPosts   map[string]int

	down           atomic.Bool // connections are cut: the console is unreachable
	dropEnrollOnce atomic.Bool // the first enroll response is lost after the console stored it
	omitCustomer   atomic.Bool // the enroll response carries no customer id
	forbid         atomic.Bool // every authenticated route answers 403 (a valid token, wrongly used)
	retiredToken   string      // the device token revoke() swapped out, for restore() (under mu)
	rejectRuns     map[string]string

	// Start lease (ADR 0005): leaseMode selects the answer (leaseNotFound, the
	// zero value, is a console that predates the route); leaseReqs records the
	// requests; leaseRelease ends a leaseHang.
	leaseMode    atomic.Int32
	leaseRetryS  atomic.Int32
	leaseReqs    []api.StartLeaseRequest
	leaseRelease chan struct{}
}

// Answers of the fake console's start-lease route.
const (
	leaseNotFound  int32 = iota // 404: a console without the route
	leaseGrant                  // granted with a lease
	leaseUnlimited              // granted without a lease (no cap)
	leaseRefused                // refused with leaseRetryS
	leaseUnavail                // 503
	leaseLimited                // 429
	leaseUnauth                 // 401
	leaseHang                   // never answers (until leaseRelease or the client gives up)
)

func newFakeConsole(t *testing.T) *fakeConsole {
	t.Helper()
	f := &fakeConsole{
		t: t, nonces: map[string]string{}, runs: map[string]api.RunRecord{},
		results: map[string][]api.CommandResultRequest{}, rejectRuns: map[string]string{},
		leaseRelease: make(chan struct{}),
	}
	f.policy = api.PolicyResponse{Policy: testPolicy(), PolicyID: testPolicyID, Version: 1, ETag: `"policy-v1"`, ServerTime: time.Now()}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// testPolicy is the default policy with one explicit root and no waiting.
func testPolicy() policy.Policy {
	p := policy.Default(policy.ClassDesktop)
	p.Selection.Roots = []string{`C:\fixture\tree`}
	p.Selection.Presets = []string{}
	p.Schedule.JitterMinutes = 0
	p.Schedule.StartupDelayS = 0
	return p
}

func (f *fakeConsole) URL() string { return f.srv.URL }

// revoke retires the device: its token no longer matches, so every
// authenticated route answers 401 until restore.
func (f *fakeConsole) revoke() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.retiredToken == "" {
		f.retiredToken = f.deviceToken
	}
	f.deviceToken = "revoked-token-for-tests"
}

// restore puts the token revoke swapped out back (the console was restored, or
// the device row re-created: the same token is valid again).
func (f *fakeConsole) restore() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.retiredToken != "" {
		f.deviceToken, f.retiredToken = f.retiredToken, ""
	}
}

func (f *fakeConsole) setPolicy(p api.PolicyResponse) {
	f.mu.Lock()
	f.policy = p
	f.mu.Unlock()
}

func (f *fakeConsole) enqueue(c api.Command) {
	f.mu.Lock()
	f.queue = append(f.queue, c)
	f.mu.Unlock()
}

func (f *fakeConsole) serve(w http.ResponseWriter, r *http.Request) {
	if f.down.Load() {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
		return
	}
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, recorded{r.Method, r.URL.Path, body})
	f.mu.Unlock()
	w.Header().Set(api.HeaderServerTime, api.FormatServerTime(time.Now()))

	if r.URL.Path == api.RouteEnroll {
		f.enroll(w, r, body)
		return
	}
	f.mu.Lock()
	tok := f.deviceToken
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+tok || tok == "" {
		f.problem(w, api.CodeUnauthorized)
		return
	}
	if f.forbid.Load() {
		f.problem(w, api.CodeForbidden)
		return
	}
	switch {
	case r.URL.Path == api.RoutePolicy && r.Method == http.MethodGet:
		f.mu.Lock()
		f.policyHits++
		pol := f.policy
		f.mu.Unlock()
		if r.Header.Get(api.HeaderIfNoneMatch) == pol.ETag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		pol.ServerTime = time.Now()
		f.json(w, 200, pol)
	case r.URL.Path == api.RouteHeartbeat:
		var hb api.HeartbeatRequest
		if json.Unmarshal(body, &hb) != nil || hb.Validate() != nil {
			f.problem(w, api.CodeInvalidRequest)
			return
		}
		f.mu.Lock()
		f.heartbeats = append(f.heartbeats, hb)
		etag := f.policy.ETag
		pending := len(f.queue) > 0
		f.mu.Unlock()
		f.json(w, 200, api.HeartbeatResponse{ServerTime: time.Now(), PolicyETag: etag, CommandsPending: pending})
	case r.URL.Path == api.RouteRuns:
		f.runsHandler(w, body)
	case r.URL.Path == api.RouteStartLease && r.Method == http.MethodPost:
		f.startLease(w, r, body)
	case r.URL.Path == api.RouteSnapshots:
		var req api.SnapshotsRequest
		if json.Unmarshal(body, &req) != nil || req.Validate() != nil {
			f.problem(w, api.CodeInvalidRequest)
			return
		}
		f.mu.Lock()
		f.snapshots = append(f.snapshots, req)
		f.mu.Unlock()
		f.json(w, 200, api.SnapshotsResponse{Upserted: len(req.Snapshots), ServerTime: time.Now()})
	case r.URL.Path == api.RouteCommands && r.Method == http.MethodGet:
		f.commands(w, r)
	case r.Method == http.MethodPost && len(r.URL.Path) > len(api.PathPrefix+"/commands/") && r.URL.Path[:len(api.PathPrefix+"/commands/")] == api.PathPrefix+"/commands/":
		id := r.URL.Path[len(api.PathPrefix+"/commands/") : len(r.URL.Path)-len("/result")]
		var res api.CommandResultRequest
		if json.Unmarshal(body, &res) != nil || res.Validate() != nil {
			f.problem(w, api.CodeInvalidRequest)
			return
		}
		f.mu.Lock()
		f.results[id] = append(f.results[id], res)
		if f.resultPosts == nil {
			f.resultPosts = map[string]int{}
		}
		f.resultPosts[id]++
		answer := http.StatusNoContent
		if q := f.resultAnswers[id]; len(q) > 0 {
			answer, f.resultAnswers[id] = q[0], q[1:]
		}
		f.mu.Unlock()
		if answer != http.StatusNoContent {
			w.Header().Set("Content-Type", api.ContentTypeProblem)
			w.WriteHeader(answer)
			_, _ = w.Write([]byte(`{"title":"fake"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeConsole) enroll(w http.ResponseWriter, r *http.Request, body []byte) {
	if r.Header.Get("Authorization") != "Bearer "+testEnrollToken {
		f.problem(w, api.CodeUnauthorized)
		return
	}
	var req api.EnrollRequest
	if json.Unmarshal(body, &req) != nil || req.Validate() != nil {
		f.problem(w, api.CodeInvalidRequest)
		return
	}
	f.mu.Lock()
	f.enrollNonces = append(f.enrollNonces, req.Nonce)
	id, ok := f.nonces[req.Nonce]
	if !ok {
		id = testDeviceID
		f.nonces[req.Nonce] = id
	}
	f.tokensIssued++
	f.deviceToken = "device-token-" + strconv.Itoa(f.tokensIssued) // a retry revokes the earlier token
	resp := api.EnrollResponse{
		DeviceID: id, CustomerID: testCustomerID, DeviceToken: api.Secret(f.deviceToken),
		Gateway:      api.Gateway{URL: "https://gateway.example:9443", Username: id, Password: testGatewaySec},
		RepoPassword: testRepoSecret, Custody: api.CustodyEscrowed, Policy: f.policy, ServerTime: time.Now(),
	}
	f.mu.Unlock()
	if f.omitCustomer.Load() {
		resp.CustomerID = ""
	}
	if f.dropEnrollOnce.CompareAndSwap(true, false) {
		// The console stored the enrollment; the response never arrives.
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
		return
	}
	f.json(w, 200, resp)
}

func (f *fakeConsole) runsHandler(w http.ResponseWriter, body []byte) {
	var req api.RunsRequest
	if json.Unmarshal(body, &req) != nil || len(req.Runs) == 0 {
		f.problem(w, api.CodeInvalidRequest)
		return
	}
	out := api.RunsResponse{ServerTime: time.Now()}
	f.mu.Lock()
	f.runPosts = append(f.runPosts, req)
	for _, run := range req.Runs {
		res := api.RunResult{ID: run.ID}
		prev, known := f.runs[run.ID]
		switch {
		case f.rejectRuns[run.ID] != "":
			res.Status, res.Reason = api.RunRejected, f.rejectRuns[run.ID]
		case run.Validate() != nil:
			res.Status, res.Reason = api.RunRejected, api.ReasonInvalid
		case !known:
			res.Status = api.RunCreated
			f.runs[run.ID] = run
			f.runOrder = append(f.runOrder, run.ID)
		case prev.Open() && !run.Open():
			res.Status = api.RunUpdated
			f.runs[run.ID] = run
		default:
			res.Status = api.RunDuplicate
		}
		out.Results = append(out.Results, res)
	}
	f.mu.Unlock()
	f.json(w, 200, out)
}

func (f *fakeConsole) startLease(w http.ResponseWriter, r *http.Request, body []byte) {
	var req api.StartLeaseRequest
	if json.Unmarshal(body, &req) != nil || req.Validate() != nil {
		f.problem(w, api.CodeInvalidRequest)
		return
	}
	f.mu.Lock()
	f.leaseReqs = append(f.leaseReqs, req)
	f.mu.Unlock()
	switch f.leaseMode.Load() {
	case leaseGrant:
		until := time.Now().Add(api.StartLeaseTTL)
		f.json(w, 200, api.StartLeaseResponse{Granted: true, LeaseUntil: &until, ServerTime: time.Now()})
	case leaseUnlimited:
		f.json(w, 200, api.StartLeaseResponse{Granted: true, ServerTime: time.Now()})
	case leaseRefused:
		f.json(w, 200, api.StartLeaseResponse{Granted: false, RetryAfterS: int(f.leaseRetryS.Load()), ServerTime: time.Now()})
	case leaseUnavail:
		f.problem(w, api.CodeUnavailable)
	case leaseLimited:
		f.problem(w, api.CodeRateLimited)
	case leaseUnauth:
		f.problem(w, api.CodeUnauthorized)
	case leaseHang:
		select {
		case <-f.leaseRelease:
		case <-r.Context().Done():
		}
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeConsole) leaseRequests() []api.StartLeaseRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.StartLeaseRequest(nil), f.leaseReqs...)
}

func (f *fakeConsole) commands(w http.ResponseWriter, r *http.Request) {
	wait, _ := strconv.Atoi(r.URL.Query().Get(api.QueryWait))
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	if max := time.Now().Add(300 * time.Millisecond); deadline.After(max) {
		deadline = max // tests do not hold a poll for 30 s
	}
	for {
		f.mu.Lock()
		q := f.queue
		f.queue = nil
		f.mu.Unlock()
		if len(q) > 0 {
			f.json(w, 200, api.CommandsResponse{Commands: q, ServerTime: time.Now()})
			return
		}
		if time.Now().After(deadline) {
			f.json(w, 200, api.CommandsResponse{Commands: []api.Command{}, ServerTime: time.Now()})
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *fakeConsole) json(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", api.ContentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeConsole) problem(w http.ResponseWriter, code string) {
	e := api.NewError(code)
	w.Header().Set("Content-Type", api.ContentTypeProblem)
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(e)
}

// ---- inspection (all safe for concurrent use) ------------------------------

func (f *fakeConsole) run(id string) (api.RunRecord, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[id]
	return r, ok
}

// closedRuns returns the closed run records in arrival order.
func (f *fakeConsole) closedRuns() []api.RunRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []api.RunRecord
	for _, id := range f.runOrder {
		if r := f.runs[id]; !r.Open() {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeConsole) heartbeatList() []api.HeartbeatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.HeartbeatRequest(nil), f.heartbeats...)
}

func (f *fakeConsole) allBodies() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.bodies...)
}

func (f *fakeConsole) snapshotUploads() []api.SnapshotsRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.SnapshotsRequest(nil), f.snapshots...)
}

// answerResults makes the next result posts of command id get these statuses.
func (f *fakeConsole) answerResults(id string, statuses ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resultAnswers == nil {
		f.resultAnswers = map[string][]int{}
	}
	f.resultAnswers[id] = append(f.resultAnswers[id], statuses...)
}

// resultPostCount is how many times the agent posted a result for command id.
func (f *fakeConsole) resultPostCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resultPosts[id]
}

func (f *fakeConsole) commandResults(id string) []api.CommandResultRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.CommandResultRequest(nil), f.results[id]...)
}

func (f *fakeConsole) postCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runPosts)
}
