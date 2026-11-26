package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// P22-H (ADR 0030): POST /api/v1/devices/{id}/escrow/verify runs one escrow verification now.

func (e *env) escrowVerify(s *session, dev string, hdr ...string) (int, []byte, string) {
	e.t.Helper()
	res, b := e.req("POST", "/api/v1/devices/"+dev+"/escrow/verify", s, nil, hdr...)
	code := ""
	if res.StatusCode >= 400 {
		code = problemCode(e.t, res, b)
	}
	return res.StatusCode, b, code
}

func (e *env) escrowRow(dev string) store.Escrow {
	e.t.Helper()
	esc, err := e.st.ActiveEscrow(context.Background(), store.ScopeAll(), dev)
	if err != nil {
		e.t.Fatal(err)
	}
	return esc
}

// pathFreeAudit fails when an audit row of the action carries anything that looks like a path or
// a host name.
func (e *env) pathFreeAudit(action, dev, host string) []store.AuditEvent {
	e.t.Helper()
	rows := e.auditOf(action, dev)
	for _, a := range rows {
		d := string(a.Details)
		if strings.ContainsAny(d, `\/`) || strings.Contains(d, host) || strings.Contains(strings.ToLower(d), "c:") {
			e.t.Errorf("audit row carries a path or a host name: %s", d)
		}
	}
	return rows
}

func TestP22HEscrowVerifyNowVerdicts(t *testing.T) {
	e := newEnv(t)
	var logs bytes.Buffer
	e.svc.Log = logx.New(&logs, logx.LevelDebug)
	owner := e.login("admin", pw)
	dev := e.dev
	retired := e.deviceNamed("OLD-RECEPTION-PC")

	// Verified: the record and one audit row, the fixed sentence of what it proves.
	code, b, _ := e.escrowVerify(owner, dev.ID)
	var out EscrowVerifyResponse
	if code != 200 || json.Unmarshal(b, &out) != nil {
		t.Fatalf("verify: %d %s", code, b)
	}
	now := e.clk.Now().Truncate(time.Millisecond)
	if out.Verdict != EscrowVerified || out.ErrorClass != "" || out.Retired || out.VerifiedAt == nil || !out.VerifiedAt.Equal(now) || out.Proves != escrowProves {
		t.Fatalf("verified answer: %+v", out)
	}
	if esc := e.escrowRow(dev.ID); esc.VerifiedAt == nil || !esc.VerifiedAt.Equal(now) || esc.LastVerifyErrorClass != "" {
		t.Fatalf("record: %+v", esc)
	}
	if rows := e.pathFreeAudit("device.escrow.verify", dev.ID, dev.Hostname); len(rows) != 1 || rows[0].Result != "ok" || !strings.Contains(string(rows[0].Details), `"verdict":"verified"`) {
		t.Fatalf("audit rows: %+v", rows)
	}
	if n := e.be.Count(adminpath.MethodCatConfig); n != 1 {
		t.Fatalf("cat config ran %d times", n)
	}

	// A retired device is verified too; a verdict on its key is recorded as a failure.
	e.be.Errors = map[string]error{adminpath.MethodCatConfig: adminpath.NewError(adminpath.CodeRepoMissing, "")}
	e.clk.Advance(time.Minute)
	code, b, _ = e.escrowVerify(owner, retired.ID)
	out = EscrowVerifyResponse{}
	if code != 200 || json.Unmarshal(b, &out) != nil || out.Verdict != EscrowFailed || out.ErrorClass != "repo_missing" || !out.Retired {
		t.Fatalf("retired, repo missing: %d %s", code, b)
	}
	if esc := e.escrowRow(retired.ID); esc.LastVerifyErrorClass != "repo_missing" {
		t.Fatalf("retired record: %+v", esc)
	}
	if rows := e.pathFreeAudit("device.escrow.verify", retired.ID, retired.Hostname); len(rows) != 1 || rows[0].Result != "error" || !strings.Contains(string(rows[0].Details), `"class":"repo_missing"`) {
		t.Fatalf("retired audit rows: %+v", rows)
	}

	// No verdict (storage unreachable): only the attempt; the earlier proof stands.
	e.be.Errors = map[string]error{adminpath.MethodCatConfig: adminpath.NewError(adminpath.CodeStorageUnreachable, "")}
	e.clk.Advance(time.Minute)
	code, b, _ = e.escrowVerify(owner, dev.ID)
	out = EscrowVerifyResponse{}
	if code != 200 || json.Unmarshal(b, &out) != nil || out.Verdict != EscrowNotVerified || out.ErrorClass != "storage_unreachable" {
		t.Fatalf("unreachable: %d %s", code, b)
	}
	esc := e.escrowRow(dev.ID)
	if esc.VerifiedAt == nil || !esc.VerifiedAt.Equal(now) || esc.LastVerifyErrorClass != "" || !esc.LastVerifyAttemptAt.Equal(e.clk.Now().Truncate(time.Millisecond)) {
		t.Fatalf("an unproven check changed the proof: %+v", esc)
	}
	// The worker is down: 503, an audit row that says so, nothing recorded.
	e.be.Errors = map[string]error{adminpath.MethodPing: adminpath.NewError(adminpath.CodeUnavailable, "")}
	before := e.escrowRow(dev.ID)
	if code, _, pc := e.escrowVerify(owner, dev.ID); code != 503 || pc != api.CodeUnavailable {
		t.Fatalf("worker down: %d %s", code, pc)
	}
	if after := e.escrowRow(dev.ID); !after.LastVerifyAttemptAt.Equal(*before.LastVerifyAttemptAt) {
		t.Fatal("a check that never ran was recorded")
	}
	rows := e.pathFreeAudit("device.escrow.verify", dev.ID, dev.Hostname)
	if len(rows) != 3 || !strings.Contains(string(rows[0].Details), "worker_unavailable") {
		t.Fatalf("audit rows after worker down: %d %s", len(rows), rows[0].Details)
	}
	// The worker has no master key: 503 master_key_unavailable, nothing recorded.
	e.be.Errors = nil
	e.be.PingStatus = &adminpath.PingResult{Version: "fixtures", MasterKeyLoaded: false}
	if code, _, pc := e.escrowVerify(owner, dev.ID); code != 503 || pc != api.CodeMasterKeyUnavailable {
		t.Fatalf("no master key: %d %s", code, pc)
	}
	e.be.PingStatus = nil
	// Nothing about the device's repository in the console log.
	if l := logs.String(); strings.Contains(l, dev.Hostname) || strings.Contains(l, "fleet/") {
		t.Fatalf("the log names the device or its repository: %s", l)
	}
}

