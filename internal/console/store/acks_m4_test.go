package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func ackIn(dev, alarm string) AckInput {
	return AckInput{SubjectType: EpisodeDevice, SubjectID: dev, AlarmType: alarm}
}

func TestM4CreateAckPlainSnoozeAndNextSuccess(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	openAlarm(t, w, EpisodeDevice, w.devA.ID, "failed")
	openAlarm(t, w, EpisodeDevice, w.devA.ID, "stale")
	openAlarm(t, w, EpisodeDevice, w.devB.ID, "unseen")
	openAlarm(t, w, EpisodeCustomer, w.custB.ID, "missing_customer")

	plain := must[Ack](t)(w.s.CreateAck(w.ctx, all, AckInput{SubjectType: EpisodeDevice, SubjectID: w.devA.ID, AlarmType: "failed", Note: "  waiting for the part  "}, actor))
	if plain.SubjectType != EpisodeDevice || plain.SubjectID != w.devA.ID || plain.CustomerID != w.custA.ID || plain.AlarmType != "failed" || plain.Note != "waiting for the part" ||
		plain.AcknowledgedBy != w.admin.ID || !plain.AcknowledgedAt.Equal(t0) || plain.SnoozeUntil != nil || plain.UntilNextSuccess || plain.ClearedAt != nil {
		t.Fatalf("plain ack = %+v", plain)
	}

	until := t0.Add(48 * time.Hour)
	snooze := must[Ack](t)(w.s.CreateAck(w.ctx, all, AckInput{SubjectType: EpisodeDevice, SubjectID: w.devA.ID, AlarmType: "stale", SnoozeUntil: &until}, actor))
	if snooze.SnoozeUntil == nil || !snooze.SnoozeUntil.Equal(until) || snooze.UntilNextSuccess {
		t.Fatalf("snooze = %+v", snooze)
	}
	next := must[Ack](t)(w.s.CreateAck(w.ctx, all, AckInput{SubjectType: EpisodeDevice, SubjectID: w.devB.ID, AlarmType: "unseen", UntilNextSuccess: true}, actor))
	if !next.UntilNextSuccess || next.SnoozeUntil != nil {
		t.Fatalf("until next success = %+v", next)
	}
	cust := must[Ack](t)(w.s.CreateAck(w.ctx, all, AckInput{SubjectType: EpisodeCustomer, SubjectID: w.custB.ID, AlarmType: "missing_customer", Note: "on holiday"}, actor))
	if cust.SubjectType != EpisodeCustomer || cust.SubjectID != w.custB.ID || cust.CustomerID != w.custB.ID || cust.AlarmType != "missing_customer" {
		t.Fatalf("customer ack = %+v", cust)
	}

	active := must[[]Ack](t)(w.s.ListActiveAcks(w.ctx, all))
	if len(active) != 4 {
		t.Fatalf("active = %d, want 4: %+v", len(active), active)
	}
	if got := must[[]Ack](t)(w.s.ListActiveAcks(w.ctx, ScopeOf(w.custA.ID))); len(got) != 2 {
		t.Fatalf("A sees %d acks, want 2", len(got))
	}
	if n := auditCount(t, w, "alarm_ack.create"); n != 4 {
		t.Fatalf("%d create audit rows, want 4", n)
	}
}

