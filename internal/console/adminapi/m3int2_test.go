package adminapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M3-INT2: run-now of the health-prober, the probe's timeout answer, and the
// once-guard of the commit signal.

// "Run now" on the health-prober: RunOnce runs under a context its caller
// cancels as soon as RunOnce returns, which is before the probes Run started
// have ended. Those probes must still complete and record their facts.
func TestSystemJobRunOfTheHealthProberRecordsItsProbes(t *testing.T) { // M3-INT2 2
	e := newEnv(t)
	owner := e.login("admin", pw)
	be := newProbeBackend()
	be.hold.Store(true)
	hp, _ := e.realProber(be, "console-a")
	runner := jobs.NewRunner(e.st, "console-a", nil)
	runner.Add(hp.Job())
	e.svc.Jobs = runner
	e.mustReq("POST", "/api/v1/system/jobs/"+jobs.NameHealthProber+"/run", owner, nil, 202)
	select {
	case <-be.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the run-now tick never started its storage probe")
	}
	jobBackground.Wait() // RunOnce has returned and its context is cancelled
	time.Sleep(50 * time.Millisecond)
	close(be.release)
	done := make(chan struct{})
	go func() { hp.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run-now tick's probes did not end")
	}
	if s := hp.Facts().Storage; s.LastOKAt == 0 {
		t.Fatalf("the storage probe started by run-now recorded nothing: %+v", s)
	}
	j, err := e.st.GetJobState(context.Background(), jobs.ProbeRowStorage)
	if err != nil || j.LastStatus != "ok" {
		t.Fatalf("storage probe row after run-now: %+v %v, want ok", j, err)
	}
}

// timeoutProber's probes did not finish within the caller's bound.
type timeoutProber struct{}

func (timeoutProber) ProbeNow(context.Context) (bool, error) { return true, context.DeadlineExceeded }

// Mutants M36 (ran reported after a timeout) and M37 (a timeout counted as
// success): a probe whose wait ran out says so in the answer and in its one
// audit row; nothing reads ok.
func TestStorageProbeTimeoutIsReportedAsATimeout(t *testing.T) { // M3-INT2 4
	e := newEnv(t)
	owner := e.login("admin", pw)
	target := e.firstStorageTarget()
	e.svc.Prober = timeoutProber{}
	pr := decode[ProbeResponse](t, e.mustReq("POST", "/api/v1/storage-targets/"+target+"/probe", owner, nil, 200))
	if pr.Ran {
		t.Error("ran = true for probes that did not finish")
	}
	if pr.Job == nil || pr.Job.Status != "error" || pr.Job.ErrorClass != "timeout" {
		t.Errorf("job = %+v, want error / timeout", pr.Job)
	}
	if !strings.Contains(pr.Reason, "did not finish") {
		t.Errorf("reason %q, want it to say the probes did not finish", pr.Reason)
	}
	if len(pr.Checks) != 3 {
		t.Fatalf("checks %+v", pr.Checks)
	}
	for _, c := range pr.Checks {
		if c.Status != statusNotChecked {
			t.Errorf("%s = %s after a timeout, want not_checked", c.ID, c.Status)
		}
	}
	l, err := e.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{Action: "storage_target.probe", ObjectID: target}, store.Page{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Items) != 1 || l.Items[0].Result != "error" || !strings.Contains(string(l.Items[0].Details), `"timeout"`) {
		t.Fatalf("audit rows %+v, want exactly one error / timeout", l.Items)
	}
}

// Mutant M08: a route that calls committed() and answers 2xx (so the wrapper
// would signal too) signals exactly once per request.
func TestCommittedRouteSignalsExactlyOnce(t *testing.T) { // M3-INT2 5
	e := newEnv(t)
	src := &fakeSource{}
	e.svc.Events = src
	owner := e.login("admin", pw)
	told := func() int {
		src.mu.Lock()
		defer src.mu.Unlock()
		return len(src.changed)
	}
	before, gen := told(), e.svc.apiGen.Load()
	var cust struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(e.mustReq("POST", "/api/v1/customers", owner, map[string]any{"name": "Signal Once Co"}, 201), &cust); err != nil || cust.ID == "" {
		t.Fatalf("create customer: %v", err)
	}
	if n, g := told()-before, e.svc.apiGen.Load()-gen; n != 1 || g != 1 {
		t.Errorf("POST /customers signalled %d times (generation +%d), want exactly 1", n, g)
	}
	before, gen = told(), e.svc.apiGen.Load()
	e.mustReq("PATCH", "/api/v1/customers/"+cust.ID, owner, map[string]any{"expected": true}, 200)
	if n, g := told()-before, e.svc.apiGen.Load()-gen; n != 1 || g != 1 {
		t.Errorf("PATCH /customers/{id} signalled %d times (generation +%d), want exactly 1", n, g)
	}
}
