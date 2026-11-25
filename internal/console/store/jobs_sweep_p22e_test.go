package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// P22-E fix E3: the abandoned-run-sweeper closes a running device restore that
// never gets its run record, as FAILED interrupted, audited (jobs_sweep.go).

// runningRestore plants a running device restore of the rig's device, fetched ago.
func (r *sweepRig) runningRestore(t *testing.T, id string, ago time.Duration) {
	t.Helper()
	st := r.now.Add(-ago).UnixMilli()
	r.exec(t, `INSERT INTO restore_requests (id, device_id, snapshot_id, mode, overwrite, selection_count, selection_wrapped, status,
			started_at, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, 'device_path', 'never', 1, X'0102', 'running', ?, ?, ?, ?)`,
		id, r.device, strings.Repeat("ab", 32), st, st+24*3600*1000, st, st)
}

// heartbeat sets the device's newest heartbeat: received ago, with uptime and
// optional progress for runID.
func (r *sweepRig) heartbeat(t *testing.T, ago, uptime time.Duration, runID string) {
	t.Helper()
	var progress any
	if runID != "" {
		b, _ := json.Marshal(map[string]any{"run_id": runID, "advanced_at": r.now.Add(-ago).UnixMilli()})
		progress = string(b)
	}
	r.exec(t, `INSERT INTO device_presence (device_id, received_at, service_state, uptime_s, progress, created_at, updated_at)
		VALUES (?, ?, 'running', ?, ?, ?, ?) ON CONFLICT(device_id) DO UPDATE SET received_at = excluded.received_at,
		uptime_s = excluded.uptime_s, progress = excluded.progress`,
		r.device, r.now.Add(-ago).UnixMilli(), int64(uptime/time.Second), progress, r.now.UnixMilli(), r.now.UnixMilli())
}

func (r *sweepRig) restoreState(t *testing.T, id string) (status, class string, cleared bool) {
	t.Helper()
	var c *string
	var sel []byte
	if err := r.s.Read(context.Background(), func(tx *Tx) error {
		return tx.QueryRow(`SELECT status, error_class, selection_wrapped FROM restore_requests WHERE id = ?`, id).Scan(&status, &c, &sel)
	}); err != nil {
		t.Fatal(err)
	}
	if c != nil {
		class = *c
	}
	return status, class, sel == nil
}

const (
	rsSilent    = "01JD0000000000000000000R01"
	rsReported  = "01JD0000000000000000000R02"
	rsRestarted = "01JD0000000000000000000R03"
	rsYoung     = "01JD0000000000000000000R04"
)

func TestSweepClosesSilentDeviceRestore(t *testing.T) {
	r := newSweepRig(t)
	ctx := context.Background()
	r.runningRestore(t, rsSilent, DeviceRestoreRunningBound+time.Hour) // past the bound, nobody reports it
	r.runningRestore(t, rsYoung, 2*time.Hour)                          // inside the bound, no restart
	r.heartbeat(t, time.Minute, 30*24*time.Hour, "")                   // up for a month: no restart since the fetch

	// A failed audit write leaves the row running and is returned.
	r.exec(t, `CREATE TRIGGER refuse_audits BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'audit refused by the test'); END`)
	if _, err := r.s.SweepAbandonedRuns(ctx, SweepOptions{}); !errors.Is(err, ErrAudit) {
		t.Fatalf("unaudited sweep: %v", err)
	}
	if st, _, _ := r.restoreState(t, rsSilent); st != "running" {
		t.Fatalf("an unaudited close changed the row: %s", st)
	}
	r.exec(t, `DROP TRIGGER refuse_audits`)

	res, err := r.s.SweepAbandonedRuns(ctx, SweepOptions{})
	if err != nil || res.RestoresClosed != 1 || res.RestoresClosedBy["no_run_record"] != 1 {
		t.Fatalf("sweep %+v %v", res, err)
	}
	if st, cl, cleared := r.restoreState(t, rsSilent); st != "failed" || cl != "interrupted" || !cleared {
		t.Fatalf("silent restore: %s/%s cleared=%v", st, cl, cleared)
	}
	if st, _, _ := r.restoreState(t, rsYoung); st != "running" {
		t.Fatalf("a young restore was swept: %s", st)
	}
	l, err := r.s.ListAudit(ctx, ScopeAll(), AuditFilter{Action: "restore.finish", ObjectID: rsSilent}, Page{Limit: 5})
	if err != nil || len(l.Items) != 1 || !strings.Contains(string(l.Items[0].Details), `"reason":"no_run_record"`) {
		t.Fatalf("audit %+v %v", l.Items, err)
	}
	// Idempotent; a late run record is ingested and leaves the closed row alone.
	if again, err := r.s.SweepAbandonedRuns(ctx, SweepOptions{}); err != nil || again.RestoresClosed != 0 {
		t.Fatalf("second sweep %+v %v", again, err)
	}
	fin := r.now
	out, err := r.s.AgentIngestRuns(ctx, r.device, []RunInput{{ID: rsSilent, Kind: "restore", Trigger: "manual", StartedAt: r.now.Add(-time.Hour),
		FinishedAt: &fin, Outcome: "success", Stats: []byte(`{"files_restored":1}`)}}, r.now)
	if err != nil || out[0].Status != IngestCreated {
		t.Fatalf("late record %+v %v", out, err)
	}
	if st, cl, _ := r.restoreState(t, rsSilent); st != "failed" || cl != "interrupted" {
		t.Fatalf("a late record reopened the closed row: %s/%s", st, cl)
	}
}

