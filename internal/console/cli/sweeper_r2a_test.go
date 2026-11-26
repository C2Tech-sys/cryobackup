package cli

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// P22-R2a fix G1: serve gives the abandoned-run-sweeper the start of the console process
// (SweepOptions.ConsoleStartedAt, captured once in addMonitorJobs), so every start holds the
// device-restore reconnect grace: proved by behaviour, through the job serve registered.
func TestR2A_ServeSweeperHoldsTheGraceFromTheProcessStart(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	set := func(t time.Time) { mu.Lock(); now = t; mu.Unlock() }
	app, _ := buildFromInitWith(t, "America/Chicago", "America/Chicago", BuildOptions{Clock: clock})
	start := clock() // Build ran addMonitorJobs at this instant
	ctx := context.Background()
	// A running device restore whose device was last heard from 25 hours ago, and a previous sweep
	// five minutes ago: no outage to see, so only the process start can hold the grace.
	const id = "01JR2A0000000000000000CL01"
	if err := app.Store.Write(ctx, func(tx *store.Tx) error {
		var target string
		if err := tx.QueryRow(`SELECT id FROM storage_targets ORDER BY id LIMIT 1`).Scan(&target); err != nil {
			return err
		}
		ms := start.UnixMilli()
		const cust, dev = "01JR2A0000000000000000CU01", "01JR2A0000000000000000DV01"
		if _, err := tx.Exec(`INSERT INTO customers (id, name, slug, storage_target_id, created_at, updated_at) VALUES (?, 'R2A Test', 'r2a-test', ?, ?, ?)`,
			cust, target, ms, ms); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO devices (id, customer_id, hostname, enrolled_at, enrolled_via, repo_path, created_at, updated_at)
			VALUES (?, ?, 'R2A-PC', ?, 'token', 'r2a/dev1/', ?, ?)`, dev, cust, ms, ms, ms); err != nil {
			return err
		}
		fetched := start.Add(-26 * time.Hour).UnixMilli()
		if _, err := tx.Exec(`INSERT INTO restore_requests (id, device_id, snapshot_id, mode, overwrite, selection_count, selection_wrapped, status,
				started_at, expires_at, created_at, updated_at)
			VALUES (?, ?, ?, 'device_path', 'never', 1, X'0102', 'running', ?, ?, ?, ?)`,
			id, dev, strings.Repeat("ab", 32), fetched, fetched+24*3600*1000, fetched, fetched); err != nil {
			return err
		}
		heard := start.Add(-25 * time.Hour).UnixMilli()
		_, err := tx.Exec(`INSERT INTO device_presence (device_id, received_at, service_state, uptime_s, created_at, updated_at)
			VALUES (?, ?, 'running', ?, ?, ?) ON CONFLICT(device_id) DO UPDATE SET received_at = excluded.received_at,
			uptime_s = excluded.uptime_s, progress = NULL`, dev, heard, int64(30*24*3600), heard, heard)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	prev := start.Add(-5 * time.Minute)
	if err := app.Store.RecordJobRun(ctx, jobs.NameAbandonedRunSweeper, store.JobRun{Status: "ok", StartedAt: prev, FinishedAt: prev}); err != nil {
		t.Fatal(err)
	}
	status := func() string {
		t.Helper()
		rr, err := app.Store.GetRestoreRequest(ctx, store.ScopeAll(), id)
		if err != nil {
			t.Fatal(err)
		}
		return rr.Status
	}
	run := func() {
		t.Helper()
		if ran, err := app.Runner.RunOnce(ctx, jobs.NameAbandonedRunSweeper); !ran || err != nil {
			t.Fatalf("sweeper ran %v err %v", ran, err)
		}
	}
	set(start.Add(5 * time.Second))
	run()
	if s := status(); s != "running" {
		t.Fatalf("the first sweep after the start closed the restore: %s (ConsoleStartedAt not wired)", s)
	}
	set(start.Add(store.DeviceRestoreReconnectGrace - time.Minute))
	run()
	if s := status(); s != "running" {
		t.Fatalf("closed inside the grace: %s", s)
	}
	set(start.Add(store.DeviceRestoreReconnectGrace + time.Minute)) // the grace counted from the injected start is over
	run()
	if s := status(); s != "failed" {
		t.Fatalf("not closed after the grace: %s", s)
	}
}
