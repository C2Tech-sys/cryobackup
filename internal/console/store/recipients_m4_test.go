package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func m4Actor(w *world) AuditActor {
	return AuditActor{AdminID: w.admin.ID, Username: "root", Method: "api"}
}

// rawRule reads the notification_rules columns the three switches map onto.
func rawRule(t *testing.T, w *world, id string) (triggers string, notifyRecovery int, digest *string, minSeverity, scope, channel string, enabled int) {
	t.Helper()
	var d *string
	ok(t, w.s.Read(w.ctx, func(tx *Tx) error {
		return tx.QueryRow(`SELECT triggers, notify_recovery, digest_schedule, min_severity, scope, channel, enabled FROM notification_rules WHERE id = ?`, id).
			Scan(&triggers, &notifyRecovery, &d, &minSeverity, &scope, &channel, &enabled)
	}))
	return triggers, notifyRecovery, d, minSeverity, scope, channel, enabled
}

func TestM4RecipientCrudAndTheColumnMapping(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)

	r := must[Recipient](t)(w.s.CreateRecipient(w.ctx, all, RecipientInput{Name: "  On-call  ", Address: " oncall@example.test ", Alerts: true, Recoveries: true}, actor))
	if r.Name != "On-call" || r.Address != "oncall@example.test" || !r.Alerts || !r.Recoveries || r.Digest || r.ID == "" || r.CreatedAt.IsZero() {
		t.Fatalf("created = %+v", r)
	}
	trig, rec, dig, sev, scope, channel, enabled := rawRule(t, w, r.ID)
	if trig != `["alert"]` || rec != 1 || dig != nil || sev != "critical" || scope != "global" || channel != "email" || enabled != 1 {
		t.Fatalf("columns = triggers %s notify_recovery %d digest %v severity %s scope %s channel %s enabled %d", trig, rec, dig, sev, scope, channel, enabled)
	}

	d := must[Recipient](t)(w.s.CreateRecipient(w.ctx, all, RecipientInput{Name: "Digest only", Address: "digest@example.test", Digest: true}, actor))
	trig, rec, dig, _, _, _, _ = rawRule(t, w, d.ID)
	if trig != `[]` || rec != 0 || dig == nil || *dig != "daily" || d.Alerts || d.Recoveries || !d.Digest {
		t.Fatalf("digest-only = %+v columns %s %d %v", d, trig, rec, dig)
	}

	list := must[[]Recipient](t)(w.s.ListRecipients(w.ctx, all))
	if len(list) != 2 || list[0].Name != "Digest only" || list[1].Name != "On-call" {
		t.Fatalf("list (by name) = %+v", list)
	}
	got := must[Recipient](t)(w.s.GetRecipient(w.ctx, all, r.ID))
	if got.ID != r.ID || got.Address != r.Address {
		t.Fatalf("get = %+v", got)
	}

	// Update replaces name, address and switches.
	w.clk.Advance(time.Second)
	u := must[Recipient](t)(w.s.UpdateRecipient(w.ctx, all, r.ID, RecipientInput{Name: "Pager", Address: "pager@example.test", Alerts: false, Recoveries: false, Digest: true}, actor))
	if u.Name != "Pager" || u.Address != "pager@example.test" || u.Alerts || u.Recoveries || !u.Digest || !u.UpdatedAt.After(r.UpdatedAt) {
		t.Fatalf("updated = %+v", u)
	}
	trig, rec, dig, _, _, _, _ = rawRule(t, w, r.ID)
	if trig != `[]` || rec != 0 || dig == nil || *dig != "daily" {
		t.Fatalf("updated columns = %s %d %v", trig, rec, dig)
	}

	ok(t, w.s.DeleteRecipient(w.ctx, all, r.ID, actor))
	if _, err := w.s.GetRecipient(w.ctx, all, r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete = %v", err)
	}
	if err := w.s.DeleteRecipient(w.ctx, all, r.ID, actor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
	if _, err := w.s.UpdateRecipient(w.ctx, all, r.ID, RecipientInput{Name: "x", Address: "x@example.test"}, actor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update after delete = %v", err)
	}
}

