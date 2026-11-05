package jobs

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// M3-STORE1 B: ProbeNow's ran is true only when the probes ran. Lease lost
// mid-wait, during Stop, after Stop, a panic, or a wait on a run that was
// cancelled under an earlier lease: nothing probed, and it says so.

// probeNowResult is one ProbeNow's answer.
type probeNowResult struct {
	ran bool
	err error
}

// probeNowAsync runs ProbeNow in the background.
func probeNowAsync(hp *HealthProber) <-chan probeNowResult {
	ch := make(chan probeNowResult, 1)
	go func() {
		ran, err := hp.ProbeNow(context.Background())
		ch <- probeNowResult{ran, err}
	}()
	return ch
}

// wantNotRan waits for a ProbeNow that must answer that nothing ran.
func wantNotRan(t *testing.T, ch <-chan probeNowResult) {
	t.Helper()
	select {
	case r := <-ch:
		if r.ran || !errors.Is(r.err, context.Canceled) {
			t.Fatalf("ProbeNow = (%v, %v), want ran false and a cancellation", r.ran, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ProbeNow did not answer")
	}
}

func TestC08ProbeNowLeaseLostMidWaitDidNotRun(t *testing.T) { // M3-STORE1 B
	r := newProberRig(t, false)
	be := &countingBackend{Backend: r.be, release: make(chan struct{})}
	defer close(be.release)
	r.hp.Backend, r.hp.Holder, r.hp.renewEvery = be, "console-a", 10*time.Millisecond
	answer := probeNowAsync(r.hp)
	waitActive(t, be)
	if err := r.st.Write(r.ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE job_state SET lease_holder = 'console-b' WHERE name = ?`, ProbeLease)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	wantNotRan(t, answer)
}

func TestC08ProbeNowDuringStopDidNotRun(t *testing.T) { // M3-STORE1 B
	r := newProberRig(t, false)
	be := &countingBackend{Backend: r.be, release: make(chan struct{})}
	defer close(be.release)
	r.hp.Backend = be
	answer := probeNowAsync(r.hp)
	waitActive(t, be)
	r.hp.Stop()
	wantNotRan(t, answer)
}

func TestC08ProbeNowAfterStopProbesNothingAndTakesNoLease(t *testing.T) { // M3-STORE1 B
	r := newProberRig(t, false)
	be := &countingBackend{Backend: r.be, release: make(chan struct{})}
	close(be.release)
	r.hp.Backend, r.hp.Holder = be, "console-a"
	r.hp.Stop()
	wantNotRan(t, probeNowAsync(r.hp))
	r.hp.WaitAll()
	if n := be.total.Load(); n != 0 {
		t.Fatalf("%d storage probes after Stop, want none", n)
	}
	if ok, err := r.st.AcquireJobLease(context.Background(), ProbeLease, "console-b", time.Minute); err != nil || !ok {
		t.Fatalf("another console taking the lease after a ProbeNow that came after Stop = %v %v, want true", ok, err)
	}
}

// ctxDeafBackend's storage probe ignores its context until released, so it
// outlives the lease it started under.
type ctxDeafBackend struct {
	*fixtures.Backend
	entered, release chan struct{}
}

func (b *ctxDeafBackend) CatConfig(context.Context, adminpath.DeviceRequest) (restic.Config, error) {
	b.entered <- struct{}{}
	<-b.release
	return restic.Config{}, nil
}

// A run in flight from an earlier lease that ends cancelled has produced no
// result, however the new lease is doing: the ProbeNow waiting for it did not run.
func TestC08ProbeNowWaitingOnACancelledRunDidNotRun(t *testing.T) { // M3-STORE1 B
	r := newProberRig(t, false)
	be := &ctxDeafBackend{Backend: r.be, entered: make(chan struct{}, 4), release: make(chan struct{})}
	r.hp.Backend = be
	if err := r.hp.Run(r.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-be.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the storage probe never started")
	}
	deadline := time.Now().Add(30 * time.Second) // the tick's gateway probe has ended, so ProbeNow starts its own
	for slices.Contains(r.hp.inFlight(), ProbeRowGateway) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	r.hp.loseLease() // its context is cancelled; the probe has not noticed
	answer := probeNowAsync(r.hp)
	// ProbeNow's own gateway probe has been recorded: it started that just before it
	// claimed the storage probe, so it is now waiting on the cancelled run. No fixed
	// sleep says this on a loaded machine: a ProbeNow that reached the store only
	// after the release would claim the probe itself, and that would be a real run.
	for r.row(t, ProbeRowGateway).RunCount < 2 {
		select {
		case a := <-answer:
			t.Fatalf("ProbeNow answered (%v, %v) with the run still in flight", a.ran, a.err)
		case <-time.After(5 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("ProbeNow never started its gateway probe")
		}
	}
	close(be.release)
	wantNotRan(t, answer)
	if p := r.hp.Facts(); p.Storage.LastRunAt != 0 {
		t.Fatalf("the cancelled probe recorded %+v", p.Storage)
	}
}

// A probe that panicked learned nothing: its row says so, and ProbeNow did not run.
func TestC08ProbeNowWhenAProbePanickedDidNotRun(t *testing.T) { // M3-STORE1 B
	r := newProberRig(t, false)
	r.hp.Backend = panickingBackend{}
	wantNotRan(t, probeNowAsync(r.hp))
	if j := r.row(t, ProbeRowStorage); j.LastErrorClass != ClassPanic {
		t.Fatalf("storage row %+v, want %s", j, ClassPanic)
	}
}

// The lying direction: probes that finished are still a run.
func TestC08ProbeNowThatFinishedRan(t *testing.T) { // M3-STORE1 B
	r := newProberRig(t, false)
	ran, err := r.hp.ProbeNow(context.Background())
	if !ran || err != nil {
		t.Fatalf("ProbeNow = (%v, %v), want (true, nil)", ran, err)
	}
}
