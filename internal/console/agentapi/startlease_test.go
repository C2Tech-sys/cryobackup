package agentapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// capAll sets max_concurrent_backups on every storage target of the fixture world.
func (h *harness) capAll(n int) {
	h.t.Helper()
	ts, err := h.st.ListStorageTargets(context.Background())
	if err != nil || len(ts) == 0 {
		h.t.Fatalf("targets: %v %v", ts, err)
	}
	for _, tg := range ts {
		if _, err := h.st.SetStorageTargetMaxConcurrentBackups(context.Background(), tg.ID, n); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *harness) enrolledN(n byte) string {
	h.t.Helper()
	out, r := h.enroll(nonce(n))
	if r.code != 200 {
		h.t.Fatalf("enroll: %d %s", r.code, r.body)
	}
	return string(out.DeviceToken)
}

func (h *harness) lease(tok, runID string) (resp, api.StartLeaseResponse) {
	h.t.Helper()
	r := h.do("POST", api.RouteStartLease, tok, api.StartLeaseRequest{RunID: runID})
	var out api.StartLeaseResponse
	if r.code == 200 {
		r.json(h.t, &out)
		if err := out.Validate(); err != nil {
			h.t.Fatalf("the console's answer does not validate: %v (%s)", err, r.body)
		}
	}
	return r, out
}

func TestStartLeaseUnlimitedGrantsWithoutALease(t *testing.T) {
	h := newHarness(t)
	tok := h.enrolledN(30)
	r, out := h.lease(tok, h.mustID())
	if r.code != 200 || !out.Granted || out.LeaseUntil != nil || out.RetryAfterS != 0 || !out.ServerTime.Equal(h.clk.Now()) {
		t.Fatalf("cap 0: %d %s", r.code, r.body)
	}
	if strings.Contains(string(r.body), "lease_until") || strings.Contains(string(r.body), "retry_after_s") {
		t.Fatalf("cap 0 answer carries lease fields: %s", r.body)
	}
}

func TestStartLeaseGrantRefuseRenewRelease(t *testing.T) {
	h := newHarness(t)
	h.capAll(1)
	tokA, tokB := h.enrolledN(31), h.enrolledN(32)
	runA, runB := h.mustID(), h.mustID()

	r, a := h.lease(tokA, runA)
	if r.code != 200 || !a.Granted || a.LeaseUntil == nil || !a.LeaseUntil.Equal(h.clk.Now().Add(api.StartLeaseTTL)) {
		t.Fatalf("first: %d %s", r.code, r.body)
	}
	// Idempotent: the same request returns the same lease.
	h.clk.Advance(time.Second)
	if _, again := h.lease(tokA, runA); !again.Granted || !again.LeaseUntil.Equal(*a.LeaseUntil) {
		t.Fatalf("retry: %+v, want the same lease %v", again, a.LeaseUntil)
	}
	r, b := h.lease(tokB, runB)
	if r.code != 200 || b.Granted || b.RetryAfterS != 899 || b.LeaseUntil != nil {
		t.Fatalf("second device: %d %s; want refused, retry 899 s", r.code, r.body)
	}

	// A heartbeat naming the run renews the lease to now + 15 min.
	h.clk.Advance(10 * time.Minute)
	hb := hbReq(h)
	hb.ServiceState, hb.ActiveRunID = api.ServiceRunning, runA
	if r := h.do("POST", api.RouteHeartbeat, tokA, hb); r.code != 200 {
		t.Fatalf("heartbeat: %d %s", r.code, r.body)
	}
	h.clk.Advance(10 * time.Minute) // past the original expiry
	if _, b = h.lease(tokB, runB); b.Granted || b.RetryAfterS != 300 {
		t.Fatalf("after the renewal: %+v, want refused with 300 s left", b)
	}

	// An older agent's heartbeat carries only the progress run id: it renews too.
	hb = hbReq(h)
	hb.ServiceState = api.ServiceRunning
	hb.Progress = &api.Progress{RunID: runA, Kind: restic.KindBackup, PercentDone: 0.5, UpdatedAt: h.clk.Now()}
	if r := h.do("POST", api.RouteHeartbeat, tokA, hb); r.code != 200 {
		t.Fatalf("heartbeat: %d %s", r.code, r.body)
	}
	if _, b = h.lease(tokB, runB); b.Granted || b.RetryAfterS != 900 {
		t.Fatalf("after the progress renewal: %+v, want refused with 900 s left", b)
	}

	// The closed run upload releases the lease: device B gets the slot.
	rec := h.run(restic.OutcomeSuccess, 0)
	rec.ID = runA
	if r := h.do("POST", api.RouteRuns, tokA, api.RunsRequest{Runs: []api.RunRecord{rec}}); r.code != 200 {
		t.Fatalf("runs: %d %s", r.code, r.body)
	}
	if r, b = h.lease(tokB, runB); !b.Granted || b.LeaseUntil == nil {
		t.Fatalf("after the release: %d %s", r.code, r.body)
	}
}

func TestStartLeaseErrors(t *testing.T) {
	h := newHarness(t)
	h.capAll(2)
	tokA, tokB := h.enrolledN(33), h.enrolledN(34)
	run := h.mustID()
	if r, _ := h.lease(tokA, run); r.code != 200 {
		t.Fatalf("grant: %d", r.code)
	}
	cases := []struct {
		name, tok string
		body      any
		code      int
		problem   string
	}{
		{"no token", "", api.StartLeaseRequest{RunID: h.mustID()}, 401, api.CodeUnauthorized},
		{"bad token", "not-a-token", api.StartLeaseRequest{RunID: h.mustID()}, 401, api.CodeUnauthorized},
		{"run id not a ulid", tokA, api.StartLeaseRequest{RunID: "run-1"}, 400, api.CodeInvalidRequest},
		{"run id path", tokA, `{"run_id":"C:\\Users\\x\\y.txt"}`, 400, api.CodeInvalidRequest},
		{"malformed", tokA, `{"run_id":`, 400, api.CodeInvalidRequest},
		{"trailing data", tokA, `{"run_id":"` + h.mustID() + `"} {}`, 400, api.CodeInvalidRequest},
		{"too large", tokA, padded(api.MaxStartLeaseBody), 413, api.CodePayloadTooLarge},
		{"another device's run id", tokB, api.StartLeaseRequest{RunID: run}, 409, api.CodeConflict},
	}
	for _, c := range cases {
		h.clk.Advance(11 * time.Second) // stay under the per-device limit
		r := h.do("POST", api.RouteStartLease, c.tok, c.body)
		if r.code != c.code {
			t.Errorf("%s: %d %s, want %d", c.name, r.code, r.body, c.code)
			continue
		}
		if p := r.problem(t); p.Code != c.problem || strings.Contains(string(r.body), `Users`) {
			t.Errorf("%s: problem %+v (%s)", c.name, p, r.body)
		}
	}
	if r := h.do("GET", api.RouteStartLease, tokA, nil); r.code != 405 {
		t.Errorf("GET: %d, want 405", r.code)
	}
}

func TestStartLeaseRateLimit(t *testing.T) {
	h := newHarness(t)
	h.capAll(1)
	tok := h.enrolledN(35)
	run := h.mustID()
	for i := 0; i < api.StartLeasePerMinute; i++ {
		if r, _ := h.lease(tok, run); r.code != 200 {
			t.Fatalf("call %d: %d", i, r.code)
		}
	}
	r := h.do("POST", api.RouteStartLease, tok, api.StartLeaseRequest{RunID: run})
	if r.code != 429 || r.hdr.Get(api.HeaderRetryAfter) == "" || r.problem(t).Code != api.CodeRateLimited {
		t.Fatalf("over the limit: %d %s", r.code, r.body)
	}
	h.clk.Advance(time.Minute)
	if r, _ := h.lease(tok, run); r.code != 200 {
		t.Fatalf("after a minute: %d", r.code)
	}
}

// The lease never outlives a closed record sent in a batch with others, and a
// rejected record releases nothing.
func TestStartLeaseReleaseOnlyByTheOwnersClosedRecord(t *testing.T) {
	h := newHarness(t)
	h.capAll(1)
	tokA, tokB := h.enrolledN(36), h.enrolledN(37)
	runA := h.mustID()
	if _, a := h.lease(tokA, runA); !a.Granted {
		t.Fatal("not granted")
	}
	open := h.run("", 0)
	open.ID = runA
	if r := h.do("POST", api.RouteRuns, tokA, api.RunsRequest{Runs: []api.RunRecord{open}}); r.code != 200 {
		t.Fatalf("open: %d", r.code)
	}
	h.clk.Advance(3 * time.Second)
	if _, b := h.lease(tokB, h.mustID()); b.Granted {
		t.Fatal("the open record released the lease")
	}
	closed := h.run(restic.OutcomeSuccess, 0)
	closed.ID = runA
	h.clk.Advance(3 * time.Second)
	if r := h.do("POST", api.RouteRuns, tokB, api.RunsRequest{Runs: []api.RunRecord{closed}}); r.code != 200 {
		t.Fatalf("foreign close: %d", r.code)
	}
	leases, err := h.st.ListStartLeases(context.Background(), h.targetOf(t), h.clk.Now())
	if err != nil || len(leases) != 1 || leases[0].RunID != runA {
		t.Fatalf("leases after a foreign close: %+v %v", leases, err)
	}
}

func (h *harness) targetOf(t *testing.T) string {
	t.Helper()
	ts, err := h.st.ListStorageTargets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range ts {
		if l, _ := h.st.ListStartLeases(context.Background(), tg.ID, h.clk.Now()); len(l) > 0 {
			return tg.ID
		}
	}
	t.Fatal("no target holds a lease")
	return ""
}
