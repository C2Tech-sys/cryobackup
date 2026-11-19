package store

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// sendM4 records a delivery and settles it: result sent or failed (with its attempts), at the
// current fake time.
func sendM4(t *testing.T, w *world, kind, key string, fails int, finalOK bool) Delivery {
	t.Helper()
	d := enqueueM4(t, w, kind, key)
	for i := 0; i < fails; i++ {
		w.clk.Advance(time.Second)
		var next *time.Time
		if i < 3 {
			n := w.clk.Now().Add(time.Minute)
			next = &n
		}
		d = must[Delivery](t)(w.s.MarkDeliveryFailed(w.ctx, ScopeAll(), d.ID, "connect", next))
	}
	if finalOK {
		w.clk.Advance(time.Second)
		d = must[Delivery](t)(w.s.MarkDeliverySent(w.ctx, ScopeAll(), d.ID))
	}
	w.clk.Advance(time.Second)
	return d
}

func TestM4LoadSendFactsOnAnEmptyLog(t *testing.T) {
	w := seed(t)
	f := must[SendFacts](t)(w.s.LoadSendFacts(w.ctx, ScopeAll()))
	if f.ConsecutiveFailures != 0 || !f.LastSentAt.IsZero() || f.PendingAlerts != 0 || !f.OldestPendingAt.IsZero() || f.LastTest != nil || !f.LastDigestSentAt.IsZero() {
		t.Fatalf("empty log = %+v", f)
	}
}

func TestM4LoadSendFactsReadsTheLog(t *testing.T) {
	w := seed(t)
	all := ScopeAll()
	delivered := sendM4(t, w, DeliveryAlert, "alert:1", 0, true)
	digest := sendM4(t, w, DeliveryDigest, "digest:2026-10-22:r", 0, true)
	// A test that failed is not a failed alert send, and a test that passed is not a delivered alert.
	sendM4(t, w, DeliveryTest, "test:1", 1, false)
	// Two failures of one message, then another message that failed once: three failed attempts, newest first.
	sendM4(t, w, DeliveryAlert, "alert:2", 2, false)
	sendM4(t, w, DeliveryRecovery, "recovery:1", 1, false)
	f := must[SendFacts](t)(w.s.LoadSendFacts(w.ctx, all))
	if f.ConsecutiveFailures != 3 {
		t.Fatalf("consecutive failures = %d, want 3 (2 + 1; the test and the delivered rows do not count)", f.ConsecutiveFailures)
	}
	if !f.LastSentAt.Equal(digest.SentAt) {
		t.Fatalf("last sent = %v, want the digest's time %v (the newest delivered non-test)", f.LastSentAt, digest.SentAt)
	}
	if !f.LastDigestSentAt.Equal(digest.SentAt) {
		t.Fatalf("last digest = %v, want %v", f.LastDigestSentAt, digest.SentAt)
	}
	if f.LastTest == nil || f.LastTest.OK || f.LastTest.ErrorClass != "connect" {
		t.Fatalf("last test = %+v", f.LastTest)
	}
	if f.PendingAlerts != 2 || f.OldestPendingAt.IsZero() {
		t.Fatalf("pending = %d oldest %v, want the two with a retry scheduled", f.PendingAlerts, f.OldestPendingAt)
	}
	_ = delivered

	// A delivered message ends the streak.
	sendM4(t, w, DeliveryAlert, "alert:3", 0, true)
	if f := must[SendFacts](t)(w.s.LoadSendFacts(w.ctx, all)); f.ConsecutiveFailures != 0 {
		t.Fatalf("after a delivery: %d consecutive failures", f.ConsecutiveFailures)
	}
	// A message that was retried and finally delivered ends it too, and its own failures do not count.
	sendM4(t, w, DeliveryAlert, "alert:4", 2, true)
	if f := must[SendFacts](t)(w.s.LoadSendFacts(w.ctx, all)); f.ConsecutiveFailures != 0 {
		t.Fatalf("after alert:4: %d, want 0 (its two failures ended in a delivery)", f.ConsecutiveFailures)
	}
	sendM4(t, w, DeliveryAlert, "alert:5", 1, false)
	if f := must[SendFacts](t)(w.s.LoadSendFacts(w.ctx, all)); f.ConsecutiveFailures != 1 {
		t.Fatalf("after alert:5: %d, want 1", f.ConsecutiveFailures)
	}
	// The newest test that passed replaces a failed one.
	sendM4(t, w, DeliveryTest, "test:2", 0, true)
	if f := must[SendFacts](t)(w.s.LoadSendFacts(w.ctx, all)); f.LastTest == nil || !f.LastTest.OK || f.LastTest.ErrorClass != "" {
		t.Fatalf("last test = %+v, want a pass", f.LastTest)
	}
}

