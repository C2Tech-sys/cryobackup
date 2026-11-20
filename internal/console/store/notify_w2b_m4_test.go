package store

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// P20-W2BF C1: a closed episode left out of a retried alert is settled in one transaction with the
// check of its batch's rows: not while another row of the batch is sent or queued and never
// attempted; a sibling that failed with a retry scheduled does not count, nor does the retried row
// itself. An open episode, one marked at another time, and an id outside scope are left alone.
func TestM4W2BFSettleClosedLeftOut(t *testing.T) {
	w := seed(t)
	all := ScopeAll()
	sync := must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, all, []EpisodeKey{epKey(w.devA.ID, "FAILED"), epKey(w.devA.ID, "STALE"), epKey(w.devB.ID, "FAILED")}, t0))
	open, other, closed := sync.Opened[0].ID, sync.Opened[1].ID, sync.Opened[2].ID
	batch, elsewhere := t0.Add(time.Second), t0.Add(2*time.Second)
	must[int](t)(w.s.MarkEpisodesNotified(w.ctx, all, []string{open, closed}, batch))
	must[int](t)(w.s.MarkEpisodesNotified(w.ctx, all, []string{other}, elsewhere))
	w.clk.Advance(time.Minute)
	must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, all, []EpisodeKey{epKey(w.devA.ID, "FAILED"), epKey(w.devA.ID, "STALE")}, w.clk.Now()))
	key := "alert:" + strconv.FormatInt(batch.UnixMilli(), 10) + ":"
	mine := enqueueM4(t, w, DeliveryAlert, key+"a")
	sibling := enqueueM4(t, w, DeliveryAlert, key+"b") // queued, never attempted: in flight
	settle := t0.Add(3 * time.Second)
	if _, err := w.s.SettleClosedLeftOut(w.ctx, ScopeOf(w.custA.ID), []string{closed}, batch, settle, mine.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an id outside scope: %v", err)
	}
	if n := must[int](t)(w.s.SettleClosedLeftOut(w.ctx, all, []string{open, closed}, batch, settle, mine.ID)); n != 0 {
		t.Fatalf("settled %d while a sibling is in flight", n)
	}
	retry := w.clk.Now().Add(time.Minute)
	must[Delivery](t)(w.s.MarkDeliveryFailed(w.ctx, all, sibling.ID, "timeout", &retry)) // failed, retry scheduled: cannot carry it
	if n := must[int](t)(w.s.SettleClosedLeftOut(w.ctx, all, []string{open, other, closed}, batch, settle, mine.ID)); n != 1 {
		t.Fatalf("settled %d, want 1 (only the closed episode of the batch)", n)
	}
	if un := must[[]Episode](t)(w.s.ListUnnotifiedEpisodes(w.ctx, all)); len(un) != 0 {
		t.Fatalf("an episode left its batch: %+v", un)
	}
	if got := must[[]Episode](t)(w.s.ListEpisodesRecoveryNotifiedAt(w.ctx, all, settle)); len(got) != 1 || got[0].ID != closed {
		t.Fatalf("settled = %+v", got)
	}
}

// addW2BMatrix adds the P20-W2B method to the §3.5 scope matrix. It needs one line in matrix()
// (scope_test.go), next to addW2AMatrix: addW2BMatrix(m).
func addW2BMatrix(m map[string]matrixCase) {
	batch := time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC)
	m["SettleClosedLeftOut"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
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
		_, err = w.s.SettleClosedLeftOut(w.ctx, sc, []string{id}, batch, batch.Add(time.Second), "")
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
