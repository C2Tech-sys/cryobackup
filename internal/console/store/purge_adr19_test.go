package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ADR 0019: purge of a retired device and delete of a customer, the database side.

// adr19Fresh holds the customer a matrix prep made for the call that follows it.
var adr19Fresh = map[*target]string{}

// addADR19Matrix adds the ADR 0019 methods to the §3.5 scope matrix (matrix() in scope_test.go calls
// it). Every one of them is owner-level, so a scoped caller is ErrScope and an all-customer caller
// succeeds on a row made by prep; the one read, ListDevicesPurging, follows the scope.
func addADR19Matrix(m map[string]matrixCase) {
	prepPurging := func(request bool) func(w *world, t *target) {
		return func(w *world, t *target) {
			tok := map[string]string{w.custA.ID: "tok-a", w.custB.ID: "tok-b"}[t.cust]
			dev := w.enroll(tok, uniq("PURGE-ME")).Device.ID
			adr19Retire(w, dev)
			if request {
				must[PurgeRequestResult](w.t)(w.s.RequestDevicePurge(w.ctx, ScopeAll(), dev, "matrix", adr19Owner(w)))
			}
			t.freshDevice = dev
		}
	}
	m["RequestDevicePurge"] = matrixCase{kind: globalWrite, prep: prepPurging(false),
		call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.RequestDevicePurge(w.ctx, sc, t.freshDevice, "matrix", adr19Owner(w))
			return nil, err
		}}
	m["RecordDevicePurgeAttempt"] = matrixCase{kind: globalWrite, prep: prepPurging(true),
		call: func(w *world, sc Scope, t *target) ([]string, error) {
			return nil, w.s.RecordDevicePurgeAttempt(w.ctx, sc, t.freshDevice, "storage_unreachable")
		}}
	m["CompleteDevicePurge"] = matrixCase{kind: globalWrite, prep: prepPurging(true),
		call: func(w *world, sc Scope, t *target) ([]string, error) {
			return nil, w.s.CompleteDevicePurge(w.ctx, sc, t.freshDevice, 3, 100)
		}}
	m["ListDevicesPurging"] = matrixCase{kind: list,
		prep: func(w *world, _ *target) { // called once: a purging device for each customer
			for _, c := range []struct{ cust, tok string }{{w.custA.ID, "tok-a"}, {w.custB.ID, "tok-b"}} {
				dev := w.enroll(c.tok, uniq("PURGING")).Device.ID
				adr19Retire(w, dev)
				must[PurgeRequestResult](w.t)(w.s.RequestDevicePurge(w.ctx, ScopeAll(), dev, "matrix", adr19Owner(w)))
			}
		},
		call: func(w *world, sc Scope, t *target) ([]string, error) {
			devs, err := w.s.ListDevicesPurging(w.ctx, sc)
			var rows []string
			for _, d := range devs {
				rows = append(rows, d.CustomerID)
			}
			return rows, err
		}}
	m["DeleteCustomer"] = matrixCase{kind: globalWrite,
		prep: func(w *world, t *target) {
			adr19Fresh[t] = must[Customer](w.t)(w.s.CreateCustomer(w.ctx, ScopeAll(), Customer{Name: uniq("Empty Customer")})).ID
		},
		call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.DeleteCustomer(w.ctx, sc, adr19Fresh[t], "matrix", adr19Owner(w))
			return nil, err
		}}
}

func adr19Owner(w *world) AuditActor {
	return AuditActor{AdminID: w.admin.ID, Username: w.admin.Username, IP: "192.0.2.10", Method: "api"}
}

// adr19Retire retires a device (the precondition of a purge).
func adr19Retire(w *world, dev string) {
	w.t.Helper()
	must[RetireResult](w.t)(w.s.RetireDeviceAudited(w.ctx, ScopeAll(), dev, RetireRequest{Reason: "decommissioned"}, adr19Owner(w)))
}

func adr19Exec(w *world, q string, args ...any) {
	w.t.Helper()
	ok(w.t, w.s.Write(w.ctx, func(tx *Tx) error { _, err := tx.Exec(q, args...); return err }))
}

func adr19Device(w *world, id string) Device {
	w.t.Helper()
	return must[Device](w.t)(w.s.GetDevice(w.ctx, ScopeAll(), id))
}

func adr19Audit(w *world, action, objectID string) []AuditEvent {
	return auditRows(w.t, w.s, action, objectID)
}

// A purge request marks a retired device, deletes nothing, and writes one audit row without a host name.
func TestADR19RequestPurgeMarksARetiredDeviceAndAuditsOnce(t *testing.T) {
	w := seed(t)
	dev := w.devA.ID
	adr19Retire(w, dev)
	w.clk.Advance(time.Hour)
	res := must[PurgeRequestResult](t)(w.s.RequestDevicePurge(w.ctx, ScopeAll(), dev, "  lab machine gone  ", adr19Owner(w)))
	d := res.Device
	if !res.Changed || !d.Purging() || d.PurgeRequestedAt == nil || !d.PurgeRequestedAt.Equal(w.clk.Now()) ||
		d.PurgeRequestedBy != w.admin.ID || d.PurgeReason != "lab machine gone" || d.PurgeLastErrorClass != "" || d.PurgeLastAttemptAt != nil {
		t.Fatalf("after the request: %+v", d)
	}
	if got := adr19Device(w, dev); !got.Purging() || got.RetiredAt == nil {
		t.Fatalf("the stored device is not purging and retired: %+v", got)
	}
	rows := adr19Audit(w, "device.purge.request", dev)
	if len(rows) != 1 || rows[0].Severity != "warning" || rows[0].CustomerID != w.custA.ID || rows[0].ActorID != w.admin.ID {
		t.Fatalf("audit rows = %+v", rows)
	}
	if body := string(rows[0].Details); !strings.Contains(body, "lab machine gone") || strings.Contains(body, w.devA.Hostname) {
		t.Fatalf("audit details %s: want the reason and no host name", body)
	}
	if n := countWhere(w, `SELECT COUNT(*) FROM runs WHERE device_id = ?`, dev) + countWhere(w, `SELECT COUNT(*) FROM repo_key_escrow WHERE device_id = ?`, dev); n == 0 {
		t.Fatal("setup: the device has no rows left to protect")
	}
}

