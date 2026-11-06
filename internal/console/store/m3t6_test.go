package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// addM3T6Matrix adds the M3-T6 writes to the §3.5 scope matrix (matrix() in
// scope_test.go calls it). Each run acts on a fresh row made by prep, so one
// principal's successful retire or delete leaves the next principal's call
// something to act on.
func addM3T6Matrix(m map[string]matrixCase) {
	actor := AuditActor{AdminID: "01J9Y4K3A0B1C2D3E4F5G6H7AA", Username: "matrix", Method: "api"}
	m["RetireDeviceAudited"] = matrixCase{kind: byID,
		prep: func(w *world, t *target) {
			tok := map[string]string{w.custA.ID: "tok-a", w.custB.ID: "tok-b"}[t.cust]
			t.freshDevice = w.enroll(tok, uniq("RETIRE-ME")).Device.ID
		},
		call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.RetireDeviceAudited(w.ctx, sc, t.freshDevice, RetireRequest{Reason: "matrix"}, actor)
			return nil, err
		}}
	m["DeleteEnrollmentTokenAudited"] = matrixCase{kind: byID,
		prep: func(w *world, t *target) {
			t.freshToken = must[EnrollmentToken](w.t)(w.s.CreateEnrollmentToken(w.ctx, ScopeAll(), EnrollmentToken{CustomerID: t.cust, TokenHash: sha256Hex(uniq("del")), ExpiresAt: w.clk.Now().Add(time.Hour)})).ID
			ok(w.t, w.s.RevokeEnrollmentToken(w.ctx, ScopeAll(), t.freshToken))
		},
		call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.DeleteEnrollmentTokenAudited(w.ctx, sc, t.freshToken, actor)
			return nil, err
		}}
}

func countWhere(w *world, q string, args ...any) int {
	w.t.Helper()
	var n int
	ok(w.t, w.s.Read(w.ctx, func(tx *Tx) error { return tx.QueryRow(q, args...).Scan(&n) }))
	return n
}

// The whole retire, in one transaction: credentials gone, the pause over, queued
// commands cancelled, one audit row, and nothing deleted.
func TestRetireRevokesCredentialsKeepsDataAndWritesOneAuditRow(t *testing.T) {
	w := seed(t)
	dev := w.devA.ID
	w.heartbeat(dev)
	ingest(t, w, dev, runInput(runID1, t0.Add(-time.Hour)))
	snap := strings.Repeat("a", 64)
	must2(t)(w.s.AgentReplaceSnapshots(w.ctx, dev, []Snapshot{{SnapshotID: snap, TakenAt: t0.Add(-time.Hour), Paths: []string{`C:\Users`}}}))
	must[QueueResult](t)(w.s.QueueCommandAudited(w.ctx, ScopeAll(), dev, CommandInput{Type: CommandCheckIn, IdempotencyKey: "k1"}, wActor))
	must[PauseResult](t)(w.s.PauseDeviceAudited(w.ctx, ScopeAll(), dev, PauseRequest{Reason: "moving office"}, wActor))
	escrow := countWhere(w, `SELECT count(*) FROM repo_key_escrow WHERE device_id = ? AND retired_at IS NULL`, dev)
	audits := countWhere(w, `SELECT count(*) FROM audit_events`)
	if escrow != 1 {
		t.Fatalf("seed: %d active escrow rows", escrow)
	}

	res := must[RetireResult](t)(w.s.RetireDeviceAudited(w.ctx, ScopeAll(), dev, RetireRequest{Reason: "dead lab machine"}, wActor))
	if !res.Changed || res.Device.RetiredAt == nil || res.Device.RetiredReason != "dead lab machine" || res.Device.PausedAt != nil {
		t.Fatalf("after retire: %+v", res)
	}
	if res.TokensRevoked != 1 || res.LoginsRevoked != 1 {
		t.Fatalf("revoked %d tokens and %d logins, want 1 and 1", res.TokensRevoked, res.LoginsRevoked)
	}
	if res.CommandsCancelled < 1 {
		t.Fatalf("no queued command cancelled: %+v", res)
	}
	if n := countWhere(w, `SELECT count(*) FROM commands WHERE device_id = ? AND status = 'queued'`, dev); n != 0 {
		t.Errorf("%d commands still queued for a retired device", n)
	}

	// The credentials stop working, every way a device could present them.
	if _, _, err := w.s.AgentLookupDeviceToken(w.ctx, sha256Hex("device-token-A-DESK-1")); !errors.Is(err, ErrNotFound) {
		t.Errorf("device token after retire: %v, want ErrNotFound", err)
	}
	for _, e := range must[[]HtpasswdEntry](t)(w.s.ListGatewayLoginsForHtpasswd(w.ctx)) {
		if e.Username == dev {
			t.Error("the retired device's login is still rendered into the htpasswd file")
		}
	}
	if b, _ := w.s.AgentPendingGatewaySecret(w.ctx, dev); b != nil {
		t.Error("a pending gateway secret survived the retire")
	}
	if n := countWhere(w, `SELECT count(*) FROM gateway_logins WHERE device_id = ? AND pending_secret_wrapped IS NOT NULL`, dev); n != 0 {
		t.Errorf("%d wrapped gateway secrets survived the retire", n)
	}

	// Nothing is deleted: the repository path, the escrowed key, snapshots and runs stay.
	if res.Device.RepoPath == "" {
		t.Error("the repository path was cleared")
	}
	if n := countWhere(w, `SELECT count(*) FROM repo_key_escrow WHERE device_id = ? AND retired_at IS NULL`, dev); n != escrow {
		t.Errorf("active escrow rows %d -> %d: the key must outlive the retire", escrow, n)
	}
	if n := countWhere(w, `SELECT count(*) FROM snapshots WHERE device_id = ?`, dev); n != 1 {
		t.Errorf("snapshots = %d, want 1", n)
	}
	if n := countWhere(w, `SELECT count(*) FROM runs WHERE device_id = ?`, dev); n != 1 {
		t.Errorf("runs = %d, want 1", n)
	}

	// One audit row, naming what it did, with no second one on a repeat.
	if n := countWhere(w, `SELECT count(*) FROM audit_events`); n != audits+1 {
		t.Fatalf("audit rows %d -> %d, want exactly one more", audits, n)
	}
	l := must[Listing[AuditEvent]](t)(w.s.ListAudit(w.ctx, ScopeAll(), AuditFilter{Action: "device.retire"}, Page{Limit: 10}))
	if len(l.Items) != 1 || l.Items[0].ObjectID != dev || l.Items[0].CustomerID != w.custA.ID || l.Items[0].Severity != "warning" || l.Items[0].Result != "ok" {
		t.Fatalf("audit: %+v", l.Items)
	}
	if d := string(l.Items[0].Details); !strings.Contains(d, `"was_parked":true`) || !strings.Contains(d, "dead lab machine") {
		t.Errorf("audit details: %s", d)
	}
}

