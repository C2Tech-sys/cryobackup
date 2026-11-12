package agent

import (
	"context"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
	"github.com/c2tech-sys/cryobackup/internal/agent/journal"
	"github.com/c2tech-sys/cryobackup/internal/agent/scheduler"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// M3-L10 (ADR 0018): a refused agent's run is turned away by the gateway
// (deferred, class auth), and the run ladder would retry it about hourly for
// good. While REFUSED the next attempt waits for the policy's interval; when
// REFUSED ends the ladder is back and the run is due again at the next plan.
//
// The deferral is written into the journal before the agent starts, finished
// long enough ago that the ladder's gate (30 s to 36 s with the 30 s minimum the
// policy allows) has passed: the ladder would start the run the moment the
// startup delay ends, so an attempt before the interval is up can only be the
// ladder. (No fake restic scenario ends with class auth; the journal row is what
// such a run leaves.)

// l10Start enrolls a device whose journal already holds one auth deferral from a
// minute ago and starts the agent with a startup delay of delayS seconds (the
// time the test has to get the agent refused before its first plan can start a
// run). It returns the deferral's finish time.
func l10Start(t *testing.T, delayS int, tune func(*Options)) (*fakeConsole, *running, time.Time) {
	t.Helper()
	fakerestic.Use(t, "backup_ok")
	fc := newFakeConsole(t)
	pol := fc.policy
	pol.Policy.Schedule.StartupDelayS = delayS
	pol.Policy.Schedule.Retries.BaseS, pol.Policy.Schedule.Retries.CapS = 30, 30
	fc.setPolicy(pol)
	dir := enroll(t, fc)

	finished := time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond)
	started := finished.Add(-time.Second)
	exit := 1
	rec := api.RunRecord{
		ID: newULID(), Kind: restic.KindBackup, Trigger: api.TriggerSchedule, StartedAt: started, FinishedAt: &finished,
		Outcome: restic.OutcomeDeferred, DeferredReason: restic.DeferredGateway, ErrorClass: restic.ClassAuth,
		ExitCode: &exit, Attempt: 1, AgentVersion: versionLabel(), PolicyVersion: 1,
	}
	if err := rec.Validate(); err != nil {
		t.Fatalf("the injected record does not validate: %v", err)
	}
	jr, err := journal.Open(context.Background(), journal.Options{Path: config.Layout{Root: dir}.JournalFile()})
	if err != nil {
		t.Fatal(err)
	}
	if err := jr.InsertFinished(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if err := jr.Close(); err != nil {
		t.Fatal(err)
	}

	o := fakeOptions(t, dir)
	if tune != nil {
		tune(&o)
	}
	return fc, start(t, o), finished
}

func l10Runs(t *testing.T, r *running) []journal.RunRow {
	t.Helper()
	rows, err := r.a.jr.RecentRuns(context.Background(), restic.KindBackup, 20)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// A refused agent is not asked to try again until the policy's interval is up:
// no attempt after the startup delay (when the ladder would have started one),
// the plan says so, and the end of REFUSED brings the ladder back: the run
// starts again within moments.
func TestRefusedAgentTriesOncePerInterval(t *testing.T) {
	const delayS = 6 // far more than getting refused takes (about half a second, under load a second or two)
	fc, r, deferredAt := l10Start(t, delayS, nil)
	ctx := context.Background()

	waitFor(t, "an accepted heartbeat", 10*time.Second, func() bool { return len(fc.heartbeatList()) >= 1 })
	boot := time.Now()
	l4Refuse(t, fc, r)
	if time.Since(boot) > (delayS-2)*time.Second {
		t.Fatalf("the agent took %v to be refused: the startup delay is no margin", time.Since(boot))
	}

	// The plan: wait for the interval counted from the deferral, not the ladder.
	in, ok := r.a.plannerInput(ctx, time.Now())
	if !ok || !in.AuthDeferralsWait {
		t.Fatalf("plannerInput = ok %v, AuthDeferralsWait %v, want true for a refused agent", ok, in.AuthDeferralsWait)
	}
	in.BootAt = time.Now().Add(-time.Hour) // past the startup delay: only the retry rule is in question
	out := scheduler.Plan(in)
	if out.Action != scheduler.ActionNone {
		t.Fatalf("Plan = %s %q, want none while refused", out.Action, out.Reason)
	}
	if d := out.NextCheck.Sub(deferredAt); d < 23*time.Hour || d > 26*time.Hour {
		t.Errorf("the next attempt is %v after the deferral, want about the policy's 24 h interval", d)
	}

	// The startup delay ends and several scheduler ticks pass: nothing starts.
	time.Sleep(time.Until(boot.Add(delayS*time.Second)) + 2*time.Second)
	if rows := l10Runs(t, r); len(rows) != 1 {
		t.Fatalf("%d backup rows while refused, want only the deferral: the ladder started a run", len(rows))
	}
	if !r.a.isRefused() {
		t.Fatal("REFUSED ended by itself")
	}

	// The token is accepted again: the ladder is back, its gate is long past, so
	// the run starts and succeeds.
	fc.restore()
	waitFor(t, "the agent to leave REFUSED", 5*time.Second, func() bool { return !r.a.isRefused() })
	restored := time.Now()
	waitFor(t, "the run to be attempted again", 15*time.Second, func() bool {
		rows := l10Runs(t, r)
		return len(rows) == 2 && rows[0].Outcome == restic.OutcomeSuccess
	})
	t.Logf("attempted again %v after REFUSED ended", time.Since(restored).Round(10*time.Millisecond))
	if rows := l10Runs(t, r); rows[0].Trigger != api.TriggerRetry {
		t.Errorf("the run after REFUSED is a %q run, want retry (the ladder)", rows[0].Trigger)
	}
}

// Not refused, nothing changes: the same auth deferral stays on the ladder, so
// the run is attempted as soon as the ladder's gate and the startup delay allow.
func TestAuthDeferralWithAHealthyConsoleKeepsTheLadder(t *testing.T) {
	_, r, _ := l10Start(t, 1, nil)
	waitFor(t, "the run to be attempted on the ladder", 15*time.Second, func() bool {
		rows := l10Runs(t, r)
		return len(rows) == 2 && rows[0].Outcome == restic.OutcomeSuccess && rows[0].Trigger == api.TriggerRetry
	})
	if r.a.isRefused() {
		t.Error("refused with a healthy console")
	}
}
