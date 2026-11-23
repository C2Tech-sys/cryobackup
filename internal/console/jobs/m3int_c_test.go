package jobs

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/maintain"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M3-INT item C: the probe lease's renewal interval (C1), the worker's settings
// store error (C3) and the bounded Stop (C5).

// M3-T2 r3 LOW-1 (mutant m4): the renewer renews the lease well inside its TTL,
// whatever the interval, so a probe longer than the TTL keeps its lease.
func TestC08ProbeLeaseRenewsInsideItsTTL(t *testing.T) { // M3-INT C1
	for _, iv := range []time.Duration{0, time.Second, 30 * time.Second, time.Minute, 10 * time.Minute} {
		h := &HealthProber{Interval: iv}
		ttl, every := h.leaseTTL(), h.renewInterval()
		if every <= 0 || every*2 >= ttl {
			t.Errorf("interval %v: renew every %v with a TTL of %v; want at least two renewals inside the TTL", iv, every, ttl)
		}
	}
	// And the renewer uses it: set a short interval (a TTL of 90 ms on the store's clock, a renewal
	// every 30 ms on the renewer's real ticker) and watch the lease outlive its TTL under a probe
	// that does not end.
	//
	// P21-T2 G: no fixed sleep. This slept 200 ms of real time and then moved the store's clock a
	// whole second at once; on a loaded runner under -race the renewal that followed could miss its
	// bound (a third of the TTL, 30 ms of real time), find the lease already past its end on the
	// prober's clock and drop it, so the test failed without any fault in the renewer. Now the
	// store's clock moves in steps shorter than the TTL, each one only after a renewal has been
	// seen, until it is a whole TTL past the first expiry. A slow renewal is then retried at the next
	// tick, as in production, and every wait is on an observed renewal with a generous deadline.
	r := newProberRig(t, false)
	be := &countingBackend{Backend: r.be, release: make(chan struct{})}
	defer close(be.release)
	r.hp.Backend, r.hp.Holder, r.hp.Interval = be, "console-a", 30*time.Millisecond
	if err := r.hp.Run(r.ctx); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); be.active.Load() == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the storage probe did not start")
		}
	}
	first := r.row(t, ProbeLease)
	ttl := r.hp.leaseTTL()
	until := *first.LeaseUntil
	steps := 0
	for !r.clk.Now().After(first.LeaseUntil.Add(ttl)) { // the store's clock: well past the first expiry
		r.clk.Advance(ttl * 2 / 3) // inside the lease the last renewal gave: it cannot lapse in between
		waitLeaseRenewedAfter(t, r, "console-a", until)
		until = *r.row(t, ProbeLease).LeaseUntil
		steps++
	}
	if steps < 3 {
		t.Fatalf("only %d renewals were waited for: the clock did not pass the first expiry by a whole TTL", steps)
	}
	if be.active.Load() == 0 {
		t.Fatal("the probe ended while its lease was being renewed: the lease was lost and the probe cancelled")
	}
}

// M3-T2 r3 LOW-3: the worker's settings store failing during the probe (after a
// clean list and cat config) is its own class, read as not checked: C02b once
// the last result ages, never C02, and never counted as a storage failure.
func TestC08WorkerStoreErrorIsNotCheckedNeverC02(t *testing.T) { // M3-INT C3
	r := newProberRig(t, false)
	r.run(t) // a passing probe first
	r.be.Errors = map[string]error{adminpath.MethodCatConfig: adminpath.NewError(adminpath.CodeUnavailable, maintain.ProbeRecordClass)}
	for i := 0; i < 4; i++ {
		r.clk.Advance(time.Minute)
		r.run(t)
	}
	got := r.evaluate()
	if slices.Contains(got, "C02") || !slices.Contains(got, "C02b") {
		t.Fatalf("a worker whose settings store fails for four intervals: %v, want C02b and no C02", got)
	}
	if p := r.hp.Facts(); p.Storage.ConsecutiveFailures != 0 {
		t.Fatalf("a worker store error counted as a storage failure: %+v", p.Storage)
	}
	if j := r.row(t, ProbeRowStorage); j.LastStatus != "skipped" || j.LastErrorClass != ClassWorkerStore {
		t.Fatalf("row %+v, want skipped / %s", j, ClassWorkerStore)
	}
}

// M3-T2 r3 LOW-6: store.IsJobRow (the job listings' filter) leaves out exactly
// the health-prober's lease and per-probe rows, and keeps every job.
func TestProbeRowsAreNotJobRows(t *testing.T) { // M3-INT C6
	for _, row := range []string{ProbeLease, ProbeRowGateway, ProbeRowStorage, ProbeRowSMTP, ProbeRowMaintain, ProbeRowDatabase,
		ProbeRowIntegrity, ProbeRowMasterKey, ProbeRowEndpoint, ProbeRowLoginFile} {
		if store.IsJobRow(row) {
			t.Errorf("%s is listed as a job", row)
		}
	}
	for _, job := range []string{NameHealthProber, NameHtpasswdSync, NameHeartbeatRingTrim, NameRestorePurge, NameSessionExpire, NameCommandExpiry} {
		if !store.IsJobRow(job) {
			t.Errorf("the job %s is not listed", job)
		}
	}
}

// M3-T2 r3 LOW-5 (mutant m23): Stop releases the probe lease, so another
// console can take it at once.
func TestC08StopReleasesTheProbeLease(t *testing.T) { // M3-INT C5
	r := newProberRig(t, false)
	r.hp.Holder = "console-a"
	r.run(t)
	if j := r.row(t, ProbeLease); j.LeaseHolder != "console-a" {
		t.Fatalf("lease %+v after a tick", j)
	}
	r.hp.Stop()
	if ok, err := r.st.AcquireJobLease(context.Background(), ProbeLease, "console-b", time.Minute); err != nil || !ok {
		t.Fatalf("another console taking the lease after Stop = %v %v, want true", ok, err)
	}
}

// M3-T2 r3 LOW-5: a probe that ignores its context cannot hold Stop. Stop
// returns within its bound, names the probe it abandoned (no path), and still
// releases the lease.
func TestC08StopIsBoundedAndLogsWhatItAbandoned(t *testing.T) { // M3-INT C5
	r := newProberRig(t, false)
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) }) // registered after the rig's own cleanup, so it runs first
	r.hp.Holder, r.hp.stopWait = "console-a", 200*time.Millisecond
	r.hp.IntegrityCheck = func(context.Context) (string, error) { <-hang; return "ok", nil } // ignores its context, like a hung disk
	if err := r.hp.Run(r.ctx); err != nil {
		t.Fatal(err)
	}
	r.hp.Wait()
	stopped := make(chan struct{})
	go func() { r.hp.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop hung behind a probe that ignores its context")
	}
	logs := r.logs.String()
	if !strings.Contains(logs, "health probe abandoned at shutdown") || !strings.Contains(logs, `"probe":"integrity"`) {
		t.Fatalf("Stop did not log the probe it abandoned: %s", logs)
	}
	if ok, err := r.st.AcquireJobLease(context.Background(), ProbeLease, "console-b", time.Minute); err != nil || !ok {
		t.Fatalf("the lease after a bounded Stop = %v %v, want released", ok, err)
	}
}
