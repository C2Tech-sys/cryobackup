package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func epKey(dev, alarm string) EpisodeKey {
	return EpisodeKey{SubjectType: EpisodeDevice, SubjectID: dev, AlarmType: alarm}
}

// An alarm opens one episode, however many passes see it; when it is gone the episode closes; the
// same alarm coming back is a new episode. A second call with the same input changes nothing.
func TestM4SyncEpisodesOpensOnceAndClosesWhenTheAlarmEnds(t *testing.T) {
	w := seed(t)
	all := ScopeAll()
	since := t0.Add(-3 * time.Hour)
	in := []EpisodeKey{{SubjectType: EpisodeDevice, SubjectID: w.devA.ID, AlarmType: "FAILED", Since: since}}

	first := must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, all, in, t0))
	if len(first.Opened) != 1 || len(first.Closed) != 0 {
		t.Fatalf("first sync = %+v", first)
	}
	e := first.Opened[0]
	if e.SubjectType != EpisodeDevice || e.SubjectID != w.devA.ID || e.CustomerID != w.custA.ID || e.AlarmType != "FAILED" ||
		!e.OpenedAt.Equal(t0) || !e.StateSince.Equal(since) || e.NotifiedAt != nil || !e.Open() {
		t.Fatalf("opened episode = %+v", e)
	}

	// Idempotent: the same input a minute later changes nothing, and a moved Since does not open another.
	w.clk.Advance(time.Minute)
	in[0].Since = t0.Add(-time.Minute)
	again := must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, all, in, w.clk.Now()))
	if len(again.Opened) != 0 || len(again.Closed) != 0 || again.ClearedAcks != 0 {
		t.Fatalf("second sync with the same alarm = %+v, want nothing", again)
	}
	open := must[[]Episode](t)(w.s.ListOpenEpisodes(w.ctx, all))
	if len(open) != 1 || open[0].ID != e.ID || !open[0].StateSince.Equal(since) {
		t.Fatalf("open episodes = %+v, want the first one untouched (state_since is the value at opening)", open)
	}

	// The alarm ends: the episode closes. It was never alerted, so no recovery notice is owed.
	w.clk.Advance(time.Minute)
	closed := must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, all, nil, w.clk.Now()))
	if len(closed.Closed) != 1 || closed.Closed[0].ID != e.ID || closed.Closed[0].ClosedAt == nil || !closed.Closed[0].ClosedAt.Equal(w.clk.Now()) {
		t.Fatalf("closing sync = %+v", closed)
	}
	if n := len(must[[]Episode](t)(w.s.ListOpenEpisodes(w.ctx, all))); n != 0 {
		t.Fatalf("%d episodes still open", n)
	}
	if n := len(must[[]Episode](t)(w.s.ListRecoveryDueEpisodes(w.ctx, all))); n != 0 {
		t.Fatalf("a never-alerted episode owes a recovery notice (%d due)", n)
	}
	if again := must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, all, nil, w.clk.Now())); len(again.Closed) != 0 {
		t.Fatalf("closing twice: %+v", again)
	}

	// The alarm returns: a new episode.
	w.clk.Advance(time.Hour)
	back := must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, all, in, w.clk.Now()))
	if len(back.Opened) != 1 || back.Opened[0].ID == e.ID {
		t.Fatalf("returning alarm = %+v, want a new episode", back)
	}
}

// An unknown Since (zero) is stored as the time the episode opened; the column is never zero.
func TestM4SyncEpisodesWithoutSinceUsesTheOpeningTime(t *testing.T) {
	w := seed(t)
	r := must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, ScopeAll(), []EpisodeKey{epKey(w.devA.ID, "UNSEEN")}, t0))
	if len(r.Opened) != 1 || !r.Opened[0].StateSince.Equal(t0) {
		t.Fatalf("opened = %+v", r.Opened)
	}
	// now zero means the store clock.
	r = must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, ScopeAll(), []EpisodeKey{epKey(w.devA.ID, "UNSEEN"), epKey(w.devB.ID, "BLANK")}, time.Time{}))
	if len(r.Opened) != 1 || !r.Opened[0].OpenedAt.Equal(t0) {
		t.Fatalf("zero now: %+v", r.Opened)
	}
}

