package jobs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// waitLeaseRenewedAfter waits until holder has renewed ProbeLease past until. A
// fixed sleep here read a machine under load as a renewer that had stopped.
func waitLeaseRenewedAfter(t *testing.T, r *proberRig, holder string, until time.Time) {
	t.Helper()
	var j store.JobState
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		j = r.row(t, ProbeLease)
		if j.LeaseHolder == holder && j.LeaseUntil != nil && j.LeaseUntil.After(until) {
			return
		}
	}
	t.Fatalf("lease not renewed past %v while a probe runs: %+v", until, j)
}

// holdWrites takes the store's one writer and keeps it until the returned func
// runs, as a hung disk or a stuck job would.
func holdWrites(t *testing.T, st *store.Store) (release func()) {
	t.Helper()
	held, done := make(chan struct{}), make(chan struct{})
	ended := make(chan error, 1)
	go func() {
		ended <- st.Write(context.Background(), func(*store.Tx) error { close(held); <-done; return nil })
	}()
	<-held
	var once sync.Once
	release = func() {
		once.Do(func() {
			close(done)
			if err := <-ended; err != nil {
				t.Errorf("the held write: %v", err)
			}
		})
	}
	t.Cleanup(release)
	return release
}

// Mutant m30: a store that hangs counts against the lease as an error does. The
// renewal gives up on its bound, and once the lease has run out the probes are
// cancelled. Without the bound the renewer would wait on the store for ever.
func TestC08RenewerHungStoreSpanningTheLeaseCancelsTheProbes(t *testing.T) { // M3-STORE2 4
	r := newProberRig(t, false)
	be := &countingBackend{Backend: r.be, release: make(chan struct{})}
	defer close(be.release)
	r.hp.Backend, r.hp.Holder, r.hp.Interval = be, "console-a", 30*time.Millisecond // a renewal gives up after one interval
	if err := r.hp.Run(r.ctx); err != nil {
		t.Fatal(err)
	}
	waitActive(t, be)
	defer holdWrites(t, r.st)() // before the rig's cleanup, whose Stop writes
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(r.logs.String(), "health probe lease not renewed") {
		if time.Now().After(deadline) {
			t.Fatalf("a renewal against a hung store never gave up: %s", r.logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if be.active.Load() != 1 {
		t.Fatal("a hung store inside the lease cancelled the probe")
	}
	r.clk.Advance(r.hp.leaseTTL()) // the lease has now run out
	for be.active.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the probe kept running after the lease ran out under a hung store")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if logs := r.logs.String(); !strings.Contains(logs, "health probe lease lost") {
		t.Fatalf("no lease-lost line: %s", logs)
	}
}

// Mutant m11a: enter's stopped check. A call after Stop is refused before it
// touches the store, so it answers at once even when the store is stuck, and it
// never takes the lease even for a moment. acquire's undo alone would do neither.
func TestC08CallsAfterStopDoNotTouchTheStore(t *testing.T) { // M3-STORE2 5
	r := newProberRig(t, false)
	r.hp.Holder = "console-a"
	r.hp.Stop()
	defer holdWrites(t, r.st)()
	answered := make(chan error, 2)
	go func() { answered <- r.hp.Run(context.Background()) }()
	go func() { _, err := r.hp.ProbeNow(context.Background()); answered <- err }()
	for range 2 {
		select {
		case err := <-answered:
			if !errors.Is(err, errDidNotRun) && !errors.Is(err, errProbesCancelled) {
				t.Fatalf("a call after Stop = %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("a call after Stop waited on the store")
		}
	}
}

// Mutant m21: Stop's wait for the calls already taking the lease. Stop must not
// return, or release the lease, while one is still in progress: acquire's undo
// only covers a Stop whose bound ran out. The call is held inside the store, past
// Stop's start, by two writes in a row; the second proves Stop was still waiting.
func TestC08StopWaitsForACallTakingTheLease(t *testing.T) { // M3-STORE2 5
	r := newProberRig(t, false)
	entered := make(chan struct{})
	var once sync.Once
	hp := &HealthProber{Store: r.st, Backend: r.be, Master: r.mk, Holder: "console-a", Gateway: r.hp.Gateway,
		Log: logx.New(r.logs, logx.LevelDebug), stopWait: 2 * time.Minute,
		Clock: func() time.Time { once.Do(func() { close(entered) }); return r.clk.Now() }} // the first call to the clock is inside acquire
	first := holdWrites(t, r.st)
	runDone := make(chan error, 1)
	go func() { runDone <- hp.Run(context.Background()) }()
	<-entered
	time.Sleep(50 * time.Millisecond) // the Run queues on the store
	stopDone := make(chan struct{})
	go func() { hp.Stop(); close(stopDone) }()
	time.Sleep(50 * time.Millisecond) // a Stop that does not wait queues its release here

	first()
	second := holdWrites(t, r.st) // the queue moves on one or two places, then stops here
	select {
	case <-stopDone:
		t.Fatal("Stop returned while a Run was still taking the lease")
	case <-time.After(300 * time.Millisecond):
	}
	second()
	waitBounded(t, "Stop", 30*time.Second, func() { <-stopDone })
	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return")
	}
	if j := r.row(t, ProbeLease); j.LeaseHolder != "" {
		t.Fatalf("lease held by %q after Run and Stop returned", j.LeaseHolder)
	}
}

// Item 1, at the runner: a job stuck where its context cannot reach it does not
// hold Stop past the bound, and the abandonment is logged by class.
func TestRunnerStopIsBoundedBehindAStuckJob(t *testing.T) { // M3-STORE2 1
	r := newProberRig(t, false)
	log := logx.New(r.logs, logx.LevelDebug)
	runner := NewRunner(r.st, "console-a", log)
	runner.stopWait = 100 * time.Millisecond
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) })
	var once sync.Once
	runner.Add(Job{Name: "stuck", Every: time.Millisecond, Run: func(context.Context) error {
		once.Do(func() { close(entered) })
		<-release
		return nil
	}})
	runner.Start(context.Background())
	<-entered
	stopped := make(chan struct{})
	go func() { runner.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("Stop is still blocked behind a job that ignores its context")
	}
	if logs := r.logs.String(); !strings.Contains(logs, "a job did not stop at shutdown") || !strings.Contains(logs, `"class":"shutdown"`) {
		t.Fatalf("logs %s, want the abandonment by class", logs)
	}
}
