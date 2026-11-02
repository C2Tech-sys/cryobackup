package agent

import (
	"context"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
	"github.com/c2tech-sys/cryobackup/internal/agent/scheduler"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// M1 acceptance (architecture 10.2): "gateway down mid-run gives deferred,
// never failed". The fake restic of the "gateway_midrun" scenario does what
// restic does when the gateway is lost after the upload began (progress, then
// its retry messages, then a Fatal for a refused connection, exit 1); the
// agent is the real one. The live counterpart with the real restic and a
// gateway that is really stopped is in resume_t5_test.go.

// retryBounds returns the window the scheduler's backoff allows for the delay
// after the attempt-th incomplete attempt (RetryDelay spreads it over 0.8 to
// 1.2 of the step).
func retryBounds(attempt int) (lo, hi time.Duration) {
	rt := testPolicy().Schedule.Retries
	return scheduler.RetryDelay(rt, attempt, false, 0), scheduler.RetryDelay(rt, attempt, false, 0.999999)
}

func TestT5GatewayLostMidRunIsDeferredNotFailed(t *testing.T) {
	ctx := context.Background()
	fc := newFakeConsole(t)
	dir := enroll(t, fc)
	fakerestic.Use(t, "gateway_midrun")
	r := start(t, fakeOptions(t, dir))
	jr := openJournalRO(t, dir)

	waitFor(t, "the deferred run at the console", 30*time.Second, func() bool { return len(fc.closedRuns()) == 1 })
	run := fc.closedRuns()[0]
	if run.Outcome != restic.OutcomeDeferred || run.DeferredReason != restic.DeferredGateway || run.ErrorClass != restic.ClassNetwork {
		t.Fatalf("run = outcome %s reason %s class %s, want deferred / gateway / network", run.Outcome, run.DeferredReason, run.ErrorClass)
	}
	if run.ExitCode == nil || *run.ExitCode != 1 || run.SnapshotID != "" || run.Stats.FailedFiles != 0 || run.Attempt != 1 {
		t.Errorf("run = %+v", run)
	}
	if err := run.Validate(); err != nil {
		t.Errorf("the deferred record does not validate: %v", err)
	}
	if err := api.AssertPathFree(run); err != nil {
		t.Errorf("the deferred record is not path-free: %v", err)
	}

	// The journal says the same, and the run is delivered.
	rows, _ := jr.RecentRuns(ctx, restic.KindBackup, 5)
	if len(rows) != 1 || rows[0].Outcome != restic.OutcomeDeferred || rows[0].DeferredReason != restic.DeferredGateway {
		t.Fatalf("journal rows = %+v", rows)
	}
	waitFor(t, "delivery of the deferred run", 30*time.Second, func() bool {
		row, ok, _ := jr.Run(ctx, run.ID)
		return ok && row.DeliveredAt != nil
	})

	// The device reports the deferral on its heartbeat, with the class; nothing
	// is called a failure and no snapshot list is pushed for a run without a snapshot.
	waitFor(t, "a heartbeat that says deferred / gateway", 30*time.Second, func() bool {
		for _, hb := range fc.heartbeatList() {
			if hb.ServiceState == api.ServiceDeferred && hb.DeferredReason == restic.DeferredGateway && hb.LastErrorClass == restic.ClassNetwork {
				return true
			}
		}
		return false
	})
	if len(fc.snapshotUploads()) != 0 {
		t.Error("a snapshot list was uploaded after a run that made no snapshot")
	}

	// The next attempt is owed after the scheduler's backoff (5 minutes, spread
	// 0.8 to 1.2, for the first), not at once.
	f := r.a.facts(ctx)
	lo, hi := retryBounds(1)
	if !f.Retry.Pending || f.Attempts != 1 || f.Last == nil || f.Last.FinishedAt == nil {
		t.Fatalf("facts after the deferral = %+v", f)
	}
	if wait := f.Retry.NotBefore.Sub(*f.Last.FinishedAt); wait < lo || wait > hi {
		t.Errorf("next attempt in %v, want %v..%v", wait, lo, hi)
	}
	time.Sleep(900 * time.Millisecond)
	if n := len(fc.closedRuns()); n != 1 {
		t.Fatalf("%d runs; a deferred run must wait out its backoff", n)
	}

	// A second attempt (the operator's backup_now) is deferred again; the ladder
	// climbs (attempt 2, a doubled wait) and nothing ever becomes "failed".
	fc.enqueue(command(1, api.CommandBackupNow, ""))
	waitFor(t, "the second deferred run", 30*time.Second, func() bool { return len(fc.closedRuns()) == 2 })
	second := fc.closedRuns()[1]
	if second.Outcome != restic.OutcomeDeferred || second.DeferredReason != restic.DeferredGateway || second.Attempt != 2 || second.Trigger != api.TriggerManual {
		t.Fatalf("second run = %+v", second)
	}
	f = r.a.facts(ctx)
	lo, hi = retryBounds(2)
	if f.Attempts != 2 || !f.Retry.Pending || f.Last == nil || f.Last.FinishedAt == nil {
		t.Fatalf("facts after two deferrals = %+v", f)
	}
	if wait := f.Retry.NotBefore.Sub(*f.Last.FinishedAt); wait < lo || wait > hi {
		t.Errorf("after two deferrals the next attempt is in %v, want %v..%v", wait, lo, hi)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	for id, rec := range fc.runs {
		if rec.Outcome == restic.OutcomeFailed {
			t.Errorf("run %s was reported failed", id)
		}
	}
}