// Pending counts what still waits: queued, or failed with a retry. The oldest one gives the age.
func TestM4PendingCountsAndTheAgeOfTheOldest(t *testing.T) {
	w := seed(t)
	first := enqueueM4(t, w, DeliveryAlert, "alert:p1")
	w.clk.Advance(time.Hour)
	enqueueM4(t, w, DeliveryRecovery, "recovery:p2")
	enqueueM4(t, w, DeliveryDigest, "digest:2026-10-22:p3") // a digest is not an alert
	enqueueM4(t, w, DeliveryTest, "test:p4")                // neither is a test
	f := must[SendFacts](t)(w.s.LoadSendFacts(w.ctx, ScopeAll()))
	if f.PendingAlerts != 2 || !f.OldestPendingAt.Equal(first.CreatedAt) {
		t.Fatalf("pending = %d oldest %v, want 2 and %v", f.PendingAlerts, f.OldestPendingAt, first.CreatedAt)
	}
	// Gave up: no longer pending.
	must[Delivery](t)(w.s.MarkDeliveryFailed(w.ctx, ScopeAll(), first.ID, "connect", nil))
	if f := must[SendFacts](t)(w.s.LoadSendFacts(w.ctx, ScopeAll())); f.PendingAlerts != 1 || f.OldestPendingAt.Equal(first.CreatedAt) {
		t.Fatalf("after giving up: %+v", f)
	}
}

func worldWith(smtp *state.SMTPFacts, digest *state.DigestFacts) state.World {
	return state.World{LoadComplete: true, LoadedAt: t0, Probes: state.ProbeFacts{Wired: true, SMTP: smtp, Digest: digest}}
}

func bannerIDs(w state.World, now time.Time) map[string]state.Banner {
	out := map[string]state.Banner{}
	for _, b := range state.Evaluate(w, now).Banners {
		out[b.ID] = b
	}
	return out
}

// The loaders fill the facts the banners read, from rows: three failed attempts raise C03, a missed
// digest raises C14 and a sent one clears it. Before this the facts were never filled.
func TestM4MailFactsFeedBannersC03AndC14(t *testing.T) {
	w := seed(t)
	all := ScopeAll()
	probe := state.ProbeRun{LastRunAt: state.Millis(t0.UnixMilli()), LastOKAt: state.Millis(t0.UnixMilli())}

	smtp, digest, err := w.s.MailFacts(w.ctx, all, probe)
	ok(t, err)
	if smtp.Probe != probe || smtp.NotRead != "" || digest.NotRead != "" || digest.NotInBuild || digest.Disabled || digest.LocalTime != "07:00" || digest.LastSentAt != 0 {
		t.Fatalf("fresh facts: smtp %+v digest %+v", smtp, digest)
	}
	// 13:42 UTC on a console in UTC: 07:00 and its grace are past and no digest went out.
	if b := bannerIDs(worldWith(smtp, digest), t0); b["C14"].Severity != state.SeverityCritical || b["C03"].ID != "" {
		t.Fatalf("banners with no digest sent = %v", b)
	}

	// The digest goes out.
	sendM4(t, w, DeliveryDigest, "digest:2026-10-22:r", 0, true)
	smtp, digest, _ = w.s.MailFacts(w.ctx, all, probe)
	if digest.LastSentAt == 0 || smtp.LastSendOKAt == 0 {
		t.Fatalf("after a digest: smtp %+v digest %+v", smtp, digest)
	}
	if b := bannerIDs(worldWith(smtp, digest), w.clk.Now()); b["C14"].ID != "" || b["C03"].ID != "" {
		t.Fatalf("banners after a sent digest = %v", b)
	}

	// Three failed attempts in a row: C03.
	sendM4(t, w, DeliveryAlert, "alert:f", 3, false)
	smtp, digest, _ = w.s.MailFacts(w.ctx, all, probe)
	if smtp.ConsecutiveSendFailures != 3 || smtp.PendingAlerts != 1 {
		t.Fatalf("smtp facts = %+v", smtp)
	}
	if b := bannerIDs(worldWith(smtp, digest), w.clk.Now()); b["C03"].Severity != state.SeverityCritical {
		t.Fatalf("banners after three failures = %v", b)
	}

	// A failed test alone raises C03 as well (the banner already says so); a passed one later clears it.
	w2 := seed(t)
	sendM4(t, w2, DeliveryTest, "test:1", 1, false)
	smtp, digest, _ = w2.s.MailFacts(w2.ctx, all, probe)
	if !smtp.LastTestFailed {
		t.Fatalf("LastTestFailed not set: %+v", smtp)
	}
	sendM4(t, w2, DeliveryTest, "test:2", 0, true)
	smtp, _, _ = w2.s.MailFacts(w2.ctx, all, probe)
	if smtp.LastTestFailed {
		t.Fatal("LastTestFailed still set after a passing test")
	}
}

