package cli

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/maintain"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

func TestMaintainKeysMapsCustodyAndReportsTheMasterKey(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	st, err := store.OpenMemory(ctx, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.LoadFixtures(ctx, st, now); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "master.key")
	if _, err := escrow.GenerateMasterKey(keyPath, nil); err != nil {
		t.Fatal(err)
	}

	kw, err := MaintainKeys(keyPath, st)
	if err != nil {
		t.Fatalf("MaintainKeys: %v", err)
	}
	// The optional probe maintain's Ping uses survives the adapter.
	rep, ok := kw.(interface{ MasterKeyLoaded() bool })
	if !ok || !rep.MasterKeyLoaded() {
		t.Fatalf("MasterKeyLoaded probe missing or false: %v", ok)
	}
	// A customer-held device is reported with maintain's own sentinel, not escrow's.
	held, err := st.ListDevices(ctx, store.ScopeAll(), store.ListDevicesOptions{Search: "LEGAL-PC-01"})
	if err != nil || len(held.Items) != 1 {
		t.Fatalf("fixture device: %v", err)
	}
	if _, err := kw.Unwrap(ctx, held.Items[0].ID); !errors.Is(err, maintain.ErrCustodyCustomerHeld) {
		t.Fatalf("customer-held Unwrap = %v, want maintain.ErrCustodyCustomerHeld", err)
	}
	// An unknown device stays store.ErrNotFound (the worker maps it to not_found).
	if _, err := kw.Unwrap(ctx, "01J9ZZ0000000000000000ZZZZ"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown device = %v, want store.ErrNotFound", err)
	}
	// No key file: the factory fails and maintain refuses to start.
	if _, err := MaintainKeys(filepath.Join(t.TempDir(), "absent.key"), st); err == nil {
		t.Fatal("a missing master key file was accepted")
	}
}