// Every mutation writes one audit row, and the row names ids and switches only: not the address, not the name.
func TestM4RecipientAuditRowsCarryNoAddressAndNoName(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	const name, addr, addr2 = "Quirky Label Zed", "someone.private@example.test", "other.private@example.test"
	r := must[Recipient](t)(w.s.CreateRecipient(w.ctx, all, RecipientInput{Name: name, Address: addr, Alerts: true}, actor))
	must[Recipient](t)(w.s.UpdateRecipient(w.ctx, all, r.ID, RecipientInput{Name: name, Address: addr2, Alerts: true, Digest: true}, actor))
	ok(t, w.s.DeleteRecipient(w.ctx, all, r.ID, actor))
	for _, action := range []string{"recipient.create", "recipient.update", "recipient.delete"} {
		if n := auditCount(t, w, action); n != 1 {
			t.Errorf("%s rows = %d, want 1", action, n)
		}
	}
	var rows []string
	ok(t, w.s.Read(w.ctx, func(tx *Tx) error {
		var err error
		rows, err = collect(tx, `SELECT action || ' ' || COALESCE(object_id,'') || ' ' || COALESCE(actor_label,'') || ' ' || details FROM audit_events WHERE action LIKE 'recipient.%'`, nil,
			func(sc scanner) (string, error) { var s string; return s, sc.Scan(&s) })
		return err
	}))
	for _, row := range rows {
		for _, secret := range []string{name, "Quirky", addr, addr2, "private", "@"} {
			if strings.Contains(row, secret) {
				t.Errorf("audit row %q contains %q", row, secret)
			}
		}
		if !strings.Contains(row, r.ID) {
			t.Errorf("audit row %q lacks the rule id", row)
		}
	}
}

func TestM4RecipientValidation(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	bad := []string{
		"", "   ", "ops", "@example.test", "ops@", "ops@@example.test", "ops@example.test, other@example.test", "ops@example.test; other@example.test",
		"Ops Team <ops@example.test>", "<ops@example.test>", `"Ops" <ops@example.test>`, "ops@example.test (Ops)", `"a b"@example.test`,
		"ops@example.test\r\nBcc: spy@example.test", "ops@example.test\n", "\nops@example.test", "ops@example.test\r", "ops @example.test",
		"ops@exa mple.test", "pöps@example.test", "ops@exämple.test", "ops@example.test\x00", strings.Repeat("a", 65) + "@example.test",
		strings.Repeat("a", 60) + "@" + strings.Repeat("b", 190) + ".test", // 256 characters
		// P20-T2F item 5: the mail sender's rule. An address literal, a label that starts or ends with a hyphen, an empty
		// label, one label only, a quoted or misplaced-dot local part, and characters outside atext.
		"a@[127.0.0.1]", "a@[IPv6:::1]", "a@-example.test", "a@example-.test", "a@exa-.mple.test", "a@example..test", "a@.example.test",
		"a@example.test.", "a@localhost", "ops@localhost", "a@1", "a..b@example.test", ".a@example.test", "a.@example.test",
		"a(b)@example.test", "a,b@example.test", "a:b@example.test", "a\\b@example.test", "a[b]@example.test", "a@exa_mple.test",
		"a@exa!mple.test", "a@exa+mple.test", "a@example.te$t", "a@@",
	}
	// P20-T2F batch 2 item 1: a domain label is at most 63 octets (RFC 1035), as in the mail sender.
	b64, b63 := strings.Repeat("b", 64), strings.Repeat("b", 63)
	bad = append(bad, "a@"+b64+".test", "a@example."+b64, "a@"+b64+"."+b64, strings.Repeat("a", 58)+"@"+strings.Repeat("b", 190)+".test",
		strings.Repeat("a", 60)+"@"+strings.Repeat("b", 188)+".test") // 254 octets, one 188-octet label
	for _, a := range bad {
		_, err := w.s.CreateRecipient(w.ctx, all, RecipientInput{Name: "X", Address: a}, actor)
		fe := FieldErrors(err)
		if !errors.Is(err, ErrInvalid) || len(fe) != 1 || fe[0].Field != "address" {
			t.Errorf("address %q: err = %v, want a field error on address", a, err)
			continue
		}
		if len(strings.TrimSpace(a)) > 3 && strings.Contains(err.Error(), strings.TrimSpace(a)) {
			t.Errorf("address %q: the error echoes the input: %v", a, err)
		}
	}
	long := strings.Repeat("a", 60) + "@" + b63 + "." + b63 + "." + b63 + ".x" // exactly 254, every label within 63
	if len(long) != 254 {
		t.Fatalf("test address is %d long", len(long))
	}
	good := []string{"ops@example.test", "a.b+tag@sub.example.test", "O'Brien@example.test", "x@y.z", "a-b.c_d!#$%&'*+/=?^`{|}~@a-b.c1.example.test", "A@B.C", "a@" + b63 + ".test", long}
	for _, a := range good {
		if _, err := w.s.CreateRecipient(w.ctx, all, RecipientInput{Name: "G " + a[:3], Address: a}, actor); err != nil {
			t.Errorf("address %q refused: %v", a, err)
		}
	}
	for _, n := range []string{"", "  ", strings.Repeat("n", 101), "two\nlines", "tab\there", "bell\a"} {
		_, err := w.s.CreateRecipient(w.ctx, all, RecipientInput{Name: n, Address: uniq("ok") + "@example.test"}, actor)
		if fe := FieldErrors(err); !errors.Is(err, ErrInvalid) || len(fe) != 1 || fe[0].Field != "name" {
			t.Errorf("name %q: err = %v, want a field error on name", n, err)
		}
	}
	if _, ok := CleanMailAddress("  ops@example.test "); !ok {
		t.Error("surrounding spaces are trimmed")
	}
}