// When the delivery log or the settings cannot be read, the facts say so and the banners are the
// "not checked" warnings, never silence.
func TestM4MailFactsSayWhenTheyCannotBeRead(t *testing.T) {
	probe := state.ProbeRun{LastRunAt: state.Millis(t0.UnixMilli()), LastOKAt: state.Millis(t0.UnixMilli())}

	t.Run("delivery log", func(t *testing.T) {
		w := seed(t)
		ok(t, w.s.Write(w.ctx, func(tx *Tx) error { _, err := tx.Exec(`DROP TABLE notification_log`); return err }))
		if _, err := w.s.LoadSendFacts(w.ctx, ScopeAll()); err == nil {
			t.Fatal("LoadSendFacts on a missing table returned no error")
		}
		smtp, digest, err := w.s.MailFacts(w.ctx, ScopeAll(), probe)
		ok(t, err)
		if smtp.NotRead == "" || digest.NotRead == "" || smtp.ConsecutiveSendFailures != 0 {
			t.Fatalf("facts = %+v %+v", smtp, digest)
		}
		for _, reason := range []string{smtp.NotRead, digest.NotRead} {
			if strings.ContainsAny(reason, `/\:`) || strings.Contains(reason, "notification_log") {
				t.Errorf("reason %q is not a path-free fixed sentence", reason)
			}
		}
		b := bannerIDs(worldWith(smtp, digest), t0)
		if b["C03b"].Severity != state.SeverityAlarm || b["C14b"].Severity != state.SeverityAlarm || b["C03"].ID != "" || b["C14"].ID != "" {
			t.Fatalf("banners = %v, want C03b and C14b only", b)
		}
	})

	t.Run("digest settings", func(t *testing.T) {
		w := seed(t)
		ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
			_, err := tx.Exec(`UPDATE settings SET value = '"maybe"' WHERE key = 'digest_enabled'`)
			return err
		}))
		if _, err := w.s.ReadDigestSettings(w.ctx, ScopeAll()); !errors.Is(err, ErrInvalid) {
			t.Fatalf("garbled digest_enabled = %v, want ErrInvalid", err)
		}
		smtp, digest, err := w.s.MailFacts(w.ctx, ScopeAll(), probe)
		ok(t, err)
		if digest.NotRead == "" || smtp.NotRead != "" {
			t.Fatalf("facts = %+v %+v", smtp, digest)
		}
		if b := bannerIDs(worldWith(smtp, digest), t0); b["C14b"].Severity != state.SeverityAlarm || b["C14"].ID != "" {
			t.Fatalf("banners = %v", b)
		}
	})
}

