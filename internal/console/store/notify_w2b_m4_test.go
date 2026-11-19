package store

import (
	"errors"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// P20-W2B item 7 and P20-W2BF item 6: leaving an alert batch is one transaction. An open episode
// still marked at the batch id is released (unmarked); one that closed meanwhile is settled
// (recovery done, no message); one marked at another time, and an id outside scope, are left alone.
func TestM4W2BLeaveAlertBatch(t *testing.T) {
	w := seed(t)
	all := ScopeAll()
	sync := must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, all, []EpisodeKey{epKey(w.devA.ID, "FAILED"), epKey(w.devA.ID, "STALE"), epKey(w.devB.ID, "FAILED")}, t0))
	if len(sync.Opened) != 3 {
		t.Fatalf("opened %d", len(sync.Opened))
	}
	aFailed, aStale, bFailed := sync.Opened[0].ID, sync.Opened[1].ID, sync.Opened[2].ID
	batch, other, settle := t0.Add(time.Second), t0.Add(2*time.Second), t0.Add(3*time.Second)
	must[int](t)(w.s.MarkEpisodesNotified(w.ctx, all, []string{aFailed, bFailed}, batch))
	must[int](t)(w.s.MarkEpisodesNotified(w.ctx, all, []string{aStale}, other))

	if _, _, err := w.s.LeaveAlertBatch(w.ctx, ScopeOf(w.custA.ID), []string{aFailed, bFailed}, batch, settle); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an id outside scope: %v", err)
	}
	if got := must[[]Episode](t)(w.s.ListEpisodesNotifiedAt(w.ctx, all, batch)); len(got) != 2 {
		t.Fatalf("a refused call changed the batch: %+v", got)
	}
	// bFailed closes before the call (the race of item 6); aStale is marked at another time.
	w.clk.Advance(time.Minute)
	must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, all, []EpisodeKey{epKey(w.devA.ID, "FAILED"), epKey(w.devA.ID, "STALE")}, w.clk.Now()))
	released, settled, err := w.s.LeaveAlertBatch(w.ctx, all, []string{aFailed, aStale, bFailed}, batch, settle)
	if err != nil || released != 1 || settled != 1 {
		t.Fatalf("released %d settled %d (%v), want 1 and 1", released, settled, err)
	}
	if un := must[[]Episode](t)(w.s.ListUnnotifiedEpisodes(w.ctx, all)); len(un) != 1 || un[0].ID != aFailed {
		t.Fatalf("unnotified = %+v", un)
	}
	if due := must[[]Episode](t)(w.s.ListRecoveryDueEpisodes(w.ctx, all)); len(due) != 0 {
		t.Fatalf("a settled episode still owes a recovery: %+v", due)
	}
	if got := must[[]Episode](t)(w.s.ListEpisodesRecoveryNotifiedAt(w.ctx, all, settle)); len(got) != 1 || got[0].ID != bFailed {
		t.Fatalf("settled = %+v", got)
	}
}

// addW2BMatrix adds the P20-W2B method to the §3.5 scope matrix. It needs one line in matrix()
// (scope_test.go), next to addW2AMatrix: addW2BMatrix(m).
func addW2BMatrix(m map[string]matrixCase) {
	batch := time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC)
	m["LeaveAlertBatch"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		open, err := w.s.ListOpenEpisodes(w.ctx, ScopeAll())
		if err != nil {
			return nil, err
		}
		keys := []EpisodeKey{{SubjectType: EpisodeDevice, SubjectID: t.dev, AlarmType: "BLANK"}}
		for _, e := range open {
			keys = append(keys, EpisodeKey{SubjectType: e.SubjectType, SubjectID: e.SubjectID, AlarmType: e.AlarmType})
		}
		if _, err := w.s.SyncEpisodes(w.ctx, ScopeAll(), keys, time.Time{}); err != nil {
			return nil, err
		}
		if open, err = w.s.ListOpenEpisodes(w.ctx, ScopeAll()); err != nil {
			return nil, err
		}
		id := ""
		for _, e := range open {
			if e.SubjectID == t.dev && e.AlarmType == "BLANK" {
				id = e.ID
			}
		}
		if id == "" {
			return nil, errors.New("the seeded episode is missing")
		}
		if _, err := w.s.MarkEpisodesNotified(w.ctx, ScopeAll(), []string{id}, batch); err != nil {
			return nil, err
		}
		_, _, err = w.s.LeaveAlertBatch(w.ctx, sc, []string{id}, batch, batch.Add(time.Second))
		return nil, err
	}}
}

