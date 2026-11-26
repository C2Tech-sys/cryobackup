package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M2-FIX-B1 (lab finding F7): RecordEscrowVerification writes the outcome of a
// cat config with the escrowed key onto the active escrow row, and nothing else.
func TestRecordEscrowVerification(t *testing.T) {
	w := seed(t)
	id := w.devA.ID
	auditBefore := must[Listing[AuditEvent]](t)(w.s.ListAudit(w.ctx, ScopeAll(), AuditFilter{}, Page{Limit: 200}))

	// Could not verify: only the attempt is recorded.
	ok(t, w.s.RecordEscrowVerification(w.ctx, ScopeAll(), id, EscrowVerification{}))
	esc := must[Escrow](t)(w.s.ActiveEscrow(w.ctx, ScopeAll(), id))
	if esc.VerifiedAt != nil || esc.LastVerifyErrorClass != "" || esc.LastVerifyAttemptAt == nil || !esc.LastVerifyAttemptAt.Equal(t0) {
		t.Fatalf("after an attempt: %+v", esc)
	}
	// A verdict on the key.
	w.clk.Advance(time.Minute)
	ok(t, w.s.RecordEscrowVerification(w.ctx, ScopeAll(), id, EscrowVerification{ErrorClass: "wrong_password"}))
	esc = must[Escrow](t)(w.s.ActiveEscrow(w.ctx, ScopeAll(), id))
	if esc.VerifiedAt != nil || esc.LastVerifyErrorClass != "wrong_password" || !esc.LastVerifyAttemptAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("after a failure: %+v", esc)
	}
	// A later "could not verify" keeps the verdict.
	ok(t, w.s.RecordEscrowVerification(w.ctx, ScopeAll(), id, EscrowVerification{}))
	if esc = must[Escrow](t)(w.s.ActiveEscrow(w.ctx, ScopeAll(), id)); esc.LastVerifyErrorClass != "wrong_password" {
		t.Fatalf("an unverifiable attempt cleared the verdict: %+v", esc)
	}
	// Success sets verified_at and clears the class.
	w.clk.Advance(time.Minute)
	ok(t, w.s.RecordEscrowVerification(w.ctx, ScopeAll(), id, EscrowVerification{OK: true}))
	esc = must[Escrow](t)(w.s.ActiveEscrow(w.ctx, ScopeAll(), id))
	if esc.VerifiedAt == nil || !esc.VerifiedAt.Equal(t0.Add(2*time.Minute)) || esc.LastVerifyErrorClass != "" || !esc.LastVerifyAttemptAt.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("after a success: %+v", esc)
	}
	// The other device is untouched.
	if other := must[Escrow](t)(w.s.ActiveEscrow(w.ctx, ScopeAll(), w.devB.ID)); other.VerifiedAt != nil || other.LastVerifyAttemptAt != nil {
		t.Fatalf("device B's escrow changed: %+v", other)
	}
	// No audit rows.
	if after := must[Listing[AuditEvent]](t)(w.s.ListAudit(w.ctx, ScopeAll(), AuditFilter{}, Page{Limit: 200})); len(after.Items) != len(auditBefore.Items) {
		t.Fatalf("audit rows %d -> %d", len(auditBefore.Items), len(after.Items))
	}
	// An unknown device, and a scope that does not hold the device, are ErrNotFound.
	if err := w.s.RecordEscrowVerification(w.ctx, ScopeAll(), "01ZZZZZZZZZZZZZZZZZZZZZZZZ", EscrowVerification{OK: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown device = %v", err)
	}
	if err := w.s.RecordEscrowVerification(w.ctx, ScopeOf(w.custB.ID), id, EscrowVerification{OK: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("out-of-scope device = %v", err)
	}
}

// fixtureEscrowStore is a store with the fictional fleet: escrowed and customer-held customers, a
// never-verified escrowed key (TECH-BENCH-07), an unconfirmed device (KIOSK-WAIT-01), a retired
// device (OLD-RECEPTION-PC) and a retired device being purged (SPARE-PC-09).
func fixtureEscrowStore(t *testing.T) (*Store, *fakeClock, func(host string) Device) {
	t.Helper()
	s, clk := newStore(t)
	ok(t, LoadFixtures(t.Context(), s, clk.Now()))
	byHost := func(host string) Device {
		t.Helper()
		l := must[Listing[Device]](t)(s.ListDevices(t.Context(), ScopeAll(), ListDevicesOptions{Search: host, IncludeRetired: true}))
		for _, d := range l.Items {
			if d.Hostname == host {
				return d
			}
		}
		t.Fatalf("fixture %s", host)
		return Device{}
	}
	return s, clk, byHost
}

// P22-H (ADR 0030): the verifier's candidate set is every device with an active escrowed key and a
// confirmed enrollment that is not being purged, retired devices included.
func TestP22HEscrowVerifyCandidateSet(t *testing.T) {
	s, _, byHost := fixtureEscrowStore(t)
	keys := must[[]state.EscrowKeyFacts](t)(s.EscrowVerifyKeys(t.Context(), ScopeAll()))
	got := map[string]state.EscrowKeyFacts{}
	for i, k := range keys {
		got[k.DeviceID] = k
		if i > 0 && keys[i-1].DeviceID >= k.DeviceID {
			t.Fatalf("not ordered by device id: %s then %s", keys[i-1].DeviceID, k.DeviceID)
		}
		if k.Escrow.Custody != "escrowed" || k.Escrow.CreatedAt == 0 {
			t.Errorf("key %s: %+v", k.DeviceID, k)
		}
	}
	retired, purging, unconfirmed, never := byHost("OLD-RECEPTION-PC"), byHost("SPARE-PC-09"), byHost("KIOSK-WAIT-01"), byHost("TECH-BENCH-07")
	held := byHost("LEGAL-PC-01")
	if k, in := got[retired.ID]; !in || !k.Retired || k.Escrow.VerifiedAt == 0 {
		t.Errorf("the retired device is not a candidate, or not marked retired: %+v %v", k, in)
	}
	if _, in := got[purging.ID]; in {
		t.Error("a device being purged is a candidate")
	}
	if _, in := got[unconfirmed.ID]; in {
		t.Error("an unconfirmed device is a candidate")
	}
	if _, in := got[held.ID]; in {
		t.Error("a customer-held device is a candidate")
	}
	if k, in := got[never.ID]; !in || k.Retired || k.Escrow.VerifiedAt != 0 || k.LastAttemptAt != 0 {
		t.Errorf("the never-verified device: %+v %v", k, in)
	}
	// Every candidate is escrowed, confirmed and not purging; every such device is a candidate.
	var want int
	ok(t, s.Read(t.Context(), func(tx *Tx) error {
		return tx.QueryRow(`SELECT COUNT(*) FROM devices d JOIN repo_key_escrow e ON e.device_id = d.id AND e.retired_at IS NULL
			JOIN customers c ON c.id = d.customer_id
			WHERE e.custody = 'escrowed' AND c.key_custody = 'escrowed' AND d.enroll_confirmed_at IS NOT NULL AND d.purge_requested_at IS NULL`).Scan(&want)
	}))
	if len(keys) != want || want < 10 {
		t.Fatalf("%d candidates, want %d", len(keys), want)
	}
	// A scope sees its own customers' keys only.
	scoped := must[[]state.EscrowKeyFacts](t)(s.EscrowVerifyKeys(t.Context(), ScopeOf(never.CustomerID)))
	for _, k := range scoped {
		if d := deviceByID(t, s, k.DeviceID); d.CustomerID != never.CustomerID {
			t.Errorf("scoped listing holds another customer's device %s", k.DeviceID)
		}
	}
	if len(scoped) == 0 || len(scoped) >= len(keys) {
		t.Errorf("scoped %d of %d", len(scoped), len(keys))
	}
	// The facts form carries the same keys; a cancelled read is NotRead, never an empty list.
	if f, err := s.EscrowVerifyFactsOf(t.Context(), ScopeAll()); err != nil || f.NotRead != "" || len(f.Keys) != len(keys) {
		t.Errorf("facts: %+v", f)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if f, err := s.EscrowVerifyFactsOf(ctx, ScopeAll()); err != nil || f.NotRead == "" || len(f.Keys) != 0 || strings.Contains(f.NotRead, "context") {
		t.Errorf("a failed read: %+v", f)
	}
}

func deviceByID(t *testing.T, s *Store, id string) Device {
	t.Helper()
	return must[Device](t)(s.GetDevice(t.Context(), ScopeAll(), id))
}

// A retired device's verification is recorded like any other; the operator's verification and its
// audit row commit together or not at all.
func TestP22HRecordEscrowVerificationRetiredAndAudited(t *testing.T) {
	s, clk, byHost := fixtureEscrowStore(t)
	retired := byHost("OLD-RECEPTION-PC")
	ok(t, s.RecordEscrowVerification(t.Context(), ScopeAll(), retired.ID, EscrowVerification{ErrorClass: "repo_missing"}))
	if esc := must[Escrow](t)(s.ActiveEscrow(t.Context(), ScopeAll(), retired.ID)); esc.LastVerifyErrorClass != "repo_missing" {
		t.Fatalf("retired device: %+v", esc)
	}

	clk.Advance(time.Minute)
	ev := AuditEvent{ActorType: "admin", ActorID: "x", Method: "api", Action: "device.escrow.verify", ObjectType: "device", ObjectID: retired.ID,
		Details: []byte(`{"verdict":"verified"}`)}
	ok(t, s.RecordEscrowVerificationAudited(t.Context(), ScopeAll(), retired.ID, EscrowVerification{OK: true}, ev))
	esc := must[Escrow](t)(s.ActiveEscrow(t.Context(), ScopeAll(), retired.ID))
	if esc.VerifiedAt == nil || !esc.VerifiedAt.Equal(clk.Now()) || esc.LastVerifyErrorClass != "" {
		t.Fatalf("after an audited success: %+v", esc)
	}
	l := must[Listing[AuditEvent]](t)(s.ListAudit(t.Context(), ScopeAll(), AuditFilter{}, Page{Limit: 500}))
	found := false
	for _, a := range l.Items {
		found = found || (a.Action == "device.escrow.verify" && a.ObjectID == retired.ID)
	}
	if !found {
		t.Fatal("no audit row")
	}
	// An audit row that cannot be written (a path in it) takes the record with it.
	clk.Advance(time.Minute)
	bad := ev
	bad.Details = []byte(`{"where":"C:/Users/x"}`)
	if err := s.RecordEscrowVerificationAudited(t.Context(), ScopeAll(), retired.ID, EscrowVerification{ErrorClass: "wrong_password"}, bad); err == nil {
		t.Fatal("a path in the audit details was accepted")
	}
	if after := must[Escrow](t)(s.ActiveEscrow(t.Context(), ScopeAll(), retired.ID)); after.LastVerifyErrorClass != "" || !after.LastVerifyAttemptAt.Equal(*esc.LastVerifyAttemptAt) {
		t.Fatalf("the record survived its refused audit row: %+v", after)
	}
}

func TestP22HEscrowVerifyTarget(t *testing.T) {
	s, _, byHost := fixtureEscrowStore(t)
	for _, c := range []struct {
		host string
		want EscrowVerifyTarget
	}{
		{"FRONTDESK-01", EscrowVerifyTarget{Custody: "escrowed", Confirmed: true}},
		{"OLD-RECEPTION-PC", EscrowVerifyTarget{Custody: "escrowed", Confirmed: true, Retired: true}},
		{"SPARE-PC-09", EscrowVerifyTarget{Custody: "escrowed", Confirmed: true, Retired: true, Purging: true}},
		{"KIOSK-WAIT-01", EscrowVerifyTarget{Custody: "escrowed"}},
		{"LEGAL-PC-01", EscrowVerifyTarget{Custody: "customer_held", Confirmed: true}},
	} {
		d := byHost(c.host)
		got := must[EscrowVerifyTarget](t)(s.GetEscrowVerifyTarget(t.Context(), ScopeAll(), d.ID))
		c.want.DeviceID, c.want.CustomerID = d.ID, d.CustomerID
		if got != c.want {
			t.Errorf("%s: %+v, want %+v", c.host, got, c.want)
		}
	}
	d := byHost("FRONTDESK-01")
	if _, err := s.GetEscrowVerifyTarget(t.Context(), ScopeOf("01ZZZZZZZZZZZZZZZZZZZZZZZZ"), d.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("out of scope: %v", err)
	}
	if _, err := s.GetEscrowVerifyTarget(t.Context(), ScopeAll(), "01ZZZZZZZZZZZZZZZZZZZZZZZZ"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown device: %v", err)
	}
}

// addEscrowP22Matrix puts the P22-H escrow verifier's scoped methods in
// the scope matrix (registered from matrix() in scope_test.go).
func addEscrowP22Matrix(m map[string]matrixCase) {
	m["RecordEscrowVerificationAudited"] = matrixCase{kind: byID,
		call: func(w *world, sc Scope, t *target) ([]string, error) {
			return nil, w.s.RecordEscrowVerificationAudited(w.ctx, sc, t.dev, EscrowVerification{OK: true},
				AuditEvent{ActorType: "admin", ActorID: "matrix", Method: "api", Action: "device.escrow.verify", ObjectType: "device", ObjectID: t.dev,
					Details: []byte(`{"verdict":"verified"}`)})
		}}
	m["EscrowVerifyKeys"] = matrixCase{kind: list,
		call: func(w *world, sc Scope, t *target) ([]string, error) {
			keys, err := w.s.EscrowVerifyKeys(w.ctx, sc)
			var rows []string
			for _, k := range keys {
				d, derr := w.s.GetDevice(w.ctx, ScopeAll(), k.DeviceID)
				if derr != nil {
					return rows, derr
				}
				rows = append(rows, d.CustomerID)
			}
			return rows, err
		}}
	m["EscrowVerifyFactsOf"] = matrixCase{kind: list,
		call: func(w *world, sc Scope, t *target) ([]string, error) {
			f, err := w.s.EscrowVerifyFactsOf(w.ctx, sc)
			if err != nil {
				return nil, err
			}
			if f.NotRead != "" {
				return nil, errors.New(f.NotRead)
			}
			var rows []string
			for _, k := range f.Keys {
				d, derr := w.s.GetDevice(w.ctx, ScopeAll(), k.DeviceID)
				if derr != nil {
					return rows, derr
				}
				rows = append(rows, d.CustomerID)
			}
			return rows, nil
		}}
	m["GetEscrowVerifyTarget"] = matrixCase{kind: byID,
		call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.GetEscrowVerifyTarget(w.ctx, sc, t.dev)
			return nil, err
		}}
}

// P22-H fix batch 1 (H1): banner C22 is raised by the running console, so the real loader fills
// World.EscrowVerify. A retired device whose escrowed key fails with wrong_password raises C22 with
// its count; a read of the keys that fails is NotRead (C22b), never an empty list read as healthy.
func TestP22HLoadWorldFeedsBannerC22(t *testing.T) {
	s, clk, byHost := fixtureEscrowStore(t)
	ctx := t.Context()
	banner := func(w state.World, id string) (state.Banner, bool) {
		for _, b := range state.Evaluate(w, clk.Now()).Banners {
			if b.ID == id {
				return b, true
			}
		}
		return state.Banner{}, false
	}
	retired := byHost("OLD-RECEPTION-PC")
	w := must[state.World](t)(s.LoadWorld(ctx, ScopeAll(), LoadOptions{}))
	if w.EscrowVerify == nil || w.EscrowVerify.NotRead != "" || len(w.EscrowVerify.Keys) == 0 {
		t.Fatalf("LoadWorld did not fill the escrow verifier's keys: %+v", w.EscrowVerify)
	}
	found := false
	for _, k := range w.EscrowVerify.Keys {
		found = found || (k.DeviceID == retired.ID && k.Retired)
	}
	if !found {
		t.Fatal("the retired device's key is not among the loaded keys")
	}
	if _, ok := banner(w, "C22"); ok {
		t.Fatal("C22 raised before any failure")
	}
	ok(t, s.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE repo_key_escrow SET last_verify_error_class = 'wrong_password' WHERE device_id = ? AND retired_at IS NULL`, retired.ID)
		return err
	}))
	w = must[state.World](t)(s.LoadWorld(ctx, ScopeAll(), LoadOptions{}))
	b, raised := banner(w, "C22")
	if !raised || b.Count != 1 || !strings.Contains(b.Text, "1 retired device") {
		t.Fatalf("C22 for one retired wrong_password: raised=%v %+v", raised, b)
	}
	// A scope that cannot see the retired device's customer does not count it.
	if wo, err := s.LoadWorld(ctx, ScopeOf("no-such-customer"), LoadOptions{}); err == nil {
		if _, raised := banner(wo, "C22"); raised {
			t.Fatal("C22 counts a device outside the scope")
		}
	}
	// The keys cannot be read: NotRead and C22b, never an empty (green) fact and never a C22 count.
	ok(t, s.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`ALTER TABLE repo_key_escrow RENAME COLUMN last_verify_attempt_at TO last_verify_attempt_gone`)
		return err
	}))
	wr, _ := s.LoadWorld(ctx, ScopeAll(), LoadOptions{})
	if wr.EscrowVerify == nil || wr.EscrowVerify.NotRead == "" || len(wr.EscrowVerify.Keys) != 0 {
		t.Fatalf("a failed read must be NotRead: %+v", wr.EscrowVerify)
	}
	if strings.Contains(wr.EscrowVerify.NotRead, "last_verify") || strings.Contains(wr.EscrowVerify.NotRead, "SQL") {
		t.Errorf("NotRead leaks the error text: %q", wr.EscrowVerify.NotRead)
	}
	if _, raised := banner(wr, "C22b"); !raised {
		t.Error("an unreadable escrow set does not raise C22b")
	}
	if _, raised := banner(wr, "C22"); raised {
		t.Error("C22 counted on facts that were not read")
	}
}