func TestP22HEscrowVerifyNowRefusals(t *testing.T) {
	e := newEnv(t)
	owner := e.login("admin", pw)
	purging, unconfirmed := e.deviceNamed("SPARE-PC-09"), e.deviceNamed("KIOSK-WAIT-01")
	calls := func() int { return e.be.Count(adminpath.MethodCatConfig) + e.be.Count(adminpath.MethodPing) }
	audits := e.audits()
	for _, c := range []struct {
		name, dev string
		status    int
		code      string
	}{
		{"customer-held", e.held.ID, 409, api.CodeCustodyCustomerHeld},
		{"purging", purging.ID, 409, api.CodeDevicePurging},
		{"unconfirmed", unconfirmed.ID, 409, api.CodeConflict},
		{"unknown", "01ZZZZZZZZZZZZZZZZZZZZZZZZ", 404, api.CodeNotFound},
	} {
		if status, b, code := e.escrowVerify(owner, c.dev); status != c.status || code != c.code {
			t.Errorf("%s: %d %s %s, want %d %s", c.name, status, code, b, c.status, c.code)
		}
	}
	if calls() != 0 || e.audits() != audits {
		t.Fatalf("a refused request reached the worker (%d calls) or wrote audit rows (%d -> %d)", calls(), audits, e.audits())
	}

	// Roles: admin may, operator may not; a scope that does not hold the device sees 404.
	op := e.as("op1", "operator", nil)
	if status, _, code := e.escrowVerify(op, e.dev.ID); status != 403 || code != api.CodeForbidden {
		t.Errorf("operator: %d %s", status, code)
	}
	ad := e.as("ad1", "admin", nil)
	if status, b, _ := e.escrowVerify(ad, e.dev.ID); status != 200 {
		t.Errorf("admin: %d %s", status, b)
	}
	// (An admin account is never customer-scoped (schema check); a scoped operator is refused by role
	// first. The store's scope rule is TestP22HEscrowVerifyTarget's.)
	scoped := e.as("op2", "operator", []string{e.customerNamed("Pinecrest Legal").ID})
	if status, _, code := e.escrowVerify(scoped, e.dev.ID); status != 403 || code != api.CodeForbidden {
		t.Errorf("scoped operator: %d %s", status, code)
	}
	// No fresh-MFA step-up: an admin's full-scope API token runs it.
	r, b := e.req("POST", "/api/v1/me/api-tokens", ad, TokenCreateRequest{Name: "verify", Scope: "full", Days: 7})
	var tok TokenCreateResponse
	if r.StatusCode != 201 || json.Unmarshal(b, &tok) != nil {
		t.Fatalf("token: %d %s", r.StatusCode, b)
	}
	if status, b, _ := e.escrowVerify(nil, e.dev.ID, "Authorization", "Bearer "+tok.Token); status != 200 {
		t.Errorf("admin token: %d %s", status, b)
	}
}

