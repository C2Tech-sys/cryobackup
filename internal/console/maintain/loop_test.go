package maintain

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

func usageRows(t *testing.T, e *tenv, deviceID string) (rows int, repoBytes int64) {
	t.Helper()
	err := e.st.Read(e.ctx, func(tx *store.Tx) error {
		return tx.QueryRow(`SELECT COUNT(*), COALESCE(MAX(repo_bytes), 0) FROM usage_samples WHERE device_id = ?`, deviceID).Scan(&rows, &repoBytes)
	})
	if err != nil {
		t.Fatal(err)
	}
	return rows, repoBytes
}

func TestRunOnceSamplesUsage(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "stats", Stdout: "stats_raw_data.json"}}, nil)
	e.onlyDevice(e.esc)

	rep, err := e.w.RunOnce(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Considered != 1 || rep.Sampled != 1 || rep.Failed != 0 || rep.Skipped || rep.M5Due != 3 {
		t.Fatalf("report = %+v", rep)
	}
	if n, b := usageRows(t, e, e.esc); n != 1 || b != 2111764 {
		t.Fatalf("usage rows = %d, repo bytes = %d", n, b)
	}
	js, err := e.st.GetJobState(e.ctx, SchedulerJob)
	if err != nil || js.LastStatus != "ok" || js.LeaseHolder != "" || js.NextDueAt == nil {
		t.Fatalf("job state = %+v, %v", js, err)
	}
	ms, err := e.st.GetMaintenanceSchedule(e.ctx, store.ScopeAll(), e.esc)
	if err != nil || ms.LeaseHolder != "" {
		t.Fatalf("maintenance lease not released: %+v, %v", ms, err)
	}

	// Within the interval nothing is sampled again; the hour bucket makes a
	// second sample in the same hour an upsert anyway.
	e.clk.Advance(time.Hour)
	rep, _ = e.w.RunOnce(e.ctx)
	if rep.Sampled != 0 {
		t.Fatalf("sampled again inside the interval: %+v", rep)
	}
	e.clk.Advance(6 * time.Hour)
	rep, _ = e.w.RunOnce(e.ctx)
	if rep.Sampled != 1 {
		t.Fatalf("not sampled after the interval: %+v", rep)
	}
	if n, _ := usageRows(t, e, e.esc); n != 2 {
		t.Fatalf("usage rows = %d, want 2 hour buckets", n)
	}
	if got := len(e.consoleRuns(e.esc)); got != 0 {
		t.Fatalf("usage refresh recorded %d runs; read-only calls are not runs", got)
	}
}

func TestRunOnceSkipsCustomerHeldAndDisabled(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "stats", Stdout: "stats_raw_data.json"}}, nil)
	e.onlyDevice(e.held) // enabled, but the customer holds the key
	rep, err := e.w.RunOnce(e.ctx)
	if err != nil || rep.Considered != 0 || rep.Sampled != 0 {
		t.Fatalf("report = %+v, %v", rep, err)
	}
	if len(e.repoCalls()) != 0 {
		t.Fatal("restic ran for a customer-held device")
	}
}

func TestRunOncePreflight(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "stats", Stdout: "stats_raw_data.json"}}, nil)
	e.onlyDevice(e.esc)
	now := e.clk.Now().UnixMilli()
	err := e.st.Write(e.ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT OR REPLACE INTO device_presence (device_id, received_at, service_state, created_at, updated_at)
			VALUES (?, ?, 'running', ?, ?)`, e.esc, now, now, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := e.w.RunOnce(e.ctx)
	if err != nil || rep.Preflight != 1 || rep.Sampled != 0 {
		t.Fatalf("report = %+v, %v", rep, err)
	}
	if len(e.repoCalls()) != 0 {
		t.Fatal("restic ran against a device whose backup is running")
	}
	// The reschedule holds even after the heartbeat goes idle, until 15 to 60 minutes pass.
	err = e.st.Write(e.ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE device_presence SET service_state = 'idle' WHERE device_id = ?`, e.esc)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(5 * time.Minute)
	if rep, _ = e.w.RunOnce(e.ctx); rep.Preflight != 1 || rep.Sampled != 0 {
		t.Fatalf("retried inside the reschedule window: %+v", rep)
	}
	e.clk.Advance(61 * time.Minute)
	if rep, _ = e.w.RunOnce(e.ctx); rep.Sampled != 1 {
		t.Fatalf("not retried after the window: %+v", rep)
	}
}

