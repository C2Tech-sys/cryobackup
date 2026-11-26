package restores

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// P22-G: restore.browse is written once per admin, device and snapshot per hour (ruling C5), it
// names no folder, a device outside the caller's scope is refused, and a failed audit write is an
// error that leaves nothing remembered.

func browseRows(t *testing.T, e *env) []store.AuditEvent {
	t.Helper()
	l, err := e.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{Action: "restore.browse"}, store.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func TestAuditBrowseOncePerAdminDeviceSnapshotHour(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := Actor{AdminID: "01JADMINAAAAAAAAAAAAAAAAAA", Username: "alice", IP: "198.51.100.4", Method: "web"}
	bob := Actor{AdminID: "01JADMINBBBBBBBBBBBBBBBBBB", Username: "bob", IP: "198.51.100.5"}
	other := strings.Repeat("ab", 32)
	step := func(who Actor, dev, snap string, want int) {
		t.Helper()
		if err := e.svc.AuditBrowse(ctx, who, store.ScopeAll(), dev, snap); err != nil {
			t.Fatalf("browse: %v", err)
		}
		if got := len(browseRows(t, e)); got != want {
			t.Fatalf("%d restore.browse rows, want %d", got, want)
		}
	}
	step(alice, e.device.ID, fixtures.FixtureSnapshotID, 1)
	step(alice, e.device.ID, fixtures.FixtureSnapshotID, 1) // the same hour: no second row
	e.now = e.now.Add(59 * time.Minute)
	step(alice, e.device.ID, fixtures.FixtureSnapshotID, 1)
	step(alice, e.device.ID, other, 2)                    // another snapshot
	step(bob, e.device.ID, fixtures.FixtureSnapshotID, 3) // another admin
	step(alice, e.held.ID, fixtures.FixtureSnapshotID, 4) // another device
	e.now = e.now.Add(2 * time.Minute)                    // past the hour of the first row
	step(alice, e.device.ID, fixtures.FixtureSnapshotID, 5)

	for _, r := range browseRows(t, e) {
		if r.ObjectType != "device" || r.ActorType != "admin" || r.Method != "web" {
			t.Errorf("row shape: %+v", r)
		}
		if strings.Contains(string(r.Details), "/") || strings.Contains(string(r.Details), `\`) || !strings.Contains(string(r.Details), `"snapshot_id"`) {
			t.Errorf("details must name the snapshot and no folder: %s", r.Details)
		}
		if r.CustomerID == "" {
			t.Errorf("the row carries no customer, so a scoped auditor cannot see it: %+v", r)
		}
	}
}

func TestAuditBrowseRefusals(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	who := Actor{AdminID: "01JADMINCCCCCCCCCCCCCCCCCC", Username: "carol"}
	if err := e.svc.AuditBrowse(ctx, who, store.ScopeAll(), e.device.ID, "latest"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a malformed snapshot id: %v", err)
	}
	if err := e.svc.AuditBrowse(ctx, who, store.ScopeOf("01JNOTACUSTOMER00000000000"), e.device.ID, fixtures.FixtureSnapshotID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a device outside scope: %v", err)
	}
	if n := len(browseRows(t, e)); n != 0 {
		t.Fatalf("a refused browse wrote %d rows", n)
	}
	// The audit table is gone: the browse is refused as NOT AUDITED and nothing is remembered.
	if err := e.st.Write(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`ALTER TABLE audit_events RENAME TO audit_events_gone`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.AuditBrowse(ctx, who, store.ScopeAll(), e.device.ID, fixtures.FixtureSnapshotID); !errors.Is(err, store.ErrAudit) {
		t.Fatalf("a failed audit write: %v", err)
	}
	if err := e.st.Write(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`ALTER TABLE audit_events_gone RENAME TO audit_events`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.AuditBrowse(ctx, who, store.ScopeAll(), e.device.ID, fixtures.FixtureSnapshotID); err != nil {
		t.Fatal(err)
	}
	if n := len(browseRows(t, e)); n != 1 {
		t.Fatalf("after the failure the next browse must write its row: %d", n)
	}
}

// TestFixturesSelectionMatchesTheService pins fixtures' copy of the wrapped selection format
// (fixtures.SeedP22) to the service's: a seeded download unwraps to its paths here.
func TestFixturesSelectionMatchesTheService(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := fixtures.SeedP22(ctx, e.st, e.now, e.svc.Master); err != nil {
		t.Fatal(err)
	}
	r, ok := fixtures.P22RestoreByKey("zip-ready")
	if !ok {
		t.Fatal("no zip-ready fixture")
	}
	row, err := e.st.GetRestoreRequest(ctx, store.ScopeAll(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	paths, legacy, err := unwrapSelection(e.svc.Master, row)
	if err != nil || legacy || len(paths) != 1 || paths[0] != fixtures.P22FormsFolder {
		t.Fatalf("fixture selection: %v %v %v", paths, legacy, err)
	}
	for _, f := range fixtures.P22Restores() {
		got, err := e.st.GetRestoreRequest(ctx, store.ScopeAll(), f.ID)
		if err != nil || got.Status != f.Status || got.ErrorClass != f.ErrorClass {
			t.Errorf("%s: %+v %v", f.Key, got.Status, err)
		}
		if oc := DescribeOutcome(got); oc.Text == "" {
			t.Errorf("%s: no outcome sentence", f.Key)
		}
	}
}
