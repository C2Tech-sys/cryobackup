package store

import (
	"errors"
	"testing"
	"time"
)

// The episodes of a batch are found again from the batch id (the time they were marked) alone.
func TestM4W2AEpisodesOfABatchAreFoundByTheirMarkTime(t *testing.T) {
	w := seed(t)
	all := ScopeAll()
	sync := must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, all, []EpisodeKey{epKey(w.devA.ID, "FAILED"), epKey(w.devB.ID, "STALE"),
		{SubjectType: EpisodeCustomer, SubjectID: w.custB.ID, AlarmType: AlarmMissing}}, t0))
	if len(sync.Opened) != 3 {
		t.Fatalf("opened %d", len(sync.Opened))
	}
	batch1, batch2 := t0.Add(time.Second), t0.Add(2*time.Second)
	must[int](t)(w.s.MarkEpisodesNotified(w.ctx, all, []string{sync.Opened[0].ID, sync.Opened[2].ID}, batch1))
	must[int](t)(w.s.MarkEpisodesNotified(w.ctx, all, []string{sync.Opened[1].ID}, batch2))

	got := must[[]Episode](t)(w.s.ListEpisodesNotifiedAt(w.ctx, all, batch1))
	if len(got) != 2 || got[0].ID != sync.Opened[0].ID || got[1].ID != sync.Opened[2].ID {
		t.Fatalf("batch 1 = %+v", got)
	}
	if got := must[[]Episode](t)(w.s.ListEpisodesNotifiedAt(w.ctx, all, batch2)); len(got) != 1 || got[0].ID != sync.Opened[1].ID {
		t.Fatalf("batch 2 = %+v", got)
	}
	if got := must[[]Episode](t)(w.s.ListEpisodesNotifiedAt(w.ctx, all, t0.Add(3*time.Second))); len(got) != 0 {
		t.Fatalf("an unknown batch id = %+v", got)
	}
	// Scope applies: customer A sees only its own episode of batch 1.
	if got := must[[]Episode](t)(w.s.ListEpisodesNotifiedAt(w.ctx, ScopeOf(w.custA.ID), batch1)); len(got) != 1 || got[0].CustomerID != w.custA.ID {
		t.Fatalf("scoped batch 1 = %+v", got)
	}

	// Recovery batches by their own column; a closed episode stays in its alert batch.
	w.clk.Advance(time.Minute)
	must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, all, []EpisodeKey{epKey(w.devB.ID, "STALE")}, w.clk.Now()))
	rec := w.clk.Now().Add(time.Second)
	must[int](t)(w.s.MarkRecoveryNotified(w.ctx, all, []string{sync.Opened[0].ID, sync.Opened[2].ID}, rec))
	if got := must[[]Episode](t)(w.s.ListEpisodesRecoveryNotifiedAt(w.ctx, all, rec)); len(got) != 2 || got[0].Open() {
		t.Fatalf("recovery batch = %+v", got)
	}
	if got := must[[]Episode](t)(w.s.ListEpisodesNotifiedAt(w.ctx, all, batch1)); len(got) != 2 || got[0].Open() {
		t.Fatalf("closed episodes leave their alert batch: %+v", got)
	}
}