func TestRunOnceOpenBackupRunDefers(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "stats", Stdout: "stats_raw_data.json"}}, nil)
	e.onlyDevice(e.esc)
	now := e.clk.Now().UnixMilli()
	err := e.st.Write(e.ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO runs (id, device_id, origin, kind, trigger_kind, started_at, received_at, created_at, updated_at)
			VALUES ('01TESTOPENRUN00000000000AA', ?, 'agent', 'backup', 'schedule', ?, ?, ?, ?)`, e.esc, now, now, now, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep, err := e.w.RunOnce(e.ctx); err != nil || rep.Preflight != 1 {
		t.Fatalf("report = %+v, %v", rep, err)
	}
}

func TestRunOnceLockDeferralsThenUnlock(t *testing.T) {
	e := newTenv(t, []fakeRule{
		{Cmd: "stats", Exit: 11, Stderr: "exit11_lock.stderr.jsonl"},
		{Cmd: "unlock"},
	}, nil)
	e.onlyDevice(e.esc)

	for i := 1; i <= 2; i++ {
		rep, err := e.w.RunOnce(e.ctx)
		if err != nil || rep.Deferred != 1 || rep.Failed != 0 || rep.Unlocked != 0 {
			t.Fatalf("pass %d: report = %+v, %v", i, rep, err)
		}
		if js, _ := e.st.GetJobState(e.ctx, SchedulerJob); js.LastStatus != "ok" {
			t.Fatalf("a lock deferral made the job %q; deferral is not a failure", js.LastStatus)
		}
		ms, _ := e.st.GetMaintenanceSchedule(e.ctx, store.ScopeAll(), e.esc)
		if ms.DeferredCount != i || ms.LastDeferredAt == nil {
			t.Fatalf("pass %d: deferred_count = %d", i, ms.DeferredCount)
		}
	}
	rep, err := e.w.RunOnce(e.ctx)
	if err != nil || rep.Deferred != 1 || rep.Unlocked != 1 {
		t.Fatalf("third pass: report = %+v, %v", rep, err)
	}
	ms, _ := e.st.GetMaintenanceSchedule(e.ctx, store.ScopeAll(), e.esc)
	if ms.DeferredCount != 0 {
		t.Fatalf("deferred_count = %d after the unlock", ms.DeferredCount)
	}
	runs := e.consoleRuns(e.esc)
	if len(runs) != 1 || runs[0].Kind != "unlock" || runs[0].Trigger != "schedule" {
		t.Fatalf("runs = %+v", runs)
	}
	for _, c := range e.repoCalls() {
		if c.Args[0] == "unlock" {
			for _, a := range c.Args {
				if a == "--remove-all" {
					t.Fatal("the scheduler ran unlock --remove-all")
				}
			}
		}
	}
}

func TestRunOnceFailureIsLoud(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "stats", Exit: 12, Stderr: "exit12_wrong_password_plain.stderr.txt"}}, nil)
	e.onlyDevice(e.esc)
	rep, err := e.w.RunOnce(e.ctx)
	if err != nil || rep.Failed != 1 || rep.ErrorClass != restic.ClassWrongPassword {
		t.Fatalf("report = %+v, %v", rep, err)
	}
	js, err := e.st.GetJobState(e.ctx, SchedulerJob)
	if err != nil || js.LastStatus != "error" || js.LastErrorClass != "wrong_password" {
		t.Fatalf("job state = %+v, %v", js, err)
	}
	if n, _ := usageRows(t, e, e.esc); n != 0 {
		t.Fatal("a failed stats wrote a usage sample")
	}
}

func TestRunOnceSkipsWhenLeaseHeld(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "stats", Stdout: "stats_raw_data.json"}}, nil)
	e.onlyDevice(e.esc)
	ok, err := e.st.AcquireJobLease(e.ctx, SchedulerJob, "another-maintain", time.Hour)
	if err != nil || !ok {
		t.Fatalf("lease: %v %v", ok, err)
	}
	// The holder's last pass failed: C07 must keep seeing that, whatever a standby does.
	t0 := e.clk.Now().Add(-time.Minute)
	if err := e.st.RecordJobRun(e.ctx, SchedulerJob, store.JobRun{
		Status: "error", ErrorClass: string(restic.ClassNetwork), StartedAt: t0.Add(-time.Second), FinishedAt: t0,
	}); err != nil {
		t.Fatal(err)
	}
	before, err := e.st.GetJobState(e.ctx, SchedulerJob)
	if err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(5 * time.Minute)

	rep, err := e.w.RunOnce(e.ctx)
	if err != nil || !rep.Skipped || rep.Sampled != 0 || rep.Considered != 0 {
		t.Fatalf("report = %+v, %v", rep, err)
	}
	after, err := e.st.GetJobState(e.ctx, SchedulerJob)
	if err != nil {
		t.Fatal(err)
	}
	if after.LastStatus != "error" || after.LastErrorClass != string(restic.ClassNetwork) {
		t.Fatalf("a non-holder overwrote the holder's status: %+v", after)
	}
	if after.LastFinishedAt == nil || before.LastFinishedAt == nil || !after.LastFinishedAt.Equal(*before.LastFinishedAt) {
		t.Fatalf("last_finished_at changed: before %v after %v", before.LastFinishedAt, after.LastFinishedAt)
	}
	if after.RunCount != before.RunCount || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("job_state row was written: before %+v after %+v", before, after)
	}
	if after.LeaseHolder != "another-maintain" {
		t.Fatalf("lease holder = %q", after.LeaseHolder)
	}
}

// A non-holder must not create the row either (nothing has ever run).
func TestRunOnceSkipWritesNoJobStateRow(t *testing.T) {
	e := newTenv(t, nil, nil)
	if ok, err := e.st.AcquireJobLease(e.ctx, SchedulerJob, "another-maintain", time.Hour); err != nil || !ok {
		t.Fatalf("lease: %v %v", ok, err)
	}
	if rep, err := e.w.RunOnce(e.ctx); err != nil || !rep.Skipped {
		t.Fatalf("report = %+v, %v", rep, err)
	}
	js, err := e.st.GetJobState(e.ctx, SchedulerJob)
	if err != nil {
		t.Fatal(err)
	}
	if js.LastStatus != "" || js.LastFinishedAt != nil || js.RunCount != 0 {
		t.Fatalf("a skipped pass wrote run data: %+v", js)
	}
}

func TestLoopServesTheAdminPath(t *testing.T) {
	dir, err := os.MkdirTemp("", "cm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "a.sock")
	e := newTenv(t, []fakeRule{
		{Cmd: "cat", Stdout: "cat_config.json"},
		{Cmd: "stats", Stdout: "stats_raw_data.json"},
	}, func(c *Config) { c.SocketPath = sock; c.TickEvery = time.Hour })
	e.onlyDevice(e.esc)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.w.Loop(ctx) }()

	cl := adminpath.NewClient(sock, adminpath.ClientOptions{})
	defer cl.Close()
	var pingErr error
	for i := 0; i < 100; i++ {
		if _, pingErr = cl.Ping(ctx); pingErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pingErr != nil {
		t.Fatalf("ping: %v", pingErr)
	}
	cfg, err := cl.CatConfig(ctx, dev(e.esc))
	if err != nil || cfg.ID == "" {
		t.Fatalf("cat config over the socket = %+v, %v", cfg, err)
	}
	_, err = cl.CatConfig(ctx, dev(e.held))
	wantCode(t, err, adminpath.CodeCustodyCustomerHeld, "")
	_, err = cl.Forget(ctx, adminpath.ForgetRequest{DeviceRequest: dev(e.esc), KeepWithin: "30d", Rules: adminpath.KeepRules{KeepLast: 3}})
	wantCode(t, err, adminpath.CodeUnavailable, "")

	// The loop's first pass ran as soon as it started.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if n, _ := usageRows(t, e, e.esc); n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the scheduler did not take a usage sample")
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Loop returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Loop did not stop")
	}
}

func TestLoopNeedsASocket(t *testing.T) {
	e := newTenv(t, nil, nil)
	if err := e.w.Loop(e.ctx); err == nil {
		t.Fatal("Loop without SocketPath succeeded")
	}
}
