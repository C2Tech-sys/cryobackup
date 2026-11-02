package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// firstClosed waits for the first closed run at the console and returns it.
func firstClosed(t *testing.T, fc *fakeConsole, what string) api.RunRecord {
	t.Helper()
	waitFor(t, what, 30*time.Second, func() bool { return len(fc.closedRuns()) >= 1 })
	return fc.closedRuns()[0]
}

// sentOpen reports whether an open record of run id ever reached the console
// (it does only when restic was about to start).
func sentOpen(fc *fakeConsole, id string) bool {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	for _, post := range fc.runPosts {
		for _, r := range post.Runs {
			if r.ID == id && r.Open() {
				return true
			}
		}
	}
	return false
}

func TestLeaseGrantedStartsTheRunAndHeartbeatsNameIt(t *testing.T) {
	fc := newFakeConsole(t)
	fc.leaseMode.Store(leaseGrant)
	dir := enroll(t, fc)
	fakerestic.Use(t, "slow")
	start(t, fakeOptions(t, dir))

	run := firstClosed(t, fc, "the granted run")
	if run.Outcome != restic.OutcomeSuccess || run.Stats.Lease != api.LeaseGranted || run.Trigger != api.TriggerBoot {
		t.Fatalf("run = %+v (lease %q)", run, run.Stats.Lease)
	}
	reqs := fc.leaseRequests()
	if len(reqs) != 1 || reqs[0].RunID != run.ID {
		t.Fatalf("lease requests = %+v, want one for run %s", reqs, run.ID)
	}
	if !sentOpen(fc, run.ID) {
		t.Error("the open record of the granted run never reached the console")
	}
	named := 0
	for _, hb := range fc.heartbeatList() {
		if hb.ActiveRunID == run.ID {
			named++
			if hb.ServiceState != api.ServiceRunning {
				t.Errorf("a heartbeat named the run while %s", hb.ServiceState)
			}
		} else if hb.ActiveRunID != "" {
			t.Errorf("a heartbeat named another run %q", hb.ActiveRunID)
		}
	}
	if named == 0 {
		t.Error("no heartbeat named the running backup: the console could not renew its lease")
	}
}

func TestLeaseUnlimitedIsNotRequired(t *testing.T) {
	fc := newFakeConsole(t)
	fc.leaseMode.Store(leaseUnlimited)
	dir := enroll(t, fc)
	fakerestic.Use(t, "backup_ok")
	start(t, fakeOptions(t, dir))
	if run := firstClosed(t, fc, "the run"); run.Outcome != restic.OutcomeSuccess || run.Stats.Lease != api.LeaseNotRequired {
		t.Fatalf("run = %+v (lease %q)", run, run.Stats.Lease)
	}
}

// A refused lease: the run is journaled deferred/fleet_cap, restic never
// starts, the heartbeat says deferred/fleet_cap, nothing is asked again before
// the wait is over, and the next run is still attempt 1 and still the boot run.
func TestLeaseRefusedDefersThenRuns(t *testing.T) {
	fc := newFakeConsole(t)
	fc.leaseMode.Store(leaseRefused)
	fc.leaseRetryS.Store(420)
	dir := enroll(t, fc)
	fakerestic.Use(t, "backup_ok")
	o := fakeOptions(t, dir)
	var askedMu sync.Mutex
	var asked []time.Duration
	o.leaseWait = func(retry time.Duration) time.Duration {
		askedMu.Lock()
		asked = append(asked, retry)
		askedMu.Unlock()
		return 1500 * time.Millisecond
	}
	start(t, o)

	deferred := firstClosed(t, fc, "the deferred run")
	if deferred.Outcome != restic.OutcomeDeferred || deferred.DeferredReason != restic.DeferredFleetCap ||
		deferred.ExitCode != nil || deferred.SnapshotID != "" || deferred.Stats.Lease != "" || deferred.Attempt != 1 {
		t.Fatalf("deferred run = %+v", deferred)
	}
	if sentOpen(fc, deferred.ID) {
		t.Fatal("an open record was sent for a refused run: restic was about to start")
	}
	if reqs := fc.leaseRequests(); len(reqs) != 1 || reqs[0].RunID != deferred.ID {
		t.Fatalf("lease requests = %+v", reqs)
	}
	askedMu.Lock()
	first := append([]time.Duration(nil), asked...)
	askedMu.Unlock()
	if len(first) == 0 || first[0] != 420*time.Second {
		t.Fatalf("the wait was computed from %v, want the console's 420 s", first)
	}
	waitFor(t, "a heartbeat that reports the deferral", 10*time.Second, func() bool {
		for _, hb := range fc.heartbeatList() {
			if hb.ServiceState == api.ServiceDeferred && hb.DeferredReason == restic.DeferredFleetCap {
				return true
			}
		}
		return false
	})
	time.Sleep(500 * time.Millisecond) // well inside the 1.5 s wait
	if n := len(fc.leaseRequests()); n != 1 {
		t.Fatalf("%d lease requests inside the wait, want 1", n)
	}

	fc.leaseMode.Store(leaseGrant)
	waitFor(t, "the run after the wait", 30*time.Second, func() bool {
		for _, r := range fc.closedRuns() {
			if r.Outcome == restic.OutcomeSuccess {
				return true
			}
		}
		return false
	})
	var ok api.RunRecord
	for _, r := range fc.closedRuns() {
		if r.Outcome == restic.OutcomeSuccess {
			ok = r
		}
	}
	if ok.Attempt != 1 || ok.Trigger != api.TriggerBoot || ok.Stats.Lease != api.LeaseGranted {
		t.Fatalf("the run after the deferral = %+v (lease %q): the deferral moved the retry ladder or lost the boot label", ok, ok.Stats.Lease)
	}
}