// P20-W2BF item 3: notification_overrides.digest_include reaches the engine. Off excludes the
// device from the digest; on and inherit (nil) do not.
func TestM4W2BFThresholdsCarryDigestInclude(t *testing.T) {
	off, on := false, true
	for _, tc := range []struct {
		include *bool
		want    bool
	}{{nil, false}, {&on, false}, {&off, true}} {
		p := policy.Default(policy.ClassUnknown)
		p.NotificationOverrides.DigestInclude = tc.include
		if got := ThresholdsFromPolicy(p).DigestExcluded; got != tc.want {
			t.Errorf("digest_include %v: excluded %v, want %v", tc.include, got, tc.want)
		}
	}
}

// P20-W2BF item 4: a forced send-now ("digest-forced:...") is not the day's digest, so it does
// not count for C14; a daily row ("digest:<date>:<rule>") does.
func TestM4W2BFForcedDigestIsNotTheDaysDigest(t *testing.T) {
	w := seed(t)
	sendM4(t, w, DeliveryDigest, "digest-forced:1794949440000:r1", 0, true)
	f := must[SendFacts](t)(w.s.LoadSendFacts(w.ctx, ScopeAll()))
	if !f.LastDigestSentAt.IsZero() {
		t.Fatalf("a forced send counts as the day's digest: %v", f.LastDigestSentAt)
	}
	d := sendM4(t, w, DeliveryDigest, "digest:2026-10-22:r1", 0, true)
	f = must[SendFacts](t)(w.s.LoadSendFacts(w.ctx, ScopeAll()))
	if !f.LastDigestSentAt.Equal(d.SentAt) {
		t.Fatalf("the daily digest: %v, want %v", f.LastDigestSentAt, d.SentAt)
	}
}

// P20-W2BF item 5: the alert, recovery and digest rows that became final without being sent in
// the last 24 hours. Suppressed rows, test sends, rows still in retry and older failures do not
// count.
func TestM4W2BFUndeliveredInTheLastDay(t *testing.T) {
	w := seed(t)
	sendM4(t, w, DeliveryAlert, "alert:old", 4, false) // failed for good...
	w.clk.Advance(25 * time.Hour)                      // ...more than a day ago
	sendM4(t, w, DeliveryDigest, "digest:2026-10-23:a", 4, false)
	sendM4(t, w, DeliveryRecovery, "recovery:1:a", 4, false)
	sendM4(t, w, DeliveryTest, "test:1", 4, false)
	sendM4(t, w, DeliveryAlert, "alert:retrying", 1, false) // a retry is still scheduled
	sendM4(t, w, DeliveryDigest, "digest:2026-10-23:b", 0, true)
	d := enqueueM4(t, w, DeliveryAlert, "alert:suppressed")
	must[Delivery](t)(w.s.SuppressDelivery(w.ctx, ScopeAll(), d.ID, "deduped"))
	f := must[SendFacts](t)(w.s.LoadSendFacts(w.ctx, ScopeAll()))
	if f.Undelivered24h != 2 {
		t.Fatalf("undelivered in the last day = %d, want 2", f.Undelivered24h)
	}
	smtp, _ := mailFactsOf(t)(w.s.MailFacts(w.ctx, ScopeAll(), state.ProbeRun{}))
	if smtp.Undelivered24h != 2 {
		t.Fatalf("SMTP facts %+v", smtp)
	}
}

func mailFactsOf(t *testing.T) func(*state.SMTPFacts, *state.DigestFacts, error) (*state.SMTPFacts, *state.DigestFacts) {
	return func(s *state.SMTPFacts, d *state.DigestFacts, err error) (*state.SMTPFacts, *state.DigestFacts) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return s, d
	}
}
