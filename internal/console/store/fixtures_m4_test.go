package store

import (
	"context"
	"testing"
)

// P20-T2F batch 2 item 2: the fixture world obeys the rule CreateAck enforces. Every uncleared
// acknowledgement has an open episode of its subject and type, opened before the ack was made, so the
// first SyncEpisodes pass that names the alarm does not clear it as stale.
func TestM4FixtureAcknowledgementsHaveTheirOpenEpisode(t *testing.T) {
	s, clk := loadedFixtures(t)
	ctx := context.Background()
	type ack struct {
		id, dev, kind string
		at            int64
	}
	var acks []ack
	orphans := 0
	ok(t, s.Read(ctx, func(tx *Tx) error {
		rows, err := collect(tx, `SELECT id, device_id, alarm_type, acknowledged_at FROM alarm_acks WHERE cleared_at IS NULL AND device_id IS NOT NULL`, nil,
			func(sc scanner) (ack, error) {
				var a ack
				return a, sc.Scan(&a.id, &a.dev, &a.kind, &a.at)
			})
		acks = rows
		for _, a := range rows {
			alarm, mapped := EpisodeAlarmOfAck(a.kind)
			var opened int64
			err := tx.QueryRow(`SELECT opened_at FROM alarm_episodes WHERE device_id = ? AND alarm_type = ? AND closed_at IS NULL`, a.dev, alarm).Scan(&opened)
			if !mapped || err != nil || opened >= a.at {
				orphans++
			}
		}
		return err
	}))
	if len(acks) == 0 {
		t.Fatal("the fixtures hold no acknowledgement: the test checks nothing")
	}
	if orphans != 0 {
		t.Fatalf("%d fixture acknowledgements have no open episode opened before them", orphans)
	}
	// A pass that names the alarm leaves the acknowledgement in place.
	r := must[EpisodeSync](t)(s.SyncEpisodes(ctx, ScopeAll(), []EpisodeKey{{SubjectType: EpisodeDevice, SubjectID: acks[0].dev, AlarmType: "STALE"}}, clk.Now()))
	if r.ClearedAcks != 0 || len(r.Opened) != 0 {
		t.Fatalf("sync = %+v, want the fixture episode kept and its ack untouched", r)
	}
	var cleared int
	ok(t, s.Read(ctx, func(tx *Tx) error {
		return tx.QueryRow(`SELECT count(*) FROM alarm_acks WHERE id = ? AND cleared_at IS NOT NULL`, acks[0].id).Scan(&cleared)
	}))
	if cleared != 0 {
		t.Fatal("the fixture acknowledgement was cleared")
	}
}