func TestM4CreateAckValidation(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	past, soon, far := t0.Add(-time.Minute), t0.Add(time.Hour), t0.Add(MaxSnooze+time.Hour)
	cases := map[string]struct {
		in    AckInput
		field string
	}{
		"unknown alarm type":       {AckInput{SubjectType: EpisodeDevice, SubjectID: w.devA.ID, AlarmType: "vibes"}, "alarm_type"},
		"customer type on device":  {ackIn(w.devA.ID, "missing_customer"), "alarm_type"},
		"device type on customer":  {AckInput{SubjectType: EpisodeCustomer, SubjectID: w.custA.ID, AlarmType: "failed"}, "alarm_type"},
		"unknown subject":          {AckInput{SubjectType: "fleet", SubjectID: "x", AlarmType: "failed"}, "subject_type"},
		"no subject id":            {AckInput{SubjectType: EpisodeDevice, AlarmType: "failed"}, "subject_id"},
		"snooze in the past":       {AckInput{SubjectType: EpisodeDevice, SubjectID: w.devA.ID, AlarmType: "failed", SnoozeUntil: &past}, "snooze_until"},
		"snooze beyond a year":     {AckInput{SubjectType: EpisodeDevice, SubjectID: w.devA.ID, AlarmType: "failed", SnoozeUntil: &far}, "snooze_until"},
		"both snooze forms":        {AckInput{SubjectType: EpisodeDevice, SubjectID: w.devA.ID, AlarmType: "failed", SnoozeUntil: &soon, UntilNextSuccess: true}, "snooze_until"},
		"note too long":            {AckInput{SubjectType: EpisodeDevice, SubjectID: w.devA.ID, AlarmType: "failed", Note: strings.Repeat("n", maxAckNote+1)}, "note"},
		"note with a control char": {AckInput{SubjectType: EpisodeDevice, SubjectID: w.devA.ID, AlarmType: "failed", Note: "bad\x07note"}, "note"},
	}
	for name, c := range cases {
		_, err := w.s.CreateAck(w.ctx, all, c.in, actor)
		fe := FieldErrors(err)
		if !errors.Is(err, ErrInvalid) || len(fe) == 0 || fe[0].Field != c.field {
			t.Errorf("%s: err = %v (fields %v), want a field error on %s", name, err, fe, c.field)
		}
	}
	if n := len(must[[]Ack](t)(w.s.ListActiveAcks(w.ctx, all))); n != 0 {
		t.Fatalf("%d acks exist after refused requests", n)
	}
	// Subjects outside scope, or missing, are not found.
	if _, err := w.s.CreateAck(w.ctx, ScopeOf(w.custA.ID), ackIn(w.devB.ID, "failed"), actor); !errors.Is(err, ErrNotFound) {
		t.Errorf("device of another customer = %v", err)
	}
	if _, err := w.s.CreateAck(w.ctx, ScopeOf(w.custA.ID), AckInput{SubjectType: EpisodeCustomer, SubjectID: w.custB.ID, AlarmType: "missing_customer"}, actor); !errors.Is(err, ErrNotFound) {
		t.Errorf("another customer = %v", err)
	}
	if _, err := w.s.CreateAck(w.ctx, all, ackIn("01JNOSUCHDEVICE0000000000", "failed"), actor); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown device = %v", err)
	}
	if _, err := w.s.CreateAck(w.ctx, all, AckInput{SubjectType: EpisodeCustomer, SubjectID: "01JNOSUCHCUSTOMER00000000", AlarmType: "missing_customer"}, actor); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown customer = %v", err)
	}
	if got := AckAlarmTypes("fleet"); got != nil {
		t.Errorf("types of an unknown subject = %v", got)
	}
}

// One active ack per subject and alarm type: a new one replaces the old one, and the audit row says so.
func TestM4CreateAckReplacesTheActiveOne(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	openAlarm(t, w, EpisodeDevice, w.devA.ID, "failed")
	first := must[Ack](t)(w.s.CreateAck(w.ctx, all, ackIn(w.devA.ID, "failed"), actor))
	w.clk.Advance(time.Minute)
	second := must[Ack](t)(w.s.CreateAck(w.ctx, all, AckInput{SubjectType: EpisodeDevice, SubjectID: w.devA.ID, AlarmType: "failed", Note: "second"}, actor))
	if second.ID == first.ID {
		t.Fatal("the same row came back")
	}
	active := must[[]Ack](t)(w.s.ListActiveAcks(w.ctx, all))
	if len(active) != 1 || active[0].ID != second.ID {
		t.Fatalf("active = %+v, want only the second", active)
	}
	var cleared int
	ok(t, w.s.Read(w.ctx, func(tx *Tx) error {
		return tx.QueryRow(`SELECT count(*) FROM alarm_acks WHERE id = ? AND cleared_at IS NOT NULL`, first.ID).Scan(&cleared)
	}))
	if cleared != 1 {
		t.Fatal("the replaced ack was not cleared")
	}
	var d1, d2 string
	ok(t, w.s.Read(w.ctx, func(tx *Tx) error {
		if err := tx.QueryRow(`SELECT details FROM audit_events WHERE action = 'alarm_ack.create' AND object_id = ?`, first.ID).Scan(&d1); err != nil {
			return err
		}
		return tx.QueryRow(`SELECT details FROM audit_events WHERE action = 'alarm_ack.create' AND object_id = ?`, second.ID).Scan(&d2)
	}))
	if !strings.Contains(d1, `"replaced":false`) || !strings.Contains(d2, `"replaced":true`) {
		t.Fatalf("audit details %s / %s", d1, d2)
	}
}

