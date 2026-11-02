package adminapi

import (
	"bytes"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// M2-W3D: the restore.request audit row is written with the restore, and it
// names the transport the request came in on (it used to say "web" for the API).
func TestRestoreRequestAuditRowNamesTheAPIAndCommitsWithTheRestore(t *testing.T) {
	e := newEnv(t)
	s := e.login("admin", pw)
	rj := e.newRestore(s, "/C/Users/Public/Documents/Quarterly-Report.txt")
	rows := e.auditOf("restore.request", rj.ID)
	if len(rows) != 1 {
		t.Fatalf("%d restore.request rows, want 1", len(rows))
	}
	if rows[0].Method != "api" || rows[0].ActorLabel != "admin" || rows[0].Result != "ok" {
		t.Fatalf("audit row: method %q actor %q result %q", rows[0].Method, rows[0].ActorLabel, rows[0].Result)
	}
	if string(rows[0].Details) != `{"mode":"download_zip","selection_count":1}` {
		t.Fatalf("details %s", rows[0].Details)
	}
}

// An audit failure on a restore is logged as what it is (class audit, "not
// audited"), the same as the customer, device and policy routes, and not as a
// generic admin API failure; the answer is the usual 500 and names no cause.
func TestUnauditedRestoreIsLoggedAsNotAudited(t *testing.T) {
	e := newEnv(t)
	var logs bytes.Buffer
	e.svc.Log = logx.New(&logs, logx.LevelDebug)
	s := e.login("admin", pw)
	e.setAuditRefusal(true)
	res, b := e.req("POST", "/api/v1/devices/"+e.dev.ID+"/restores", s,
		CreateRestoreRequest{SnapshotID: fixtures.FixtureSnapshotID, Mode: "download_zip", Path: "/C/Users/Public/Documents/Quarterly-Report.txt"})
	if res.StatusCode != 500 || problemCode(t, res, b) != api.CodeInternal || strings.Contains(string(b), "audit refused") {
		t.Fatalf("unaudited restore: %d %s", res.StatusCode, b)
	}
	out := logs.String()
	if !strings.Contains(out, "change refused, not audited") || !strings.Contains(out, `"class":"audit"`) || strings.Contains(out, "admin api failed") {
		t.Fatalf("log lines: %s", out)
	}
}
