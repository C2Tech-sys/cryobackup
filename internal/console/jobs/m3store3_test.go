package jobs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// M3-STORE3 2: the release at Stop cannot hold the shutdown. A COMMIT runs
// uncancellable, so a hung disk holds the store call whatever its context does; this
// release ignores its context the same way. Stop stops waiting at the bound and logs
// it, and the lease is left to expire at its TTL.
func TestC08StopIsBoundedBehindAHungRelease(t *testing.T) { // M3-STORE3 2
	r := newProberRig(t, false)
	r.hp.Holder, r.hp.releaseWait = "console-a", 100*time.Millisecond
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) }) // before the rig's own cleanup, whose Stop releases again
	r.hp.releaseLease = func(context.Context) error { <-hang; return nil }
	r.run(t)
	r.waitAll(t)

	stopped := make(chan struct{})
	go func() { r.hp.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("Stop is still blocked behind a release that ignores its context")
	}
	if logs := r.logs.String(); !strings.Contains(logs, "health probe lease release abandoned at shutdown") || !strings.Contains(logs, `"class":"store"`) {
		t.Fatalf("no abandon line by class in the log: %s", logs)
	}
	// It was not released, and it expires on its own.
	if j := r.row(t, ProbeLease); j.LeaseHolder != "console-a" {
		t.Fatalf("lease holder %q after an abandoned release, want console-a until it expires", j.LeaseHolder)
	}
	r.clk.Advance(r.hp.leaseTTL())
	wantLeaseFree(t, r)
}

// Mutant m21: Stop's wait for the calls already taking the lease. The Run is held by
// a gate inside acquire until Stop has returned, so Stop can only reach its release by
// using up its whole bound. A Stop that does not wait gets there at once. The check
// compares against Stop's own bound, so a slow machine cannot raise a false alarm: the
// release can never come early unless Stop did not wait.
func TestC08StopWaitsForACallTakingTheLease(t *testing.T) { // M3-STORE1 C1, M3-STORE3 4
	const bound = 300 * time.Millisecond
	r := newProberRig(t, false)
	entered, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var openGate sync.Once
	letGo := func() { openGate.Do(func() { close(gate) }) }
	t.Cleanup(letGo)
	hp := &HealthProber{Store: r.st, Backend: r.be, Master: r.mk, Holder: "console-a", Gateway: r.hp.Gateway,
		Log: logx.New(r.logs, logx.LevelDebug), stopWait: bound,
		// The first call to the clock is inside acquire, past enter and before the store.
		Clock: func() time.Time {
			once.Do(func() { close(entered); <-gate })
			return r.clk.Now()
		}}
	runDone := make(chan error, 1)
	go func() { runDone <- hp.Run(context.Background()) }()
	<-entered

	began := time.Now()
	var waitedNS atomic.Int64 // the release runs on its own goroutine, and on the slow path Stop can return before it
	hp.releaseLease = func(ctx context.Context) error {
		waitedNS.Store(int64(time.Since(began)))
		return r.st.ReleaseJobLease(ctx, ProbeLease, "console-a")
	}
	hp.Stop() // returns with the Run still at the gate: the bound ran out
	if waited := time.Duration(waitedNS.Load()); waited != 0 && waited < bound {
		t.Fatalf("Stop released the lease after %v, inside its bound of %v, with a Run still taking the lease", waited, bound)
	}

	// The Run goes on after Stop gave up on it: it takes the lease and undoes that at once.
	letGo()
	select {
	case err := <-runDone:
		if !errors.Is(err, errDidNotRun) {
			t.Fatalf("the Run past Stop = %v, want errDidNotRun", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the Run did not return")
	}
	if j := r.row(t, ProbeLease); j.LeaseHolder != "" {
		t.Fatalf("lease held by %q after Run and Stop returned", j.LeaseHolder)
	}
	if p := hp.Facts(); p.Maintain.RPC.LastRunAt != 0 {
		t.Fatalf("a probe started after Stop: %+v", p.Maintain.RPC)
	}
}
