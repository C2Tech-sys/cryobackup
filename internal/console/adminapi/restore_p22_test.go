package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// P22-E: POST /api/v1/devices/{id}/restores for every mode (restore_p22.go).

const p22Path = "/C/Users/SENTINEL-p22e/Ledger"

func (e *env) p22Exec(q string, args ...any) {
	e.t.Helper()
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error { _, err := tx.Exec(q, args...); return err }); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) p22Create(s *session, dev string, req CreateRestoreRequest, hdr ...string) (int, []byte, string) {
	e.t.Helper()
	res, b := e.req("POST", "/api/v1/devices/"+dev+"/restores", s, req, hdr...)
	code := ""
	if res.StatusCode >= 400 {
		code = problemCode(e.t, res, b)
	}
	return res.StatusCode, b, code
}

func TestP22CreateDeviceRestoreAndRefusals(t *testing.T) {
	e := newEnv(t)
	var logs bytes.Buffer
	e.svc.Log = logx.New(&logs, logx.LevelDebug)
	e.rest.Log = logx.New(&logs, logx.LevelDebug)
	woken := map[string]int{}
	e.svc.WakeAgent = func(id string) { woken[id]++ }
	s := e.login("admin", pw)
	side := CreateRestoreRequest{SnapshotID: fixtures.FixtureSnapshotID, Mode: "device_side", Paths: []string{p22Path, "/D/Shared/Plan.docx"}}

	// An agent older than 0.4.0.
	e.p22Exec(`UPDATE devices SET agent_version = '0.3.2' WHERE id = ?`, e.dev.ID)
	if code, b, pc := e.p22Create(s, e.dev.ID, side); code != 409 || pc != api.CodeAgentTooOld {
		t.Fatalf("old agent: %d %s", code, b)
	}
	e.p22Exec(`UPDATE devices SET agent_version = '0.4.0' WHERE id = ?`, e.dev.ID)

	// Shape refusals: a device mode takes paths, not path; replace needs the host name.
	if code, b, pc := e.p22Create(s, e.dev.ID, CreateRestoreRequest{SnapshotID: fixtures.FixtureSnapshotID, Mode: "device_side", Path: p22Path}); code != 400 || pc != api.CodeInvalidRequest {
		t.Fatalf("path on a device mode: %d %s", code, b)
	}
	replace := CreateRestoreRequest{SnapshotID: fixtures.FixtureSnapshotID, Mode: "device_original", Paths: []string{p22Path}, Overwrite: "if_changed"}
	code, b, pc := e.p22Create(s, e.dev.ID, replace)
	if code != 400 || pc != api.CodeInvalidRequest || !strings.Contains(string(b), "confirm_host_name") || strings.Contains(string(b), e.dev.Hostname) {
		t.Fatalf("replace without confirmation: %d %s", code, b)
	}

	// Created: queued, wire words, no path in the answer, the agent woken.
	code, b, _ = e.p22Create(s, e.dev.ID, side)
	if code != 202 {
		t.Fatalf("create: %d %s", code, b)
	}
	var rj RestoreJSON
	_ = json.Unmarshal(b, &rj)
	if rj.Mode != "device_side" || rj.Overwrite != "never" || rj.Status != "queued" || rj.SelectionCount != 2 || rj.DownloadReady ||
		!strings.HasPrefix(rj.Outcome, "Queued") || woken[e.dev.ID] != 1 || strings.Contains(string(b), "SENTINEL") {
		t.Fatalf("created: %s (woken %v)", b, woken)
	}
	if rows := e.auditOf("restore.create", rj.ID); len(rows) != 1 || rows[0].Method != "api" || strings.Contains(string(rows[0].Details), "SENTINEL") {
		t.Fatalf("audit %+v", rows)
	}
	// A second active device restore.
	if code, b, pc := e.p22Create(s, e.dev.ID, side); code != 409 || pc != api.CodeDeviceRestoreActive {
		t.Fatalf("second active: %d %s", code, b)
	}
	// Customer-held.
	if code, b, pc := e.p22Create(s, e.held.ID, side); code != 409 || pc != api.CodeCustodyCustomerHeld {
		t.Fatalf("customer-held: %d %s", code, b)
	}
	// A failed audit write refuses the create and leaves nothing.
	e.p22Exec(`UPDATE restore_requests SET status = 'failed' WHERE id = ?`, rj.ID)
	e.setAuditRefusal(true)
	before := e.digest()
	if code, b, pc := e.p22Create(s, e.dev.ID, side); code != 500 || pc != api.CodeInternal || strings.Contains(string(b), "audit") {
		t.Fatalf("unaudited: %d %s", code, b)
	}
	if e.digest() != before {
		t.Fatal("an unaudited create changed stored rows")
	}
	e.setAuditRefusal(false)

	// Retired: a download is allowed, a device mode is refused by name.
	e.p22Exec(`UPDATE devices SET retired_at = ? WHERE id = ?`, e.clk.Now().UnixMilli(), e.dev.ID)
	if code, b, pc := e.p22Create(s, e.dev.ID, side); code != 409 || pc != api.CodeDeviceRetiredDownloadOnly {
		t.Fatalf("retired device mode: %d %s", code, b)
	}
	zip := CreateRestoreRequest{SnapshotID: fixtures.FixtureSnapshotID, Mode: "download_zip", Paths: []string{"/C/Users/Public/Documents", "/C/Users/Public/Desktop/Welcome.txt"}}
	if code, b, _ := e.p22Create(s, e.dev.ID, zip); code != 202 {
		t.Fatalf("retired zip: %d %s", code, b)
	}
	// Purging refuses both.
	e.p22Exec(`UPDATE devices SET purge_requested_at = ? WHERE id = ?`, e.clk.Now().UnixMilli(), e.dev.ID)
	for _, req := range []CreateRestoreRequest{side, zip} {
		if code, b, pc := e.p22Create(s, e.dev.ID, req); code != 409 || pc != api.CodeDevicePurging {
			t.Fatalf("purging %s: %d %s", req.Mode, code, b)
		}
	}
	if strings.Contains(logs.String(), "SENTINEL") || strings.Contains(logs.String(), "Ledger") {
		t.Fatalf("a path reached a log line:\n%s", logs.String())
	}
}