// A request on a device that is already purging changes nothing and writes no second audit row.
func TestADR19RequestPurgeIsIdempotent(t *testing.T) {
	w := seed(t)
	dev := w.devA.ID
	adr19Retire(w, dev)
	first := must[PurgeRequestResult](t)(w.s.RequestDevicePurge(w.ctx, ScopeAll(), dev, "first reason", adr19Owner(w)))
	w.clk.Advance(time.Hour)
	again := must[PurgeRequestResult](t)(w.s.RequestDevicePurge(w.ctx, ScopeAll(), dev, "second reason", adr19Owner(w)))
	if again.Changed || again.Device.PurgeReason != "first reason" || !again.Device.PurgeRequestedAt.Equal(*first.Device.PurgeRequestedAt) ||
		!again.Device.UpdatedAt.Equal(first.Device.UpdatedAt) {
		t.Fatalf("second request: changed=%v %+v", again.Changed, again.Device)
	}
	if n := len(adr19Audit(w, "device.purge.request", dev)); n != 1 {
		t.Fatalf("%d audit rows after two requests, want 1", n)
	}
}

// Only a retired device can be purged; the refusal is typed and changes nothing.
func TestADR19RequestPurgeRefusesAnActiveDevice(t *testing.T) {
	w := seed(t)
	before := adr19Device(w, w.devA.ID)
	_, err := w.s.RequestDevicePurge(w.ctx, ScopeAll(), w.devA.ID, "no", adr19Owner(w))
	if !errors.Is(err, ErrDeviceNotRetired) || !errors.Is(err, ErrConflict) {
		t.Fatalf("active device: %v, want ErrDeviceNotRetired (a conflict)", err)
	}
	if after := adr19Device(w, w.devA.ID); after.Purging() || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("a refused request changed the device: %+v", after)
	}
	if n := auditTotal(t, w.s); n != countWhere(w, `SELECT COUNT(*) FROM audit_events`) || len(adr19Audit(w, "device.purge.request", "")) != 0 {
		t.Fatal("a refused request wrote an audit row")
	}
	if _, err := w.s.RequestDevicePurge(w.ctx, ScopeAll(), "01JNOSUCHDEVICE0000000000", "x", adr19Owner(w)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown device: %v", err)
	}
	// The schema says the same below the store: an active device cannot carry the mark.
	if err := w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE devices SET purge_requested_at = ? WHERE id = ?`, w.clk.Now().UnixMilli(), w.devA.ID)
		return mapErr(err)
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("the schema accepted a purge mark on an active device: %v", err)
	}
}

// The owner role is re-asserted in the store: an admin, a disabled owner, an unknown account and a scoped caller are refused, and nothing is written.
func TestADR19DestructiveCallsNeedAnEnabledOwnerAndScopeAll(t *testing.T) {
	w := seed(t)
	dev := w.devA.ID
	adr19Retire(w, dev)
	adminRole := must[Admin](t)(w.s.CreateAdmin(w.ctx, Admin{Username: "second", Email: "second@example.invalid", Role: "admin", PasswordHash: "$argon2id$test"}))
	spare := must[Admin](t)(w.s.CreateAdmin(w.ctx, Admin{Username: "spare", Email: "spare@example.invalid", Role: "owner", PasswordHash: "$argon2id$test"}))
	ok(t, w.s.DisableAdmin(w.ctx, spare.ID))
	empty := must[Customer](t)(w.s.CreateCustomer(w.ctx, ScopeAll(), Customer{Name: "Empty Co"}))
	before := countWhere(w, `SELECT COUNT(*) FROM audit_events`)
	for name, actor := range map[string]AuditActor{
		"admin role":     {AdminID: adminRole.ID, Username: "second", Method: "api"},
		"disabled owner": {AdminID: spare.ID, Username: "spare", Method: "api"},
		"unknown":        {AdminID: "01JNOSUCHADMIN00000000000", Username: "ghost", Method: "api"},
		"no id":          {Username: "nobody", Method: "api"},
	} {
		if _, err := w.s.RequestDevicePurge(w.ctx, ScopeAll(), dev, "x", actor); !errors.Is(err, ErrOwnerRequired) || !errors.Is(err, ErrScope) {
			t.Errorf("%s: RequestDevicePurge = %v, want ErrOwnerRequired (an ErrScope)", name, err)
		}
		if _, err := w.s.DeleteCustomer(w.ctx, ScopeAll(), empty.ID, "x", actor); !errors.Is(err, ErrOwnerRequired) {
			t.Errorf("%s: DeleteCustomer = %v, want ErrOwnerRequired", name, err)
		}
	}
	for name, sc := range map[string]Scope{"scoped": ScopeOf(w.custA.ID), "empty": ScopeOf(), "zero": {}} {
		if _, err := w.s.RequestDevicePurge(w.ctx, sc, dev, "x", adr19Owner(w)); !errors.Is(err, ErrScope) {
			t.Errorf("%s scope: RequestDevicePurge = %v, want ErrScope", name, err)
		}
		if _, err := w.s.DeleteCustomer(w.ctx, sc, empty.ID, "x", adr19Owner(w)); !errors.Is(err, ErrScope) {
			t.Errorf("%s scope: DeleteCustomer = %v, want ErrScope", name, err)
		}
		if err := w.s.CompleteDevicePurge(w.ctx, sc, dev, 0, 0); !errors.Is(err, ErrScope) {
			t.Errorf("%s scope: CompleteDevicePurge = %v, want ErrScope", name, err)
		}
		if err := w.s.RecordDevicePurgeAttempt(w.ctx, sc, dev, ""); !errors.Is(err, ErrScope) {
			t.Errorf("%s scope: RecordDevicePurgeAttempt = %v, want ErrScope", name, err)
		}
	}
	if adr19Device(w, dev).Purging() || countWhere(w, `SELECT COUNT(*) FROM customers WHERE id = ?`, empty.ID) != 1 ||
		countWhere(w, `SELECT COUNT(*) FROM audit_events`) != before {
		t.Fatal("a refused call changed data or wrote an audit row")
	}
}

func TestADR19RequestPurgeNeedsAReasonWithoutPaths(t *testing.T) {
	w := seed(t)
	adr19Retire(w, w.devA.ID)
	for name, reason := range map[string]string{"empty": "  ", "path": `cleaning C:\Users\x`, "multi-line": "a\nb", "long": strings.Repeat("x", 501)} {
		_, err := w.s.RequestDevicePurge(w.ctx, ScopeAll(), w.devA.ID, reason, adr19Owner(w))
		if f := fieldsOf(err); f["reason"] == "" {
			t.Errorf("%s: err = %v, want a validation error on reason", name, err)
		}
	}
	if adr19Device(w, w.devA.ID).Purging() {
		t.Fatal("a rejected reason still marked the device")
	}
}

// A request that cannot be audited is refused whole.
func TestADR19RequestPurgeIsRefusedWhenItCannotBeAudited(t *testing.T) {
	w := seed(t)
	adr19Retire(w, w.devA.ID)
	refuseAudits(t, w.s)
	if _, err := w.s.RequestDevicePurge(w.ctx, ScopeAll(), w.devA.ID, "x", adr19Owner(w)); !errors.Is(err, ErrAudit) {
		t.Fatalf("want ErrAudit, got %v", err)
	}
	if adr19Device(w, w.devA.ID).Purging() {
		t.Fatal("the device was marked although the audit row was refused")
	}
}

func TestADR19RecordAttemptSetsAndClearsTheErrorClass(t *testing.T) {
	w := seed(t)
	dev := w.devA.ID
	if err := w.s.RecordDevicePurgeAttempt(w.ctx, ScopeAll(), dev, "x"); !errors.Is(err, ErrNotPurging) || !errors.Is(err, ErrConflict) {
		t.Fatalf("a device with no purge request: %v, want ErrNotPurging", err)
	}
	adr19Retire(w, dev)
	if err := w.s.RecordDevicePurgeAttempt(w.ctx, ScopeAll(), dev, "x"); !errors.Is(err, ErrNotPurging) {
		t.Fatalf("a retired device with no purge request: %v, want ErrNotPurging", err)
	}
	must[PurgeRequestResult](t)(w.s.RequestDevicePurge(w.ctx, ScopeAll(), dev, "x", adr19Owner(w)))
	w.clk.Advance(time.Minute)
	auditBefore := countWhere(w, `SELECT COUNT(*) FROM audit_events`)
	ok(t, w.s.RecordDevicePurgeAttempt(w.ctx, ScopeAll(), dev, "storage_unreachable"))
	d := adr19Device(w, dev)
	if d.PurgeLastErrorClass != "storage_unreachable" || d.PurgeLastAttemptAt == nil || !d.PurgeLastAttemptAt.Equal(w.clk.Now()) || !d.Purging() {
		t.Fatalf("after a failed attempt: %+v", d)
	}
	w.clk.Advance(time.Minute)
	ok(t, w.s.RecordDevicePurgeAttempt(w.ctx, ScopeAll(), dev, ""))
	d = adr19Device(w, dev)
	if d.PurgeLastErrorClass != "" || d.PurgeLastAttemptAt == nil || !d.PurgeLastAttemptAt.Equal(w.clk.Now()) {
		t.Fatalf("an empty class must clear the failure and still record the attempt: %+v", d)
	}
	for _, bad := range []string{"Storage Unreachable", `C:\Users\x`, "a/b", "x y", strings.Repeat("a", 65), "9lives", "with-dash"} {
		if err := w.s.RecordDevicePurgeAttempt(w.ctx, ScopeAll(), dev, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("class %q: %v, want ErrInvalid (a class is a word, never a message)", bad, err)
		}
	}
	if n := countWhere(w, `SELECT COUNT(*) FROM audit_events`); n != auditBefore { // attempts are the job's bookkeeping, repeated on every retry
		t.Fatalf("attempts wrote %d audit rows", n-auditBefore)
	}
}

func TestADR19ListDevicesPurgingIsTheJobsWorkList(t *testing.T) {
	w := seed(t)
	if l := must[[]Device](t)(w.s.ListDevicesPurging(w.ctx, ScopeAll())); len(l) != 0 {
		t.Fatalf("nothing is purging yet: %d", len(l))
	}
	if _, err := w.s.ListDevicesPurging(w.ctx, Scope{}); !errors.Is(err, ErrScope) {
		t.Fatalf("zero scope: %v", err)
	}
	b2 := w.enroll("tok-b", "B-DESK-2").Device.ID
	adr19Retire(w, w.devA.ID)
	adr19Retire(w, b2)
	adr19Retire(w, w.enroll("tok-a", "A-DESK-9").Device.ID) // retired but not requested, and devB (active): neither is listed
	must[PurgeRequestResult](t)(w.s.RequestDevicePurge(w.ctx, ScopeAll(), b2, "later", adr19Owner(w)))
	w.clk.Advance(time.Minute)
	must[PurgeRequestResult](t)(w.s.RequestDevicePurge(w.ctx, ScopeAll(), w.devA.ID, "earlier request id, later time", adr19Owner(w)))
	l := must[[]Device](t)(w.s.ListDevicesPurging(w.ctx, ScopeAll()))
	if len(l) != 2 || l[0].ID != b2 || l[1].ID != w.devA.ID {
		t.Fatalf("work list = %v, want [%s %s] oldest request first", ids(l), b2, w.devA.ID)
	}
	if l := must[[]Device](t)(w.s.ListDevicesPurging(w.ctx, ScopeOf(w.custA.ID))); len(l) != 1 || l[0].ID != w.devA.ID {
		t.Fatalf("scoped list = %v", ids(l))
	}
}

func ids(ds []Device) []string {
	var out []string
	for _, d := range ds {
		out = append(out, d.ID)
	}
	return out
}

// adr19Refs lists every (table, column, on_delete) that points at devices.id, read from the live schema.
func adr19Refs(w *world, parent string) []string {
	w.t.Helper()
	var out []string
	ok(w.t, w.s.Read(w.ctx, func(tx *Tx) error {
		rows, err := tx.Query(`SELECT m.name, f."from", f.on_delete FROM sqlite_master m JOIN pragma_foreign_key_list(m.name) f
			WHERE m.type = 'table' AND f."table" = ? ORDER BY m.name, f."from"`, parent)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var tb, col, del string
			if err := rows.Scan(&tb, &col, &del); err != nil {
				return err
			}
			out = append(out, tb+"."+col+" "+del)
		}
		return rows.Err()
	}))
	return out
}

// adr19Counts is, per referencing column, how many rows point at the id.
func adr19Counts(w *world, parent, id string) map[string]int {
	w.t.Helper()
	out := map[string]int{}
	for _, r := range adr19Refs(w, parent) {
		tc := strings.Fields(r)[0]
		parts := strings.SplitN(tc, ".", 2)
		out[tc] = countWhere(w, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s = ?`, parts[0], parts[1]), id)
	}
	return out
}