func TestM4DigestSettingsAreEditableAndValidated(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	ds := must[DigestSettings](t)(w.s.ReadDigestSettings(w.ctx, all))
	if !ds.Enabled || ds.LocalTime != "07:00" {
		t.Fatalf("defaults = %+v, want on at 07:00", ds)
	}
	set := func(key, raw string) ([]SettingChange, error) {
		return w.s.SetSettingsAudited(w.ctx, all, []SettingInput{{Key: key, Value: json.RawMessage(raw)}}, actor)
	}
	if _, err := set(SettingDigestLocalTime, `"06:30"`); err != nil {
		t.Fatal(err)
	}
	if _, err := set(SettingDigestEnabled, `false`); err != nil {
		t.Fatal(err)
	}
	ds = must[DigestSettings](t)(w.s.ReadDigestSettings(w.ctx, all))
	if ds.Enabled || ds.LocalTime != "06:30" {
		t.Fatalf("after editing = %+v", ds)
	}
	if n := auditCount(t, w, "settings.update"); n != 2 {
		t.Fatalf("%d settings audit rows, want 2", n)
	}
	for _, bad := range []string{`"7:00"`, `"07:0"`, `"24:00"`, `"07:60"`, `"0700"`, `"07-00"`, `"ab:cd"`, `""`, `700`, `true`, `null`, `"07:00 "x`, `"٠٧:00"`} {
		_, err := set(SettingDigestLocalTime, bad)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("digest_local_time %s = %v, want ErrInvalid", bad, err)
		}
	}
	for _, bad := range []string{`"true"`, `1`, `0`, `"yes"`, `null`, `[]`, ``} {
		if _, err := set(SettingDigestEnabled, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("digest_enabled %s = %v, want ErrInvalid", bad, err)
		}
	}
	// Whitespace around a valid time is trimmed to the canonical form.
	ch, err := set(SettingDigestLocalTime, `" 21:05 "`)
	if err != nil || string(ch[0].New) != `"21:05"` {
		t.Fatalf("trimmed time = %v %v", ch, err)
	}
	// Both are listed as editable, with no "takes effect with" milestone left.
	vals := must[[]SettingValue](t)(w.s.ListSettingValues(w.ctx, all))
	seen := 0
	for _, v := range vals {
		if v.Key == SettingDigestLocalTime || v.Key == SettingDigestEnabled {
			seen++
			if !v.Editable || v.Since != "" {
				t.Errorf("%s: editable %v since %q", v.Key, v.Editable, v.Since)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("saw %d digest settings", seen)
	}
	// A row that is not there means on (the migration seeds it, but a deleted row must not switch the digest off).
	ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`DELETE FROM settings WHERE key IN ('digest_enabled','digest_local_time')`)
		return err
	}))
	ds = must[DigestSettings](t)(w.s.ReadDigestSettings(w.ctx, all))
	if !ds.Enabled || ds.LocalTime != "" {
		t.Fatalf("with the rows gone = %+v, want on and no time (C14b says so)", ds)
	}
	smtp, digest, _ := w.s.MailFacts(w.ctx, all, state.ProbeRun{})
	if b := bannerIDs(worldWith(smtp, digest), t0); b["C14b"].ID == "" {
		t.Fatalf("a missing digest time is not reported: %v", b)
	}
	if ValidLocalTime("23:59") != true || ValidLocalTime("00:00") != true || ValidLocalTime("7:00") || ValidLocalTime("23:60") {
		t.Fatal("ValidLocalTime")
	}
}

// flagsOf is the state engine's verdict on the device with no acknowledgement considered.
func flagsOf(t *testing.T, w *world, dev string) []state.State {
	t.Helper()
	return state.Classify(facts(t, w, dev), w.clk.Now()).Flags
}

