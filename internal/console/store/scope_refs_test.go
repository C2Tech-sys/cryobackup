package store

import (
	"errors"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// §3.5 item 3: no cross-customer reference passes through the store
// ---------------------------------------------------------------------------

// refRow is one attempt to smuggle a reference to another customer's row, or to
// change an owner-level field, through a method that is also in the scope
// matrix. The matrix (scope_test.go) addresses one row per method; these rows
// are its second axis, and each names the matrix method it extends.
type refRow struct {
	name   string
	method string // the *Store method under test; must be in matrix()
	call   func(w *world, a, b *target, other StorageTarget) error
	want   error
}

// refRows are the rows of change 8 (P1-FIX3). "operator A" is ScopeOf(custA).
func refRows() []refRow {
	newTok := func(w *world, sc Scope, mut func(*EnrollmentToken)) error {
		tok := EnrollmentToken{TokenHash: sha256Hex(uniq("ref")), ExpiresAt: w.clk.Now().Add(time.Hour)}
		mut(&tok)
		_, err := w.s.CreateEnrollmentToken(w.ctx, sc, tok)
		return err
	}
	retire := func(w *world, sc Scope, device, replacedBy string) error {
		_, err := w.s.UpdateDevice(w.ctx, sc, device, DeviceUpdate{Retire: &RetireSpec{Reason: "swap", ReplacedByDeviceID: replacedBy}})
		return err
	}
	rows := []refRow{
		{"CreateEnrollmentToken adopts B's device, operator A", "CreateEnrollmentToken",
			func(w *world, a, b *target, _ StorageTarget) error {
				return newTok(w, ScopeOf(w.custA.ID), func(t *EnrollmentToken) { one := 1; t.CustomerID, t.AdoptsDeviceID, t.MaxUses = a.cust, b.dev, &one })
			}, ErrNotFound},
		{"CreateEnrollmentToken adopts B's device, all customers", "CreateEnrollmentToken",
			func(w *world, a, b *target, _ StorageTarget) error {
				return newTok(w, ScopeAll(), func(t *EnrollmentToken) { one := 1; t.CustomerID, t.AdoptsDeviceID, t.MaxUses = a.cust, b.dev, &one })
			}, ErrInvalid},
		{"CreateEnrollmentToken names B's policy, operator A", "CreateEnrollmentToken",
			func(w *world, a, b *target, _ StorageTarget) error {
				return newTok(w, ScopeOf(w.custA.ID), func(t *EnrollmentToken) { t.CustomerID, t.PolicyID = a.cust, b.policy })
			}, ErrInvalid},
		{"CreateEnrollmentToken for B naming B's device, operator A", "CreateEnrollmentToken",
			func(w *world, a, b *target, _ StorageTarget) error {
				return newTok(w, ScopeOf(w.custA.ID), func(t *EnrollmentToken) { one := 1; t.CustomerID, t.AdoptsDeviceID, t.MaxUses = b.cust, b.dev, &one })
			}, ErrNotFound},
		{"UpdateDevice retires A's device replaced by B's, operator A", "UpdateDevice",
			func(w *world, a, b *target, _ StorageTarget) error {
				return retire(w, ScopeOf(w.custA.ID), a.dev, b.dev)
			}, ErrNotFound},
		{"UpdateDevice retires A's device replaced by B's, all customers", "UpdateDevice",
			func(w *world, a, b *target, _ StorageTarget) error {
				return retire(w, ScopeAll(), a.dev, b.dev)
			}, ErrInvalid},
		{"UpdateDevice retires B's device replaced by A's, operator A", "UpdateDevice",
			func(w *world, a, b *target, _ StorageTarget) error {
				return retire(w, ScopeOf(w.custA.ID), b.dev, a.dev)
			}, ErrNotFound},
	}

	// Owner-level customer fields: operator A may not change them on its own
	// customer (ErrScope) and cannot reach B's customer at all (ErrNotFound).
	type field struct {
		name string
		mut  func(c *Customer, other StorageTarget)
	}
	fields := []field{
		{"storage_target_id", func(c *Customer, other StorageTarget) { c.StorageTargetID = other.ID }},
		{"key_custody", func(c *Customer, _ StorageTarget) { c.KeyCustody = "customer_held" }},
		{"device_cap", func(c *Customer, _ StorageTarget) { n := 99; c.DeviceCap = &n }},
	}
	type victim struct {
		label string
		pick  func(a, b *target) string
		want  error
	}
	victims := []victim{
		{"A (own customer)", func(a, _ *target) string { return a.cust }, ErrScope},
		{"B's customer", func(_, b *target) string { return b.cust }, ErrNotFound},
	}
	for _, f := range fields {
		for _, v := range victims {
			f, v := f, v
			rows = append(rows, refRow{"UpdateCustomer changes " + f.name + " of " + v.label + ", operator A", "UpdateCustomer",
				func(w *world, a, b *target, other StorageTarget) error {
					id := v.pick(a, b)
					c := must[Customer](w.t)(w.s.GetCustomer(w.ctx, ScopeAll(), id))
					f.mut(&c, other)
					_, err := w.s.UpdateCustomer(w.ctx, ScopeOf(w.custA.ID), id, c)
					return err
				}, v.want})
		}
	}
	rows = append(rows, refRow{"UpdateCustomer points A at a storage target that does not exist, all customers", "UpdateCustomer",
		func(w *world, a, _ *target, _ StorageTarget) error {
			c := must[Customer](w.t)(w.s.GetCustomer(w.ctx, ScopeAll(), a.cust))
			c.StorageTargetID = "01J9Y4K3A0B1C2D3E4F5G6H7NO"
			_, err := w.s.UpdateCustomer(w.ctx, ScopeAll(), a.cust, c)
			return err
		}, ErrInvalid})
	return rows
}

func secondTarget(t *testing.T, w *world) StorageTarget {
	t.Helper()
	return must[StorageTarget](t)(w.s.CreateStorageTarget(w.ctx, StorageTarget{Name: "Second Storage", Provider: "wasabi",
		Endpoint: "s3.second.invalid", Bucket: "cryo-second", GatewayURL: "https://gateway2.example.invalid:9443", CredentialRef: "CRYO_TEST_S3_2"}))
}

// Every reference row runs against seeded rows of both customers: the call fails
// with exactly the stated error and changes nothing of either customer's data.
func TestScopeMatrixReferenceRows(t *testing.T) {
	w := seed(t)
	tA := seedTarget(w, "A", w.custA, w.devA)
	tB := seedTarget(w, "B", w.custB, w.devB)
	other := secondTarget(t, w)
	methods := matrix()
	rows := refRows()
	if len(rows) < 14 {
		t.Fatalf("only %d reference rows", len(rows))
	}
	for _, r := range rows {
		r := r
		t.Run(r.name, func(t *testing.T) {
			if _, ok := methods[r.method]; !ok {
				t.Fatalf("%s is not a scoped method in the matrix", r.method)
			}
			wt := *w
			wt.t = t
			beforeA, beforeB := fingerprint(&wt, tA), fingerprint(&wt, tB)
			err := r.call(&wt, tA, tB, other)
			if !errors.Is(err, r.want) {
				t.Fatalf("err = %v, want %v", err, r.want)
			}
			if fingerprint(&wt, tA) != beforeA || fingerprint(&wt, tB) != beforeB {
				t.Fatal("a refused reference changed customer data")
			}
		})
	}
}

// The positive side of the rows above: the same calls inside one customer, and
// all-customer changes of owner-level fields, succeed.
func TestScopeReferenceRowsAllowTheLegitimateCases(t *testing.T) {
	w := seed(t)
	ctx := w.ctx
	other := secondTarget(t, w)
	opA := ScopeOf(w.custA.ID)

	// UpdateDevice: operator A retires A's device in favour of another A device.
	repl := w.enroll("tok-a", "A-DESK-REPL").Device
	d := must[Device](t)(w.s.UpdateDevice(ctx, opA, w.devA.ID, DeviceUpdate{Retire: &RetireSpec{Reason: "swap", ReplacedByDeviceID: repl.ID}}))
	if d.ReplacedByDeviceID != repl.ID || d.RetiredAt == nil {
		t.Fatalf("device after retire = %+v", d)
	}

	// UpdateCustomer: a scoped caller may pass the owner-level fields back unchanged ...
	c := must[Customer](t)(w.s.GetCustomer(ctx, ScopeAll(), w.custA.ID))
	c.Notes = "edited by operator"
	c.StorageTargetID = "" // "keep the current target"
	if _, err := w.s.UpdateCustomer(ctx, opA, w.custA.ID, c); err != nil {
		t.Fatalf("operator A, unchanged owner fields = %v", err)
	}
	// ... but all-customer scope may change them, to a target that exists.
	c = must[Customer](t)(w.s.GetCustomer(ctx, ScopeAll(), w.custA.ID))
	capN := 5
	c.StorageTargetID, c.KeyCustody, c.DeviceCap = other.ID, "customer_held", &capN
	got, err := w.s.UpdateCustomer(ctx, ScopeAll(), w.custA.ID, c)
	if err != nil {
		t.Fatalf("all-customer change of owner fields = %v", err)
	}
	if got.StorageTargetID != other.ID || got.KeyCustody != "customer_held" || got.DeviceCap == nil || *got.DeviceCap != 5 {
		t.Fatalf("customer after update = %+v", got)
	}
	// Once changed, passing the stored values back as operator A is again "unchanged".
	got.Notes = "second edit"
	if _, err := w.s.UpdateCustomer(ctx, opA, w.custA.ID, got); err != nil {
		t.Fatalf("operator A passing the stored owner fields back = %v", err)
	}
}
