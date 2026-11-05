package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-INT2: the probes run under a context the prober owns (its lifetime and
// the lease), never under a caller's; the renewer's period is pinned.

// waitActive waits until the counting backend has a storage probe in flight.
func waitActive(t *testing.T, be *countingBackend) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for be.active.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if be.active.Load() == 0 {
		t.Fatal("the storage probe never started")
	}
}

// Mutant M34: a caller of ProbeNow that goes away mid-probe (a client that
// disconnects) stops waiting, but the probe it started completes and records
// its facts, and the next scheduled tick probes normally.
func TestC08ProbeNowCallerCancelledMidProbeDoesNotCancelTheProbe(t *testing.T) { // M3-INT2 1
	r := newProberRig(t, false)
	be := &countingBackend{Backend: r.be, release: make(chan struct{})}
	r.hp.Backend = be
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := r.hp.ProbeNow(ctx); done <- err }()
	waitActive(t, be)
	cancel() // the client goes away
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ProbeNow after its caller went away = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ProbeNow kept waiting after its caller went away")
	}
	time.Sleep(50 * time.Millisecond)
	if be.active.Load() != 1 {
		t.Fatal("the storage probe was cancelled with its caller")
	}
	close(be.release)
	waitBounded(t, "the probe the gone caller started", 10*time.Second, r.hp.Wait)
	first := r.hp.Facts().Storage
	if first.LastOKAt == 0 || first.LastRunAt != state.Millis(r.clk.Now().UnixMilli()) {
		t.Fatalf("the probe of a caller that went away recorded %+v, want a pass now", first)
	}
	// The next scheduled tick (the runner's own context) probes normally.
	r.clk.Advance(time.Minute)
	r.run(t)
	if s := r.hp.Facts().Storage; s.LastRunAt != state.Millis(r.clk.Now().UnixMilli()) || s.LastOKAt != s.LastRunAt {
		t.Fatalf("the tick after it: storage %+v, want a fresh pass", s)
	}
	if j := r.row(t, ProbeRowStorage); j.RunCount != 2 || j.LastStatus != "ok" {
		t.Fatalf("storage row %+v, want two passing runs", j)
	}
}

// Run takes the lease under its caller's context and returns before its probes
// end; a caller that cancels that context right away (RunOnce's deferred
// cancel, the run-now route) must not cancel them.
func TestC08RunUnderAShortLivedContextStillRecords(t *testing.T) { // M3-INT2 1, 2
	r := newProberRig(t, false)
	be := &countingBackend{Backend: r.be, release: make(chan struct{})}
	r.hp.Backend = be
	ctx, cancel := context.WithCancel(context.Background())
	if err := r.hp.Run(ctx); err != nil {
		t.Fatal(err)
	}
	cancel() // as RunOnce's caller does once Run has returned
	waitActive(t, be)
	time.Sleep(50 * time.Millisecond)
	close(be.release)
	waitBounded(t, "the tick's probes", 10*time.Second, r.hp.Wait)
	if s := r.hp.Facts().Storage; s.LastOKAt == 0 {
		t.Fatalf("a probe whose tick context was cancelled recorded nothing: %+v", s)
	}
	if g := r.hp.Facts().Gateway; g.LastOKAt == 0 {
		t.Fatalf("the gateway probe of that tick recorded nothing: %+v", g)
	}
}

// Mutant M22: the renewer actually ticks well inside the lease TTL (not only
// the helper that computes the period).
func TestC08RenewerTicksInsideTheTTL(t *testing.T) { // M3-INT2 3
	for _, iv := range []time.Duration{time.Second, time.Minute, 10 * time.Minute} {
		r := newProberRig(t, false)
		be := &countingBackend{Backend: r.be, release: make(chan struct{})}
		r.hp.Backend, r.hp.Interval = be, iv
		if err := r.hp.Run(r.ctx); err != nil {
			t.Fatal(err)
		}
		waitActive(t, be)
		r.hp.mu.Lock()
		tick, renewing := r.hp.renewTick, r.hp.renewing
		r.hp.mu.Unlock()
		close(be.release)
		if !renewing {
			t.Fatalf("interval %v: no renewer while a probe is in flight", iv)
		}
		if ttl := r.hp.leaseTTL(); tick <= 0 || 2*tick >= ttl {
			t.Errorf("interval %v: the renewer ticks every %v with a TTL of %v; want at least two renewals inside the TTL", iv, tick, ttl)
		}
	}
}
