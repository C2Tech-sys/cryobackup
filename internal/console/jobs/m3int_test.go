package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
)

// M3-INT item A: the on-demand probe (ProbeNow) and the probe names in the log.

// ProbeNow waits for the gateway and storage probes it started, bounded by its
// context, and reports that this process probed.
func TestC08ProbeNowWaitsForItsProbes(t *testing.T) { // M3-INT A
	r := newProberRig(t, false)
	be := &countingBackend{Backend: r.be, release: make(chan struct{})}
	r.hp.Backend = be
	type answer struct {
		ran bool
		err error
	}
	done := make(chan answer, 1)
	go func() { ran, err := r.hp.ProbeNow(context.Background()); done <- answer{ran, err} }()
	deadline := time.Now().Add(5 * time.Second)
	for be.active.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case a := <-done:
		t.Fatalf("ProbeNow returned (%v, %v) while its storage probe was still running", a.ran, a.err)
	case <-time.After(200 * time.Millisecond):
	}
	close(be.release)
	var a answer
	select {
	case a = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ProbeNow did not return after its probes ended")
	}
	if !a.ran || a.err != nil {
		t.Fatalf("ProbeNow = (%v, %v), want (true, nil)", a.ran, a.err)
	}
	if p := r.hp.Facts(); p.Storage.LastOKAt == 0 || p.Gateway.LastOKAt == 0 {
		t.Fatalf("the facts were not written before ProbeNow returned: storage %+v gateway %+v", p.Storage, p.Gateway)
	}
	if n := be.total.Load(); n != 1 {
		t.Errorf("%d storage probes, want 1", n)
	}
}

// A storage probe in flight from a tick is waited for, never started twice; a
// caller whose context ends stops waiting without cutting the probe short.
func TestC08ProbeNowWaitsForTheProbeInFlight(t *testing.T) { // M3-INT A
	r := newProberRig(t, false)
	be := &countingBackend{Backend: r.be, release: make(chan struct{})}
	r.hp.Backend = be
	if err := r.hp.Run(r.ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for be.active.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	ran, err := r.hp.ProbeNow(short)
	if !ran || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ProbeNow with a short context = (%v, %v), want (true, deadline exceeded)", ran, err)
	}
	if be.active.Load() != 1 {
		t.Fatal("the probe in flight ended with the caller's context")
	}
	done := make(chan error, 1)
	go func() { _, err := r.hp.ProbeNow(context.Background()); done <- err }()
	time.Sleep(100 * time.Millisecond)
	close(be.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := be.total.Load(); n != 1 {
		t.Errorf("%d storage probes: the one in flight was doubled", n)
	}
	if p := r.hp.Facts(); p.Storage.LastOKAt == 0 {
		t.Fatalf("the waited-for probe recorded nothing: %+v", p.Storage)
	}
}

// Another console holds the probe lease: ProbeNow probes nothing and says so.
func TestC08ProbeNowWithoutTheLeaseProbesNothing(t *testing.T) { // M3-INT A
	r := newProberRig(t, false)
	be := &countingBackend{Backend: r.be, release: make(chan struct{})}
	close(be.release)
	r.hp.Backend, r.hp.Holder = be, "console-a"
	if ok, err := r.st.AcquireJobLease(context.Background(), ProbeLease, "console-b", time.Hour); err != nil || !ok {
		t.Fatalf("console-b takes the lease: %v %v", ok, err)
	}
	ran, err := r.hp.ProbeNow(context.Background())
	if ran || err != nil {
		t.Fatalf("ProbeNow without the lease = (%v, %v), want (false, nil)", ran, err)
	}
	r.hp.WaitAll()
	if be.total.Load() != 0 {
		t.Fatal("a storage probe ran without the lease")
	}
	if p := r.hp.Facts(); p.Gateway.LastRunAt != 0 || p.Storage.LastRunAt != 0 {
		t.Fatalf("facts recorded without the lease: gateway %+v storage %+v", p.Gateway, p.Storage)
	}
}

// The probe is named in the log by its short name; the row name ("health-prober.storage")
// is outside logx's token alphabet and was logged as "invalid".
func TestC08ProbeLogNamesTheProbe(t *testing.T) { // M3-INT A (log fix)
	r := newProberRig(t, false)
	r.be.Errors = map[string]error{adminpath.MethodCatConfig: adminpath.NewError(adminpath.CodeStorageUnreachable, "")}
	r.run(t)
	logs := r.logs.String()
	if !strings.Contains(logs, `"probe":"storage"`) || strings.Contains(logs, `"probe":"invalid"`) {
		t.Fatalf("probe names in the log: %s", logs)
	}
}