// The mail bookkeeping: alert marked once, recovery owed only after an alerted episode closes.
func TestM4EpisodeNotifiedAndRecoveryLifecycle(t *testing.T) {
	w := seed(t)
	all := ScopeAll()
	r := must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, all, []EpisodeKey{epKey(w.devA.ID, "FAILED"), epKey(w.devB.ID, "STALE")}, t0))
	if len(r.Opened) != 2 {
		t.Fatalf("opened %d", len(r.Opened))
	}
	if un := must[[]Episode](t)(w.s.ListUnnotifiedEpisodes(w.ctx, all)); len(un) != 2 {
		t.Fatalf("unnotified = %d, want 2", len(un))
	}
	var a, b Episode
	for _, e := range r.Opened {
		if e.SubjectID == w.devA.ID {
			a = e
		} else {
			b = e
		}
	}
	// Mark one; marking again keeps the first time and changes nothing.
	w.clk.Advance(time.Minute)
	if n := must[int](t)(w.s.MarkEpisodesNotified(w.ctx, all, []string{a.ID, a.ID}, time.Time{})); n != 1 {
		t.Fatalf("first mark changed %d", n)
	}
	first := w.clk.Now()
	w.clk.Advance(time.Hour)
	if n := must[int](t)(w.s.MarkEpisodesNotified(w.ctx, all, []string{a.ID}, time.Time{})); n != 0 {
		t.Fatalf("second mark changed %d, want 0", n)
	}
	un := must[[]Episode](t)(w.s.ListUnnotifiedEpisodes(w.ctx, all))
	if len(un) != 1 || un[0].ID != b.ID {
		t.Fatalf("unnotified after marking A = %+v", un)
	}
	open := must[[]Episode](t)(w.s.ListOpenEpisodes(w.ctx, all))
	for _, e := range open {
		if e.ID == a.ID && (e.NotifiedAt == nil || !e.NotifiedAt.Equal(first)) {
			t.Fatalf("notified_at = %v, want the first time %v", e.NotifiedAt, first)
		}
	}
	// A recovery notice cannot precede the close.
	if _, err := w.s.MarkRecoveryNotified(w.ctx, all, []string{a.ID}, time.Time{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("recovery on an open episode = %v, want ErrInvalid", err)
	}
	// Both alarms end: only the alerted one owes a recovery notice.
	must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, all, nil, w.clk.Now()))
	due := must[[]Episode](t)(w.s.ListRecoveryDueEpisodes(w.ctx, all))
	if len(due) != 1 || due[0].ID != a.ID || due[0].ClosedAt == nil {
		t.Fatalf("recovery due = %+v, want only the alerted episode", due)
	}
	if n := must[int](t)(w.s.MarkRecoveryNotified(w.ctx, all, []string{a.ID}, time.Time{})); n != 1 {
		t.Fatalf("recovery mark changed %d", n)
	}
	if n := must[int](t)(w.s.MarkRecoveryNotified(w.ctx, all, []string{a.ID}, time.Time{})); n != 0 {
		t.Fatalf("second recovery mark changed %d", n)
	}
	if due := must[[]Episode](t)(w.s.ListRecoveryDueEpisodes(w.ctx, all)); len(due) != 0 {
		t.Fatalf("recovery still due: %+v", due)
	}
}

// An id that does not exist stops the whole call: nothing is marked.
func TestM4MarkEpisodesRefusesAnUnknownIDWithoutChangingAnything(t *testing.T) {
	w := seed(t)
	r := must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, ScopeAll(), []EpisodeKey{epKey(w.devA.ID, "FAILED")}, t0))
	_, err := w.s.MarkEpisodesNotified(w.ctx, ScopeAll(), []string{r.Opened[0].ID, "01JNOSUCHEPISODE000000000"}, time.Time{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id = %v, want ErrNotFound", err)
	}
	if un := must[[]Episode](t)(w.s.ListUnnotifiedEpisodes(w.ctx, ScopeAll())); len(un) != 1 {
		t.Fatalf("the known episode was marked by a refused call: %+v", un)
	}
}

// A customer episode (MISSING) follows the same rules and belongs to its customer.
func TestM4CustomerEpisode(t *testing.T) {
	w := seed(t)
	key := EpisodeKey{SubjectType: EpisodeCustomer, SubjectID: w.custA.ID, AlarmType: AlarmMissing}
	r := must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, ScopeAll(), []EpisodeKey{key}, t0))
	if len(r.Opened) != 1 || r.Opened[0].CustomerID != w.custA.ID || r.Opened[0].SubjectID != w.custA.ID || r.Opened[0].SubjectType != EpisodeCustomer {
		t.Fatalf("customer episode = %+v", r.Opened)
	}
	if again := must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, ScopeAll(), []EpisodeKey{key}, t0)); len(again.Opened) != 0 {
		t.Fatal("a second MISSING episode opened")
	}
	// Scoped to the other customer, the episode is invisible.
	if l := must[[]Episode](t)(w.s.ListOpenEpisodes(w.ctx, ScopeOf(w.custB.ID))); len(l) != 0 {
		t.Fatalf("B sees A's episode: %+v", l)
	}
}