// Fail open (ADR 0005): an unreachable console, a timeout, 5xx, 429 or a
// console without the route start the run anyway and record it.
func TestLeaseFailsOpen(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode int32
	}{
		{"503", leaseUnavail},
		{"429", leaseLimited},
		{"404 (a console without the route)", leaseNotFound},
		{"timeout", leaseHang},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFakeConsole(t)
			fc.leaseMode.Store(tc.mode)
			t.Cleanup(func() { close(fc.leaseRelease) })
			dir := enroll(t, fc)
			fakerestic.Use(t, "backup_ok")
			o := fakeOptions(t, dir)
			o.leaseTimeout = 300 * time.Millisecond
			start(t, o)
			run := firstClosed(t, fc, "the bypassed run")
			if run.Outcome != restic.OutcomeSuccess || run.Stats.Lease != api.LeaseBypassed {
				t.Fatalf("run = %+v (lease %q)", run, run.Stats.Lease)
			}
			if reqs := fc.leaseRequests(); tc.mode != leaseNotFound && (len(reqs) != 1 || reqs[0].RunID != run.ID) {
				t.Fatalf("lease requests = %+v", reqs)
			}
		})
	}
}

// The console cannot be reached at all at start time.
func TestLeaseFailsOpenWhenTheConsoleIsDown(t *testing.T) {
	fc := newFakeConsole(t)
	fc.leaseMode.Store(leaseRefused) // would refuse, if it could answer
	fc.leaseRetryS.Store(900)
	dir := enroll(t, fc)
	fakerestic.Use(t, "backup_ok")
	fc.down.Store(true)
	r := start(t, fakeOptions(t, dir))
	waitFor(t, "the bypassed run in the journal", 30*time.Second, func() bool {
		rows, err := r.a.jr.RecentRuns(context.Background(), restic.KindBackup, 10)
		return err == nil && len(rows) == 1 && rows[0].Outcome == restic.OutcomeSuccess
	})
	rows, _ := r.a.jr.RecentRuns(context.Background(), restic.KindBackup, 10)
	if rows[0].Stats.Lease != api.LeaseBypassed {
		t.Fatalf("run = %+v (lease %q)", rows[0].RunRecord, rows[0].Stats.Lease)
	}
	fc.down.Store(false)
	if run := firstClosed(t, fc, "the bypassed run at the console"); run.Stats.Lease != api.LeaseBypassed {
		t.Fatalf("the console's copy says lease %q", run.Stats.Lease)
	}
}

// A 4xx other than 429 (here 401) is not a reason to start: nothing runs and
// nothing is journaled until the console answers properly.
func TestLeaseUnauthorizedDoesNotStart(t *testing.T) {
	fc := newFakeConsole(t)
	fc.leaseMode.Store(leaseUnauth)
	dir := enroll(t, fc)
	fakerestic.Use(t, "backup_ok")
	r := start(t, fakeOptions(t, dir))
	waitFor(t, "two refused lease requests (with the backoff between them)", 30*time.Second, func() bool { return len(fc.leaseRequests()) >= 2 })
	if rows, _ := r.a.jr.RecentRuns(context.Background(), restic.KindBackup, 10); len(rows) != 0 {
		t.Fatalf("runs journaled after a 401: %+v", rows)
	}
	fc.mu.Lock()
	posted := len(fc.runOrder)
	fc.mu.Unlock()
	if posted != 0 {
		t.Fatalf("%d runs reached the console after a 401", posted)
	}
	fc.leaseMode.Store(leaseGrant)
	if run := firstClosed(t, fc, "the run once the console grants"); run.Outcome != restic.OutcomeSuccess || run.Stats.Lease != api.LeaseGranted {
		t.Fatalf("run = %+v", run)
	} else if run.Trigger != api.TriggerBoot {
		// M1-FIX3 item 7: the refused starts were not this process's boot run.
		t.Fatalf("the first run after the 401s is labelled %q, want boot", run.Trigger)
	}
}

