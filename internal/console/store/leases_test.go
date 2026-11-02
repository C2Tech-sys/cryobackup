package store

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// leaseCount is the number of start_leases rows, live or not.
func leaseCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	ok(t, s.Read(t.Context(), func(tx *Tx) error {
		return tx.QueryRow(`SELECT count(*) FROM start_leases`).Scan(&n)
	}))
	return n
}

func (w *world) setCap(n int) {
	w.t.Helper()
	got := must[StorageTarget](w.t)(w.s.SetStorageTargetMaxConcurrentBackups(w.ctx, w.target.ID, n))
	if got.MaxConcurrentBackups != n {
		w.t.Fatalf("cap read back as %d, want %d", got.MaxConcurrentBackups, n)
	}
}

func (w *world) lease(deviceID, runID string) LeaseDecision {
	w.t.Helper()
	return must[LeaseDecision](w.t)(w.s.AgentStartLease(w.ctx, deviceID, runID, w.clk.Now()))
}

func TestStorageTargetCapDefaultsToUnlimitedAndIsSettable(t *testing.T) {
	w := seed(t)
	if w.target.MaxConcurrentBackups != 0 {
		t.Fatalf("a new target has cap %d, want 0 (unlimited)", w.target.MaxConcurrentBackups)
	}
	w.setCap(4)
	if got := must[StorageTarget](t)(w.s.GetStorageTarget(w.ctx, w.target.ID)); got.MaxConcurrentBackups != 4 {
		t.Fatalf("GetStorageTarget cap = %d", got.MaxConcurrentBackups)
	}
	if _, err := w.s.SetStorageTargetMaxConcurrentBackups(w.ctx, w.target.ID, -1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative cap = %v, want ErrInvalid", err)
	}
	if _, err := w.s.SetStorageTargetMaxConcurrentBackups(w.ctx, "01JNOSUCHTARGET00000000000", 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown target = %v, want ErrNotFound", err)
	}
	if _, err := w.s.CreateStorageTarget(w.ctx, StorageTarget{Name: "neg", Provider: "wasabi", Endpoint: "e", Bucket: "b",
		GatewayURL: "https://g.invalid", CredentialRef: "X", MaxConcurrentBackups: -2}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("create with a negative cap = %v, want ErrInvalid", err)
	}
	created := must[StorageTarget](t)(w.s.CreateStorageTarget(w.ctx, StorageTarget{Name: "capped", Provider: "wasabi", Endpoint: "e2", Bucket: "b",
		GatewayURL: "https://g.invalid", CredentialRef: "X", MaxConcurrentBackups: 7}))
	if created.MaxConcurrentBackups != 7 {
		t.Fatalf("created cap = %d", created.MaxConcurrentBackups)
	}
}

func TestStartLeaseUnlimitedStoresNothing(t *testing.T) {
	w := seed(t)
	d := w.lease(w.devA.ID, runID1)
	if !d.Granted || !d.Unlimited || !d.LeaseUntil.IsZero() || d.RetryAfter != 0 {
		t.Fatalf("decision with cap 0 = %+v, want granted, unlimited, no lease", d)
	}
	if n := leaseCount(t, w.s); n != 0 {
		t.Fatalf("%d leases stored with cap 0", n)
	}
}

func TestStartLeaseGrantDenyExpiry(t *testing.T) {
	w := seed(t)
	w.setCap(1)

	a := w.lease(w.devA.ID, runID1)
	if !a.Granted || a.Unlimited || !a.LeaseUntil.Equal(t0.Add(LeaseTTL)) {
		t.Fatalf("first lease = %+v, want granted until now + 15 min", a)
	}
	b := w.lease(w.devB.ID, runID2)
	if b.Granted || b.RetryAfter != 15*time.Minute {
		t.Fatalf("second device = %+v, want refused with retry 900 s (the earliest lease expiry)", b)
	}

	// Ten minutes later the earliest lease has five minutes left.
	w.clk.Advance(10 * time.Minute)
	if b = w.lease(w.devB.ID, runID2); b.Granted || b.RetryAfter != 5*time.Minute {
		t.Fatalf("after 10 min = %+v, want refused with retry 300 s", b)
	}
	// 14 min 50 s: ten seconds left, clamped up to the 60 s minimum.
	w.clk.Advance(4*time.Minute + 50*time.Second)
	if b = w.lease(w.devB.ID, runID2); b.Granted || b.RetryAfter != LeaseRetryMin {
		t.Fatalf("10 s before expiry = %+v, want retry clamped to 60 s", b)
	}
	// The first lease expires (nobody renewed it): the slot is free.
	w.clk.Advance(10 * time.Second)
	if b = w.lease(w.devB.ID, runID2); !b.Granted || !b.LeaseUntil.Equal(w.clk.Now().Add(LeaseTTL)) {
		t.Fatalf("after expiry = %+v, want granted", b)
	}
	if n := leaseCount(t, w.s); n != 1 {
		t.Fatalf("%d lease rows, want 1 (the expired one is deleted on the way)", n)
	}
}