// A scoped caller syncs only its own customers: it cannot close, or open, another customer's episode.
func TestM4SyncEpisodesIsConfinedToScope(t *testing.T) {
	w := seed(t)
	must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, ScopeAll(), []EpisodeKey{epKey(w.devA.ID, "FAILED"), epKey(w.devB.ID, "FAILED")}, t0))
	// A scoped sync with nothing closes A's episode and leaves B's alone.
	r := must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, ScopeOf(w.custA.ID), nil, t0.Add(time.Minute)))
	if len(r.Closed) != 1 || r.Closed[0].CustomerID != w.custA.ID {
		t.Fatalf("scoped close = %+v", r.Closed)
	}
	open := must[[]Episode](t)(w.s.ListOpenEpisodes(w.ctx, ScopeAll()))
	if len(open) != 1 || open[0].CustomerID != w.custB.ID {
		t.Fatalf("open after the scoped close = %+v", open)
	}
	// P20-T2F item 6: naming a subject outside scope, or one that does not exist, skips that key; it neither
	// opens an episode nor fails the pass.
	outside := epKey(w.devB.ID, "STALE")
	r = must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, ScopeOf(w.custA.ID), []EpisodeKey{outside, epKey(w.devA.ID, "UNSEEN")}, t0.Add(2*time.Minute)))
	if len(r.Opened) != 1 || r.Opened[0].AlarmType != "UNSEEN" || len(r.Skipped) != 1 || r.Skipped[0] != outside {
		t.Fatalf("out-of-scope key: opened %+v skipped %+v, want A's episode opened and the key skipped", r.Opened, r.Skipped)
	}
	missing := EpisodeKey{SubjectType: EpisodeCustomer, SubjectID: w.custB.ID, AlarmType: AlarmMissing}
	if r = must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, ScopeOf(w.custA.ID), []EpisodeKey{epKey(w.devA.ID, "UNSEEN"), missing}, t0.Add(3*time.Minute))); len(r.Opened) != 0 || len(r.Skipped) != 1 || r.Skipped[0] != missing {
		t.Fatalf("out-of-scope customer key = %+v", r)
	}
	ghost := epKey("01JNOSUCHDEVICE0000000000", "STALE")
	if r = must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, ScopeAll(), []EpisodeKey{ghost, epKey(w.devB.ID, "FAILED")}, t0.Add(4*time.Minute))); len(r.Opened) != 0 || len(r.Closed) != 1 || r.Closed[0].AlarmType != "UNSEEN" || len(r.Skipped) != 1 || r.Skipped[0] != ghost {
		t.Fatalf("unknown device = %+v, want it skipped and A's UNSEEN episode (absent from the input) closed", r)
	}
	if open := must[[]Episode](t)(w.s.ListOpenEpisodes(w.ctx, ScopeAll())); len(open) != 1 || open[0].AlarmType != "FAILED" || open[0].CustomerID != w.custB.ID {
		t.Fatalf("the skipped keys opened episodes: %+v", open)
	}
}

// P20-T2F item 6: a device deleted between the read of the fleet and the sync does not fail the
// pass: the rest of it is applied and the key is reported back.
func TestM4SyncEpisodesSkipsASubjectThatVanished(t *testing.T) {
	w := seed(t)
	all := ScopeAll()
	must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, all, []EpisodeKey{epKey(w.devA.ID, "FAILED")}, t0))
	gone := epKey(w.devB.ID, "STALE")
	ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`DELETE FROM devices WHERE id = ?`, w.devB.ID)
		return err
	}))
	cust := EpisodeKey{SubjectType: EpisodeCustomer, SubjectID: w.custA.ID, AlarmType: AlarmMissing}
	r := must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, all, []EpisodeKey{gone, cust}, t0.Add(time.Minute)))
	if len(r.Skipped) != 1 || r.Skipped[0] != gone {
		t.Fatalf("skipped = %+v, want the vanished device", r.Skipped)
	}
	if len(r.Opened) != 1 || r.Opened[0].SubjectType != EpisodeCustomer || len(r.Closed) != 1 || r.Closed[0].SubjectID != w.devA.ID {
		t.Fatalf("the rest of the pass was not applied: opened %+v closed %+v", r.Opened, r.Closed)
	}
	// Idempotent: the same input again opens and closes nothing and reports the same skip.
	again := must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, all, []EpisodeKey{gone, cust}, t0.Add(2*time.Minute)))
	if len(again.Opened) != 0 || len(again.Closed) != 0 || len(again.Skipped) != 1 {
		t.Fatalf("second pass = %+v", again)
	}
}