// The note stays in the row; the audit trail carries ids and switches only.
func TestM4AckAuditRowsNeverCarryTheNote(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	openAlarm(t, w, EpisodeDevice, w.devA.ID, "failed")
	a := must[Ack](t)(w.s.CreateAck(w.ctx, all, AckInput{SubjectType: EpisodeDevice, SubjectID: w.devA.ID, AlarmType: "failed", Note: "confidential-note-text"}, actor))
	must[Ack](t)(w.s.ClearAck(w.ctx, all, a.ID, actor))
	var rows []string
	ok(t, w.s.Read(w.ctx, func(tx *Tx) error {
		var err error
		rows, err = collect(tx, `SELECT action || ' ' || COALESCE(object_id,'') || ' ' || COALESCE(customer_id,'') || ' ' || details FROM audit_events WHERE action LIKE 'alarm_ack.%'`, nil,
			func(sc scanner) (string, error) { var s string; return s, sc.Scan(&s) })
		return err
	}))
	if len(rows) != 2 {
		t.Fatalf("%d audit rows, want create and clear", len(rows))
	}
	for _, r := range rows {
		if strings.Contains(r, "confidential") {
			t.Errorf("audit row carries the note: %s", r)
		}
		if !strings.Contains(r, a.ID) || !strings.Contains(r, w.custA.ID) || !strings.Contains(r, w.devA.ID) {
			t.Errorf("audit row lacks the ack, customer or device id: %s", r)
		}
	}
	// The note itself is stored.
	if got := must[[]Ack](t)(w.s.ListActiveAcks(w.ctx, all)); len(got) != 0 {
		t.Fatalf("cleared ack still active: %+v", got)
	}
}

func TestM4ClearAck(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	openAlarm(t, w, EpisodeDevice, w.devA.ID, "failed")
	a := must[Ack](t)(w.s.CreateAck(w.ctx, all, ackIn(w.devA.ID, "failed"), actor))
	if _, err := w.s.ClearAck(w.ctx, ScopeOf(w.custB.ID), a.ID, actor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("clearing another customer's ack = %v, want ErrNotFound", err)
	}
	if n := len(must[[]Ack](t)(w.s.ListActiveAcks(w.ctx, all))); n != 1 {
		t.Fatalf("the denied clear changed something (%d active)", n)
	}
	w.clk.Advance(time.Minute)
	c := must[Ack](t)(w.s.ClearAck(w.ctx, ScopeOf(w.custA.ID), a.ID, actor))
	if c.ClearedAt == nil || !c.ClearedAt.Equal(w.clk.Now()) {
		t.Fatalf("cleared = %+v", c)
	}
	if n := len(must[[]Ack](t)(w.s.ListActiveAcks(w.ctx, all))); n != 0 {
		t.Fatalf("%d active after clearing", n)
	}
	// Clearing again is a no-op without a second audit row.
	w.clk.Advance(time.Minute)
	again := must[Ack](t)(w.s.ClearAck(w.ctx, all, a.ID, actor))
	if again.ClearedAt == nil || !again.ClearedAt.Equal(*c.ClearedAt) || auditCount(t, w, "alarm_ack.clear") != 1 {
		t.Fatalf("second clear = %+v, %d audit rows", again, auditCount(t, w, "alarm_ack.clear"))
	}
	if _, err := w.s.ClearAck(w.ctx, all, "01JNOSUCHACK0000000000000", actor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ack = %v", err)
	}
	// With the alarm still there, a new ack can be made after the clear (the unique index covers active rows only).
	must[Ack](t)(w.s.CreateAck(w.ctx, all, ackIn(w.devA.ID, "failed"), actor))
}