// Completing a purge deletes the device row, the foreign keys carry it through everything the device
// owns, another device's rows are untouched, and one audit row says what was removed.
func TestADR19CompletePurgeDeletesTheDeviceAndEverythingItOwnsAndNothingElse(t *testing.T) {
	w := seed(t)
	dev, other := w.devA.ID, w.devB.ID
	ctx := w.ctx

	// Give both devices one row in every table that references a device.
	seedRows := func(d string, n int) {
		w.heartbeat(d)
		ingest(t, w, d, runInput(fmt.Sprintf("01J9Y4K3A0B1C2D3E4F5G6H7K%d", n), t0.Add(-time.Hour)))
		snap := strings.Repeat(fmt.Sprint(n), 64)
		must2(t)(w.s.AgentReplaceSnapshots(ctx, d, []Snapshot{{SnapshotID: snap, TakenAt: t0.Add(-time.Hour), Paths: []string{`C:\Users`}}}))
		must[RestoreRequest](t)(w.s.CreateRestoreRequest(ctx, ScopeAll(), RestoreRequest{DeviceID: d, SnapshotID: snap, Mode: "download_zip", ExpiresAt: t0.Add(time.Hour)}))
		_, _, err := w.s.EnqueueCommand(ctx, ScopeAll(), d, CommandInput{Type: "check_in", IdempotencyKey: "adr19-" + d})
		ok(t, err)
		adr19Exec(w, `INSERT INTO usage_samples (device_id, sampled_at, repo_bytes, source, created_at, updated_at) VALUES (?, 1700000000000, 5, 'restic_stats', 1, 1)`, d)
		adr19Exec(w, `INSERT OR IGNORE INTO maintenance_schedule (device_id, created_at, updated_at) VALUES (?, 1, 1)`, d)
		adr19Exec(w, `INSERT INTO start_leases (run_id, device_id, storage_target_id, granted_at, lease_until, created_at, updated_at) VALUES (?,?,?,1700000000000,1700000900000,1,1)`,
			fmt.Sprintf("01J9Y4K3A0B1C2D3E4F5G6HL%02d", n), d, w.target.ID)
		adr19Exec(w, `INSERT INTO alarm_acks (id, subject_type, device_id, alarm_type, acknowledged_at, created_at, updated_at) VALUES (?, 'device', ?, 'stale', 1, 1, 1)`,
			fmt.Sprintf("01J9Y4K3A0B1C2D3E4F5G6HA%02d", n), d)
		adr19Exec(w, `INSERT INTO alarm_episodes (id, subject_type, device_id, alarm_type, opened_at, state_since, created_at, updated_at) VALUES (?, 'device', ?, 'FAILED', 1700000000000, 1700000000000, 1, 1)`,
			fmt.Sprintf("01J9Y4K3A0B1C2D3E4F5G6HE%02d", n), d)
		adr19Exec(w, `INSERT INTO notification_rules (id, scope, device_id, name, channel, destination, created_at, updated_at) VALUES (?, 'device', ?, 'r', 'email', 'ops@example.invalid', 1, 1)`,
			fmt.Sprintf("01J9Y4K3A0B1C2D3E4F5G6HN%02d", n), d)
		adr19Exec(w, `INSERT INTO device_policy_overrides (id, device_id, patch, reason, created_at, updated_at) VALUES (?, ?, '{}', 'big first backup', 1, 1)`,
			fmt.Sprintf("01J9Y4K3A0B1C2D3E4F5G6HO%02d", n), d)
		adr19Exec(w, `INSERT INTO enrollment_tokens (id, customer_id, token_hash, adopts_device_id, max_uses, expires_at, created_at, updated_at)
			VALUES (?, (SELECT customer_id FROM devices WHERE id = ?), ?, ?, 1, 1900000000000, 1, 1)`,
			fmt.Sprintf("01J9Y4K3A0B1C2D3E4F5G6HT%02d", n), d, sha256Hex("adopt"+d), d)
		adr19Exec(w, `INSERT INTO enrollment_requests (id, request_nonce, hostname, status, device_id, expires_at, created_at, updated_at) VALUES (?, ?, 'LOBBY-PC', 'pending', ?, 1900000000000, 1, 1)`,
			fmt.Sprintf("01J9Y4K3A0B1C2D3E4F5G6HQ%02d", n), "nonce-"+d, d)
	}
	seedRows(dev, 1)
	seedRows(other, 2)
	// A third device that the purged one replaced, with a relay restore aimed at it.
	third := w.enroll("tok-a", "A-DESK-3").Device.ID
	must[RetireResult](t)(w.s.RetireDeviceAudited(ctx, ScopeAll(), third, RetireRequest{Reason: "replaced", ReplacedBy: dev}, adr19Owner(w)))
	adr19Exec(w, `INSERT INTO restore_requests (id, device_id, snapshot_id, target_device_id, mode, expires_at, created_at, updated_at) VALUES (?, ?, ?, ?, 'device_relay', 1900000000000, 1, 1)`,
		"01J9Y4K3A0B1C2D3E4F5G6HR01", third, strings.Repeat("c", 64), dev)

	// Every table that references devices is covered by the seeding above: a new one fails here until it is.
	seeded := map[string]bool{"enrollment_tokens.adopts_device_id": true, "devices.replaced_by_device_id": true, "device_tokens.device_id": true,
		"device_presence.device_id": true, "heartbeats.device_id": true, "enrollment_requests.device_id": true, "gateway_logins.device_id": true,
		"repo_key_escrow.device_id": true, "device_policy_overrides.device_id": true, "runs.device_id": true, "snapshots.device_id": true,
		"usage_samples.device_id": true, "maintenance_schedule.device_id": true, "commands.device_id": true, "restore_requests.device_id": true,
		"restore_requests.target_device_id": true, "alarm_acks.device_id": true, "notification_rules.device_id": true, "start_leases.device_id": true,
		"alarm_episodes.device_id": true}
	for _, r := range adr19Refs(w, "devices") {
		tc, rule, _ := strings.Cut(r, " ")
		if !seeded[tc] {
			t.Errorf("%s (%s) references devices and this test does not seed it: add it, and decide what a purge must do with it", tc, rule)
		}
		if rule != "CASCADE" && rule != "SET NULL" {
			t.Errorf("%s has ON DELETE %s: a purge cannot delete the device through it", tc, rule)
		}
	}

	before, otherBefore := adr19Counts(w, "devices", dev), adr19Counts(w, "devices", other)
	for tc := range seeded {
		if before[tc] == 0 {
			t.Errorf("setup: %s has no row for the device to be purged", tc)
		}
		if otherBefore[tc] == 0 && tc != "devices.replaced_by_device_id" && tc != "restore_requests.target_device_id" {
			t.Errorf("setup: %s has no row for the untouched device", tc)
		}
	}
	custBefore := countWhere(w, `SELECT COUNT(*) FROM customers`)
	auditBefore := countWhere(w, `SELECT COUNT(*) FROM audit_events`)
	adr19Retire(w, dev)

	// Not before a request: the row is safe from every path but this one.
	if err := w.s.CompleteDevicePurge(ctx, ScopeAll(), dev, 1, 1); !errors.Is(err, ErrNotPurging) {
		t.Fatalf("complete without a request: %v, want ErrNotPurging", err)
	}
	if countWhere(w, `SELECT COUNT(*) FROM devices WHERE id = ?`, dev) != 1 {
		t.Fatal("a purge that was never requested deleted the device")
	}
	must[PurgeRequestResult](t)(w.s.RequestDevicePurge(ctx, ScopeAll(), dev, "decommissioned", adr19Owner(w)))
	if err := w.s.CompleteDevicePurge(ctx, ScopeAll(), dev, -1, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative count: %v", err)
	}

	// A failed audit write refuses the whole change.
	refuseAudits(t, w.s)
	if err := w.s.CompleteDevicePurge(ctx, ScopeAll(), dev, 7, 1234); !errors.Is(err, ErrAudit) {
		t.Fatalf("complete without audit: %v, want ErrAudit", err)
	}
	if countWhere(w, `SELECT COUNT(*) FROM devices WHERE id = ?`, dev) != 1 {
		t.Fatal("the device was deleted although its audit row was refused")
	}
	adr19Exec(w, `DROP TRIGGER refuse_audits`)

	w.clk.Advance(time.Hour)
	ok(t, w.s.CompleteDevicePurge(ctx, ScopeAll(), dev, 7, 1234))
	if _, err := w.s.GetDevice(ctx, ScopeAll(), dev); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the purged device is still readable: %v", err)
	}
	if err := w.s.CompleteDevicePurge(ctx, ScopeAll(), dev, 7, 1234); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a second completion: %v, want ErrNotFound", err)
	}
	for tc, n := range adr19Counts(w, "devices", dev) {
		if n != 0 {
			t.Errorf("%s still has %d rows for the purged device", tc, n)
		}
	}
	after := adr19Counts(w, "devices", other)
	for tc, n := range otherBefore {
		if after[tc] != n {
			t.Errorf("%s: the other device went from %d rows to %d", tc, n, after[tc])
		}
	}
	if d := adr19Device(w, other); d.RetiredAt != nil || d.Purging() || d.Hostname != "B-DESK-1" {
		t.Fatalf("the other device changed: %+v", d)
	}
	// What the schema does by design: the device that was replaced loses only its link; a relay restore aimed at the purged device goes with it.
	if d := adr19Device(w, third); d.ReplacedByDeviceID != "" || d.RetiredAt == nil {
		t.Fatalf("the replaced device after the purge: %+v", d)
	}
	if countWhere(w, `SELECT COUNT(*) FROM restore_requests WHERE id = '01J9Y4K3A0B1C2D3E4F5G6HR01'`) != 0 {
		t.Fatal("a relay restore targeting the purged device survived")
	}
	if got := countWhere(w, `SELECT COUNT(*) FROM customers`); got != custBefore {
		t.Fatalf("customers %d -> %d", custBefore, got)
	}

	// Audit: nothing deleted, one row added, with ids and counts and no host name.
	if n := countWhere(w, `SELECT COUNT(*) FROM audit_events`); n != auditBefore+3 { // retire, request, done
		t.Fatalf("audit rows %d -> %d, want +3 (retire, request, done)", auditBefore, n)
	}
	rows := adr19Audit(w, "device.purge.done", dev)
	if len(rows) != 1 {
		t.Fatalf("%d done rows", len(rows))
	}
	e := rows[0]
	body := string(e.Details)
	if e.ActorType != "system" || e.Severity != "warning" || e.CustomerID != w.custA.ID || e.Result != "ok" ||
		!strings.Contains(body, `"objects":7`) || !strings.Contains(body, `"bytes":1234`) || !strings.Contains(body, dev) || !strings.Contains(body, w.custA.ID) ||
		strings.Contains(body, w.devA.Hostname) || strings.Contains(e.ActorLabel, w.devA.Hostname) {
		t.Fatalf("done row: %+v details %s", e, body)
	}
	if n := len(adr19Audit(w, "device.retire", dev)) + len(adr19Audit(w, "device.purge.request", dev)); n != 2 {
		t.Fatalf("the earlier audit rows of the purged device: %d, want 2 (they must outlive it)", n)
	}
}