// Idempotent: the same answer, the first reason stands, no second audit row.
func TestRetireTwiceAnswersTheSameStateAndWritesNoSecondAuditRow(t *testing.T) {
	w := seed(t)
	first := must[RetireResult](t)(w.s.RetireDeviceAudited(w.ctx, ScopeAll(), w.devA.ID, RetireRequest{Reason: "first"}, wActor))
	audits := countWhere(w, `SELECT count(*) FROM audit_events`)
	w.clk.Advance(time.Hour)
	again := must[RetireResult](t)(w.s.RetireDeviceAudited(w.ctx, ScopeAll(), w.devA.ID, RetireRequest{Reason: "second"}, wActor))
	if again.Changed || again.TokensRevoked != 0 || again.LoginsRevoked != 0 {
		t.Fatalf("second retire: %+v", again)
	}
	if again.Device.RetiredReason != "first" || !again.Device.RetiredAt.Equal(*first.Device.RetiredAt) {
		t.Fatalf("second retire moved the record: %+v", again.Device)
	}
	if n := countWhere(w, `SELECT count(*) FROM audit_events`); n != audits {
		t.Fatalf("audit rows %d -> %d", audits, n)
	}
}

func TestRetireInputRules(t *testing.T) {
	w := seed(t)
	other := w.enroll("tok-a", "A-DESK-2").Device
	fieldOf := func(err error) string {
		t.Helper()
		fes := FieldErrors(err)
		if len(fes) != 1 {
			t.Fatalf("want one field error, got %v", err)
		}
		return fes[0].Field
	}
	for name, reason := range map[string]string{"empty": "  ", "two lines": "a\nb", "a path": `moved C:\Users\frank\docs`, "too long": strings.Repeat("x", 501)} {
		if _, err := w.s.RetireDeviceAudited(w.ctx, ScopeAll(), w.devA.ID, RetireRequest{Reason: reason}, wActor); fieldOf(err) != "reason" {
			t.Errorf("%s reason: %v", name, err)
		}
	}
	// A replacement: not itself, not another customer's (and that reads like one outside scope), not retired.
	for name, repl := range map[string]string{"self": w.devA.ID, "another customer": w.devB.ID, "no such device": "01J9Y4K3A0B1C2D3E4F5G6H7ZZ"} {
		if _, err := w.s.RetireDeviceAudited(w.ctx, ScopeAll(), w.devA.ID, RetireRequest{Reason: "r", ReplacedBy: repl}, wActor); fieldOf(err) != "replaced_by" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := w.s.RetireDeviceAudited(w.ctx, ScopeOf(w.custA.ID), w.devA.ID, RetireRequest{Reason: "r", ReplacedBy: w.devB.ID}, wActor); fieldOf(err) != "replaced_by" {
		t.Errorf("a replacement outside scope: %v", err)
	}
	if d := must[Device](t)(w.s.GetDevice(w.ctx, ScopeAll(), w.devA.ID)); d.RetiredAt != nil {
		t.Fatal("a refused retire changed the device")
	}
	must[RetireResult](t)(w.s.RetireDeviceAudited(w.ctx, ScopeAll(), other.ID, RetireRequest{Reason: "r"}, wActor))
	if _, err := w.s.RetireDeviceAudited(w.ctx, ScopeAll(), w.devA.ID, RetireRequest{Reason: "r", ReplacedBy: other.ID}, wActor); fieldOf(err) != "replaced_by" {
		t.Errorf("a retired replacement: %v", err)
	}
	third := w.enroll("tok-a", "A-DESK-3").Device
	res := must[RetireResult](t)(w.s.RetireDeviceAudited(w.ctx, ScopeAll(), w.devA.ID, RetireRequest{Reason: "r", ReplacedBy: third.ID}, wActor))
	if res.Device.ReplacedByDeviceID != third.ID {
		t.Fatalf("replaced_by not stored: %+v", res.Device)
	}
}

// A refused audit write refuses the retire: credentials and device untouched.
func TestRetireNotAuditedChangesNothing(t *testing.T) {
	w := seed(t)
	ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER refuse_audits BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'no'); END`)
		return err
	}))
	if _, err := w.s.RetireDeviceAudited(w.ctx, ScopeAll(), w.devA.ID, RetireRequest{Reason: "r"}, wActor); !errors.Is(err, ErrAudit) {
		t.Fatalf("err = %v, want ErrAudit", err)
	}
	if d := must[Device](t)(w.s.GetDevice(w.ctx, ScopeAll(), w.devA.ID)); d.RetiredAt != nil {
		t.Fatal("the device was retired without an audit row")
	}
	if _, _, err := w.s.AgentLookupDeviceToken(w.ctx, sha256Hex("device-token-A-DESK-1")); err != nil {
		t.Fatalf("the token was revoked without an audit row: %v", err)
	}
}

// B14, B15 and D02 of §4.8, through the store's real facts: retired never alarms,
// leaves the rollup, and a retired last device leaves an expected customer MISSING.
func TestStateRetiredDeviceThroughTheLoader_B14_B15_D02(t *testing.T) {
	w := seed(t)
	second := w.enroll("tok-a", "A-DESK-2").Device
	expected := true
	if _, _, err := w.s.UpdateCustomerAudited(w.ctx, ScopeAll(), w.custA.ID, CustomerPatch{Expected: &expected}, wActor); err != nil {
		t.Fatal(err)
	}
	verdict := func() (state.CustomerState, map[string]state.DeviceState) {
		t.Helper()
		world := must[state.World](t)(w.s.LoadWorld(w.ctx, ScopeOf(w.custA.ID), LoadOptions{}))
		view := state.Evaluate(world, w.clk.Now())
		devs := map[string]state.DeviceState{}
		for _, d := range view.Devices {
			devs[d.ID] = d
		}
		for _, c := range view.Customers {
			if c.ID == w.custA.ID {
				return c, devs
			}
		}
		t.Fatal("customer not in the view")
		return state.CustomerState{}, nil
	}
	w.clk.Advance(100 * 24 * time.Hour) // both devices are long past every freshness limit
	before, devs := verdict()
	if before.Active != 2 || devs[w.devA.ID].Alarm == state.SeverityNone {
		t.Fatalf("seed: active %d, alarm %v", before.Active, devs[w.devA.ID].Alarm)
	}

	must[RetireResult](t)(w.s.RetireDeviceAudited(w.ctx, ScopeAll(), w.devA.ID, RetireRequest{Reason: "r"}, wActor))
	one, devs := verdict()
	if d := devs[w.devA.ID]; d.Primary != state.StateRetired || d.Alarm != state.SeverityNone || len(d.Flags) != 1 {
		t.Errorf("B14: %+v", d)
	}
	if one.Active != 1 || one.Retired != 1 || one.Status == state.CustomerMissing {
		t.Errorf("B15: active %d retired %d status %s", one.Active, one.Retired, one.Status)
	}

	must[RetireResult](t)(w.s.RetireDeviceAudited(w.ctx, ScopeAll(), second.ID, RetireRequest{Reason: "r"}, wActor))
	none, _ := verdict()
	if none.Status != state.CustomerMissing || none.Active != 0 || none.Retired != 2 {
		t.Errorf("D02: status %s, active %d, retired %d: retiring the last device of an expected customer must read MISSING", none.Status, none.Active, none.Retired)
	}
}

// ---- token delete ----

func TestDeleteTokenHidesItKeepsTheLinkAndTheAuditTrail(t *testing.T) {
	w := seed(t)
	tok := w.tokA.ID // A-DESK-1 enrolled with it
	if _, err := w.s.DeleteEnrollmentTokenAudited(w.ctx, ScopeAll(), tok, wActor); !errors.Is(err, ErrTokenLive) || !errors.Is(err, ErrConflict) {
		t.Fatalf("deleting an active token: %v, want ErrTokenLive", err)
	}
	if n := countWhere(w, `SELECT count(*) FROM audit_events WHERE action = 'enrollment_token.delete'`); n != 0 {
		t.Fatalf("a refused delete wrote %d audit rows", n)
	}
	must[bool](t)(w.s.RevokeEnrollmentTokenAudited(w.ctx, ScopeAll(), tok, wActor))
	revokes := countWhere(w, `SELECT count(*) FROM audit_events WHERE action = 'enrollment_token.revoke'`)

	changed := must[bool](t)(w.s.DeleteEnrollmentTokenAudited(w.ctx, ScopeAll(), tok, wActor))
	if !changed {
		t.Fatal("the first delete changed nothing")
	}
	// Hidden from every list, and from the counts.
	l := must[EnrollTokenList](t)(w.s.ListEnrollmentTokenRows(w.ctx, ScopeOf(w.custA.ID), EnrollTokenFilter{}))
	if len(l.Rows) != 0 || l.All != 0 || l.Counts[TokenRevoked] != 0 {
		t.Fatalf("still listed: %d rows, all %d, counts %v", len(l.Rows), l.All, l.Counts)
	}
	if ts := must[[]EnrollmentToken](t)(w.s.ListEnrollmentTokens(w.ctx, ScopeOf(w.custA.ID), "")); len(ts) != 0 {
		t.Fatalf("still in ListEnrollmentTokens: %d", len(ts))
	}
	// The row and everything pointing at it stay.
	row := must[EnrollTokenRow](t)(w.s.GetEnrollmentTokenRow(w.ctx, ScopeAll(), tok))
	if row.DeletedAt == nil || row.Devices != 1 {
		t.Fatalf("row: %+v", row)
	}
	if d := must[Device](t)(w.s.GetDevice(w.ctx, ScopeAll(), w.devA.ID)); d.EnrollmentTokenID != tok {
		t.Fatalf("the device's enrolled-via link is %q", d.EnrollmentTokenID)
	}
	if devs := must[[]EnrolledDevice](t)(w.s.ListEnrollmentTokenDevices(w.ctx, ScopeAll(), tok)); len(devs) != 1 {
		t.Fatalf("devices of the deleted token: %d", len(devs))
	}
	if n := countWhere(w, `SELECT count(*) FROM audit_events WHERE action = 'enrollment_token.revoke'`); n != revokes {
		t.Fatalf("revoke audit rows %d -> %d", revokes, n)
	}
	if n := countWhere(w, `SELECT count(*) FROM audit_events WHERE action = 'enrollment_token.delete' AND object_id = ?`, tok); n != 1 {
		t.Fatalf("delete audit rows = %d, want 1", n)
	}
	// A second delete is a no-op with no audit row.
	if must[bool](t)(w.s.DeleteEnrollmentTokenAudited(w.ctx, ScopeAll(), tok, wActor)) {
		t.Error("the second delete reported a change")
	}
	if n := countWhere(w, `SELECT count(*) FROM audit_events WHERE action = 'enrollment_token.delete'`); n != 1 {
		t.Fatalf("delete audit rows after a repeat = %d", n)
	}
}

func TestDeleteTokenStatusRules(t *testing.T) {
	w := seed(t)
	limit := 1
	used := must[EnrollmentToken](t)(w.s.CreateEnrollmentToken(w.ctx, ScopeAll(), EnrollmentToken{CustomerID: w.custA.ID, TokenHash: sha256Hex("used"), ExpiresAt: w.clk.Now().Add(time.Hour), MaxUses: &limit}))
	w.enroll("used", "USED-1")
	if st := must[EnrollTokenRow](t)(w.s.GetEnrollmentTokenRow(w.ctx, ScopeAll(), used.ID)).Status(w.clk.Now()); st != TokenUsedUp {
		t.Fatalf("seed: status %s", st)
	}
	if _, err := w.s.DeleteEnrollmentTokenAudited(w.ctx, ScopeAll(), used.ID, wActor); !errors.Is(err, ErrTokenLive) {
		t.Errorf("a used-up, unexpired token: %v, want ErrTokenLive (revoke first)", err)
	}
	// Expired: deletable without a revoke, and the audit row says expired.
	w.clk.Advance(2 * time.Hour)
	if !must[bool](t)(w.s.DeleteEnrollmentTokenAudited(w.ctx, ScopeAll(), used.ID, wActor)) {
		t.Error("an expired token was not deleted")
	}
	l := must[Listing[AuditEvent]](t)(w.s.ListAudit(w.ctx, ScopeAll(), AuditFilter{Action: "enrollment_token.delete"}, Page{Limit: 10}))
	if len(l.Items) != 1 || !strings.Contains(string(l.Items[0].Details), `"status":"expired"`) {
		t.Fatalf("audit: %+v", l.Items)
	}
}

// Active tokens first, whatever their age; the status filter and counts are right.
func TestTokenListPutsActiveFirstAndCountsEachStatus(t *testing.T) {
	w := seed(t)
	mk := func(secret string) EnrollmentToken {
		x := must[EnrollmentToken](t)(w.s.CreateEnrollmentToken(w.ctx, ScopeAll(), EnrollmentToken{CustomerID: w.custA.ID, TokenHash: sha256Hex(secret), ExpiresAt: w.clk.Now().Add(100 * time.Hour)}))
		w.clk.Advance(time.Minute)
		return x
	}
	mk("older")
	newest := mk("newest")
	must[bool](t)(w.s.RevokeEnrollmentTokenAudited(w.ctx, ScopeAll(), newest.ID, wActor))
	l := must[EnrollTokenList](t)(w.s.ListEnrollmentTokenRows(w.ctx, ScopeOf(w.custA.ID), EnrollTokenFilter{}))
	if len(l.Rows) != 3 || l.Rows[2].ID != newest.ID {
		t.Fatalf("the revoked token is not last: %d rows, last %s", len(l.Rows), l.Rows[len(l.Rows)-1].ID)
	}
	if l.Counts[TokenActive] != 2 || l.Counts[TokenRevoked] != 1 || l.Counts[TokenExpired] != 0 {
		t.Fatalf("counts: %v", l.Counts)
	}
	f := must[EnrollTokenList](t)(w.s.ListEnrollmentTokenRows(w.ctx, ScopeOf(w.custA.ID), EnrollTokenFilter{Status: TokenRevoked}))
	if len(f.Rows) != 1 || f.Counts[TokenActive] != 2 {
		t.Fatalf("filtered: %d rows, counts %v", len(f.Rows), f.Counts)
	}
}

// A deleted token's secret cannot enroll, even with a clock that has moved back.
func TestDeletedTokenCannotEnroll(t *testing.T) {
	w := seed(t)
	tok := must[EnrollmentToken](t)(w.s.CreateEnrollmentToken(w.ctx, ScopeAll(), EnrollmentToken{CustomerID: w.custA.ID, TokenHash: sha256Hex("gone"), ExpiresAt: w.clk.Now().Add(time.Hour)}))
	w.clk.Advance(2 * time.Hour)
	must[bool](t)(w.s.DeleteEnrollmentTokenAudited(w.ctx, ScopeAll(), tok.ID, wActor))
	ok(t, w.s.Write(w.ctx, func(tx *Tx) error { // an operator's clock or a restored row: the token reads unexpired again
		_, err := tx.Exec(`UPDATE enrollment_tokens SET expires_at = ? WHERE id = ?`, msOf(w.clk.Now()), tok.ID)
		return err
	}))
	w.clk.Advance(-time.Hour)
	if _, err := w.s.AgentEnroll(w.ctx, enrollInput("gone", "NEVER-1")); err == nil {
		t.Fatal("a deleted token enrolled a device")
	}
}