// A snooze past its time is not active; a plain one is until cleared; a device snoozed until the
// next success stops being active when a backup succeeds after it was made.
func TestM4ActiveAcksEndWhenTheirSnoozeDoes(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	openAlarm(t, w, EpisodeDevice, w.devA.ID, "stale")
	openAlarm(t, w, EpisodeDevice, w.devA.ID, "failed")
	openAlarm(t, w, EpisodeDevice, w.devB.ID, "failed")
	openAlarm(t, w, EpisodeCustomer, w.custB.ID, "missing_customer")
	until := t0.Add(time.Hour)
	must[Ack](t)(w.s.CreateAck(w.ctx, all, AckInput{SubjectType: EpisodeDevice, SubjectID: w.devA.ID, AlarmType: "stale", SnoozeUntil: &until}, actor))
	must[Ack](t)(w.s.CreateAck(w.ctx, all, ackIn(w.devA.ID, "failed"), actor))
	ingest(t, w, w.devB.ID, runInput(runID1, t0.Add(-5*time.Hour)))
	must[Ack](t)(w.s.CreateAck(w.ctx, all, AckInput{SubjectType: EpisodeDevice, SubjectID: w.devB.ID, AlarmType: "failed", UntilNextSuccess: true}, actor))
	must[Ack](t)(w.s.CreateAck(w.ctx, all, AckInput{SubjectType: EpisodeCustomer, SubjectID: w.custB.ID, AlarmType: "missing_customer", UntilNextSuccess: true}, actor))
	types := func() map[string]bool {
		out := map[string]bool{}
		for _, a := range must[[]Ack](t)(w.s.ListActiveAcks(w.ctx, all)) {
			out[a.SubjectID+"/"+a.AlarmType] = true
		}
		return out
	}
	got := types()
	if len(got) != 4 {
		t.Fatalf("before: %v", got)
	}

	// The snooze time passes.
	w.clk.Advance(2 * time.Hour)
	got = types()
	if got[w.devA.ID+"/stale"] || !got[w.devA.ID+"/failed"] || !got[w.devB.ID+"/failed"] || len(got) != 3 {
		t.Fatalf("after the snooze time: %v", got)
	}

	// A backup that finished after the acknowledgement succeeds.
	ingest(t, w, w.devB.ID, runInput(runID2, w.clk.Now().Add(-time.Minute)))
	got = types()
	if got[w.devB.ID+"/failed"] || !got[w.devA.ID+"/failed"] || !got[w.custB.ID+"/missing_customer"] || len(got) != 2 {
		t.Fatalf("after a success: %v", got)
	}
}

// The state engine's per-device ack and the store's list agree on a snooze until the next success.
func TestM4NextSuccessAckInForceMatchesTheStateEngine(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	openAlarm(t, w, EpisodeDevice, w.devA.ID, "stale")
	openAlarm(t, w, EpisodeDevice, w.devB.ID, "blank")
	// A success before the ack is not "next".
	ingest(t, w, w.devA.ID, runInput(runID1, t0.Add(-3*time.Hour)))
	a := must[Ack](t)(w.s.CreateAck(w.ctx, all, AckInput{SubjectType: EpisodeDevice, SubjectID: w.devA.ID, AlarmType: "stale", UntilNextSuccess: true}, actor))
	if n := len(must[[]Ack](t)(w.s.ListActiveAcks(w.ctx, all))); n != 1 {
		t.Fatalf("active = %d, want 1", n)
	}
	// No success at all before the ack (device B): any success afterwards ends it.
	b := must[Ack](t)(w.s.CreateAck(w.ctx, all, AckInput{SubjectType: EpisodeDevice, SubjectID: w.devB.ID, AlarmType: "blank", UntilNextSuccess: true}, actor))
	w.clk.Advance(time.Hour)
	ingest(t, w, w.devB.ID, runInput(runID2, w.clk.Now().Add(-time.Minute)))
	active := must[[]Ack](t)(w.s.ListActiveAcks(w.ctx, all))
	if len(active) != 1 || active[0].ID != a.ID {
		t.Fatalf("active = %+v, want only A's (B had a success after its ack %s)", active, b.ID)
	}
}
