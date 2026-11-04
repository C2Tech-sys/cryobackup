package health

import (
	"context"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// §4.7 (test E04 through the loader): a paused device's strip is none only from
// the pause day on; the days before keep their runs. Before M3-T2 the loader
// passed a 1970 pause, greying all ten days.
func TestE04TenDayPausedFromThePauseDayOnly(t *testing.T) {
	e, _ := newEval(t)
	ctx := context.Background()
	f, _ := e.Fleet(ctx, store.ScopeAll())
	loc, _ := time.LoadLocation("America/Chicago")
	var dev *state.DeviceState
	for i := range f.View.Devices {
		if f.View.Devices[i].Hostname == "OPS-ND-01" {
			dev = &f.View.Devices[i]
		}
	}
	if dev == nil {
		t.Fatal("fixture device OPS-ND-01 missing")
	}
	// Pause it three local days ago at noon (the view is what the engine said; only the pause changes).
	pausedAt := time.Date(fixtures.Now().In(loc).Year(), fixtures.Now().In(loc).Month(), fixtures.Now().In(loc).Day()-3, 12, 0, 0, 0, loc)
	dev.Primary, dev.Since = state.StatePaused, pausedAt.UTC()
	strips, err := TenDay(ctx, e.Store, store.ScopeAll(), f.View, fixtures.Now(), loc)
	if err != nil {
		t.Fatal(err)
	}
	s := strips[dev.ID]
	for i := StripDays - 4; i < StripDays; i++ {
		if s[i].Kind != DayNone {
			t.Fatalf("cell %d (pause day on) is %s, want none: %+v", i, s[i].Kind, s)
		}
	}
	green := 0
	for i := 0; i < StripDays-4; i++ {
		if s[i].Kind == DaySuccess {
			green++
		}
	}
	if green == 0 {
		t.Fatalf("the days before the pause lost their runs: %+v", s)
	}
}

// §4.7: a device whose policy has no schedule never shows a missed day (the
// engine's no_schedule attribute reaches the strip).
func TestE04TenDayNoScheduleNeverMissed(t *testing.T) {
	e, _ := newEval(t)
	ctx := context.Background()
	f, _ := e.Fleet(ctx, store.ScopeAll())
	loc, _ := time.LoadLocation("America/Chicago")
	strips, err := TenDay(ctx, e.Store, store.ScopeAll(), f.View, fixtures.Now(), loc)
	if err != nil {
		t.Fatal(err)
	}
	var dev *state.DeviceState
	for i := range f.View.Devices {
		for _, c := range strips[f.View.Devices[i].ID] {
			if c.Kind == DayMissed {
				dev = &f.View.Devices[i]
			}
		}
	}
	if dev == nil {
		t.Fatal("the fixture fleet has no device with a missed day to test against")
	}
	dev.Attrs = append(dev.Attrs, state.AttrNoSchedule)
	strips, err = TenDay(ctx, e.Store, store.ScopeAll(), f.View, fixtures.Now(), loc)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range strips[dev.ID] {
		if c.Kind == DayMissed {
			t.Fatalf("cell %d missed for a device without a schedule: %+v", i, strips[dev.ID])
		}
	}
}