func TestM4SyncEpisodesValidatesItsKeys(t *testing.T) {
	w := seed(t)
	for name, k := range map[string]EpisodeKey{
		"unknown alarm":            epKey(w.devA.ID, "EXPLODED"),
		"customer alarm on device": epKey(w.devA.ID, AlarmMissing),
		"device alarm on customer": {SubjectType: EpisodeCustomer, SubjectID: w.custA.ID, AlarmType: "FAILED"},
		"unknown subject type":     {SubjectType: "fleet", SubjectID: w.custA.ID, AlarmType: "FAILED"},
		"no subject":               {SubjectType: EpisodeDevice, AlarmType: "FAILED"},
		"lower case":               epKey(w.devA.ID, "failed"),
	} {
		_, err := w.s.SyncEpisodes(w.ctx, ScopeAll(), []EpisodeKey{k}, t0)
		if !errors.Is(err, ErrInvalid) || len(FieldErrors(err)) == 0 {
			t.Errorf("%s: err = %v, want a *ValidationError", name, err)
		}
	}
	if _, err := w.s.SyncEpisodes(w.ctx, Scope{}, nil, t0); !errors.Is(err, ErrScope) {
		t.Errorf("zero scope = %v", err)
	}
	if got := EpisodeAlarmTypes("fleet"); got != nil {
		t.Errorf("alarm types of an unknown subject = %v", got)
	}
	for _, want := range []string{"FAILED", "STALE", "UNSEEN", "BLANK", "STUCK", "IDENTITY_CONFLICT", "UNKNOWN"} {
		found := false
		for _, a := range EpisodeAlarmTypes(EpisodeDevice) {
			found = found || a == want
		}
		if !found {
			t.Errorf("%s (a critical state, ADR 0020 decision 4) is not an episode alarm type", want)
		}
	}
}

// The database itself allows one open episode per subject and alarm type.
func TestM4OneOpenEpisodePerSubjectIsEnforcedByTheDatabase(t *testing.T) {
	w := seed(t)
	must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, ScopeAll(), []EpisodeKey{epKey(w.devA.ID, "FAILED")}, t0))
	err := w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`INSERT INTO alarm_episodes (id, subject_type, device_id, alarm_type, opened_at, state_since, created_at, updated_at)
			VALUES (?, 'device', ?, 'FAILED', ?, ?, ?, ?)`, tx.id(), w.devA.ID, t0.UnixMilli(), t0.UnixMilli(), t0.UnixMilli(), t0.UnixMilli())
		return err
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("a second open episode = %v, want ErrConflict", err)
	}
}

// When an episode closes, the acknowledgement that covered it is cleared with it and audited as the
// system; one for another alarm type is left alone.
func TestM4ClosingAnEpisodeClearsItsAcknowledgement(t *testing.T) {
	w := seed(t)
	actor := AuditActor{AdminID: w.admin.ID, Username: "root", Method: "api"}
	must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, ScopeAll(), []EpisodeKey{epKey(w.devA.ID, "FAILED"), epKey(w.devA.ID, "STALE")}, t0))
	failed := must[Ack](t)(w.s.CreateAck(w.ctx, ScopeAll(), AckInput{SubjectType: EpisodeDevice, SubjectID: w.devA.ID, AlarmType: "failed", Note: "known"}, actor))
	must[Ack](t)(w.s.CreateAck(w.ctx, ScopeAll(), AckInput{SubjectType: EpisodeDevice, SubjectID: w.devA.ID, AlarmType: "stale"}, actor))
	if n := auditCount(t, w, "alarm_ack.clear"); n != 0 {
		t.Fatalf("%d clear rows before anything closed", n)
	}
	r := must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, ScopeAll(), []EpisodeKey{epKey(w.devA.ID, "STALE")}, t0.Add(time.Minute))) // FAILED ends, STALE stays
	if r.ClearedAcks != 1 {
		t.Fatalf("cleared %d acks, want 1", r.ClearedAcks)
	}
	active := must[[]Ack](t)(w.s.ListActiveAcks(w.ctx, ScopeAll()))
	if len(active) != 1 || active[0].AlarmType != "stale" {
		t.Fatalf("active acks = %+v, want only the stale one", active)
	}
	var by, actorType, details string
	ok(t, w.s.Read(w.ctx, func(tx *Tx) error {
		return tx.QueryRow(`SELECT actor_type, COALESCE(actor_id,''), details FROM audit_events WHERE action = 'alarm_ack.clear' AND object_id = ?`, failed.ID).Scan(&actorType, &by, &details)
	}))
	if actorType != "system" || by != "" || !strings.Contains(details, "alarm_resolved") || strings.Contains(details, "known") {
		t.Fatalf("clear audit = %s %q %s", actorType, by, details)
	}
	// Closing again clears nothing more.
	if again := must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, ScopeAll(), []EpisodeKey{epKey(w.devA.ID, "STALE")}, t0.Add(2*time.Minute))); again.ClearedAcks != 0 {
		t.Fatalf("second close cleared %d", again.ClearedAcks)
	}
}
