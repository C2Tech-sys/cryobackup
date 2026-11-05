package adminapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// eventWait bounds the wait for one SSE event in tests whose cases run in parallel.
// It is only the point of giving up: under heavy load 5 s read a slow machine as a
// stream that delivered nothing, in TestEveryFleetMutationAdvancesTheChangeGeneration.
const eventWait = 30 * time.Second

// oneProbeAudit is the one audit row of a target's probe, with its outcome.
func (e *env) oneProbeAudit(t *testing.T, target string) (result, outcome string) {
	t.Helper()
	l, err := e.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{Action: "storage_target.probe", ObjectID: target}, store.Page{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Items) != 1 {
		t.Fatalf("audit rows %+v, want exactly one", l.Items)
	}
	var details map[string]any
	if len(l.Items[0].Details) > 0 {
		if err := json.Unmarshal(l.Items[0].Details, &details); err != nil {
			t.Fatal(err)
		}
	}
	out, _ := details["outcome"].(string)
	return l.Items[0].Result, out
}

// panicBackend panics in the storage probe.
type panicBackend struct{ *fixtures.Backend }

func (panicBackend) CatConfig(context.Context, adminpath.DeviceRequest) (restic.Config, error) {
	panic("boom")
}

// A panicked probe is an internal failure. It is not a lost lease or a shutdown,
// which is what the answer used to say.
func TestStorageProbeWhenAProbePanickedAnswersAnInternalFailure(t *testing.T) { // M3-STORE2 7
	e := newEnv(t)
	owner := e.login("admin", pw)
	target := e.firstStorageTarget()
	e.realProber(panicBackend{Backend: &fixtures.Backend{}}, "console-a")
	pr := decode[ProbeResponse](t, e.mustReq("POST", "/api/v1/storage-targets/"+target+"/probe", owner, nil, 200))
	if pr.Ran {
		t.Error("ran = true for a probe that panicked")
	}
	if pr.Job == nil || pr.Job.Status != "error" || pr.Job.ErrorClass != jobs.ClassPanic {
		t.Errorf("job = %+v, want error / %s", pr.Job, jobs.ClassPanic)
	}
	if !strings.Contains(pr.Reason, "internal error") || strings.Contains(pr.Reason, "lease") || strings.Contains(pr.Reason, "shutting down") {
		t.Errorf("reason %q, want an internal error and no word of a lease or a shutdown", pr.Reason)
	}
	for _, c := range pr.Checks {
		if c.Status != statusNotChecked {
			t.Errorf("%s = %s, want not_checked", c.ID, c.Status)
		}
	}
	if result, outcome := e.oneProbeAudit(t, target); result != "error" || outcome != jobs.ClassPanic {
		t.Fatalf("audit %s / %s, want error / %s", result, outcome, jobs.ClassPanic)
	}
}

// No prober runs here (fixtures mode): nothing was probed, so the audit row must
// not say ok.
func TestStorageProbeWithoutAProberAuditsNotRun(t *testing.T) { // M3-STORE2 8
	e := newEnv(t)
	owner := e.login("admin", pw)
	target := e.firstStorageTarget()
	pr := decode[ProbeResponse](t, e.mustReq("POST", "/api/v1/storage-targets/"+target+"/probe", owner, nil, 200))
	if pr.Ran || pr.Reason == "" {
		t.Fatalf("probe without a prober: %+v", pr)
	}
	if result, outcome := e.oneProbeAudit(t, target); result != "error" || outcome != "not_run" {
		t.Fatalf("audit %s / %s, want error / not_run", result, outcome)
	}
}