func TestM4RecipientAddressesAreUniqueWithoutRegardToCase(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	a := must[Recipient](t)(w.s.CreateRecipient(w.ctx, all, RecipientInput{Name: "A", Address: "ops@example.test"}, actor))
	if _, err := w.s.CreateRecipient(w.ctx, all, RecipientInput{Name: "B", Address: "OPS@Example.Test"}, actor); !errors.Is(err, ErrConflict) {
		t.Fatalf("same address in another case = %v, want ErrConflict", err)
	}
	b := must[Recipient](t)(w.s.CreateRecipient(w.ctx, all, RecipientInput{Name: "B", Address: "b@example.test"}, actor))
	if _, err := w.s.UpdateRecipient(w.ctx, all, b.ID, RecipientInput{Name: "B", Address: "ops@example.test"}, actor); !errors.Is(err, ErrConflict) {
		t.Fatalf("update onto A's address = %v, want ErrConflict", err)
	}
	// Saving a recipient with its own address is fine.
	must[Recipient](t)(w.s.UpdateRecipient(w.ctx, all, a.ID, RecipientInput{Name: "A2", Address: "ops@example.test", Alerts: true}, actor))
}

// Saving what is already stored writes nothing and no audit row.
func TestM4RecipientNoOpUpdateIsNotAudited(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	in := RecipientInput{Name: "A", Address: "a@example.test", Alerts: true, Digest: true}
	r := must[Recipient](t)(w.s.CreateRecipient(w.ctx, all, in, actor))
	w.clk.Advance(time.Second)
	u := must[Recipient](t)(w.s.UpdateRecipient(w.ctx, all, r.ID, in, actor))
	if !u.UpdatedAt.Equal(r.UpdatedAt) || auditCount(t, w, "recipient.update") != 0 {
		t.Fatalf("a no-op update changed the row or was audited (%v vs %v, %d rows)", u.UpdatedAt, r.UpdatedAt, auditCount(t, w, "recipient.update"))
	}
}