// The lobby rows of a purged device (enrollment_requests.device_id is SET NULL in the schema, so
// they would keep the host name and address) are deleted in the purge's own transaction; another
// device's and an unattached request stay, and a purge refused for its audit row deletes nothing.
func TestADR19CompletePurgeDeletesTheDevicesLobbyRows(t *testing.T) {
	w := seed(t)
	dev, other := w.devA.ID, w.devB.ID
	lobby := func(id, d, host string) {
		adr19Exec(w, `INSERT INTO enrollment_requests (id, request_nonce, hostname, source_ip, status, customer_id, device_id, decided_at, expires_at, created_at, updated_at)
			VALUES (?, ?, ?, '192.0.2.44', 'approved', (SELECT customer_id FROM devices WHERE id = ?), ?, 1, 1900000000000, 1, 1)`, id, "n-"+id, host, d, d)
	}
	lobby("01J9Y4K3A0B1C2D3E4F5G6HQ11", dev, "PURGED-HOST-1")
	lobby("01J9Y4K3A0B1C2D3E4F5G6HQ12", dev, "PURGED-HOST-2")
	lobby("01J9Y4K3A0B1C2D3E4F5G6HQ13", other, "KEPT-HOST")
	adr19Exec(w, `INSERT INTO enrollment_requests (id, request_nonce, hostname, status, expires_at, created_at, updated_at)
		VALUES ('01J9Y4K3A0B1C2D3E4F5G6HQ14', 'n-14', 'PENDING-HOST', 'pending', 1900000000000, 1, 1)`)
	purged := func() int {
		return countWhere(w, `SELECT COUNT(*) FROM enrollment_requests WHERE hostname LIKE 'PURGED-HOST-%'`)
	}
	adr19Retire(w, dev)
	must[PurgeRequestResult](t)(w.s.RequestDevicePurge(w.ctx, ScopeAll(), dev, "decommissioned", adr19Owner(w)))

	refuseAudits(t, w.s)
	if err := w.s.CompleteDevicePurge(w.ctx, ScopeAll(), dev, 1, 1); !errors.Is(err, ErrAudit) {
		t.Fatalf("complete without audit: %v, want ErrAudit", err)
	}
	if n := purged(); n != 2 {
		t.Fatalf("a refused purge deleted lobby rows: %d left, want 2", n)
	}
	adr19Exec(w, `DROP TRIGGER refuse_audits`)

	ok(t, w.s.CompleteDevicePurge(w.ctx, ScopeAll(), dev, 1, 1))
	if n := purged(); n != 0 {
		t.Fatalf("%d lobby rows of the purged device survived it (with their host names)", n)
	}
	if n := countWhere(w, `SELECT COUNT(*) FROM enrollment_requests WHERE hostname = 'KEPT-HOST' AND device_id = ?`, other); n != 1 {
		t.Fatalf("another device's lobby row: %d, want 1", n)
	}
	if n := countWhere(w, `SELECT COUNT(*) FROM enrollment_requests WHERE hostname = 'PENDING-HOST'`); n != 1 {
		t.Fatalf("an unattached lobby row: %d, want 1", n)
	}
}

