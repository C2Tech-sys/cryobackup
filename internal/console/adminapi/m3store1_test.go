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

// M3-STORE1 B: a probe that ended without a result never reads ok: ran false,
// job error / cancelled, every check not_checked, and one audit row saying so.

// wantCancelledProbe checks the answer and the one audit row.
func (e *env) wantCancelledProbe(t *testing.T, target string, a probeAnswer) {
	t.Helper()
	if a.err != nil || a.status != 200 {
		t.Fatalf("probe = %d %s (%v)", a.status, a.body, a.err)
	}
	pr := decode[ProbeResponse](t, a.body)
	if pr.Ran {
		t.Error("ran = true although nothing probed")
	}
	if pr.Job == nil || pr.Job.Status != "error" || pr.Job.ErrorClass != "cancelled" {
		t.Errorf("job = %+v, want error / cancelled", pr.Job)
	}
	if !strings.Contains(pr.Reason, "cancelled") {
		t.Errorf("reason %q, want it to say the probes were cancelled", pr.Reason)
	}
	if len(pr.Checks) != 3 {
		t.Fatalf("checks %+v", pr.Checks)
	}
	for _, c := range pr.Checks {
		if c.Status != statusNotChecked {
			t.Errorf("%s = %s, want not_checked", c.ID, c.Status)
		}
	}
	l, err := e.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{Action: "storage_target.probe", ObjectID: target}, store.Page{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Items) != 1 || l.Items[0].Result != "error" {
		t.Fatalf("audit rows %+v, want exactly one error", l.Items)
	}
	var details map[string]any
	if err := json.Unmarshal(l.Items[0].Details, &details); err != nil || details["outcome"] != "cancelled" {
		t.Fatalf("audit details %s (%v), want outcome cancelled", l.Items[0].Details, err)
	}
}

func waitAnswer(t *testing.T, ch <-chan probeAnswer) probeAnswer {
	t.Helper()
	select {
	case a := <-ch:
		return a
	case <-time.After(15 * time.Second):
		t.Fatal("no answer from the probe route")
		return probeAnswer{}
	}
}

func TestStorageProbeDuringStopSaysNothingProbed(t *testing.T) { // M3-STORE1 B
	e := newEnv(t)
	owner := e.login("admin", pw)
	target := e.firstStorageTarget()
	be := newProbeBackend()
	be.hold.Store(true)
	hp, _ := e.realProber(be, "console-a")
	answer := e.postProbe(owner, target)
	select {
	case <-be.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the storage probe never started")
	}
	hp.Stop()
	e.wantCancelledProbe(t, target, waitAnswer(t, answer))
}

func TestStorageProbeAfterStopSaysNothingProbed(t *testing.T) { // M3-STORE1 B
	e := newEnv(t)
	owner := e.login("admin", pw)
	target := e.firstStorageTarget()
	be := newProbeBackend()
	hp, _ := e.realProber(be, "console-a")
	hp.Stop()
	e.wantCancelledProbe(t, target, waitAnswer(t, e.postProbe(owner, target)))
	if n := be.calls.Load(); n != 0 {
		t.Errorf("%d storage probes after Stop, want none", n)
	}
}

func TestStorageProbeLeaseLostMidWaitSaysNothingProbed(t *testing.T) { // M3-STORE1 B
	e := newEnv(t)
	owner := e.login("admin", pw)
	target := e.firstStorageTarget()
	be := newProbeBackend()
	be.hold.Store(true)
	hp, _ := e.realProber(be, "console-a")
	hp.Interval = 30 * time.Millisecond // the renewer's period: a third of the lease
	answer := e.postProbe(owner, target)
	select {
	case <-be.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the storage probe never started")
	}
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE job_state SET lease_holder = 'console-b' WHERE name = ?`, jobs.ProbeLease)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.wantCancelledProbe(t, target, waitAnswer(t, answer))
}
