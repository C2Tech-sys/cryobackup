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

// M3-STORE1 C: after Stop a late Run takes no lease and probes nothing, and a
// renewer whose store has failed for the rest of the lease treats it as lost.

// wantLeaseFree checks that another console can take the probe lease at once.
func wantLeaseFree(t *testing.T, r *proberRig) {
	t.Helper()
	if ok, err := r.st.AcquireJobLease(context.Background(), ProbeLease, "console-b", time.Minute); err != nil || !ok {
		j := r.row(t, ProbeLease)
		t.Fatalf("another console taking the lease = %v %v (holder %q), want it free", ok, err, j.LeaseHolder)
	}
}

func TestC08RunAfterStopTakesNoLeaseAndProbesNothing(t *testing.T) { // M3-STORE1 C1, D
	r := newProberRig(t, false)
	be := &countingBackend{Backend: r.be, release: make(chan struct{})}
	close(be.release)
	r.hp.Backend, r.hp.Holder = be, "console-a"
	r.hp.Stop()
	if err := r.hp.Run(r.ctx); err != nil {
		t.Fatalf("Run after Stop = %v, want a no-op", err)
	}
	r.waitAll(t)
	if n := be.total.Load(); n != 0 {
		t.Fatalf("%d storage probes after Stop, want none", n)
	}
	if p := r.hp.Facts(); p.Gateway.LastRunAt != 0 || p.Maintain.RPC.LastRunAt != 0 || p.Database.MeasuredAt != 0 {
		t.Fatalf("a probe ran after Stop: gateway %+v rpc %+v db %+v", p.Gateway, p.Maintain.RPC, p.Database)
	}
	wantLeaseFree(t, r)
}

// Run racing Stop, the shape of a run-now goroutine at shutdown: whichever wins,
// the lease is free once both have returned.
func TestC08RunRacingStopLeavesTheLeaseFree(t *testing.T) { // M3-STORE1 C1
	r := newProberRig(t, false)
	for i := range 60 {
		hp := &HealthProber{Store: r.st, Backend: r.be, Master: r.mk, Clock: r.clk.Now, Holder: "console-a",
			Log: logx.New(r.logs, logx.LevelDebug), Gateway: r.hp.Gateway}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = hp.Run(context.Background()) }()
		go func() { defer wg.Done(); hp.Stop() }()
		wg.Wait()
		j, err := r.st.GetJobState(context.Background(), ProbeLease)
		if errors.Is(err, store.ErrNotFound) {
			continue // Stop won before the lease was ever taken
		}
		if err != nil {
			t.Fatal(err)
		}
		if j.LeaseHolder != "" {
			t.Fatalf("iteration %d: lease held by %q after Run and Stop returned", i, j.LeaseHolder)
		}
	}
}

// A Run still waiting for the store when Stop's bounded wait gives up: its lease
// lands after Stop released it, and is undone.
func TestC08RunThatOutlivesStopsWaitDoesNotKeepTheLease(t *testing.T) { // M3-STORE1 C1
	r := newProberRig(t, false)
	r.hp.Holder, r.hp.stopWait = "console-a", 20*time.Millisecond
	hold, held := make(chan struct{}), make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- r.st.Write(context.Background(), func(*store.Tx) error { close(held); <-hold; return nil })
	}()
	<-held
	runDone := make(chan error, 1)
	go func() { runDone <- r.hp.Run(context.Background()) }() // queues behind the held write
	time.Sleep(20 * time.Millisecond)
	stopDone := make(chan struct{})
	go func() { r.hp.Stop(); close(stopDone) }()
	time.Sleep(60 * time.Millisecond) // past Stop's bounded wait: its release queues too
	close(hold)
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	waitBounded(t, "Stop", 10*time.Second, func() { <-stopDone })
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	if j := r.row(t, ProbeLease); j.LeaseHolder != "" {
		t.Fatalf("lease held by %q after Run outlived Stop", j.LeaseHolder)
	}
	if p := r.hp.Facts(); p.Maintain.RPC.LastRunAt != 0 {
		t.Fatalf("a probe started after Stop: %+v", p.Maintain.RPC)
	}
}

// The lease a call takes after Stop has begun is undone at once. Stop waits for
// such a call, but only within its bound, so this is what remains when it gave up.
func TestC08LeaseTakenAfterStopBeganIsUndone(t *testing.T) { // M3-STORE1 C1
	r := newProberRig(t, false)
	r.hp.Holder = "console-a"
	r.hp.mu.Lock()
	r.hp.stopped = true
	r.hp.mu.Unlock()
	got, err := r.hp.acquire(context.Background())
	if got || !errors.Is(err, errProbesCancelled) {
		t.Fatalf("acquire after Stop began = (%v, %v), want (false, errProbesCancelled)", got, err)
	}
	wantLeaseFree(t, r)
}

// The store fails under a running probe. Errors inside the lease keep the
// probes running; once they span the rest of the lease the lease is treated as
// lost. The log has the class and no error text.
func TestC08RenewerStoreOutageSpanningTheLeaseCancelsTheProbes(t *testing.T) { // M3-STORE1 C2
	r := newProberRig(t, false)
	be := &countingBackend{Backend: r.be, release: make(chan struct{})}
	defer close(be.release)
	r.hp.Backend, r.hp.Holder, r.hp.renewEvery = be, "console-a", 5*time.Millisecond
	if err := r.hp.Run(r.ctx); err != nil {
		t.Fatal(err)
	}
	waitActive(t, be)
	if err := r.st.Close(); err != nil { // every renewal now answers an error
		t.Fatal(err)
	}
	r.clk.Advance(r.hp.leaseTTL() - time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(r.logs.String(), "health probe lease not renewed") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(r.logs.String(), "health probe lease not renewed") {
		t.Fatalf("no log line for failing renewals: %s", r.logs.String())
	}
	time.Sleep(50 * time.Millisecond) // several failing renewals, all inside the lease
	if be.active.Load() != 1 {
		t.Fatal("store errors inside the lease cancelled the probe")
	}
	r.clk.Advance(time.Second) // the lease has now run out
	waitBounded(t, "the probes after the lease ran out", 5*time.Second, func() {
		for be.active.Load() != 0 {
			time.Sleep(5 * time.Millisecond)
		}
	})
	if t.Failed() {
		t.FailNow()
	}
	logs := r.logs.String()
	if !strings.Contains(logs, "health probe lease lost") || !strings.Contains(logs, `"class":"lease_lost"`) || !strings.Contains(logs, `"class":"store"`) {
		t.Fatalf("logs %s, want the lease_lost and store classes", logs)
	}
	if strings.Contains(logs, "closed") || strings.Contains(logs, r.st.Path()) {
		t.Fatalf("the log carries more than the class: %s", logs)
	}
}
