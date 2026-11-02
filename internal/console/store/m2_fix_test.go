package store

import (
	"errors"
	"testing"
)

// The device cap is an owner-level field, as UpdateCustomer already insists: a
// customer-scoped caller may pass it back unchanged but not change or clear it.
func TestScopedCallerCannotChangeTheDeviceCap(t *testing.T) {
	w := seed(t)
	sc := ScopeOf(w.custA.ID)
	cap10, cap20 := 10, 20
	if _, changed, err := w.s.UpdateCustomerAudited(w.ctx, ScopeAll(), w.custA.ID, CustomerPatch{DeviceCap: &cap10}, testActor); err != nil || !changed {
		t.Fatalf("All scope sets the cap: %v %v", err, changed)
	}
	before := auditTotal(t, w.s)
	if _, _, err := w.s.UpdateCustomerAudited(w.ctx, sc, w.custA.ID, CustomerPatch{DeviceCap: &cap20}, testActor); !errors.Is(err, ErrScope) {
		t.Fatalf("scoped change of the cap: want ErrScope, got %v", err)
	}
	if _, _, err := w.s.UpdateCustomerAudited(w.ctx, sc, w.custA.ID, CustomerPatch{ClearDeviceCap: true}, testActor); !errors.Is(err, ErrScope) {
		t.Fatalf("scoped clearing of the cap: want ErrScope, got %v", err)
	}
	if got := must[Customer](t)(w.s.GetCustomer(w.ctx, ScopeAll(), w.custA.ID)); got.DeviceCap == nil || *got.DeviceCap != 10 || auditTotal(t, w.s) != before {
		t.Fatalf("a refused change moved the cap or wrote an audit row: %+v", got.DeviceCap)
	}
	// Passing the stored value back (a read-modify-write) is not a change.
	notes := "scoped note"
	if _, changed, err := w.s.UpdateCustomerAudited(w.ctx, sc, w.custA.ID, CustomerPatch{DeviceCap: &cap10, Notes: &notes}, testActor); err != nil || !changed {
		t.Fatalf("scoped edit that passes the cap back: %v %v", err, changed)
	}
}

func TestDeviceAuditTotalIsScoped(t *testing.T) {
	w := seed(t)
	for i := 0; i < 3; i++ {
		n := "note " + string(rune('a'+i))
		must[Device](t)(func() (Device, error) {
			d, _, err := w.s.UpdateDeviceSettings(w.ctx, ScopeAll(), w.devA.ID, DeviceSettingsPatch{Notes: &n}, testActor)
			return d, err
		}())
	}
	all := must[int](t)(w.s.DeviceAuditTotal(w.ctx, ScopeAll(), w.devA.ID))
	l := must[Listing[AuditEvent]](t)(w.s.ListDeviceAudit(w.ctx, ScopeAll(), w.devA.ID, Page{Limit: 100}))
	if all != len(l.Items) || all < 3 {
		t.Fatalf("total %d, listed %d", all, len(l.Items))
	}
	if got := must[int](t)(w.s.DeviceAuditTotal(w.ctx, ScopeOf(w.custA.ID), w.devA.ID)); got != all {
		t.Fatalf("scoped total %d, want %d", got, all)
	}
	if _, err := w.s.DeviceAuditTotal(w.ctx, ScopeOf(w.custA.ID), w.devB.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B's device through A's scope: %v", err)
	}
	if _, err := w.s.DeviceAuditTotal(w.ctx, Scope{}, w.devA.ID); !errors.Is(err, ErrScope) {
		t.Fatalf("zero scope: %v", err)
	}
}
