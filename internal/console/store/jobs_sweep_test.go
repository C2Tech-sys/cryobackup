package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// sweepRig is an in-memory store with the fixture fleet and one device whose
// open runs the tests plant.
type sweepRig struct {
	s      *Store
	now    time.Time
	device string
	target string
}

func newSweepRig(t *testing.T) *sweepRig {
	t.Helper()
	r := &sweepRig{now: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	s, err := OpenMemory(context.Background(), func() time.Time { return r.now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := LoadFixtures(context.Background(), s, r.now); err != nil {
		t.Fatal(err)
	}
	r.s = s
	err = s.Read(context.Background(), func(tx *Tx) error {
		return tx.QueryRow(`SELECT d.id, c.storage_target_id FROM devices d JOIN customers c ON c.id = d.customer_id
			WHERE d.retired_at IS NULL ORDER BY d.id LIMIT 1`).Scan(&r.device, &r.target)
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *sweepRig) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if err := r.s.Write(context.Background(), func(tx *Tx) error { _, err := tx.Exec(q, args...); return err }); err != nil {
		t.Fatal(err)
	}
}

// openRun plants an open agent backup run of the rig's device that started ago.
func (r *sweepRig) openRun(t *testing.T, id string, ago time.Duration) {
	t.Helper()
	st := r.now.Add(-ago).UnixMilli()
	r.exec(t, `INSERT INTO runs (id, device_id, origin, kind, trigger_kind, started_at, received_at, created_at, updated_at)
		VALUES (?, ?, 'agent', 'backup', 'schedule', ?, ?, ?, ?)`, id, r.device, st, st, st, st)
}

func (r *sweepRig) outcome(t *testing.T, id string) (string, int64) {
	t.Helper()
	var o *string
	var fin *int64
	if err := r.s.Read(context.Background(), func(tx *Tx) error {
		return tx.QueryRow(`SELECT outcome, finished_at FROM runs WHERE id = ?`, id).Scan(&o, &fin)
	}); err != nil {
		t.Fatal(err)
	}
	if o == nil {
		return "", 0
	}
	return *o, *fin
}

const (
	runOld      = "01JD0000000000000000000001"
	runReported = "01JD0000000000000000000002"
	runLeased   = "01JD0000000000000000000003"
	runYoung    = "01JD0000000000000000000004"
)

// §5.7 abandoned-run-sweeper: an open run past max runtime + slack that the
// device no longer reports is abandoned at its last known activity; a run the
// heartbeat reports (progress or a live start lease) or still inside its limit
// is not. The sweep is idempotent, and a late real outcome overwrites abandoned.
func TestAbandonedRunSweeper(t *testing.T) {
	r := newSweepRig(t)
	ctx := context.Background()
	r.openRun(t, runOld, 20*time.Hour)      // default max run 12 h + 1 h slack
	r.openRun(t, runReported, 20*time.Hour) // the newest heartbeat carries its progress
	r.openRun(t, runLeased, 20*time.Hour)   // its start lease is live
	r.openRun(t, runYoung, 2*time.Hour)
	advanced := r.now.Add(-15 * time.Hour).UnixMilli()
	progress, _ := json.Marshal(map[string]any{"run_id": runReported, "advanced_at": advanced})
	r.exec(t, `INSERT INTO device_presence (device_id, received_at, service_state, progress, created_at, updated_at)
		VALUES (?, ?, 'running', ?, ?, ?) ON CONFLICT(device_id) DO UPDATE SET received_at = excluded.received_at,
		service_state = excluded.service_state, progress = excluded.progress`,
		r.device, r.now.Add(-time.Minute).UnixMilli(), string(progress), r.now.UnixMilli(), r.now.UnixMilli())
	r.exec(t, `INSERT INTO start_leases (run_id, device_id, storage_target_id, granted_at, lease_until, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, runLeased, r.device, r.target, r.now.Add(-20*time.Hour).UnixMilli(), r.now.Add(time.Minute).UnixMilli(),
		r.now.UnixMilli(), r.now.UnixMilli())

	res, err := r.s.SweepAbandonedRuns(ctx, SweepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if o, fin := r.outcome(t, runOld); o != "abandoned" || fin != r.now.Add(-20*time.Hour).UnixMilli() {
		t.Fatalf("old run: %q finished %d (want its start: no activity was ever reported)", o, fin)
	}
	for _, id := range []string{runReported, runLeased, runYoung} {
		if o, _ := r.outcome(t, id); o != "" {
			t.Errorf("run %s swept: %q", id, o)
		}
	}
	if res.Abandoned < 1 {
		t.Fatalf("result %+v", res)
	}

	// The heartbeat stops reporting it: abandoned at the last progress it reported.
	r.exec(t, `UPDATE device_presence SET received_at = ? WHERE device_id = ?`, r.now.Add(-time.Hour).UnixMilli(), r.device)
	if _, err := r.s.SweepAbandonedRuns(ctx, SweepOptions{}); err != nil {
		t.Fatal(err)
	}
	if o, fin := r.outcome(t, runReported); o != "abandoned" || fin != advanced {
		t.Fatalf("reported run after the heartbeat went quiet: %q finished %d, want %d", o, fin, advanced)
	}

	// Idempotent: a second sweep changes nothing.
	again, err := r.s.SweepAbandonedRuns(ctx, SweepOptions{})
	if err != nil || again.Abandoned != 0 {
		t.Fatalf("second sweep %+v %v", again, err)
	}

	// A late real outcome overwrites abandoned.
	fin := r.now.Add(-19 * time.Hour)
	out, err := r.s.AgentIngestRuns(ctx, r.device, []RunInput{{ID: runOld, Kind: "backup", Trigger: "schedule",
		StartedAt: r.now.Add(-20 * time.Hour), FinishedAt: &fin, Outcome: "success"}}, r.now)
	if err != nil || len(out) != 1 || out[0].Status != IngestUpdated {
		t.Fatalf("late report: %+v %v", out, err)
	}
	if o, _ := r.outcome(t, runOld); o != "success" {
		t.Fatalf("late success did not overwrite abandoned: %q", o)
	}
}

// Console runs are the maintenance worker's, not the agent's to report: never swept.
func TestAbandonedRunSweeperLeavesConsoleRuns(t *testing.T) {
	r := newSweepRig(t)
	st := r.now.Add(-48 * time.Hour).UnixMilli()
	r.exec(t, `INSERT INTO runs (id, device_id, origin, kind, trigger_kind, started_at, received_at, created_at, updated_at)
		VALUES (?, ?, 'console', 'check', 'schedule', ?, ?, ?, ?)`, runOld, r.device, st, st, st, st)
	if _, err := r.s.SweepAbandonedRuns(context.Background(), SweepOptions{}); err != nil {
		t.Fatal(err)
	}
	if o, _ := r.outcome(t, runOld); o != "" {
		t.Fatalf("console run swept: %q", o)
	}
}