// A snooze that ends while the alarm persists re-arms the alert exactly once; a plain
// acknowledgement and a snooze in force are left alone.
func TestM4W2ARearmLapsedSnoozesReRaisesOnce(t *testing.T) {
	w := seed(t)
	all, actor := ScopeAll(), m4Actor(w)
	openAlarm(t, w, EpisodeDevice, w.devA.ID, "stale")
	openAlarm(t, w, EpisodeDevice, w.devA.ID, "failed")
	openAlarm(t, w, EpisodeDevice, w.devB.ID, "stale")
	open := must[[]Episode](t)(w.s.ListOpenEpisodes(w.ctx, all))
	var ids []string
	for _, e := range open {
		ids = append(ids, e.ID)
	}
	must[int](t)(w.s.MarkEpisodesNotified(w.ctx, all, ids, t0))
	until := t0.Add(time.Hour)
	snooze := must[Ack](t)(w.s.CreateAck(w.ctx, all, AckInput{SubjectType: EpisodeDevice, SubjectID: w.devA.ID, AlarmType: "stale", SnoozeUntil: &until}, actor))
	must[Ack](t)(w.s.CreateAck(w.ctx, all, ackIn(w.devA.ID, "failed"), actor))
	later := t0.Add(10 * time.Hour)
	must[Ack](t)(w.s.CreateAck(w.ctx, all, AckInput{SubjectType: EpisodeDevice, SubjectID: w.devB.ID, AlarmType: "stale", SnoozeUntil: &later}, actor))

	// Nothing has lapsed yet.
	if n := must[int](t)(w.s.RearmLapsedSnoozes(w.ctx, all)); n != 0 {
		t.Fatalf("re-armed %d before any snooze ended", n)
	}
	if un := must[[]Episode](t)(w.s.ListUnnotifiedEpisodes(w.ctx, all)); len(un) != 0 {
		t.Fatalf("unnotified before: %+v", un)
	}

	w.clk.Advance(2 * time.Hour) // device A's snooze ends; device B's does not
	if n := must[int](t)(w.s.RearmLapsedSnoozes(w.ctx, all)); n != 1 {
		t.Fatalf("re-armed %d, want 1", n)
	}
	un := must[[]Episode](t)(w.s.ListUnnotifiedEpisodes(w.ctx, all))
	if len(un) != 1 || un[0].SubjectID != w.devA.ID || un[0].AlarmType != "STALE" {
		t.Fatalf("unnotified after the snooze ended = %+v", un)
	}
	acks := must[[]Ack](t)(w.s.ListActiveAcks(w.ctx, all))
	if len(acks) != 2 {
		t.Fatalf("active acks = %+v, want the plain ack and B's snooze", acks)
	}
	var cleared *time.Time
	ok(t, w.s.Read(w.ctx, func(tx *Tx) error {
		var ms *int64
		if err := tx.QueryRow(`SELECT cleared_at FROM alarm_acks WHERE id = ?`, snooze.ID).Scan(&ms); err != nil {
			return err
		}
		if ms != nil {
			c := timeOf(*ms)
			cleared = &c
		}
		return nil
	}))
	if cleared == nil || !cleared.Equal(w.clk.Now()) {
		t.Fatalf("the lapsed snooze was not cleared: %v", cleared)
	}
	var reason string
	ok(t, w.s.Read(w.ctx, func(tx *Tx) error {
		return tx.QueryRow(`SELECT json_extract(details, '$.reason') FROM audit_events WHERE action = 'alarm_ack.clear' AND object_id = ? AND actor_type = 'system'`, snooze.ID).Scan(&reason)
	}))
	if reason != ackClearSnoozeEnded {
		t.Fatalf("audit reason = %q", reason)
	}

	// Once mailed again, it stays mailed: the cleared snooze cannot lapse a second time.
	must[int](t)(w.s.MarkEpisodesNotified(w.ctx, all, []string{un[0].ID}, w.clk.Now()))
	w.clk.Advance(time.Hour)
	if n := must[int](t)(w.s.RearmLapsedSnoozes(w.ctx, all)); n != 0 {
		t.Fatalf("re-armed %d on the second call", n)
	}
	if un := must[[]Episode](t)(w.s.ListUnnotifiedEpisodes(w.ctx, all)); len(un) != 0 {
		t.Fatalf("unnotified after the re-raise went out: %+v", un)
	}
}

// A snooze until the next success lapses when a backup succeeds after it.
func TestM4W2ARearmAfterTheNextSuccess(t *testing.T) {
	w := seed(t)
	all := ScopeAll()
	openAlarm(t, w, EpisodeDevice, w.devB.ID, "failed")
	ingest(t, w, w.devB.ID, runInput(runID1, t0.Add(-5*time.Hour)))
	must[Ack](t)(w.s.CreateAck(w.ctx, all, AckInput{SubjectType: EpisodeDevice, SubjectID: w.devB.ID, AlarmType: "failed", UntilNextSuccess: true}, m4Actor(w)))
	ids := []string{must[[]Episode](t)(w.s.ListOpenEpisodes(w.ctx, all))[0].ID}
	must[int](t)(w.s.MarkEpisodesNotified(w.ctx, all, ids, t0))
	if n := must[int](t)(w.s.RearmLapsedSnoozes(w.ctx, all)); n != 0 {
		t.Fatalf("re-armed %d with no success since the snooze", n)
	}
	w.clk.Advance(time.Hour)
	ingest(t, w, w.devB.ID, runInput(runID2, w.clk.Now().Add(-time.Minute)))
	if n := must[int](t)(w.s.RearmLapsedSnoozes(w.ctx, all)); n != 1 {
		t.Fatalf("re-armed %d after a success, want 1", n)
	}
}