// Rules this file does not own are invisible: a per-device rule, a webhook; and a rule disabled
// outside the console reads with every switch off.
func TestM4RecipientsSeeOnlyGlobalEmailRules(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
		now := tx.nowMS()
		for _, q := range []struct{ id, scope, channel, dev string }{
			{"01JRULEDEVICE00000000000A1", "device", "email", w.devA.ID},
			{"01JRULEWEBHOOK0000000000A2", "global", "webhook", ""},
		} {
			var dev any
			if q.dev != "" {
				dev = q.dev
			}
			if _, err := tx.Exec(`INSERT INTO notification_rules (id, scope, device_id, name, channel, destination, triggers, notify_recovery, digest_schedule, created_at, updated_at)
				VALUES (?,?,?,?,?,?,'["alert"]',1,'daily',?,?)`, q.id, q.scope, dev, "other", q.channel, "dest", now, now); err != nil {
				return err
			}
		}
		return nil
	}))
	if l := must[[]Recipient](t)(w.s.ListRecipients(w.ctx, all)); len(l) != 0 {
		t.Fatalf("list shows rules that are not global e-mail: %+v", l)
	}
	for _, id := range []string{"01JRULEDEVICE00000000000A1", "01JRULEWEBHOOK0000000000A2"} {
		if _, err := w.s.GetRecipient(w.ctx, all, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("get %s = %v, want ErrNotFound", id, err)
		}
		if err := w.s.DeleteRecipient(w.ctx, all, id, actor); !errors.Is(err, ErrNotFound) {
			t.Errorf("delete %s = %v, want ErrNotFound", id, err)
		}
		if _, err := w.s.UpdateRecipient(w.ctx, all, id, RecipientInput{Name: "x", Address: "x@example.test"}, actor); !errors.Is(err, ErrNotFound) {
			t.Errorf("update %s = %v, want ErrNotFound", id, err)
		}
	}
	// A disabled global e-mail rule: every switch reads off, and saving it enables it again.
	r := must[Recipient](t)(w.s.CreateRecipient(w.ctx, all, RecipientInput{Name: "A", Address: "a@example.test", Alerts: true, Recoveries: true, Digest: true}, actor))
	ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE notification_rules SET enabled = 0 WHERE id = ?`, r.ID)
		return err
	}))
	if g := must[Recipient](t)(w.s.GetRecipient(w.ctx, all, r.ID)); g.Alerts || g.Recoveries || g.Digest {
		t.Fatalf("a disabled rule reads with switches on: %+v", g)
	}
	u := must[Recipient](t)(w.s.UpdateRecipient(w.ctx, all, r.ID, RecipientInput{Name: "A", Address: "a@example.test", Alerts: true, Recoveries: true, Digest: true}, actor))
	if !u.Alerts || !u.Recoveries || !u.Digest {
		t.Fatalf("saving did not re-enable the rule: %+v", u)
	}
	if _, _, _, _, _, _, enabled := rawRule(t, w, r.ID); enabled != 1 {
		t.Fatal("enabled is still 0 after saving")
	}
}

// Trigger entries this code does not know are kept when the alert switch changes.
func TestM4RecipientUpdateKeepsOtherTriggers(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	r := must[Recipient](t)(w.s.CreateRecipient(w.ctx, all, RecipientInput{Name: "A", Address: "a@example.test", Alerts: true}, actor))
	ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE notification_rules SET triggers = '["alert","nag"]' WHERE id = ?`, r.ID)
		return err
	}))
	must[Recipient](t)(w.s.UpdateRecipient(w.ctx, all, r.ID, RecipientInput{Name: "A", Address: "a@example.test", Alerts: false}, actor))
	if trig, _, _, _, _, _, _ := rawRule(t, w, r.ID); trig != `["nag"]` {
		t.Fatalf("triggers = %s, want [\"nag\"]", trig)
	}
	must[Recipient](t)(w.s.UpdateRecipient(w.ctx, all, r.ID, RecipientInput{Name: "A", Address: "a@example.test", Alerts: true}, actor))
	if trig, _, _, _, _, _, _ := rawRule(t, w, r.ID); trig != `["nag","alert"]` {
		t.Fatalf("triggers = %s", trig)
	}
}

// Deleting a recipient keeps its delivery-log rows (the log outlives the rule).
func TestM4DeletingARecipientKeepsItsLog(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	r := must[Recipient](t)(w.s.CreateRecipient(w.ctx, all, RecipientInput{Name: "A", Address: "a@example.test", Digest: true}, actor))
	d, _, err := w.s.EnqueueDelivery(w.ctx, all, DeliveryInput{Kind: DeliveryDigest, RuleID: r.ID, DedupeKey: "digest:2026-10-22:" + r.ID, SubjectType: "console"})
	ok(t, err)
	if d.RuleID != r.ID {
		t.Fatalf("delivery rule = %q", d.RuleID)
	}
	ok(t, w.s.DeleteRecipient(w.ctx, all, r.ID, actor))
	l := must[Listing[Delivery]](t)(w.s.ListRecentDeliveries(w.ctx, all, DeliveryFilter{}, Page{}))
	if len(l.Items) != 1 || l.Items[0].RuleID != "" {
		t.Fatalf("log after deleting the rule = %+v", l.Items)
	}
}

// P20-T2F item 5: only digest_schedule = 'daily' reads as the digest switch on; a value this code
// does not write (another schedule) is not a daily digest.
func TestM4RecipientDigestIsOnlyTheDailySchedule(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	r := must[Recipient](t)(w.s.CreateRecipient(w.ctx, all, RecipientInput{Name: "A", Address: "a@example.test", Digest: true}, actor))
	if !r.Digest {
		t.Fatal("a daily schedule must read as digest on")
	}
	for _, sched := range []string{"weekly", "hourly", "DAILY"} {
		ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
			_, err := tx.Exec(`UPDATE notification_rules SET digest_schedule = ? WHERE id = ?`, sched, r.ID)
			return err
		}))
		if g := must[Recipient](t)(w.s.GetRecipient(w.ctx, all, r.ID)); g.Digest {
			t.Errorf("digest_schedule %q reads as digest on", sched)
		}
	}
}
