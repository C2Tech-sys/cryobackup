package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// The heartbeat ring trim job also deletes expired start leases (ADR 0005) and
// leaves live ones alone.
func TestHeartbeatTrimDeletesExpiredStartLeases(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dev := e.enrollOne(t, "trim-lease-1")
	var otherID string
	if err := e.st.Read(ctx, func(tx *store.Tx) error {
		return tx.QueryRow(`SELECT id FROM devices WHERE id <> ? AND retired_at IS NULL ORDER BY id LIMIT 1`, dev.ID).Scan(&otherID)
	}); err != nil {
		t.Fatal(err)
	}
	ts, err := e.st.ListStorageTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range ts {
		if _, err := e.st.SetStorageTargetMaxConcurrentBackups(ctx, tg.ID, 5); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		var n int
		_ = e.st.Read(ctx, func(tx *store.Tx) error { return tx.QueryRow(`SELECT count(*) FROM start_leases`).Scan(&n) })
		return n
	}
	if d, err := e.st.AgentStartLease(ctx, dev.ID, "01J9Y4K3A0B1C2D3E4F5G6H7T1", e.now); err != nil || !d.Granted {
		t.Fatalf("lease: %+v %v", d, err)
	}
	e.now = e.now.Add(10 * time.Minute)
	if d, err := e.st.AgentStartLease(ctx, otherID, "01J9Y4K3A0B1C2D3E4F5G6H7T2", e.now); err != nil || !d.Granted {
		t.Fatalf("lease: %+v %v", d, err)
	}
	if err := (&HeartbeatRingTrim{Store: e.st}).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 2 {
		t.Fatalf("%d leases after a trim with both live, want 2", n)
	}
	e.now = e.now.Add(6 * time.Minute) // the first lease is 16 min old, the second 6
	if err := (&HeartbeatRingTrim{Store: e.st}).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("%d leases after the trim, want 1 (the expired one deleted)", n)
	}
}
