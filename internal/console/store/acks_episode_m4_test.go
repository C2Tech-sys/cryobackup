package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// openAlarm makes the alarm an acknowledgement of type ackType would cover exist: it opens the episode
// of the state that maps to ackType for the subject, keeping every episode that is already open (a bare
// SyncEpisodes with one key would close the others). Tests call it before CreateAck.
func openAlarm(t *testing.T, w *world, subjectType, subjectID, ackType string) {
	t.Helper()
	alarm, found := EpisodeAlarmOfAck(ackType)
	if !found {
		t.Fatalf("no state maps to the acknowledgement type %q", ackType)
	}
	keys := []EpisodeKey{{SubjectType: subjectType, SubjectID: subjectID, AlarmType: alarm}}
	for _, e := range must[[]Episode](t)(w.s.ListOpenEpisodes(w.ctx, ScopeAll())) {
		keys = append(keys, EpisodeKey{SubjectType: e.SubjectType, SubjectID: e.SubjectID, AlarmType: e.AlarmType})
	}
	must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, ScopeAll(), keys, w.clk.Now()))
}

// P20-T2F item 1a: an acknowledgement answers an alarm that exists. With no open episode of the same
// subject and alarm type nothing is stored and no audit row is written: a stale ack would otherwise
// mute an episode that opens weeks later.
func TestM4CreateAckNeedsAnOpenEpisode(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	// No episode at all.
	_, err := w.s.CreateAck(w.ctx, all, ackIn(w.devA.ID, "failed"), actor)
	if !errors.Is(err, ErrAlarmNotOpen) || !errors.Is(err, ErrConflict) {
		t.Fatalf("no episode: err = %v, want ErrAlarmNotOpen (an ErrConflict)", err)
	}
	// An episode of another type does not do.
	openAlarm(t, w, EpisodeDevice, w.devA.ID, "stale")
	if _, err := w.s.CreateAck(w.ctx, all, ackIn(w.devA.ID, "failed"), actor); !errors.Is(err, ErrAlarmNotOpen) {
		t.Fatalf("episode of another type: err = %v", err)
	}
	// Nor does an open episode of the same type on another device.
	if _, err := w.s.CreateAck(w.ctx, all, ackIn(w.devB.ID, "stale"), actor); !errors.Is(err, ErrAlarmNotOpen) {
		t.Fatalf("episode of another device: err = %v", err)
	}
	// A customer alarm needs a customer episode.
	if _, err := w.s.CreateAck(w.ctx, all, AckInput{SubjectType: EpisodeCustomer, SubjectID: w.custA.ID, AlarmType: "missing_customer"}, actor); !errors.Is(err, ErrAlarmNotOpen) {
		t.Fatalf("customer without a MISSING episode: err = %v", err)
	}
	if n := len(must[[]Ack](t)(w.s.ListActiveAcks(w.ctx, all))) + auditCount(t, w, "alarm_ack.create"); n != 0 {
		t.Fatalf("%d acks or audit rows after refused requests", n)
	}
	// With the episode open it is accepted; once the episode closes it is refused again.
	a := must[Ack](t)(w.s.CreateAck(w.ctx, all, ackIn(w.devA.ID, "stale"), actor))
	if a.AlarmType != "stale" {
		t.Fatalf("ack = %+v", a)
	}
	must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, all, nil, w.clk.Now()))
	if _, err := w.s.CreateAck(w.ctx, all, ackIn(w.devA.ID, "stale"), actor); !errors.Is(err, ErrAlarmNotOpen) {
		t.Fatalf("closed episode: err = %v", err)
	}
	// A customer episode (MISSING) is answered by missing_customer.
	must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, all, []EpisodeKey{{SubjectType: EpisodeCustomer, SubjectID: w.custA.ID, AlarmType: AlarmMissing}}, w.clk.Now()))
	if _, err := w.s.CreateAck(w.ctx, all, AckInput{SubjectType: EpisodeCustomer, SubjectID: w.custA.ID, AlarmType: "missing_customer"}, actor); err != nil {
		t.Fatalf("customer ack with its episode open: %v", err)
	}
	// Subjects outside scope stay ErrNotFound, whatever is open.
	if _, err := w.s.CreateAck(w.ctx, ScopeOf(w.custB.ID), AckInput{SubjectType: EpisodeCustomer, SubjectID: w.custA.ID, AlarmType: "missing_customer"}, actor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("out of scope: err = %v", err)
	}
}

