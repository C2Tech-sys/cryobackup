package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"Summit Eye Care":        "summit-eye-care",
		"  Harbor & Family  ":    "harbor-family",
		"Northwind_Dental #2":    "northwind-dental-2",
		"Blue Ridge Accounting!": "blue-ridge-accounting",
		"---":                    "",
		strings.Repeat("a", 80):  strings.Repeat("a", 63),
	} {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCustomerCRUDAndRules(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	c := w.custA
	if c.Slug != "alpha-dental" || c.State != "active" || c.KeyCustody != "escrowed" || c.StorageTargetID != w.target.ID || string(c.Contacts) != "[]" {
		t.Fatalf("defaults = %+v", c)
	}
	// Names are unique case-insensitively; the slug too.
	if _, err := w.s.CreateCustomer(ctx, ScopeAll(), Customer{Name: "ALPHA DENTAL"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate name = %v, want ErrConflict", err)
	}
	// Only a scope that sees everything may create one.
	if _, err := w.s.CreateCustomer(ctx, ScopeOf(w.custA.ID), Customer{Name: "Sneaky"}); !errors.Is(err, ErrScope) {
		t.Fatalf("scoped create = %v, want ErrScope", err)
	}
	// An expected customer cannot be archived (and nothing changes when it is refused).
	c.Expected, c.State = true, "archived"
	if _, err := w.s.UpdateCustomer(ctx, ScopeAll(), c.ID, c); !errors.Is(err, ErrInvalid) {
		t.Fatalf("archiving an expected customer = %v, want ErrInvalid", err)
	}
	// Archive sets archived_at, un-archive clears it.
	c.Expected = false
	archived := must[Customer](t)(w.s.UpdateCustomer(ctx, ScopeAll(), c.ID, c))
	if archived.State != "archived" || archived.ArchivedAt == nil || !archived.ArchivedAt.Equal(t0) {
		t.Fatalf("archived = %+v", archived)
	}
	c.State = "active"
	if back := must[Customer](t)(w.s.UpdateCustomer(ctx, ScopeAll(), c.ID, c)); back.ArchivedAt != nil || back.State != "active" {
		t.Fatalf("restored = %+v", back)
	}
	// Onboarding window, contacts, display zone, notes round-trip.
	until := t0.Add(7 * 24 * time.Hour)
	c.Expected, c.OnboardingUntil, c.DisplayTZ, c.Notes = true, &until, "America/Chicago", "pilot customer"
	c.Contacts = []byte(`[{"name":"Dana"}]`)
	got := must[Customer](t)(w.s.UpdateCustomer(ctx, ScopeAll(), c.ID, c))
	if !got.Expected || got.OnboardingUntil == nil || !got.OnboardingUntil.Equal(until) || got.DisplayTZ != "America/Chicago" || string(got.Contacts) != `[{"name":"Dana"}]` {
		t.Fatalf("updated = %+v", got)
	}
	if _, err := w.s.UpdateCustomer(ctx, ScopeAll(), c.ID, Customer{Name: "x", Slug: "Bad Slug!", State: "active", KeyCustody: "escrowed"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad slug = %v, want ErrInvalid", err)
	}
	if list := must[[]Customer](t)(w.s.ListCustomers(ctx, ScopeAll())); len(list) != 2 || list[0].Name != "Alpha Dental" {
		t.Fatalf("ListCustomers = %+v", list)
	}
	if _, err := w.s.GetCustomer(ctx, ScopeAll(), "01J9Y4K3A0B1C2D3E4F5G6H7ZZ"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown customer = %v", err)
	}
}

func TestDeviceListFiltersSortAndPaging(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	for _, h := range []string{"A-LAPTOP-1", "A-LAPTOP-2", "A-SRV_1", "A-100%"} {
		in := enrollInput("tok-a", h)
		in.DeviceClass = "laptop"
		if strings.HasPrefix(h, "A-SRV") || strings.HasPrefix(h, "A-100") {
			in.DeviceClass = "desktop"
		}
		must[EnrollResult](t)(w.s.AgentEnroll(ctx, in))
	}
	all := must[Listing[Device]](t)(w.s.ListDevices(ctx, ScopeAll(), ListDevicesOptions{}))
	if len(all.Items) != 6 || all.NextCursor != "" {
		t.Fatalf("all devices = %d next=%q", len(all.Items), all.NextCursor)
	}
	// Sorted by hostname, case-insensitively.
	for i := 1; i < len(all.Items); i++ {
		if strings.ToLower(all.Items[i-1].Hostname) > strings.ToLower(all.Items[i].Hostname) {
			t.Fatalf("not sorted by hostname: %s before %s", all.Items[i-1].Hostname, all.Items[i].Hostname)
		}
	}
	// Customer filter, class filter.
	if l := must[Listing[Device]](t)(w.s.ListDevices(ctx, ScopeAll(), ListDevicesOptions{CustomerID: w.custB.ID})); len(l.Items) != 1 || l.Items[0].CustomerID != w.custB.ID {
		t.Fatalf("customer filter = %+v", l.Items)
	}
	if l := must[Listing[Device]](t)(w.s.ListDevices(ctx, ScopeAll(), ListDevicesOptions{DeviceClass: "laptop"})); len(l.Items) != 2 {
		t.Fatalf("class filter = %d", len(l.Items))
	}
	// Search is a substring match with LIKE metacharacters escaped.
	for q, want := range map[string]int{"laptop": 2, "A-SRV_1": 1, "_": 1, "%": 1, "a-d": 1, "zzz": 0} {
		if l := must[Listing[Device]](t)(w.s.ListDevices(ctx, ScopeAll(), ListDevicesOptions{Search: q})); len(l.Items) != want {
			t.Errorf("search %q = %d devices, want %d", q, len(l.Items), want)
		}
	}
	// Paging: three pages of two, no row twice, no row lost, a cursor only while more remain.
	seen := map[string]bool{}
	cursor, pages := "", 0
	for {
		l := must[Listing[Device]](t)(w.s.ListDevices(ctx, ScopeAll(), ListDevicesOptions{Page: Page{Limit: 2, Cursor: cursor}}))
		pages++
		for _, d := range l.Items {
			if seen[d.ID] {
				t.Fatalf("device %s listed twice", d.ID)
			}
			seen[d.ID] = true
		}
		if l.NextCursor == "" {
			break
		}
		cursor = l.NextCursor
	}
	if len(seen) != 6 || pages != 3 {
		t.Fatalf("paged %d devices in %d pages", len(seen), pages)
	}
	if _, err := w.s.ListDevices(ctx, ScopeAll(), ListDevicesOptions{Sort: "random"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown sort = %v", err)
	}
	// Retired devices are hidden unless asked for.
	_, err := w.s.UpdateDevice(ctx, ScopeAll(), w.devB.ID, DeviceUpdate{Retire: &RetireSpec{Reason: "gone"}})
	ok(t, err)
	if l := must[Listing[Device]](t)(w.s.ListDevices(ctx, ScopeAll(), ListDevicesOptions{CustomerID: w.custB.ID})); len(l.Items) != 0 {
		t.Fatalf("retired device listed by default: %+v", l.Items)
	}
	if l := must[Listing[Device]](t)(w.s.ListDevices(ctx, ScopeAll(), ListDevicesOptions{CustomerID: w.custB.ID, IncludeRetired: true})); len(l.Items) != 1 {
		t.Fatalf("IncludeRetired = %d", len(l.Items))
	}
}

func TestUpdateDevicePauseRetireRules(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	id := w.devA.ID
	name, notes := "Front desk", "behind the counter"
	d := must[Device](t)(w.s.UpdateDevice(ctx, ScopeAll(), id, DeviceUpdate{FriendlyName: &name, Notes: &notes}))
	if d.FriendlyName != name || d.Notes != notes {
		t.Fatalf("device = %+v", d)
	}
	// Pause without a reason: the CHECK surfaces as ErrInvalid and nothing changes.
	if _, err := w.s.UpdateDevice(ctx, ScopeAll(), id, DeviceUpdate{Pause: &PauseSpec{}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("pause without a reason = %v, want ErrInvalid", err)
	}
	// A pause that ends before it starts.
	past := t0.Add(-time.Hour)
	if _, err := w.s.UpdateDevice(ctx, ScopeAll(), id, DeviceUpdate{Pause: &PauseSpec{Reason: "r", Until: &past}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("pause ending in the past = %v, want ErrInvalid", err)
	}
	if _, err := w.s.UpdateDevice(ctx, ScopeAll(), id, DeviceUpdate{Pause: &PauseSpec{Reason: "r"}, Unpause: true}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("pause+unpause = %v, want ErrInvalid", err)
	}
	until := t0.Add(48 * time.Hour)
	paused := must[Device](t)(w.s.UpdateDevice(ctx, ScopeAll(), id, DeviceUpdate{Pause: &PauseSpec{Reason: "awaiting replacement", Until: &until}, By: w.admin.ID}))
	if paused.PausedAt == nil || !paused.PausedAt.Equal(t0) || paused.PauseReason != "awaiting replacement" || paused.PausedBy != w.admin.ID || paused.PausedUntil == nil {
		t.Fatalf("paused = %+v", paused)
	}
	// A device may not be both retired and paused: retire while paused is refused, as a unit.
	_, err := w.s.UpdateDevice(ctx, ScopeAll(), id, DeviceUpdate{Retire: &RetireSpec{Reason: "old"}})
	var ce *ConstraintError
	if !errors.Is(err, ErrInvalid) || !errors.As(err, &ce) || !strings.Contains(ce.Constraint, "paused_at") {
		t.Fatalf("retire while paused = %v, want ErrInvalid naming the retired/paused rule", err)
	}
	if _, _, err := w.s.AgentLookupDeviceToken(ctx, sha256Hex("device-token-A-DESK-1")); err != nil {
		t.Fatalf("a refused retire revoked the token: %v", err)
	}
	// Unpause, then retire works, and "replaced" implies retired.
	unp := must[Device](t)(w.s.UpdateDevice(ctx, ScopeAll(), id, DeviceUpdate{Unpause: true}))
	if unp.PausedAt != nil || unp.PausedUntil != nil || unp.PauseReason != "" || unp.PausedBy != "" {
		t.Fatalf("unpaused = %+v", unp)
	}
	repl := w.enroll("tok-a", "A-DESK-NEW").Device
	ret := must[Device](t)(w.s.UpdateDevice(ctx, ScopeAll(), id, DeviceUpdate{Retire: &RetireSpec{Reason: "replaced by new hardware", ReplacedByDeviceID: repl.ID}}))
	if ret.RetiredAt == nil || ret.ReplacedByDeviceID != repl.ID || ret.RetiredReason == "" {
		t.Fatalf("retired = %+v", ret)
	}
	// Pinning a version and moving rings; an unknown ring is a CHECK failure.
	ring, pin := "pilot", "1.5.0"
	pinned := must[Device](t)(w.s.UpdateDevice(ctx, ScopeAll(), repl.ID, DeviceUpdate{UpdateRing: &ring, TargetVersion: &pin}))
	if pinned.UpdateRing != "pilot" || pinned.TargetVersion != "1.5.0" {
		t.Fatalf("pinned = %+v", pinned)
	}
	bad := "nightly"
	if _, err := w.s.UpdateDevice(ctx, ScopeAll(), repl.ID, DeviceUpdate{UpdateRing: &bad}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown ring = %v", err)
	}
	ok(t, w.s.AgentConfirmEnrollment(ctx, repl.ID))
	ok(t, w.s.AgentConfirmEnrollment(ctx, repl.ID)) // idempotent
	if err := w.s.AgentConfirmEnrollment(ctx, "01J9Y4K3A0B1C2D3E4F5G6H7ZZ"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("confirm unknown = %v", err)
	}
}

// ---------------------------------------------------------------------------
// constraint violations surface as typed errors that name the rule (architecture §3.4)
// ---------------------------------------------------------------------------

func TestConstraintViolationsAreTyped(t *testing.T) {
	w := seed(t)
	ctx := context.Background()

	check := func(name string, err error, kind error, containing string) {
		t.Helper()
		var ce *ConstraintError
		if !errors.Is(err, kind) || !errors.As(err, &ce) {
			t.Errorf("%s: err = %v, want a *ConstraintError wrapping %v", name, err, kind)
			return
		}
		if !strings.Contains(ce.Constraint+" "+ce.Message, containing) {
			t.Errorf("%s: constraint %q / message %q does not name %q", name, ce.Constraint, ce.Message, containing)
		}
	}

	// second default policy
	_, err := w.s.CreatePolicy(ctx, ScopeAll(), PolicyInput{Scope: "default", Name: "Another default"})
	check("second default policy", err, ErrConflict, "uq_policies_one_default")

	// duplicate token hashes (enrollment, device, session)
	_, err = w.s.CreateEnrollmentToken(ctx, ScopeAll(), EnrollmentToken{CustomerID: w.custA.ID, TokenHash: sha256Hex("tok-a"), ExpiresAt: t0.Add(time.Hour)})
	check("duplicate enrollment token hash", err, ErrConflict, "token_hash")
	_, bTok, lerr := w.s.AgentLookupDeviceToken(ctx, sha256Hex("device-token-B-DESK-1"))
	ok(t, lerr)
	_, err = w.s.AgentRotateDeviceToken(ctx, w.devB.ID, bTok.ID, sha256Hex("device-token-A-DESK-1"), time.Hour)
	check("duplicate device token hash", err, ErrConflict, "token_hash")

	// pause without a reason / retired and paused: covered through the API in TestUpdateDevicePauseRetireRules; here raw.
	err = w.s.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE devices SET paused_at = ? WHERE id = ?`, t0.UnixMilli(), w.devA.ID)
		return err
	})
	check("pause requires a reason", err, ErrInvalid, "pause_reason")
	err = w.s.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE devices SET paused_at = ?, pause_reason = 'x', retired_at = ? WHERE id = ?`, t0.UnixMilli(), t0.UnixMilli(), w.devA.ID)
		return err
	})
	check("not both retired and paused", err, ErrInvalid, "retired_at IS NOT NULL AND paused_at IS NOT NULL")

	// audit rows are immutable
	ev := must[AuditEvent](t)(w.s.AppendAudit(ctx, AuditEvent{ActorType: "system", Action: "x"}))
	err = w.s.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE audit_events SET result = 'error' WHERE id = ?`, ev.ID)
		return err
	})
	check("audit immutability trigger", err, ErrInvalid, "immutable")

	// one active gateway login per device, one current token per device, one active escrow key per device
	err = w.s.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`INSERT INTO gateway_logins (id, device_id, username, password_hash, created_at, updated_at) VALUES (?,?,?,?,1,1)`,
			tx.id(), w.devA.ID, "ZZZZZZZZZZZZZZZZZZZZZZZZZZ", fixtureBcrypt) // another username: the device rule must fire
		return err
	})
	check("one active login per device", err, ErrConflict, "uq_gateway_logins_active_device")
	err = w.s.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`INSERT INTO device_tokens (id, device_id, token_hash, created_at, updated_at) VALUES (?,?,?,1,1)`, tx.id(), w.devA.ID, sha256Hex("second-current"))
		return err
	})
	check("one current token per device", err, ErrConflict, "uq_device_tokens_current")
	err = w.s.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`INSERT INTO repo_key_escrow (id, device_id, key_version, custody, wrapped_key, wrap_alg, master_key_id, created_at, updated_at)
			VALUES (?,?,2,'escrowed',x'00','aes-256-gcm+hkdf-sha256/v1','k',1,1)`, tx.id(), w.devA.ID)
		return err
	})
	check("one active key per device", err, ErrConflict, "uq_repo_key_escrow_active")
	err = w.s.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`INSERT INTO repo_key_escrow (id, device_id, key_version, custody, created_at, updated_at)
			VALUES (?,?,9,'escrowed',1,1)`, tx.id(), w.devB.ID)
		return err
	})
	check("escrowed key needs wrapped_key", err, ErrInvalid, "custody")

	// unique command idempotency key, run id, snapshot
	in := CommandInput{Type: "check_in", IdempotencyKey: "dup"}
	must[Command](t)(func() (Command, error) {
		c, _, err := w.s.EnqueueCommand(ctx, ScopeAll(), w.devA.ID, in)
		return c, err
	}())
	err = w.s.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`INSERT INTO commands (id, device_id, type, idempotency_key, expires_at, created_at, updated_at) VALUES (?,?,'check_in','dup',?,1,1)`,
			tx.id(), w.devA.ID, t0.Add(time.Hour).UnixMilli())
		return err
	})
	check("unique idempotency key per device", err, ErrConflict, "idempotency_key")

	// adoption tokens are single use; used_count never exceeds the cap
	two := 2
	err = w.s.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`INSERT INTO enrollment_tokens (id, customer_id, token_hash, adopts_device_id, expires_at, max_uses, created_at, updated_at) VALUES (?,?,?,?,?,?,1,1)`,
			tx.id(), w.custA.ID, sha256Hex("adopt"), w.devA.ID, t0.Add(time.Hour).UnixMilli(), two)
		return err
	})
	check("adoption token must be single-use", err, ErrInvalid, "adopts_device_id")
	err = w.s.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE enrollment_tokens SET max_uses = 1, used_count = 2 WHERE id = ?`, w.tokA.ID)
		return err
	})
	check("used_count <= max_uses", err, ErrInvalid, "used_count")

	// restic: --verify cannot combine with --dry-run
	_, err = w.s.CreateRestoreRequest(ctx, ScopeAll(), RestoreRequest{DeviceID: w.devA.ID, SnapshotID: strings.Repeat("a", 64), Mode: "device_original", Verify: true, Simulate: true, ExpiresAt: t0.Add(time.Hour)})
	check("verify cannot combine with simulate", err, ErrInvalid, "simulate")

	// a customer with devices cannot be deleted (RESTRICT); an expected customer cannot be archived
	err = w.s.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`DELETE FROM customers WHERE id = ?`, w.custA.ID)
		return err
	})
	check("customer with devices cannot be deleted", err, ErrInvalid, "foreign key")

	// a seconds-epoch timestamp is rejected at insert in every high-volume table
	err = w.s.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`INSERT INTO heartbeats (id, device_id, received_at, service_state, created_at) VALUES (?,?,?,'idle',1)`, tx.id(), w.devA.ID, 1_800_000_000)
		return err
	})
	check("heartbeat in seconds", err, ErrInvalid, "1000000000000")
	err = w.s.Write(ctx, func(tx *Tx) error {
		_, err := tx.Exec(`INSERT INTO snapshots (device_id, snapshot_id, taken_at, created_at, updated_at) VALUES (?,?,?,1,1)`, w.devA.ID, strings.Repeat("f", 64), 1_800_000_000)
		return err
	})
	check("snapshot in seconds", err, ErrInvalid, "1000000000000")
}