// The System page's "Escrow verification" row: the verifier's keys judged by the engine's rules.
func TestP22HSystemEscrowRow(t *testing.T) {
	e := newEnv(t)
	owner := e.login("admin", pw)
	row := func() SystemComponentJSON {
		t.Helper()
		var h SystemHealthResponse
		r, b := e.req("GET", "/api/v1/system/health", owner, nil)
		if r.StatusCode != 200 || json.Unmarshal(b, &h) != nil {
			t.Fatalf("health: %d %s", r.StatusCode, b)
		}
		for _, c := range h.Components {
			if c.ID == "escrow" {
				return c
			}
		}
		t.Fatalf("no escrow row in %s", b)
		return SystemComponentJSON{}
	}
	setAll := func(q string, args ...any) {
		t.Helper()
		if err := e.st.Write(context.Background(), func(tx *store.Tx) error { _, err := tx.Exec(q, args...); return err }); err != nil {
			t.Fatal(err)
		}
	}
	// The fixtures hold a never-verified key enrolled 40 days ago: the verifier has not kept up.
	if c := row(); c.Status != statusWarn || !strings.Contains(c.Detail, "not proven for more than 8 days") {
		t.Fatalf("with a never-verified key: %+v", c)
	}
	// Every key proven now: the row's own attempt passes (this test console's self-checks are not
	// wired, so C00 holds every passing row at not_checked; a wired console reads ok, as the property
	// test TestR5EveryRowObeysTheOneRule and the lab view below show).
	setAll(`UPDATE repo_key_escrow SET verified_at = ?, last_verify_error_class = NULL WHERE retired_at IS NULL AND custody = 'escrowed'`, e.clk.Now().UnixMilli())
	if c := row(); c.Status != statusNotChecked || !strings.Contains(c.Detail, "(C00)") || c.CheckedAt == nil {
		t.Fatalf("every key proven: %+v", c)
	}
	// A retired device's key fails: warn, by count, with no name.
	retired := e.deviceNamed("OLD-RECEPTION-PC")
	setAll(`UPDATE repo_key_escrow SET last_verify_error_class = 'wrong_password' WHERE device_id = ? AND retired_at IS NULL`, retired.ID)
	c := row()
	if c.Status != statusWarn || !strings.Contains(c.Detail, "escrow verification failing for 1 retired device") || strings.Contains(c.Detail, retired.Hostname) {
		t.Fatalf("a retired failure: %+v", c)
	}
	// The same rule as banner C22: the engine counts this device.
	keys, err := e.st.EscrowVerifyKeys(context.Background(), store.ScopeAll())
	if err != nil {
		t.Fatal(err)
	}
	w := state.World{EscrowVerify: &state.EscrowVerifyFacts{Keys: keys}, LoadComplete: true, LoadedAt: e.clk.Now()}
	found := false
	for _, b := range state.Evaluate(w, e.clk.Now()).Banners {
		found = found || (b.ID == "C22" && b.Count == 1)
	}
	if !found {
		t.Fatal("banner C22 does not count the retired failure the row shows")
	}
	// The lab view of a healthy console reads ok on this row.
	if _, rows := LabHealthy(fixtures.Now()); !rowOK(rows, "escrow") {
		t.Errorf("lab escrow row: %+v", rows)
	}
}

func rowOK(rows []SystemComponentJSON, id string) bool {
	for _, r := range rows {
		if r.ID == id {
			return r.Status == statusOK
		}
	}
	return false
}