// DeleteCustomer refuses while any device exists, with counts, and touches nothing.
func TestADR19DeleteCustomerRefusesWhileDevicesExist(t *testing.T) {
	w := seed(t)
	r := w.enroll("tok-a", "A-DESK-2").Device.ID
	adr19Retire(w, r)
	_, err := w.s.DeleteCustomer(w.ctx, ScopeAll(), w.custA.ID, "closing", adr19Owner(w))
	var he *CustomerHasDevicesError
	if !errors.As(err, &he) || he.Active != 1 || he.Retired != 1 || !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want a CustomerHasDevicesError with 1 active and 1 retired", err)
	}
	if strings.Contains(err.Error(), "A-DESK") {
		t.Fatalf("the refusal names a device: %v", err)
	}
	// A purging device still counts; only the completed purge frees the customer.
	adr19Retire(w, w.devA.ID)
	must[PurgeRequestResult](t)(w.s.RequestDevicePurge(w.ctx, ScopeAll(), w.devA.ID, "x", adr19Owner(w)))
	must[PurgeRequestResult](t)(w.s.RequestDevicePurge(w.ctx, ScopeAll(), r, "x", adr19Owner(w)))
	before := countWhere(w, `SELECT COUNT(*) FROM audit_events`)
	if _, err := w.s.DeleteCustomer(w.ctx, ScopeAll(), w.custA.ID, "closing", adr19Owner(w)); !errors.As(err, &he) || he.Active != 0 || he.Retired != 2 {
		t.Fatalf("with two purging devices: %v", err)
	}
	if countWhere(w, `SELECT COUNT(*) FROM customers WHERE id = ?`, w.custA.ID) != 1 || countWhere(w, `SELECT COUNT(*) FROM audit_events`) != before || len(adr19Audit(w, "customer.delete", "")) != 0 {
		t.Fatal("a refused delete changed data or wrote a customer.delete row")
	}
	// The road a customer with history takes: retire, purge, delete.
	ok(t, w.s.CompleteDevicePurge(w.ctx, ScopeAll(), w.devA.ID, 1, 1))
	ok(t, w.s.CompleteDevicePurge(w.ctx, ScopeAll(), r, 1, 1))
	res := must[CustomerDeleteResult](t)(w.s.DeleteCustomer(w.ctx, ScopeAll(), w.custA.ID, "closing", adr19Owner(w)))
	if res.CustomerID != w.custA.ID || res.Name != "Alpha Dental" || res.TokensDeleted != 1 {
		t.Fatalf("result = %+v", res)
	}
	if _, err := w.s.GetCustomer(w.ctx, ScopeAll(), w.custA.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the customer is still there: %v", err)
	}
}

