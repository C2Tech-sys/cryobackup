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
	deadline := time.Now().Add(30 * time.Second)
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

// Mutants M22 and M22c: the renewer really renews well inside the lease TTL, under
// the production period, not only the helper that computes it (the helper's table
// over every interval is in TestC08ProbeLeaseRenewsInsideItsTTL). Renewals are
// counted from the lease row itself: at least two inside one TTL of real time.
func TestC08RenewerTicksInsideTheTTL(t *testing.T) { // M3-INT2 3, M3-STORE1 D
	r := newProberRig(t, false)
	be := &countingBackend{Backend: r.be, release: make(chan struct{})}
	defer close(be.release)
	r.hp.Backend, r.hp.Interval = be, 2*time.Second // long enough to tell renewals apart on a busy machine
	start := time.Now()
	if err := r.hp.Run(r.ctx); err != nil {
		t.Fatal(err)
	}
	waitActive(t, be)
	ttl := r.hp.leaseTTL()
	if got := countLeaseRenewals(t, r, 2, ttl-time.Since(start)); got < 2 {
		t.Fatalf("%d renewals in one TTL of %v under a running probe; want at least two", got, ttl)
	}
}

// countLeaseRenewals watches the lease row for at most d and returns how many
// times its expiry moved, stopping once it has seen want. The store's clock moves
// on every look, so each renewal writes a new expiry.
func countLeaseRenewals(t *testing.T, r *proberRig, want int, d time.Duration) int {
	t.Helper()
	last := *r.row(t, ProbeLease).LeaseUntil
	seen := 0
	for end := time.Now().Add(d); seen < want && time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		r.clk.Advance(time.Millisecond)
		if until := *r.row(t, ProbeLease).LeaseUntil; !until.Equal(last) {
			last = until
			seen++
		}
	}
	return seen
}

// A renewer outlives the context of the call that started it, for a tick and for
// an on-demand probe alike: the caller going away must not let the lease lapse
// under a running probe.
func TestC08RenewerOutlivesTheCallersContext(t *testing.T) { // M3-STORE1 D
	t.Run("Run", func(t *testing.T) {
		r := newProberRig(t, false)
		be := &countingBackend{Backend: r.be, release: make(chan struct{})}
		defer close(be.release)
		r.hp.Backend, r.hp.renewEvery = be, 5*time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		if err := r.hp.Run(ctx); err != nil {
			t.Fatal(err)
		}
		cancel()
		waitActive(t, be)
		if got := countLeaseRenewals(t, r, 3, 10*time.Second); got < 3 {
			t.Fatalf("%d renewals after the caller's context ended, want the renewer still running", got)
		}
	})
	t.Run("ProbeNow", func(t *testing.T) {
		r := newProberRig(t, false)
		be := &countingBackend{Backend: r.be, release: make(chan struct{})}
		defer close(be.release)
		r.hp.Backend, r.hp.renewEvery = be, 5*time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := r.hp.ProbeNow(ctx); done <- err }()
		waitActive(t, be)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("ProbeNow after its caller went away = %v", err)
		}
		if got := countLeaseRenewals(t, r, 3, 10*time.Second); got < 3 {
			t.Fatalf("%d renewals after the caller's context ended, want the renewer still running", got)
		}
	})
}

// Losing the lease and taking it again gives the probes a fresh context: the one
// that was cancelled with the loss must not be reused, or nothing would ever probe
// on this console again.
func TestC08ReacquiringAfterALossGetsAFreshContext(t *testing.T) { // M3-STORE1 D
	r := newProberRig(t, false)
	r.hp.Holder = "console-a"
	r.run(t)
	if ok, err := r.st.AcquireJobLease(r.ctx, ProbeLease, "console-a", time.Nanosecond); err != nil || !ok {
		t.Fatal(err) // the lease lapses at once, so console-b can take it
	}
	r.clk.Advance(time.Minute)
	if ok, err := r.st.AcquireJobLease(r.ctx, ProbeLease, "console-b", time.Minute); err != nil || !ok {
		t.Fatalf("console-b takes the lease: %v %v", ok, err)
	}
	r.run(t) // loses it: the probes' context is cancelled
	if err := r.st.ReleaseJobLease(r.ctx, ProbeLease, "console-b"); err != nil {
		t.Fatal(err)
	}
	r.clk.Advance(time.Minute)
	r.run(t)
	now := state.Millis(r.clk.Now().UnixMilli())
	if s := r.hp.Facts().Storage; s.LastOKAt != now {
		t.Fatalf("storage after taking the lease back: %+v, want a pass now (%d)", s, now)
	}
	if g := r.hp.Facts().Gateway; g.LastOKAt != now {
		t.Fatalf("gateway after taking the lease back: %+v, want a pass now (%d)", g, now)
	}
}

// Stop ends the prober's lifetime, and the probes' context derives from it: a
// probe context made after Stop is already cancelled, so nothing can run under it.
func TestC08StopEndsTheProbersLifetime(t *testing.T) { // M3-STORE1 D
	r := newProberRig(t, false)
	r.hp.Stop()
	if err := r.hp.lifetime().Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("the lifetime after Stop: %v, want cancelled", err)
	}
	if err := r.hp.probeContext().Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("a probe context after Stop: %v, want cancelled", err)
	}
}