// The alarm types the schema names but no state maps to (first_backup_overdue, quota) are invalid
// input: nothing in M4 consumes them, so an acknowledgement of them would mute nothing.
func TestM4CreateAckRefusesTypesNoStateMapsTo(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	for _, in := range []AckInput{
		ackIn(w.devA.ID, "first_backup_overdue"),
		{SubjectType: EpisodeCustomer, SubjectID: w.custA.ID, AlarmType: "quota"},
	} {
		_, err := w.s.CreateAck(w.ctx, all, in, actor)
		if fe := FieldErrors(err); !errors.Is(err, ErrInvalid) || errors.Is(err, ErrConflict) || len(fe) != 1 || fe[0].Field != "alarm_type" {
			t.Errorf("%s: err = %v, want a field error on alarm_type", in.AlarmType, err)
		}
	}
	for _, typ := range append(AckAlarmTypes(EpisodeDevice), AckAlarmTypes(EpisodeCustomer)...) {
		alarm, found := EpisodeAlarmOfAck(typ)
		if typ == "first_backup_overdue" || typ == "quota" {
			if found {
				t.Errorf("%s maps to %s", typ, alarm)
			}
			continue
		}
		if back, ok := AckAlarmType(alarm); !found || !ok || back != typ {
			t.Errorf("%s -> %q -> %q: the two mappings disagree", typ, alarm, back)
		}
	}
}

// P20-T2F item 1b: when SyncEpisodes opens an episode it clears an active ack of the same subject
// and type that is already there (a row written before v8, or by a fixture), audited as the system,
// and leaves acks of other types and subjects alone.
func TestM4SyncEpisodesClearsAStaleAckWhenItOpensTheEpisode(t *testing.T) {
	w := seed(t)
	all := ScopeAll()
	insertAck := func(id, dev, kind string) {
		ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
			_, err := tx.Exec(`INSERT INTO alarm_acks (id, subject_type, device_id, alarm_type, acknowledged_at, note, created_at, updated_at)
				VALUES (?, 'device', ?, ?, ?, 'old', ?, ?)`, id, dev, kind, t0.UnixMilli(), t0.UnixMilli(), t0.UnixMilli())
			return err
		}))
	}
	const staleID, otherType, otherDevice = "01JSTALEACK0000000000000A1", "01JSTALEACK0000000000000A2", "01JSTALEACK0000000000000B1"
	insertAck(staleID, w.devA.ID, "failed")
	insertAck(otherType, w.devA.ID, "stale")
	insertAck(otherDevice, w.devB.ID, "failed")
	w.clk.Advance(14 * 24 * time.Hour) // weeks later
	r := must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, all, []EpisodeKey{epKey(w.devA.ID, "FAILED")}, w.clk.Now()))
	if len(r.Opened) != 1 || r.ClearedAcks != 1 {
		t.Fatalf("sync = %+v, want one episode opened and one stale ack cleared", r)
	}
	ids := map[string]bool{}
	for _, a := range must[[]Ack](t)(w.s.ListActiveAcks(w.ctx, all)) {
		ids[a.ID] = true
	}
	if len(ids) != 2 || ids[staleID] || !ids[otherType] || !ids[otherDevice] {
		t.Fatalf("active acks after the episode opened = %v", ids)
	}
	var actorType, details string
	ok(t, w.s.Read(w.ctx, func(tx *Tx) error {
		return tx.QueryRow(`SELECT actor_type, details FROM audit_events WHERE action = 'alarm_ack.clear' AND object_id = ?`, staleID).Scan(&actorType, &details)
	}))
	if actorType != "system" || !strings.Contains(details, "stale_before_alarm") || !strings.Contains(details, r.Opened[0].ID) || strings.Contains(details, "old") {
		t.Fatalf("clear audit = %s %s", actorType, details)
	}
	// A second pass with the same alarm clears nothing more.
	if again := must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, all, []EpisodeKey{epKey(w.devA.ID, "FAILED")}, w.clk.Now())); again.ClearedAcks != 0 || len(again.Opened) != 0 {
		t.Fatalf("second pass = %+v", again)
	}
}