// backup_now does not ask for a lease, even while scheduled runs are capped.
func TestLeaseBackupNowSkipsTheCall(t *testing.T) {
	fc := newFakeConsole(t)
	fc.leaseMode.Store(leaseRefused)
	fc.leaseRetryS.Store(900)
	dir := enroll(t, fc)
	fakerestic.Use(t, "backup_ok")
	o := fakeOptions(t, dir)
	o.leaseWait = func(time.Duration) time.Duration { return time.Hour }
	start(t, o)
	if d := firstClosed(t, fc, "the capped scheduled run"); d.DeferredReason != restic.DeferredFleetCap {
		t.Fatalf("first run = %+v", d)
	}
	before := len(fc.leaseRequests())
	fc.enqueue(command(1, api.CommandBackupNow, ""))
	waitFor(t, "the manual run", 30*time.Second, func() bool { return len(fc.closedRuns()) >= 2 })
	manual := fc.closedRuns()[1]
	if manual.Trigger != api.TriggerManual || manual.Outcome != restic.OutcomeSuccess || manual.Stats.Lease != api.LeaseNotRequired {
		t.Fatalf("manual run = %+v (lease %q)", manual, manual.Stats.Lease)
	}
	if after := len(fc.leaseRequests()); after != before {
		t.Fatalf("backup_now asked for a lease (%d -> %d requests)", before, after)
	}
}

// leaseAbortBound is how long a stop or a cancel may take to end a hung lease
// call: far below the one-minute bound the tests give the call, far above the
// tens of milliseconds it takes.
const leaseAbortBound = 2 * time.Second

// A service stop while the lease call hangs aborts it at once: the run is
// interrupted, restic never starts, the agent stops promptly.
func TestStopDuringTheLeaseCall(t *testing.T) {
	fc := newFakeConsole(t)
	fc.leaseMode.Store(leaseHang)
	t.Cleanup(func() { close(fc.leaseRelease) })
	dir := enroll(t, fc)
	fakerestic.Use(t, "backup_ok")
	o := fakeOptions(t, dir)
	// The agent's and the uplink's bound (Options.StartLeaseTimeout) are both a
	// minute: only the stop can end the call within leaseAbortBound.
	o.leaseTimeout = time.Minute
	r := start(t, o)
	waitFor(t, "the lease call", 30*time.Second, func() bool { return len(fc.leaseRequests()) == 1 })
	began := time.Now()
	r.stop()
	if d := time.Since(began); d > leaseAbortBound {
		t.Fatalf("the stop took %v with a hung lease call (bound %v)", d, leaseAbortBound)
	}
	run := firstClosed(t, fc, "the interrupted run")
	if run.Outcome != restic.OutcomeInterrupted || run.ID != fc.leaseRequests()[0].RunID || run.ExitCode != nil {
		t.Fatalf("run = %+v", run)
	}
	if sentOpen(fc, run.ID) {
		t.Fatal("restic was about to start after a stop during the lease call")
	}
}

// A cancel command while the lease call hangs cancels the run.
func TestCancelDuringTheLeaseCall(t *testing.T) {
	fc := newFakeConsole(t)
	fc.leaseMode.Store(leaseHang)
	t.Cleanup(func() { close(fc.leaseRelease) })
	dir := enroll(t, fc)
	fakerestic.Use(t, "backup_ok")
	o := fakeOptions(t, dir)
	o.leaseTimeout = time.Minute // and so the uplink's bound: only the cancel can end the call
	r := start(t, o)
	waitFor(t, "the lease call", 30*time.Second, func() bool { return len(fc.leaseRequests()) == 1 })
	cancel := command(1, api.CommandCancel, "")
	began := time.Now()
	fc.enqueue(cancel)
	waitFor(t, "the cancelled run (the cancel must end the hung lease call at once)", leaseAbortBound,
		func() bool { return len(fc.closedRuns()) >= 1 })
	t.Logf("cancel to closed run: %v", time.Since(began))
	run := fc.closedRuns()[0]
	if run.Outcome != restic.OutcomeCancelled || run.ID != fc.leaseRequests()[0].RunID || run.Trigger != api.TriggerBoot {
		t.Fatalf("run = %+v", run)
	}
	// M1-FIX3 item 7: restic never started, so the next start is still the
	// boot run.
	waitFor(t, "the boot label back for the next start", 5*time.Second, func() bool {
		r.a.mu.Lock()
		defer r.a.mu.Unlock()
		return r.a.bootPending && r.a.running == nil
	})
	waitFor(t, "the cancel result", 10*time.Second, func() bool { return len(fc.commandResults(cancel.ID)) >= 1 })
	if res := fc.commandResults(cancel.ID)[0]; res.Status != api.CommandSucceeded || string(res.Result) != `{"cancelled":true}` {
		t.Fatalf("cancel result = %+v", res)
	}
}