// TestP22EveryRestoreNeedsFreshMFA (ruling C5): a stale session gets 403
// mfa_step_up_required for every mode and nothing is created; an API token is
// never fresh.
func TestP22EveryRestoreNeedsFreshMFA(t *testing.T) {
	e := newEnv(t)
	e.p22Exec(`UPDATE devices SET agent_version = '0.4.0' WHERE id = ?`, e.dev.ID)
	s := e.login("admin", pw)
	reqs := []CreateRestoreRequest{
		{SnapshotID: fixtures.FixtureSnapshotID, Mode: "download_zip", Path: "/C/Users/Public/Desktop/Welcome.txt"},
		{SnapshotID: fixtures.FixtureSnapshotID, Mode: "download_zip", Paths: []string{"/C/Users/Public/Desktop/Welcome.txt"}},
		{SnapshotID: fixtures.FixtureSnapshotID, Mode: "device_side", Paths: []string{p22Path}},
		{SnapshotID: fixtures.FixtureSnapshotID, Mode: "device_original", Paths: []string{p22Path}},
	}
	// A full-scope API token: never fresh.
	r, b := e.req("POST", "/api/v1/me/api-tokens", s, TokenCreateRequest{Name: "full", Scope: "full", Days: 30})
	var tok TokenCreateResponse
	if r.StatusCode != 201 || json.Unmarshal(b, &tok) != nil {
		t.Fatalf("token: %d %s", r.StatusCode, b)
	}
	for _, req := range reqs {
		if code, b, pc := e.p22Create(nil, e.dev.ID, req, "Authorization", "Bearer "+tok.Token); code != 403 || pc != api.CodeMFAStepUpRequired {
			t.Fatalf("token %s: %d %s", req.Mode, code, b)
		}
	}
	e.clk.Advance(6 * time.Minute) // past the five-minute window
	for _, req := range reqs {
		if code, b, pc := e.p22Create(s, e.dev.ID, req); code != 403 || pc != api.CodeMFAStepUpRequired {
			t.Fatalf("stale %s: %d %s", req.Mode, code, b)
		}
	}
	var n int
	_ = e.st.Read(context.Background(), func(tx *store.Tx) error {
		return tx.QueryRow(`SELECT count(*) FROM restore_requests`).Scan(&n)
	})
	if n != 0 {
		t.Fatalf("%d restores were created without a fresh MFA check", n)
	}
	// A fresh session creates again.
	s = e.login("admin", pw)
	if code, b, _ := e.p22Create(s, e.dev.ID, reqs[2]); code != 202 {
		t.Fatalf("fresh session: %d %s", code, b)
	}
}