// An acknowledgement covers the device only for the alarm type it names: the fact loader reads the
// newest active ack that matches one of the device's own states.
func TestM4TheLoaderMatchesAcknowledgementsPerAlarmType(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	// Two days after enrollment a device whose last success is old and whose newest run failed is FAILED
	// (and whatever else the engine finds); before its first success it would only be PENDING_FIRST.
	w.clk.Advance(48 * time.Hour)
	w.heartbeat(w.devA.ID)
	ingest(t, w, w.devA.ID, runInput(runID2, w.clk.Now().Add(-40*time.Hour)))
	fail := runInput(runID1, w.clk.Now().Add(-30*time.Minute))
	fail.Outcome = "failed"
	fail.ErrorClass = "disk"
	ingest(t, w, w.devA.ID, fail)
	flags := flagsOf(t, w, w.devA.ID)
	have := map[string]bool{}
	for _, f := range flags {
		if k, ok := AckAlarmType(string(f)); ok {
			have[k] = true
		}
	}
	if !have["failed"] {
		t.Fatalf("the device is not FAILED (flags %v): the test needs a failing device", flags)
	}
	var other string // an alarm type the device does not have
	for _, k := range AckAlarmTypes(EpisodeDevice) {
		if !have[k] {
			other = k
			break
		}
	}
	if other == "" {
		t.Fatalf("flags %v cover every ack type", flags)
	}

	// An ack of an alarm type the device does not have covers nothing.
	must[Ack](t)(w.s.CreateAck(w.ctx, all, ackIn(w.devA.ID, other), actor))
	if a := facts(t, w, w.devA.ID).Ack; a != nil {
		t.Fatalf("an acknowledgement of %q covers a device that is %v: %+v", other, flags, a)
	}
	if ds := state.Classify(facts(t, w, w.devA.ID), w.clk.Now()); ds.Ack != nil || containsAttr(ds.Attrs, state.AttrAcknowledged) {
		t.Fatalf("the device shows as acknowledged: %+v", ds.Attrs)
	}

	// An ack of FAILED covers it: the engine marks it acknowledged and never changes the state.
	before := state.Classify(facts(t, w, w.devA.ID), w.clk.Now())
	w.clk.Advance(time.Minute)
	must[Ack](t)(w.s.CreateAck(w.ctx, all, AckInput{SubjectType: EpisodeDevice, SubjectID: w.devA.ID, AlarmType: "failed", Note: "known"}, actor))
	f := facts(t, w, w.devA.ID)
	if f.Ack == nil || f.Ack.AckedAt != ms(w.clk.Now()) || f.Ack.SnoozeUntil != 0 {
		t.Fatalf("ack facts = %+v", f.Ack)
	}
	after := state.Classify(f, w.clk.Now())
	if after.Primary != before.Primary || after.Alarm != before.Alarm || !containsAttr(after.Attrs, state.AttrAcknowledged) || after.Ack == nil || !after.Ack.Muted {
		t.Fatalf("after acknowledging: primary %v (was %v), alarm %v, attrs %v, ack %+v", after.Primary, before.Primary, after.Alarm, after.Attrs, after.Ack)
	}

	// Several acks: the newest matching one wins (a snooze on the same type replaces the plain one).
	until := w.clk.Now().Add(24 * time.Hour)
	w.clk.Advance(time.Minute)
	must[Ack](t)(w.s.CreateAck(w.ctx, all, AckInput{SubjectType: EpisodeDevice, SubjectID: w.devA.ID, AlarmType: "failed", SnoozeUntil: &until}, actor))
	if f := facts(t, w, w.devA.ID); f.Ack == nil || f.Ack.SnoozeUntil != ms(until) {
		t.Fatalf("ack facts after the snooze = %+v", f.Ack)
	}

	// Through the world loader as well.
	world := must[state.World](t)(w.s.LoadWorld(w.ctx, all, LoadOptions{}))
	var found bool
	for _, d := range world.Devices {
		if d.ID == w.devA.ID {
			found = d.Ack != nil
		}
	}
	if !found {
		t.Fatal("LoadWorld did not attach the ack")
	}

	// Cleared: nothing covers the device again.
	for _, a := range must[[]Ack](t)(w.s.ListActiveAcks(w.ctx, all)) {
		must[Ack](t)(w.s.ClearAck(w.ctx, all, a.ID, actor))
	}
	if a := facts(t, w, w.devA.ID).Ack; a != nil {
		t.Fatalf("cleared acks still cover the device: %+v", a)
	}
}