func TestStartLeaseIsIdempotentOnTheRunID(t *testing.T) {
	w := seed(t)
	w.setCap(1)
	first := w.lease(w.devA.ID, runID1)
	w.clk.Advance(time.Minute)
	again := w.lease(w.devA.ID, runID1)
	if !again.Granted || !again.LeaseUntil.Equal(first.LeaseUntil) {
		t.Fatalf("retried request = %+v, want the same lease %v", again, first.LeaseUntil)
	}
	if n := leaseCount(t, w.s); n != 1 {
		t.Fatalf("%d lease rows after a retry, want 1", n)
	}
	// Another device presenting the same run id is refused outright.
	if _, err := w.s.AgentStartLease(w.ctx, w.devB.ID, runID1, w.clk.Now()); !errors.Is(err, ErrConflict) {
		t.Fatalf("another device's run id = %v, want ErrConflict", err)
	}
	// A malformed run id and an unknown device.
	if _, err := w.s.AgentStartLease(w.ctx, w.devA.ID, "short", w.clk.Now()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("short run id = %v, want ErrInvalid", err)
	}
	if _, err := w.s.AgentStartLease(w.ctx, "01JNOSUCHDEVICE00000000000", runID3, w.clk.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown device = %v, want ErrNotFound", err)
	}
}

// A device runs one backup at a time: its new request releases a lease left
// by a run that died with the previous process.
func TestStartLeaseReleasesTheDevicesOlderLease(t *testing.T) {
	w := seed(t)
	w.setCap(1)
	_ = w.lease(w.devA.ID, runID1)
	if d := w.lease(w.devA.ID, runID3); !d.Granted {
		t.Fatalf("the same device's next run = %+v, want granted (its old lease is stale)", d)
	}
	leases := must[[]StartLease](t)(w.s.ListStartLeases(w.ctx, w.target.ID, w.clk.Now()))
	if len(leases) != 1 || leases[0].RunID != runID3 || leases[0].DeviceID != w.devA.ID || leases[0].StorageTargetID != w.target.ID {
		t.Fatalf("leases = %+v, want only run 3 of device A", leases)
	}
}

func TestStartLeaseRenewal(t *testing.T) {
	w := seed(t)
	w.setCap(1)
	_ = w.lease(w.devA.ID, runID1)
	w.clk.Advance(10 * time.Minute)
	if renewed := must[bool](t)(w.s.AgentRenewStartLease(w.ctx, w.devA.ID, runID1, w.clk.Now())); !renewed {
		t.Fatal("a live lease was not renewed")
	}
	leases := must[[]StartLease](t)(w.s.ListStartLeases(w.ctx, w.target.ID, w.clk.Now()))
	if len(leases) != 1 || !leases[0].LeaseUntil.Equal(w.clk.Now().Add(LeaseTTL)) || !leases[0].GrantedAt.Equal(t0) {
		t.Fatalf("after renewal = %+v, want until now + 15 min, granted_at unchanged", leases)
	}
	// Past the original expiry the renewed lease still holds the slot.
	w.clk.Advance(10 * time.Minute)
	if d := w.lease(w.devB.ID, runID2); d.Granted {
		t.Fatalf("device B got a slot held by a renewed lease: %+v", d)
	}
	// Another device, or an unknown run, renews nothing.
	if renewed := must[bool](t)(w.s.AgentRenewStartLease(w.ctx, w.devB.ID, runID1, w.clk.Now())); renewed {
		t.Fatal("device B renewed device A's lease")
	}
	if renewed := must[bool](t)(w.s.AgentRenewStartLease(w.ctx, w.devA.ID, runID3, w.clk.Now())); renewed {
		t.Fatal("a run without a lease was renewed")
	}
	// An expired lease is not revived.
	w.clk.Advance(LeaseTTL)
	if renewed := must[bool](t)(w.s.AgentRenewStartLease(w.ctx, w.devA.ID, runID1, w.clk.Now())); renewed {
		t.Fatal("an expired lease was revived")
	}
}

