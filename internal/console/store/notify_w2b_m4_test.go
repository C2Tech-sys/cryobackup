package store

import (
	"errors"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/policy"
)

// P20-W2B item 7: an open episode still marked at the batch id leaves the batch; a closed one, one
// marked at another time, and an id outside scope are left alone.
func TestM4W2BUnmarkEpisodesNotified(t *testing.T) {
	w := seed(t)
	all := ScopeAll()
	sync := must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, all, []EpisodeKey{epKey(w.devA.ID, "FAILED"), epKey(w.devA.ID, "STALE"), epKey(w.devB.ID, "FAILED")}, t0))
	if len(sync.Opened) != 3 {
		t.Fatalf("opened %d", len(sync.Opened))
	}
	aFailed, aStale, bFailed := sync.Opened[0].ID, sync.Opened[1].ID, sync.Opened[2].ID
	batch, other := t0.Add(time.Second), t0.Add(2*time.Second)
	must[int](t)(w.s.MarkEpisodesNotified(w.ctx, all, []string{aFailed, bFailed}, batch))
	must[int](t)(w.s.MarkEpisodesNotified(w.ctx, all, []string{aStale}, other))

	if _, err := w.s.UnmarkEpisodesNotified(w.ctx, ScopeOf(w.custA.ID), []string{aFailed, bFailed}, batch); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an id outside scope: %v", err)
	}
	if got := must[[]Episode](t)(w.s.ListEpisodesNotifiedAt(w.ctx, all, batch)); len(got) != 2 {
		t.Fatalf("a refused call changed the batch: %+v", got)
	}
	if n := must[int](t)(w.s.UnmarkEpisodesNotified(w.ctx, all, []string{aFailed, aStale}, batch)); n != 1 {
		t.Fatalf("changed %d, want 1 (the other is marked at another time)", n)
	}
	un := must[[]Episode](t)(w.s.ListUnnotifiedEpisodes(w.ctx, all))
	if len(un) != 1 || un[0].ID != aFailed {
		t.Fatalf("unnotified = %+v", un)
	}
	// Closed: left alone, so its recovery stays owed.
	w.clk.Advance(time.Minute)
	must[EpisodeSync](t)(w.s.SyncEpisodes(w.ctx, all, []EpisodeKey{epKey(w.devA.ID, "FAILED"), epKey(w.devA.ID, "STALE")}, w.clk.Now()))
	if n := must[int](t)(w.s.UnmarkEpisodesNotified(w.ctx, all, []string{bFailed}, batch)); n != 0 {
		t.Fatalf("a closed episode was unmarked: %d", n)
	}
	if due := must[[]Episode](t)(w.s.ListRecoveryDueEpisodes(w.ctx, all)); len(due) != 1 || due[0].ID != bFailed {
		t.Fatalf("recovery due = %+v", due)
	}
}

// addW2BMatrix adds the P20-W2B method to the §3.5 scope matrix. It needs one line in matrix()
// (scope_test.go), next to addW2AMatrix: addW2BMatrix(m).
func addW2BMatrix(m map[string]matrixCase) {
	batch := time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC)
	m["UnmarkEpisodesNotified"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
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
		_, err = w.s.UnmarkEpisodesNotified(w.ctx, sc, []string{id}, batch)
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
