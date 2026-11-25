package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// P22-E: device restores in the store (restores_p22.go).

const p22Snap = "a1a1a1a1b2b2b2b2c3c3c3c3d4d4d4d4e5e5e5e5f6f6f6f6a7a7a7a7b8b8b8b8"

// p22Sentinel is a path that must never appear in an audit row, a constraint
// message or an error of this package.
const p22Sentinel = "/C/Users/SENTINEL-p22e/Secret Folder"

func p22Input(devID string, expires time.Time) DeviceRestoreInput {
	return DeviceRestoreInput{DeviceID: devID, SnapshotID: p22Snap, Mode: RestoreModeDeviceOriginal, Overwrite: RestoreOverwriteNever,
		SelectionCount: 1, SelectionHash: strings.Repeat("0", 64), SelectionWrapped: []byte("wrapped:" + p22Sentinel), ExpiresAt: expires}
}

func p22Actor() AuditActor {
	return AuditActor{AdminID: "", Username: "op", IP: "192.0.2.10", Method: "api"}
}

func refuseAuditsNow(t *testing.T, w *world) func() {
	t.Helper()
	ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER refuse_audits BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'audit refused by the test'); END`)
		return err
	}))
	return func() {
		ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
			_, err := tx.Exec(`DROP TRIGGER refuse_audits`)
			return err
		}))
	}
}

func p22Audit(t *testing.T, w *world, action, id string) []AuditEvent {
	t.Helper()
	l := must[Listing[AuditEvent]](t)(w.s.ListAudit(w.ctx, ScopeAll(), AuditFilter{Action: action, ObjectID: id}, Page{Limit: 50}))
	return l.Items
}

func countRows(t *testing.T, w *world, q string, args ...any) int {
	t.Helper()
	var n int
	ok(t, w.s.Read(w.ctx, func(tx *Tx) error { return tx.QueryRow(q, args...).Scan(&n) }))
	return n
}

// TestDeviceRestoreCreateCommandAndAudit: one transaction makes the row, the
// command (params: the request id only) and the audit row; a failed audit write
// refuses all three; a second active restore is refused.
func TestDeviceRestoreCreateCommandAndAudit(t *testing.T) {
	w := seed(t)
	exp := t0.Add(24 * time.Hour)

	undo := refuseAuditsNow(t, w)
	if _, _, err := w.s.CreateDeviceRestoreAudited(w.ctx, ScopeAll(), p22Input(w.devA.ID, exp), p22Actor()); !errors.Is(err, ErrAudit) {
		t.Fatalf("unaudited create: %v", err)
	}
	undo()
	if n := countRows(t, w, `SELECT count(*) FROM restore_requests`) + countRows(t, w, `SELECT count(*) FROM commands WHERE type = 'restore'`); n != 0 {
		t.Fatalf("a refused create left %d rows", n)
	}

	row, cmd, err := w.s.CreateDeviceRestoreAudited(w.ctx, ScopeAll(), p22Input(w.devA.ID, exp), p22Actor())
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "queued" || row.Mode != RestoreModeDeviceOriginal || row.CommandID != cmd.ID || !row.ExpiresAt.Equal(exp) {
		t.Fatalf("row %+v", row)
	}
	var params map[string]string
	if cmd.Type != "restore" || json.Unmarshal(cmd.Params, &params) != nil || len(params) != 1 || params["restore_request_id"] != row.ID ||
		!cmd.ExpiresAt.Equal(exp) || cmd.Status != "queued" {
		t.Fatalf("command %+v params %s", cmd, cmd.Params)
	}
	rows := p22Audit(t, w, "restore.create", row.ID)
	if len(rows) != 1 || strings.Contains(string(rows[0].Details), "SENTINEL") || rows[0].CustomerID != w.custA.ID {
		t.Fatalf("audit %+v", rows)
	}
	// One active device restore per device; another device is not affected.
	if _, _, err := w.s.CreateDeviceRestoreAudited(w.ctx, ScopeAll(), p22Input(w.devA.ID, exp), p22Actor()); !errors.Is(err, ErrDeviceRestoreActive) {
		t.Fatalf("second active: %v", err)
	}
	if _, _, err := w.s.CreateDeviceRestoreAudited(w.ctx, ScopeAll(), p22Input(w.devB.ID, exp), p22Actor()); err != nil {
		t.Fatalf("other device: %v", err)
	}
	// Input the rules refuse: side folder with replace, no selection.
	bad := p22Input(w.devB.ID, exp)
	bad.Mode, bad.Overwrite = RestoreModeDevicePath, RestoreOverwriteIfChanged
	if _, _, err := w.s.CreateDeviceRestoreAudited(w.ctx, ScopeAll(), bad, p22Actor()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("side with replace: %v", err)
	}
}

// TestDeviceRestoreGate: customer-held, retired and purging devices are refused
// at create and at fetch, each by its own error.
func TestDeviceRestoreGate(t *testing.T) {
	w := seed(t)
	exp := t0.Add(24 * time.Hour)
	row, _, err := w.s.CreateDeviceRestoreAudited(w.ctx, ScopeAll(), p22Input(w.devA.ID, exp), p22Actor())
	ok(t, err)
	rowB, _, err := w.s.CreateDeviceRestoreAudited(w.ctx, ScopeAll(), p22Input(w.devB.ID, exp), p22Actor())
	ok(t, err)
	set := func(q string, args ...any) {
		t.Helper()
		ok(t, w.s.Write(w.ctx, func(tx *Tx) error { _, err := tx.Exec(q, args...); return err }))
	}
	// Device B's key becomes customer-held (the schema's form: no wrapped key, never verified).
	set(`UPDATE repo_key_escrow SET custody = 'customer_held', wrapped_key = NULL, verified_at = NULL WHERE device_id = ?`, w.devB.ID)
	if _, err := w.s.AgentDeviceRestore(w.ctx, w.devB.ID, rowB.ID); !errors.Is(err, ErrRestoreCustomerHeld) {
		t.Fatalf("customer-held fetch: %v", err)
	}
	if _, err := w.s.AgentMarkRestoreFetched(w.ctx, w.devB.ID, rowB.ID, ""); !errors.Is(err, ErrRestoreCustomerHeld) {
		t.Fatalf("customer-held mark: %v", err)
	}
	if _, _, err := w.s.CreateDeviceRestoreAudited(w.ctx, ScopeAll(), p22Input(w.devB.ID, exp), p22Actor()); !errors.Is(err, ErrRestoreCustomerHeld) {
		t.Fatalf("customer-held create: %v", err)
	}
	set(`UPDATE devices SET retired_at = ? WHERE id = ?`, t0.UnixMilli(), w.devA.ID)
	if _, err := w.s.AgentDeviceRestore(w.ctx, w.devA.ID, row.ID); !errors.Is(err, ErrDeviceRetired) {
		t.Fatalf("retired fetch: %v", err)
	}
	set(`UPDATE devices SET purge_requested_at = ? WHERE id = ?`, t0.UnixMilli(), w.devA.ID)
	if _, err := w.s.AgentDeviceRestore(w.ctx, w.devA.ID, row.ID); !errors.Is(err, ErrRestoreDevicePurging) {
		t.Fatalf("purging fetch: %v", err)
	}
	if _, _, err := w.s.CreateDeviceRestoreAudited(w.ctx, ScopeAll(), p22Input(w.devA.ID, exp), p22Actor()); !errors.Is(err, ErrRestoreDevicePurging) {
		t.Fatalf("purging create: %v", err)
	}
}

// TestDeviceRestoreFetchAndClose: another device's id is ErrNotFound like a
// missing one; a fetch marks the row running and is audited (a failed audit
// refuses it); the run record closes the row with its class and counts and
// clears the selection; a duplicate record changes nothing.
func TestDeviceRestoreFetchAndClose(t *testing.T) {
	w := seed(t)
	row, _, err := w.s.CreateDeviceRestoreAudited(w.ctx, ScopeAll(), p22Input(w.devA.ID, t0.Add(24*time.Hour)), p22Actor())
	ok(t, err)
	_, errOther := w.s.AgentDeviceRestore(w.ctx, w.devB.ID, row.ID)
	_, errMissing := w.s.AgentDeviceRestore(w.ctx, w.devB.ID, "01J9ZZ0000000000000000ZZZZ")
	if !errors.Is(errOther, ErrNotFound) || !errors.Is(errMissing, ErrNotFound) || errOther.Error() != errMissing.Error() {
		t.Fatalf("another device's restore: %v / missing: %v", errOther, errMissing)
	}
	if _, err := w.s.AgentMarkRestoreFetched(w.ctx, w.devB.ID, row.ID, "192.0.2.20"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another device marks: %v", err)
	}
	undo := refuseAuditsNow(t, w)
	if _, err := w.s.AgentMarkRestoreFetched(w.ctx, w.devA.ID, row.ID, "192.0.2.20"); !errors.Is(err, ErrAudit) {
		t.Fatalf("unaudited fetch: %v", err)
	}
	undo()
	if cur := must[RestoreRequest](t)(w.s.GetRestoreRequest(w.ctx, ScopeAll(), row.ID)); cur.Status != "queued" {
		t.Fatalf("a refused fetch changed the row: %s", cur.Status)
	}
	got, err := w.s.AgentMarkRestoreFetched(w.ctx, w.devA.ID, row.ID, "192.0.2.20")
	if err != nil || got.Status != "running" || got.StartedAt == nil {
		t.Fatalf("fetch: %+v %v", got, err)
	}
	if rows := p22Audit(t, w, "restore.fetch", row.ID); len(rows) != 1 || rows[0].ActorType != "device" || rows[0].ActorID != w.devA.ID {
		t.Fatalf("fetch audit %+v", rows)
	}

	fin := t0
	rec := RunInput{ID: row.ID, Kind: "restore", Trigger: "manual", StartedAt: t0.Add(-time.Minute), FinishedAt: &fin, Outcome: "partial",
		Stats: []byte(`{"files_restored":7,"files_skipped":2,"bytes_restored":4096,"restore_result":"partial","restore_errors":[{"class":"target_locked","count":3}]}`)}
	wantStatus(t, ingest(t, w, w.devA.ID, rec)[0], IngestCreated, "")
	closed := must[RestoreRequest](t)(w.s.GetRestoreRequest(w.ctx, ScopeAll(), row.ID))
	var prog DeviceRestoreProgress
	if closed.Status != "delivered" || closed.ErrorClass != RestoreClassPartial || closed.SelectionWrapped != nil || closed.FinishedAt == nil ||
		json.Unmarshal(closed.Progress, &prog) != nil || prog.FilesRestored != 7 || prog.FilesSkipped != 2 || len(prog.Errors) != 1 ||
		prog.Errors[0] != (RestoreClassCounted{Class: "target_locked", Count: 3}) || closed.Bytes == nil || *closed.Bytes != 4096 {
		t.Fatalf("closed %+v progress %s", closed, closed.Progress)
	}
	if rows := p22Audit(t, w, "restore.finish", row.ID); len(rows) != 1 || rows[0].Result != "error" {
		t.Fatalf("finish audit %+v", rows)
	}
	// A closed row is no longer fetchable, and a resent record closes nothing again.
	if _, err := w.s.AgentDeviceRestore(w.ctx, w.devA.ID, row.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fetch after close: %v", err)
	}
	wantStatus(t, ingest(t, w, w.devA.ID, rec)[0], IngestDuplicate, "")
	if rows := p22Audit(t, w, "restore.finish", row.ID); len(rows) != 1 {
		t.Fatalf("a duplicate closed again: %d rows", len(rows))
	}
}

// TestRestoreCloseMapping: every run outcome closes a device restore to the
// status and class ADR 0027 names.
func TestRestoreCloseMapping(t *testing.T) {
	for _, c := range []struct {
		outcome, result, runClass string
		status, class             string
	}{
		{"success", "", "", "delivered", ""},
		{"partial", "partial", "", "delivered", "partial"},
		{"failed", "disk", "", "failed", "disk"},
		{"failed", "nothing_matched", "", "failed", "nothing_matched"},
		{"failed", "guard_refused", "", "failed", "guard_refused"},
		{"failed", "auth", "auth", "failed", "auth"},
		{"failed", "", "network", "failed", "network"},
		{"failed", "", "", "failed", "unknown"},
		{"cancelled", "cancelled", "", "cancelled", "cancelled"},
		{"interrupted", "interrupted", "", "failed", "interrupted"},
		{"skipped", "", "", "failed", "skipped"},
	} {
		if st, cl := restoreClose(c.outcome, c.result, c.runClass); st != c.status || cl != c.class {
			t.Errorf("%s/%s/%s: got %s/%s, want %s/%s", c.outcome, c.result, c.runClass, st, cl, c.status, c.class)
		}
	}
}

// TestDeviceRestoreExpiry: past its 24 hours the fetch is ErrRestoreExpired and
// the job closes the queued row EXPIRED (audited, selection cleared); a queued
// row whose command the operator cancelled closes CANCELLED.
func TestDeviceRestoreExpiry(t *testing.T) {
	w := seed(t)
	row, _, err := w.s.CreateDeviceRestoreAudited(w.ctx, ScopeAll(), p22Input(w.devA.ID, t0.Add(24*time.Hour)), p22Actor())
	ok(t, err)
	other, cmdB, err := w.s.CreateDeviceRestoreAudited(w.ctx, ScopeAll(), p22Input(w.devB.ID, t0.Add(24*time.Hour)), p22Actor())
	ok(t, err)
	if _, _, err := w.s.CancelCommandAudited(w.ctx, ScopeAll(), w.devB.ID, cmdB.ID, p22Actor()); err != nil {
		t.Fatal(err)
	}
	if n, err := w.s.ExpireDeviceRestores(w.ctx, w.clk.Now()); err != nil || n != 1 {
		t.Fatalf("cancelled close: %d %v", n, err)
	}
	if c := must[RestoreRequest](t)(w.s.GetRestoreRequest(w.ctx, ScopeAll(), other.ID)); c.Status != "cancelled" || c.SelectionWrapped != nil {
		t.Fatalf("cancelled row %+v", c)
	}
	// A new device restore is allowed once the cancelled one is not active.
	if _, _, err := w.s.CreateDeviceRestoreAudited(w.ctx, ScopeAll(), p22Input(w.devB.ID, t0.Add(48*time.Hour)), p22Actor()); err != nil {
		t.Fatalf("after cancel: %v", err)
	}

	w.clk.Advance(24 * time.Hour)
	if _, err := w.s.AgentDeviceRestore(w.ctx, w.devA.ID, row.ID); !errors.Is(err, ErrRestoreExpired) {
		t.Fatalf("expired fetch: %v", err)
	}
	undo := refuseAuditsNow(t, w)
	if n, err := w.s.ExpireDeviceRestores(w.ctx, w.clk.Now()); !errors.Is(err, ErrAudit) || n != 0 {
		t.Fatalf("unaudited expiry: %d %v", n, err)
	}
	undo()
	if c := must[RestoreRequest](t)(w.s.GetRestoreRequest(w.ctx, ScopeAll(), row.ID)); c.Status != "queued" {
		t.Fatalf("an unaudited expiry changed the row: %s", c.Status)
	}
	// Only devA's row is due: devB's second restore waits until 48 h.
	if n, err := w.s.ExpireDeviceRestores(w.ctx, w.clk.Now()); err != nil || n != 1 {
		t.Fatalf("expiry: %d %v", n, err)
	}
	c := must[RestoreRequest](t)(w.s.GetRestoreRequest(w.ctx, ScopeAll(), row.ID))
	if c.Status != "expired" || c.SelectionWrapped != nil || len(p22Audit(t, w, "restore.expire", row.ID)) != 1 {
		t.Fatalf("expired row %+v", c)
	}
	if _, err := w.s.AgentDeviceRestore(w.ctx, w.devA.ID, row.ID); !errors.Is(err, ErrRestoreExpired) {
		t.Fatalf("fetch of an expired row: %v", err)
	}
}

// TestRestoreRunNeverMovesBackupFreshness (ADR 0027, facts.go): restore runs of
// every outcome, newer than the last backup, leave every backup fact of the
// device exactly as it was: last success, last finished backup, open run, newest
// attempt and failure streak.
func TestRestoreRunNeverMovesBackupFreshness(t *testing.T) {
	w := seed(t)
	w.heartbeat(w.devA.ID)
	ingest(t, w, w.devA.ID, runInput(runID1, t0.Add(-30*time.Hour)))
	type backupFacts struct {
		LastSuccess, LastAttempt any
		LastBackup, OpenRun      any
		Failures                 int
	}
	snap := func() string {
		f := must[state.DeviceFacts](t)(w.s.LoadDeviceFacts(w.ctx, ScopeAll(), w.devA.ID))
		b, _ := json.Marshal(backupFacts{f.LastSuccess, f.LastAttemptStart, f.LastBackup, f.OpenRun, f.ConsecutiveFailures})
		return string(b)
	}
	before := snap()
	ids := []string{"01J9Y4K3A0B1C2D3E4F5G6H7K1", "01J9Y4K3A0B1C2D3E4F5G6H7K2", "01J9Y4K3A0B1C2D3E4F5G6H7K3", "01J9Y4K3A0B1C2D3E4F5G6H7K4"}
	fin := t0.Add(-time.Minute)
	recs := []RunInput{
		{ID: ids[0], Kind: "restore", Trigger: "manual", StartedAt: t0.Add(-time.Hour), FinishedAt: &fin, Outcome: "success", Stats: []byte(`{"files_restored":1}`)},
		{ID: ids[1], Kind: "restore", Trigger: "manual", StartedAt: t0.Add(-time.Hour), FinishedAt: &fin, Outcome: "failed", Stats: []byte(`{"restore_result":"disk"}`)},
		{ID: ids[2], Kind: "restore", Trigger: "manual", StartedAt: t0.Add(-time.Hour), FinishedAt: &fin, Outcome: "partial", Stats: []byte(`{"files_restored":1,"restore_result":"partial","restore_errors":[{"class":"other","count":1}]}`)},
		{ID: ids[3], Kind: "restore", Trigger: "manual", StartedAt: t0.Add(-time.Minute)}, // open
	}
	for _, r := range recs {
		wantStatus(t, ingest(t, w, w.devA.ID, r)[0], IngestCreated, "")
	}
	if after := snap(); after != before {
		t.Fatalf("a restore run moved the backup facts:\nbefore %s\nafter  %s", before, after)
	}
}

// The scope matrix entry of the device restore create (scope_test.go matrix()).
// Each call first closes the device's earlier device restores, so the one
// active device restore rule does not refuse the next principal's call.
func addRestoreP22Matrix(m map[string]matrixCase) {
	m["CreateDeviceRestoreAudited"] = matrixCase{kind: byID,
		prep: func(w *world, t *target) {
			ok(w.t, w.s.Write(w.ctx, func(tx *Tx) error {
				_, err := tx.Exec(`UPDATE restore_requests SET status = 'failed', selection_wrapped = NULL WHERE device_id = ? AND mode IN ('device_original','device_path')`, t.dev)
				return err
			}))
		},
		call: func(w *world, sc Scope, t *target) ([]string, error) {
			in := p22Input(t.dev, w.clk.Now().Add(24*time.Hour))
			in.SnapshotID = t.snapshot
			_, _, err := w.s.CreateDeviceRestoreAudited(w.ctx, sc, in, AuditActor{Username: "matrix", Method: "api"})
			return nil, err
		}}
}

// addRestoreP22Global classifies the device restore job method (globalMethods).
func addRestoreP22Global(g map[string]string) map[string]string {
	g["ExpireDeviceRestores"] = "restore-purge job: closes unfetched device restores of every device, ids only"
	return g
}