// What DeleteCustomer does to every foreign key that points at customers, with one audit row.
func TestADR19DeleteCustomerCascadesItsOwnRowsAndNothingElse(t *testing.T) {
	w := seed(t)
	ctx := w.ctx
	c := must[Customer](t)(w.s.CreateCustomer(ctx, ScopeAll(), Customer{Name: "Closing Co"}))
	custPolicy := must[PolicyRecord](t)(w.s.CreatePolicy(ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: c.ID, Name: "Closing policy", Sections: sections("schedule", `{"interval_minutes":700}`)}))
	adr19Exec(w, `UPDATE customers SET policy_id = ? WHERE id = ?`, custPolicy.ID, c.ID) // RESTRICT from customers to its own policy
	must[EnrollmentToken](t)(w.s.CreateEnrollmentToken(ctx, ScopeAll(), EnrollmentToken{CustomerID: c.ID, TokenHash: sha256Hex("closing-1"), ExpiresAt: t0.Add(time.Hour)}))
	must[EnrollmentToken](t)(w.s.CreateEnrollmentToken(ctx, ScopeAll(), EnrollmentToken{CustomerID: c.ID, TokenHash: sha256Hex("closing-2"), ExpiresAt: t0.Add(time.Hour)}))
	adr19Exec(w, `INSERT INTO alarm_acks (id, subject_type, customer_id, alarm_type, acknowledged_at, created_at, updated_at) VALUES ('01J9Y4K3A0B1C2D3E4F5G6HA90', 'customer', ?, 'missing_customer', 1, 1, 1)`, c.ID)
	adr19Exec(w, `INSERT INTO notification_rules (id, scope, customer_id, name, channel, destination, created_at, updated_at) VALUES ('01J9Y4K3A0B1C2D3E4F5G6HN90', 'customer', ?, 'r', 'email', 'ops@example.invalid', 1, 1)`, c.ID)
	adr19Exec(w, `INSERT INTO reports (id, name, scope, customer_id, kind, format, created_at, updated_at) VALUES ('01J9Y4K3A0B1C2D3E4F5G6HP90', 'monthly', 'customer', ?, 'customer_status', 'pdf', 1, 1)`, c.ID)
	// An approved lobby request names the customer; the schema's SET NULL would violate its CHECK.
	adr19Exec(w, `INSERT INTO enrollment_requests (id, request_nonce, hostname, status, customer_id, decided_at, expires_at, created_at, updated_at) VALUES ('01J9Y4K3A0B1C2D3E4F5G6HQ90', 'n90', 'LOBBY-PC', 'approved', ?, 1, 1900000000000, 1, 1)`, c.ID)
	// Rows of other customers that must survive.
	must[PolicyRecord](t)(w.s.CreatePolicy(ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: w.custB.ID, Name: "Bravo policy", Sections: sections("schedule", `{"interval_minutes":650}`)}))
	keep := map[string]int{
		"tokens":    countWhere(w, `SELECT COUNT(*) FROM enrollment_tokens WHERE customer_id <> ?`, c.ID),
		"customers": countWhere(w, `SELECT COUNT(*) FROM customers WHERE id <> ?`, c.ID),
		"devices":   countWhere(w, `SELECT COUNT(*) FROM devices`),
		"default":   countWhere(w, `SELECT COUNT(*) FROM policies WHERE scope = 'default'`),
	}

	// Refused without an audit row: nothing changes.
	refuseAudits(t, w.s)
	if _, err := w.s.DeleteCustomer(ctx, ScopeAll(), c.ID, "closing", adr19Owner(w)); !errors.Is(err, ErrAudit) {
		t.Fatalf("delete without audit: %v, want ErrAudit", err)
	}
	for _, q := range []string{`SELECT COUNT(*) FROM customers WHERE id = '` + c.ID + `'`, `SELECT COUNT(*) FROM enrollment_tokens WHERE customer_id = '` + c.ID + `'`,
		`SELECT COUNT(*) FROM policies WHERE customer_id = '` + c.ID + `'`, `SELECT COUNT(*) FROM enrollment_requests WHERE customer_id = '` + c.ID + `'`} {
		if countWhere(w, q) == 0 {
			t.Fatalf("a refused delete removed rows: %s", q)
		}
	}
	adr19Exec(w, `DROP TRIGGER refuse_audits`)

	if _, err := w.s.DeleteCustomer(ctx, ScopeAll(), c.ID, "", adr19Owner(w)); fieldsOf(err)["reason"] == "" {
		t.Fatalf("an empty reason: %v", err)
	}
	if _, err := w.s.DeleteCustomer(ctx, ScopeAll(), "01JNOSUCHCUSTOMER000000000", "x", adr19Owner(w)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown customer: %v", err)
	}
	res := must[CustomerDeleteResult](t)(w.s.DeleteCustomer(ctx, ScopeAll(), c.ID, "contract ended", adr19Owner(w)))
	want := CustomerDeleteResult{CustomerID: c.ID, Name: "Closing Co", TokensDeleted: 2, PoliciesDeleted: 1, AlarmAcksDeleted: 1, NotificationRulesDeleted: 1, ReportsDeleted: 1, EnrollmentRequestsDeleted: 1}
	if res != want {
		t.Fatalf("result = %+v, want %+v", res, want)
	}
	for _, q := range []string{`customers WHERE id`, `enrollment_tokens WHERE customer_id`, `policies WHERE customer_id`, `alarm_acks WHERE customer_id`,
		`notification_rules WHERE customer_id`, `reports WHERE customer_id`, `enrollment_requests WHERE customer_id`} {
		if n := countWhere(w, `SELECT COUNT(*) FROM `+q+` = ?`, c.ID); n != 0 {
			t.Errorf("%s: %d rows left", q, n)
		}
	}
	if countWhere(w, `SELECT COUNT(*) FROM policy_revisions WHERE policy_id = ?`, custPolicy.ID) != 0 {
		t.Error("the deleted policy's revisions are still there")
	}
	// The others are untouched.
	if got := countWhere(w, `SELECT COUNT(*) FROM customers`); got != keep["customers"] {
		t.Errorf("customers = %d, want %d", got, keep["customers"])
	}
	if got := countWhere(w, `SELECT COUNT(*) FROM policies WHERE scope = 'default'`); got != keep["default"] {
		t.Errorf("default policies = %d", got)
	}
	if got := countWhere(w, `SELECT COUNT(*) FROM policies WHERE customer_id = ?`, w.custB.ID); got != 1 {
		t.Errorf("another customer's policy: %d rows", got)
	}
	if got := countWhere(w, `SELECT COUNT(*) FROM enrollment_tokens WHERE customer_id <> ?`, c.ID); got != keep["tokens"] {
		t.Errorf("tokens of other customers = %d, want %d", got, keep["tokens"])
	}
	if got := countWhere(w, `SELECT COUNT(*) FROM devices`); got != keep["devices"] {
		t.Errorf("devices = %d, want %d", got, keep["devices"])
	}
	rows := adr19Audit(w, "customer.delete", c.ID)
	if len(rows) != 1 {
		t.Fatalf("%d customer.delete rows, want 1", len(rows))
	}
	e := rows[0]
	body := string(e.Details)
	if e.Severity != "warning" || e.ActorID != w.admin.ID || e.CustomerID != c.ID || e.ObjectType != "customer" ||
		!strings.Contains(body, "contract ended") || !strings.Contains(body, `"tokens_deleted":2`) {
		t.Fatalf("audit row %+v details %s", e, body)
	}
	// Ids and counts only, as customer.create: the customer's name is not in the details.
	if strings.Contains(body, "Closing Co") || strings.Contains(body, `"name"`) {
		t.Fatalf("customer.delete details name the customer: %s", body)
	}
	if _, err := w.s.DeleteCustomer(ctx, ScopeAll(), c.ID, "again", adr19Owner(w)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a second delete: %v, want ErrNotFound", err)
	}
}

