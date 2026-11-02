package store

import (
	"errors"
	"testing"
)

// flagConflict sets identity_conflict_at the way the heartbeat path will: the
// store has no public setter, so the tests write the column directly.
func (w *world) flagConflict(dev string) {
	w.t.Helper()
	ok(w.t, w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE devices SET identity_conflict_at = ? WHERE id = ?`, tx.nowMS(), dev)
		return err
	}))
}

func TestResolveIdentityConflictKeep(t *testing.T) {
	w := seed(t)
	w.flagConflict(w.devA.ID)
	d := must[Device](t)(w.s.ResolveIdentityConflict(w.ctx, ScopeAll(), w.devA.ID, IdentityResolution{Action: "keep", Reason: "hardware swap confirmed with the customer"}, wActor))
	if d.IdentityConflictAt != nil || d.RetiredAt != nil {
		t.Fatalf("after keep: conflict=%v retired=%v", d.IdentityConflictAt, d.RetiredAt)
	}
	l := must[Listing[AuditEvent]](t)(w.s.ListAudit(w.ctx, ScopeAll(), AuditFilter{Action: "device.identity_resolve"}, Page{Limit: 10}))
	if len(l.Items) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(l.Items))
	}
	ev := l.Items[0]
	if ev.ObjectID != w.devA.ID || ev.CustomerID != w.custA.ID || ev.Severity != "warning" || ev.Result != "ok" {
		t.Fatalf("audit row: %+v", ev)
	}
	// Resolved: resolving again is a conflict, and writes nothing.
	if _, err := w.s.ResolveIdentityConflict(w.ctx, ScopeAll(), w.devA.ID, IdentityResolution{Action: "keep", Reason: "again"}, wActor); !errors.Is(err, ErrNoIdentityConflict) || !errors.Is(err, ErrConflict) {
		t.Fatalf("second resolve: %v", err)
	}
	if n := w.cmdAuditCount("device.identity_resolve", w.devA.ID); n != 1 {
		t.Fatalf("audit rows after the refusal = %d", n)
	}
}

func TestResolveIdentityConflictRetireRevokesCredentialsAndEndsAPause(t *testing.T) {
	w := seed(t)
	must[Device](t)(w.s.UpdateDevice(w.ctx, ScopeAll(), w.devA.ID, DeviceUpdate{Pause: &PauseSpec{Reason: "moving office"}}))
	w.flagConflict(w.devA.ID)
	d := must[Device](t)(w.s.ResolveIdentityConflict(w.ctx, ScopeAll(), w.devA.ID, IdentityResolution{Action: "retire", Reason: "clone of a restored image"}, wActor))
	if d.RetiredAt == nil || d.IdentityConflictAt != nil || d.PausedAt != nil {
		t.Fatalf("after retire: %+v", d)
	}
	if _, _, err := w.s.AgentLookupDeviceToken(w.ctx, sha256Hex("device-token-A-DESK-1")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the retired identity can still authenticate: %v", err)
	}
	if n := w.cmdAuditCount("device.identity_resolve", w.devA.ID); n != 1 {
		t.Fatalf("audit rows = %d", n)
	}
	// A retired device has nothing left to resolve.
	if _, err := w.s.ResolveIdentityConflict(w.ctx, ScopeAll(), w.devA.ID, IdentityResolution{Action: "keep", Reason: "x"}, wActor); !errors.Is(err, ErrDeviceRetired) {
		t.Fatalf("resolve of a retired device: %v", err)
	}
}

func TestResolveIdentityConflictValidatesAndIsScoped(t *testing.T) {
	w := seed(t)
	w.flagConflict(w.devA.ID)
	for name, in := range map[string]IdentityResolution{
		"unknown action": {Action: "ignore", Reason: "x"},
		"no reason":      {Action: "keep"},
		"blank reason":   {Action: "keep", Reason: "   "},
		"multi-line":     {Action: "keep", Reason: "a\nb"},
		"too long":       {Action: "keep", Reason: string(make([]rune, 501))},
		"a path":         {Action: "keep", Reason: `copied C:\Users\bob to the new box`},
	} {
		_, err := w.s.ResolveIdentityConflict(w.ctx, ScopeAll(), w.devA.ID, in, wActor)
		if !errors.Is(err, ErrInvalid) || len(FieldErrors(err)) == 0 {
			t.Errorf("%s: %v, want a field error", name, err)
		}
	}
	if n := w.cmdAuditCount("device.identity_resolve", w.devA.ID); n != 0 {
		t.Fatalf("rejections wrote %d audit rows", n)
	}
	good := IdentityResolution{Action: "keep", Reason: "ok"}
	if _, err := w.s.ResolveIdentityConflict(w.ctx, ScopeOf(w.custB.ID), w.devA.ID, good, wActor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B resolving A's conflict: %v, want ErrNotFound", err)
	}
	if _, err := w.s.ResolveIdentityConflict(w.ctx, Scope{}, w.devA.ID, good, wActor); !errors.Is(err, ErrScope) {
		t.Fatalf("zero scope: %v", err)
	}
	if d := must[Device](t)(w.s.GetDevice(w.ctx, ScopeAll(), w.devA.ID)); d.IdentityConflictAt == nil {
		t.Fatal("a denied or rejected resolve cleared the conflict")
	}
}