func TestStartLeaseReleasedByTheClosedRunUpload(t *testing.T) {
	w := seed(t)
	w.setCap(1)
	w.heartbeat(w.devA.ID)
	_ = w.lease(w.devA.ID, runID1)

	// The open record goes out at start: the lease stays held.
	open := runInput(runID1, w.clk.Now())
	open.FinishedAt, open.Outcome = nil, ""
	res := must[[]IngestResult](t)(w.s.AgentIngestRuns(w.ctx, w.devA.ID, []RunInput{open}, w.clk.Now()))
	if res[0].Status != IngestCreated {
		t.Fatalf("open record = %+v", res[0])
	}
	if d := w.lease(w.devB.ID, runID2); d.Granted {
		t.Fatal("the open record released the lease")
	}
	// A closed record of ANOTHER device's run id releases nothing of device A.
	if res := must[[]IngestResult](t)(w.s.AgentIngestRuns(w.ctx, w.devB.ID, []RunInput{runInput(runID1, w.clk.Now())}, w.clk.Now())); res[0].Status != IngestRejected {
		t.Fatalf("device B closing device A's run = %+v, want rejected", res[0])
	}
	if n := leaseCount(t, w.s); n != 1 {
		t.Fatalf("a rejected record released a lease (%d left)", n)
	}
	// The closed record releases it: device B gets the slot at once.
	w.clk.Advance(5 * time.Minute)
	res = must[[]IngestResult](t)(w.s.AgentIngestRuns(w.ctx, w.devA.ID, []RunInput{runInput(runID1, w.clk.Now())}, w.clk.Now()))
	if res[0].Status != IngestUpdated {
		t.Fatalf("closed record = %+v", res[0])
	}
	if d := w.lease(w.devB.ID, runID2); !d.Granted {
		t.Fatalf("after the release = %+v, want granted", d)
	}
	// A resend of the closed record (duplicate) is harmless.
	res = must[[]IngestResult](t)(w.s.AgentIngestRuns(w.ctx, w.devA.ID, []RunInput{runInput(runID1, w.clk.Now())}, w.clk.Now()))
	if res[0].Status != IngestDuplicate || leaseCount(t, w.s) != 1 {
		t.Fatalf("duplicate = %+v, leases %d", res[0], leaseCount(t, w.s))
	}
}

func TestDeleteExpiredStartLeases(t *testing.T) {
	w := seed(t)
	w.setCap(2)
	_ = w.lease(w.devA.ID, runID1)
	w.clk.Advance(5 * time.Minute)
	_ = w.lease(w.devB.ID, runID2)
	if n := must[int64](t)(w.s.DeleteExpiredStartLeases(w.ctx, w.clk.Now())); n != 0 {
		t.Fatalf("deleted %d live leases", n)
	}
	w.clk.Advance(10 * time.Minute) // A's lease ends exactly now
	if n := must[int64](t)(w.s.DeleteExpiredStartLeases(w.ctx, w.clk.Now())); n != 1 {
		t.Fatalf("deleted %d, want the one expired lease", n)
	}
	if n := leaseCount(t, w.s); n != 1 {
		t.Fatalf("%d leases left, want 1", n)
	}
}

// N devices race for a cap of K: exactly K are granted, and a retry storm of
// the same requests changes nothing.
func TestStartLeaseConcurrentCap(t *testing.T) {
	const devices, capK = 24, 5
	w := seed(t)
	w.setCap(capK)
	ids := make([]string, devices)
	for i := range ids {
		ids[i] = w.enroll("tok-a", fmt.Sprintf("RACE-%02d", i)).Device.ID
	}
	run := func(i int) string { return fmt.Sprintf("01J9Y4K3A0B1C2D3E4F5G6H%03d", i) }

	race := func() []LeaseDecision {
		out := make([]LeaseDecision, devices)
		errs := make([]error, devices)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range ids {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				out[i], errs[i] = w.s.AgentStartLease(w.ctx, ids[i], run(i), w.clk.Now())
			}(i)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("device %d: %v", i, err)
			}
		}
		return out
	}
	first := race()
	granted := map[int]bool{}
	for i, d := range first {
		if d.Granted {
			granted[i] = true
		} else if d.RetryAfter != LeaseTTL {
			t.Errorf("device %d refused with retry %v, want 15 min", i, d.RetryAfter)
		}
	}
	if len(granted) != capK {
		t.Fatalf("%d of %d devices granted with a cap of %d", len(granted), devices, capK)
	}
	if n := leaseCount(t, w.s); n != capK {
		t.Fatalf("%d lease rows, want %d", n, capK)
	}
	// Everyone retries at once: the same K hold, nobody new gets in.
	second := race()
	for i, d := range second {
		if d.Granted != granted[i] {
			t.Errorf("device %d: granted %v on the retry, %v the first time", i, d.Granted, granted[i])
		}
	}
	if n := leaseCount(t, w.s); n != capK {
		t.Fatalf("%d lease rows after the retry storm, want %d", n, capK)
	}
}