func containsAttr(list []state.Attr, a state.Attr) bool {
	for _, x := range list {
		if x == a {
			return true
		}
	}
	return false
}

// The mapping from states to ack alarm types covers every alarm state and only valid schema values.
func TestM4AckAlarmTypeMapping(t *testing.T) {
	valid := map[string]bool{}
	for _, k := range ackAlarmTypes {
		valid[k] = true
	}
	var got []string
	for stateName, kind := range stateAckType {
		if !valid[kind] {
			t.Errorf("%s maps to %q, which the schema does not allow", stateName, kind)
		}
		got = append(got, stateName+"="+kind)
	}
	sort.Strings(got)
	for _, st := range append(criticalAlarmTypes, alarmLevelTypes...) {
		if _, ok := AckAlarmType(st); !ok {
			t.Errorf("episode alarm %s has no acknowledgement type (%v)", st, got)
		}
	}
	if k, ok := AckAlarmType(AlarmMissing); !ok || k != "missing_customer" {
		t.Errorf("MISSING = %q %v", k, ok)
	}
	for _, st := range []state.State{state.StateOK, state.StatePaused, state.StateDeferred, state.StateRetired, state.StateMissed, state.StatePendingFirst} {
		if k, ok := AckAlarmType(string(st)); ok {
			t.Errorf("%s maps to %q: it is not an alarm", st, k)
		}
	}
}

// P20-T2F item 3: the log keeps one row per message, so "failed attempts since the last delivery"
// is counted where it happens, not derived from rows. A fails twice, B is delivered, A fails once:
// one failed attempt since the last delivery.
func TestM4ConsecutiveFailuresSurviveInterleavedDeliveries(t *testing.T) {
	w := seed(t)
	all := ScopeAll()
	soon := func() *time.Time { n := w.clk.Now().Add(time.Minute); return &n }
	a := enqueueM4(t, w, DeliveryAlert, "alert:A")
	b := enqueueM4(t, w, DeliveryAlert, "alert:B")
	count := func() int { return must[SendFacts](t)(w.s.LoadSendFacts(w.ctx, all)).ConsecutiveFailures }
	for i := 0; i < 2; i++ {
		w.clk.Advance(time.Second)
		must[Delivery](t)(w.s.MarkDeliveryFailed(w.ctx, all, a.ID, "connect", soon()))
	}
	if n := count(); n != 2 {
		t.Fatalf("after A failed twice: %d, want 2", n)
	}
	w.clk.Advance(time.Second)
	must[Delivery](t)(w.s.MarkDeliverySent(w.ctx, all, b.ID))
	if n := count(); n != 0 {
		t.Fatalf("after B was delivered: %d, want 0", n)
	}
	w.clk.Advance(time.Second)
	must[Delivery](t)(w.s.MarkDeliveryFailed(w.ctx, all, a.ID, "connect", soon()))
	if n := count(); n != 1 {
		t.Fatalf("A failed once more after B was delivered: %d, want 1", n)
	}
}

// Test sends neither add to the count nor reset it; marking a delivered row sent again changes nothing.
func TestM4TestSendsDoNotMoveTheFailureCount(t *testing.T) {
	w := seed(t)
	all := ScopeAll()
	count := func() int { return must[SendFacts](t)(w.s.LoadSendFacts(w.ctx, all)).ConsecutiveFailures }
	sendM4(t, w, DeliveryAlert, "alert:1", 2, false)
	sendM4(t, w, DeliveryTest, "test:1", 1, false)
	if n := count(); n != 2 {
		t.Fatalf("a failed test moved the count to %d, want 2", n)
	}
	sendM4(t, w, DeliveryTest, "test:2", 0, true)
	if n := count(); n != 2 {
		t.Fatalf("a delivered test moved the count to %d, want 2", n)
	}
	d := sendM4(t, w, DeliveryAlert, "alert:2", 0, true)
	sendM4(t, w, DeliveryAlert, "alert:3", 1, false)
	must[Delivery](t)(w.s.MarkDeliverySent(w.ctx, all, d.ID)) // already delivered: a no-op
	if n := count(); n != 1 {
		t.Fatalf("re-marking a delivered row moved the count to %d, want 1", n)
	}
}