func TestSweepKeepsReportedRestoreAndClosesAfterRestart(t *testing.T) {
	r := newSweepRig(t)
	ctx := context.Background()
	r.runningRestore(t, rsReported, DeviceRestoreRunningBound+time.Hour)
	r.heartbeat(t, time.Minute, 30*24*time.Hour, rsReported) // the newest heartbeat carries its progress
	if res, err := r.s.SweepAbandonedRuns(ctx, SweepOptions{}); err != nil || res.RestoresClosed != 0 {
		t.Fatalf("a reported restore was swept: %+v %v", res, err)
	}
	if st, _, _ := r.restoreState(t, rsReported); st != "running" {
		t.Fatalf("reported restore: %s", st)
	}
	r.exec(t, `UPDATE restore_requests SET status = 'failed' WHERE id = ?`, rsReported)

	// The agent restarted after the fetch: within the grace nothing happens.
	r.runningRestore(t, rsRestarted, 2*time.Hour)
	r.heartbeat(t, time.Minute, 10*time.Minute, "") // booted 11 minutes ago, after the fetch
	if res, _ := r.s.SweepAbandonedRuns(ctx, SweepOptions{}); res.RestoresClosed != 0 {
		t.Fatalf("closed inside the restart grace: %+v", res)
	}
	// An open run record of the restore: the agent will report it interrupted itself.
	st0 := r.now.Add(-2 * time.Hour).UnixMilli()
	r.exec(t, `INSERT INTO runs (id, device_id, origin, kind, trigger_kind, started_at, received_at, created_at, updated_at)
		VALUES (?, ?, 'agent', 'restore', 'manual', ?, ?, ?, ?)`, rsRestarted, r.device, st0, st0, st0, st0)
	r.heartbeat(t, time.Minute, 40*time.Minute, "") // booted 41 minutes ago
	if res, _ := r.s.SweepAbandonedRuns(ctx, SweepOptions{}); res.RestoresClosed != 0 {
		t.Fatalf("closed although its run record exists: %+v", res)
	}
	// No run record at all, past the grace: closed.
	r.exec(t, `DELETE FROM runs WHERE id = ?`, rsRestarted)
	res, err := r.s.SweepAbandonedRuns(ctx, SweepOptions{})
	if err != nil || res.RestoresClosed != 1 || res.RestoresClosedBy["agent_restarted"] != 1 {
		t.Fatalf("restart sweep %+v %v", res, err)
	}
	if st, cl, _ := r.restoreState(t, rsRestarted); st != "failed" || cl != "interrupted" {
		t.Fatalf("restarted restore: %s/%s", st, cl)
	}
	l, _ := r.s.ListAudit(ctx, ScopeAll(), AuditFilter{Action: "restore.finish", ObjectID: rsRestarted}, Page{Limit: 5})
	if len(l.Items) != 1 || !strings.Contains(string(l.Items[0].Details), `"reason":"agent_restarted"`) {
		t.Fatalf("audit %+v", l.Items)
	}
}