// The unique index on a customer-scoped policy name and the case-insensitive unique names.
func TestUniqueNamesAreCaseInsensitive(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	_, err := w.s.CreatePolicy(ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: w.custA.ID, Name: "Std"})
	ok(t, err)
	_, err = w.s.CreatePolicy(ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: w.custA.ID, Name: "STD"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate policy name for one customer = %v, want ErrConflict", err)
	}
	// The same name for a different customer is fine.
	_, err = w.s.CreatePolicy(ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: w.custB.ID, Name: "Std"})
	ok(t, err)
	_, err = w.s.CreateStorageTarget(ctx, StorageTarget{Name: "EXAMPLE STORAGE", Provider: "wasabi", Endpoint: "e", Bucket: "b", GatewayURL: "g", CredentialRef: "C"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate storage target name = %v", err)
	}
}

// M2-W1B: a device cannot be retired onto a replacement that is itself retired.
func TestRetireOntoARetiredDeviceIsRefused(t *testing.T) {
	w := seed(t)
	ctx := context.Background()
	gone := w.enroll("tok-a", "A-DESK-GONE").Device
	live := w.enroll("tok-a", "A-DESK-LIVE").Device
	must[Device](t)(w.s.UpdateDevice(ctx, ScopeAll(), gone.ID, DeviceUpdate{Retire: &RetireSpec{Reason: "old"}}))
	_, err := w.s.UpdateDevice(ctx, ScopeAll(), w.devA.ID, DeviceUpdate{Retire: &RetireSpec{Reason: "replaced", ReplacedByDeviceID: gone.ID}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("retire onto a retired device = %v, want ErrInvalid", err)
	}
	cur := must[Device](t)(w.s.GetDevice(ctx, ScopeAll(), w.devA.ID))
	if cur.RetiredAt != nil {
		t.Fatalf("a refused retire left the device retired: %+v", cur)
	}
	// Control: a live replacement is accepted.
	ret := must[Device](t)(w.s.UpdateDevice(ctx, ScopeAll(), w.devA.ID, DeviceUpdate{Retire: &RetireSpec{Reason: "replaced", ReplacedByDeviceID: live.ID}}))
	if ret.RetiredAt == nil || ret.ReplacedByDeviceID != live.ID {
		t.Fatalf("retired = %+v", ret)
	}
}