// The counter is a settings row the settings page does not know: it is not listed, and the console
// cannot be told to change it. A value that is not a non-negative integer is an error, never a zero.
func TestM4FailureCounterIsHiddenAndReadStrictly(t *testing.T) {
	w := seed(t)
	all := ScopeAll()
	sendM4(t, w, DeliveryAlert, "alert:1", 2, false)
	if g, err := w.s.GetSetting(w.ctx, SettingSMTPConsecutiveFailures); err != nil || string(g.Value) != "2" {
		t.Fatalf("stored counter = %q, %v, want 2", g.Value, err)
	}
	for _, v := range must[[]SettingValue](t)(w.s.ListSettingValues(w.ctx, all)) {
		if v.Key == SettingSMTPConsecutiveFailures {
			t.Fatal("the failure counter is listed on the settings page")
		}
	}
	_, err := w.s.SetSettingsAudited(w.ctx, all, []SettingInput{{Key: SettingSMTPConsecutiveFailures, Value: json.RawMessage(`0`)}}, m4Actor(w))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("changing the counter through the settings path = %v, want ErrNotFound", err)
	}
	for _, bad := range []string{`"x"`, `-1`, `1.5`, `null`, `[1]`} {
		ok(t, w.s.PutSetting(w.ctx, SettingSMTPConsecutiveFailures, json.RawMessage(bad), ""))
		if _, err := w.s.LoadSendFacts(w.ctx, all); !errors.Is(err, ErrInvalid) {
			t.Errorf("counter %s: LoadSendFacts = %v, want ErrInvalid", bad, err)
		}
		if smtp, _, err := w.s.MailFacts(w.ctx, all, state.ProbeRun{}); err != nil || smtp.NotRead == "" {
			t.Errorf("counter %s: MailFacts = %+v, %v, want a NotRead reason", bad, smtp, err)
		}
	}
}

// P20-T2F item 2: MailFacts carries the age of the oldest pending alert into the SMTP facts, and
// the banner waits SMTPPendingMinAge before it calls a queue with no delivery a failure.
func TestM4MailFactsCarryTheAgeOfThePendingAlert(t *testing.T) {
	w := seed(t)
	all := ScopeAll()
	probe := state.ProbeRun{LastRunAt: state.Millis(t0.UnixMilli()), LastOKAt: state.Millis(t0.UnixMilli())}
	first := enqueueM4(t, w, DeliveryAlert, "alert:age1")
	smtp, _, err := w.s.MailFacts(w.ctx, all, probe)
	ok(t, err)
	if smtp.PendingAlerts != 1 || smtp.OldestPendingAt != state.Millis(first.CreatedAt.UnixMilli()) {
		t.Fatalf("pending %d oldest %d, want 1 and %d", smtp.PendingAlerts, smtp.OldestPendingAt, first.CreatedAt.UnixMilli())
	}
	digest := &state.DigestFacts{NotInBuild: true}
	at := func(d time.Duration) map[string]state.Banner {
		now := t0.Add(d)
		probe := state.ProbeRun{LastRunAt: state.Millis(now.UnixMilli()), LastOKAt: state.Millis(now.UnixMilli())}
		smtp, _, err := w.s.MailFacts(w.ctx, all, probe)
		ok(t, err)
		wd := worldWith(smtp, digest)
		wd.LoadedAt = now
		return bannerIDs(wd, now)
	}
	if b := at(time.Minute); b["C03"].ID != "" {
		t.Fatalf("C03 one minute after the first alert was queued: %+v", b["C03"])
	}
	if b := at(state.SMTPPendingMinAge - time.Second); b["C03"].ID != "" {
		t.Fatalf("C03 just under %v after queueing: %+v", state.SMTPPendingMinAge, b["C03"])
	}
	if b := at(state.SMTPPendingMinAge); b["C03"].Severity != state.SeverityCritical {
		t.Fatalf("no C03 %v after queueing with nothing delivered: %v", state.SMTPPendingMinAge, b)
	}
}