func TestM4W2ASuppressDelivery(t *testing.T) {
	w := seed(t)
	all := ScopeAll()
	q := enqueueM4(t, w, DeliveryAlert, "alert:1:q")
	w.clk.Advance(time.Minute)
	d := must[Delivery](t)(w.s.SuppressDelivery(w.ctx, all, q.ID, "deduped"))
	if d.Result != DeliverySuppressed || d.SuppressReason != "deduped" || d.NextRetryAt != nil || !d.SentAt.Equal(w.clk.Now()) {
		t.Fatalf("suppressed queued row = %+v", d)
	}
	if again := must[Delivery](t)(w.s.SuppressDelivery(w.ctx, all, q.ID, "acknowledged")); again.SuppressReason != "deduped" {
		t.Fatalf("a second suppress changed the row: %+v", again)
	}

	// A failed row with a retry pending: suppressed, the class kept, the counter untouched.
	f := enqueueM4(t, w, DeliveryAlert, "alert:1:f")
	next := w.clk.Now().Add(time.Minute)
	must[Delivery](t)(w.s.MarkDeliveryFailed(w.ctx, all, f.ID, "timeout", &next))
	before := must[SendFacts](t)(w.s.LoadSendFacts(w.ctx, all))
	d = must[Delivery](t)(w.s.SuppressDelivery(w.ctx, all, f.ID, "acknowledged"))
	if d.Result != DeliverySuppressed || d.ErrorClass != "timeout" || d.NextRetryAt != nil {
		t.Fatalf("suppressed failed row = %+v", d)
	}
	after := must[SendFacts](t)(w.s.LoadSendFacts(w.ctx, all))
	if after.ConsecutiveFailures != before.ConsecutiveFailures || after.PendingAlerts != 0 {
		t.Fatalf("send facts before %+v after %+v", before, after)
	}
	if due := must[[]Delivery](t)(w.s.ListDueRetries(w.ctx, all, w.clk.Now().Add(time.Hour), 0)); len(due) != 0 {
		t.Fatalf("suppressed rows are still due: %+v", due)
	}

	// Final rows refuse; bad input is reported.
	s := enqueueM4(t, w, DeliveryAlert, "alert:1:s")
	must[Delivery](t)(w.s.MarkDeliverySent(w.ctx, all, s.ID))
	if _, err := w.s.SuppressDelivery(w.ctx, all, s.ID, "deduped"); !errors.Is(err, ErrConflict) {
		t.Fatalf("suppress a sent row: %v", err)
	}
	g := enqueueM4(t, w, DeliveryAlert, "alert:1:g")
	must[Delivery](t)(w.s.MarkDeliveryFailed(w.ctx, all, g.ID, "auth_rejected", nil))
	if _, err := w.s.SuppressDelivery(w.ctx, all, g.ID, "deduped"); !errors.Is(err, ErrConflict) {
		t.Fatalf("suppress a row that gave up: %v", err)
	}
	var ve *ValidationError
	if _, err := w.s.SuppressDelivery(w.ctx, all, q.ID, "resolved"); !errors.As(err, &ve) {
		t.Fatalf("unknown reason: %v", err)
	}
	if _, err := w.s.SuppressDelivery(w.ctx, all, "01J9Y4K3A0B1C2D3E4F5G6H7ZZ", "deduped"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := w.s.SuppressDelivery(w.ctx, ScopeOf(w.custA.ID), q.ID, "deduped"); !errors.Is(err, ErrScope) {
		t.Fatalf("customer scope: %v", err)
	}
}
