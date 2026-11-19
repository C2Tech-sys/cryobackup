package store

import (
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// P20-T2F batch 2 item 3: the read path honours an acknowledgement only while an episode of the same
// subject and type is open. A row written by SQL (or before v8) with no episode shows no
// acknowledgement, mutes nothing and is not listed; once the episode is open it counts.
func TestM4ReadPathRequiresAnOpenEpisode(t *testing.T) {
	w := seed(t)
	all := ScopeAll()
	w.clk.Advance(48 * time.Hour)
	w.heartbeat(w.devA.ID)
	ingest(t, w, w.devA.ID, runInput(runID2, w.clk.Now().Add(-40*time.Hour)))
	fail := runInput(runID1, w.clk.Now().Add(-30*time.Minute))
	fail.Outcome, fail.ErrorClass = "failed", "disk"
	ingest(t, w, w.devA.ID, fail)
	if !containsState(flagsOf(t, w, w.devA.ID), state.StateFailed) {
		t.Fatalf("the device is not FAILED (flags %v): the test needs a failing device", flagsOf(t, w, w.devA.ID))
	}
	now := w.clk.Now().UnixMilli()
	ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`INSERT INTO alarm_acks (id, subject_type, device_id, alarm_type, acknowledged_at, created_at, updated_at)
			VALUES ('01JRAWACK000000000000000A1', 'device', ?, 'failed', ?, ?, ?)`, w.devA.ID, now, now, now)
		return err
	}))

	// No episode: not acknowledged, not muted, not listed.
	f := facts(t, w, w.devA.ID)
	ds := state.Classify(f, w.clk.Now())
	if f.Ack != nil || ds.Ack != nil || containsAttr(ds.Attrs, state.AttrAcknowledged) {
		t.Fatalf("an ack with no open episode is honoured: facts %+v, state ack %+v, attrs %v", f.Ack, ds.Ack, ds.Attrs)
	}
	if got := must[[]Ack](t)(w.s.ListActiveAcks(w.ctx, all)); len(got) != 0 {
		t.Fatalf("an ack with no open episode is listed: %+v", got)
	}

	// An episode of another type does not do either; one of the same type does.
	rawEpisode := func(alarm string) {
		ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
			_, err := tx.Exec(`INSERT INTO alarm_episodes (id, subject_type, device_id, alarm_type, opened_at, state_since, created_at, updated_at)
				VALUES (?, 'device', ?, ?, ?, ?, ?, ?)`, tx.id(), w.devA.ID, alarm, now, now, now, now)
			return err
		}))
	}
	rawEpisode("STALE")
	if f := facts(t, w, w.devA.ID); f.Ack != nil {
		t.Fatalf("an open episode of another type makes the ack count: %+v", f.Ack)
	}
	if got := must[[]Ack](t)(w.s.ListActiveAcks(w.ctx, all)); len(got) != 0 {
		t.Fatalf("listed on an episode of another type: %+v", got)
	}
	rawEpisode("FAILED")
	if f := facts(t, w, w.devA.ID); f.Ack == nil {
		t.Fatal("an ack with its open episode is not read")
	}
	if got := must[[]Ack](t)(w.s.ListActiveAcks(w.ctx, all)); len(got) != 1 {
		t.Fatalf("an ack with its open episode is not listed: %+v", got)
	}
	// A closed episode no longer counts.
	ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE alarm_episodes SET closed_at = ? WHERE alarm_type = 'FAILED'`, now)
		return err
	}))
	if f := facts(t, w, w.devA.ID); f.Ack != nil {
		t.Fatalf("a closed episode keeps the ack alive: %+v", f.Ack)
	}
	if got := must[[]Ack](t)(w.s.ListActiveAcks(w.ctx, all)); len(got) != 0 {
		t.Fatalf("listed on a closed episode: %+v", got)
	}
}

func containsState(l []state.State, s state.State) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
