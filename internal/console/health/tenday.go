package health

import (
	"context"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// DayKind is the kind of one cell of the ten-day strip (components.md §4.1).
type DayKind string

// Cell kinds.
const (
	DayNone    DayKind = "none"    // before enrollment, parked, or today with no run yet
	DaySuccess DayKind = "success" // at least one success and no failure that day
	DayMixed   DayKind = "mixed"   // a success and a failure
	DayFailed  DayKind = "failed"  // failures and no success
	DayMissed  DayKind = "missed"  // a day a run was expected and none finished
)

// Day is one cell: a calendar day in the customer's zone.
type Day struct {
	Kind    DayKind
	Date    time.Time // local midnight
	Success int
	Failed  int
}

// StripDays is the number of cells.
const StripDays = 10

// TenDay computes the strip of every non-retired device of view: finished
// backup runs bucketed into calendar days in the customer's display zone
// (defaultLoc when the customer sets none). A paused device renders all cells
// as none. The store (TenDayCounts) groups by hour so the work is bounded by
// hours, not runs; a day edge in a half-hour zone can be off by the hour.
func TenDay(ctx context.Context, st *store.Store, scope store.Scope, view state.FleetView, now time.Time, defaultLoc *time.Location) (map[string][StripDays]Day, error) {
	custs, err := st.ListCustomers(ctx, scope)
	if err != nil {
		return nil, err
	}
	locOf := map[string]*time.Location{}
	for _, c := range custs {
		loc := defaultLoc
		if c.DisplayTZ != "" {
			if l, err := time.LoadLocation(c.DisplayTZ); err == nil {
				loc = l
			}
		}
		locOf[c.ID] = loc
	}
	enrolled := map[string]time.Time{}
	for cursor := ""; ; {
		l, err := st.ListDevices(ctx, scope, store.ListDevicesOptions{IncludeRetired: false, Page: store.Page{Limit: 1000, Cursor: cursor}})
		if err != nil {
			return nil, err
		}
		for _, d := range l.Items {
			enrolled[d.ID] = d.EnrolledAt
		}
		if l.NextCursor == "" {
			break
		}
		cursor = l.NextCursor
	}

	type key struct {
		dev string
		hr  int64
	}
	type counts struct{ ok, bad int }
	buckets := map[key]*counts{}
	byDevHour := map[string][]key{}
	hours, err := st.TenDayCounts(ctx, scope, "", now.Add(-(StripDays+2)*24*time.Hour), time.Time{})
	if err != nil {
		return nil, err
	}
	for _, h := range hours {
		if _, ok := enrolled[h.DeviceID]; !ok {
			continue // retired since the store answered, or never listed
		}
		k := key{h.DeviceID, h.Hour.Unix() / 3600}
		buckets[k] = &counts{ok: h.Success, bad: h.Failure}
		byDevHour[h.DeviceID] = append(byDevHour[h.DeviceID], k)
	}

	out := make(map[string][StripDays]Day, len(view.Devices))
	for i := range view.Devices {
		d := &view.Devices[i]
		if d.Primary == state.StateRetired {
			continue
		}
		loc := locOf[d.CustomerID]
		if loc == nil {
			loc = defaultLoc
		}
		today := midnight(now.In(loc))
		var cells [StripDays]Day
		for c := 0; c < StripDays; c++ {
			cells[c] = Day{Kind: DayNone, Date: today.AddDate(0, 0, c-(StripDays-1))}
		}
		for _, k := range byDevHour[d.ID] {
			t := time.UnixMilli(k.hr * 3600000).In(loc)
			day := midnight(t)
			idx := StripDays - 1 - int(today.Sub(day).Round(24*time.Hour)/(24*time.Hour))
			if idx < 0 || idx >= StripDays {
				continue
			}
			cells[idx].Success += buckets[k].ok
			cells[idx].Failed += buckets[k].bad
		}
		enrolDay := midnight(enrolled[d.ID].In(loc))
		for c := range cells {
			cell := &cells[c]
			switch {
			case d.Primary == state.StatePaused:
				cell.Kind = DayNone
			case cell.Success > 0 && cell.Failed > 0:
				cell.Kind = DayMixed
			case cell.Success > 0:
				cell.Kind = DaySuccess
			case cell.Failed > 0:
				cell.Kind = DayFailed
			case cell.Date.Before(enrolDay), cell.Date.Equal(today), d.Primary == state.StatePendingFirst:
				cell.Kind = DayNone
			default:
				cell.Kind = DayMissed
			}
		}
		out[d.ID] = cells
	}
	return out, nil
}

func midnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