// A purging device is a retired device, so everything the store already refuses on a retired device it refuses on a purging one, and
// the maintenance worker's selection skips it. This pins the two reads the ADR needs: nothing can un-retire it, and it is never listed active.
func TestADR19APurgingDeviceIsSkippedByEverythingThatActs(t *testing.T) {
	w := seed(t)
	dev := w.devA.ID
	w.heartbeat(dev)
	adr19Retire(w, dev)
	must[PurgeRequestResult](t)(w.s.RequestDevicePurge(w.ctx, ScopeAll(), dev, "x", adr19Owner(w)))

	if _, err := w.s.QueueCommandAudited(w.ctx, ScopeAll(), dev, CommandInput{Type: CommandBackupNow, IdempotencyKey: "k-adr19b"}, adr19Owner(w)); !errors.Is(err, ErrDeviceRetired) {
		t.Errorf("QueueCommandAudited on a purging device: %v, want ErrDeviceRetired", err)
	}
	renamed := "renamed"
	if _, _, err := w.s.UpdateDeviceSettings(w.ctx, ScopeAll(), dev, DeviceSettingsPatch{FriendlyName: &renamed}, adr19Owner(w)); !errors.Is(err, ErrConflict) {
		t.Errorf("UpdateDeviceSettings on a purging device: %v, want a conflict", err)
	}
	if _, _, err := w.s.AgentLookupDeviceToken(w.ctx, sha256Hex("device-token-A-DESK-1")); !errors.Is(err, ErrNotFound) {
		t.Errorf("a purging device still authenticates: %v", err)
	}
	for _, d := range must[Listing[Device]](t)(w.s.ListDevices(w.ctx, ScopeAll(), ListDevicesOptions{Page: Page{Limit: 100}})).Items {
		if d.ID == dev {
			t.Error("a purging device is listed among the active ones")
		}
	}
	// The schema keeps it retired for as long as the purge stands.
	if err := w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE devices SET retired_at = NULL WHERE id = ?`, dev)
		return mapErr(err)
	}); !errors.Is(err, ErrInvalid) {
		t.Errorf("un-retiring a purging device: %v, want a CHECK refusal", err)
	}
}
