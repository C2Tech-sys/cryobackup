package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/enroll"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// A token policy that cannot be applied does not stop the enrollment, and it is
// not silent: one path-free WARN line names the device, the token and the class.
func TestSkippedTokenPolicyIsLoggedOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var logs bytes.Buffer
	h.svc.Log = logx.New(&logs, logx.LevelDebug)
	custs, _ := h.st.ListCustomers(ctx, store.ScopeAll())
	custID := ""
	for _, c := range custs {
		if c.Name == "Northwind Dental" {
			custID = c.ID
		}
	}
	tpl, err := h.st.CreatePolicy(ctx, store.ScopeAll(), store.PolicyInput{Scope: "template", Name: "Bad template",
		Sections: map[string]json.RawMessage{"sla": json.RawMessage(`{"first_backup_grace_hours":24}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.Write(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE policies SET sla = '{"stale_after_hours":-5}' WHERE id = ?`, tpl.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	tok, row, err := enroll.Mint(ctx, h.st, store.ScopeAll(), custID, enroll.MintOptions{Expires: time.Hour, PolicyID: tpl.ID})
	if err != nil {
		t.Fatal(err)
	}
	rr := h.do("POST", api.RouteEnroll, tok, enrollReq(nonce(40)))
	if rr.code != 200 {
		t.Fatalf("enroll: %d %s", rr.code, rr.body)
	}
	var got api.EnrollResponse
	rr.json(t, &got)
	var lines []string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "token policy not applied") {
			lines = append(lines, l)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("warn lines = %d:\n%s", len(lines), logs.String())
	}
	for _, want := range []string{"warn", got.DeviceID, row.ID, "does_not_compose"} {
		if !strings.Contains(strings.ToLower(lines[0]), strings.ToLower(want)) {
			t.Errorf("log line lacks %q: %s", want, lines[0])
		}
	}
	if strings.Contains(lines[0], "Bad template") {
		t.Errorf("the policy name is in the log line: %s", lines[0])
	}
}
