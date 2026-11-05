package adminapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M3-STORE1 cleanup 4: the gateway is not configured and the worker cannot be
// reached, so neither probe ran. The answer and the audit row say so.
func TestStorageProbeWithBothProbesSkippedSaysNothingProbed(t *testing.T) { // M3-STORE1 cleanup 4
	e := newEnv(t)
	owner := e.login("admin", pw)
	target := e.firstStorageTarget()
	be := &fixtures.Backend{Errors: map[string]error{adminpath.MethodCatConfig: adminpath.ErrUnavailable}}
	hp, _ := e.realProber(be, "console-a")
	hp.Gateway.URL = "" // not configured
	pr := decode[ProbeResponse](t, e.mustReq("POST", "/api/v1/storage-targets/"+target+"/probe", owner, nil, 200))
	if pr.Ran {
		t.Error("ran = true although nothing probed")
	}
	if pr.Job == nil || pr.Job.Status != "error" || pr.Job.ErrorClass != "nothing_probed" {
		t.Errorf("job = %+v, want error / nothing_probed", pr.Job)
	}
	if !strings.Contains(pr.Reason, "nothing was probed") {
		t.Errorf("reason %q, want it to say nothing was probed", pr.Reason)
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
	if err := json.Unmarshal(l.Items[0].Details, &details); err != nil || details["outcome"] != "nothing_probed" {
		t.Fatalf("audit details %s (%v), want outcome nothing_probed", l.Items[0].Details, err)
	}
}
