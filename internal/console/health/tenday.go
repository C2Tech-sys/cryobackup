package health

import (
	"context"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// The ten-day strip is computed by internal/state (TenDayStrip); this file
// loads its inputs and calls it. The types are aliases so callers are unchanged.

// DayKind is the kind of one cell of the ten-day strip (components.md §4.1).
type DayKind = state.DayKind

// Cell kinds.
const (
	DayNone    = state.DayNone    // before enrollment, parked, or today with no run yet
	DaySuccess = state.DaySuccess // at least one success and no failure that day
	DayMixed   = state.DayMixed   // a success and a failure
	DayFailed  = state.DayFailed  // failures and no success
	DayMissed  = state.DayMissed  // a day a run was expected and none finished
)

// Day is one cell: a calendar day in the customer's zone.
type Day = state.StripCell

// StripDays is the number of cells.
const StripDays = state.StripDays

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
	// The customer's zone, or nil (none set, or not loadable): state.StripZone then uses defaultLoc.
	locOf := map[string]*time.Location{}
	for _, c := range custs {
		if c.DisplayTZ != "" {
			if l, err := time.LoadLocation(c.DisplayTZ); err == nil {
				locOf[c.ID] = l
			}
		}
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

	hoursOf := map[string][]state.StripHour{}
	hours, err := st.TenDayCounts(ctx, scope, "", now.Add(-(StripDays+2)*24*time.Hour), time.Time{})
	if err != nil {
		return nil, err
	}
	for _, h := range hours {
		if _, ok := enrolled[h.DeviceID]; !ok {
			continue // retired since the store answered, or never listed
		}
		hoursOf[h.DeviceID] = append(hoursOf[h.DeviceID], state.StripHour{Hour: h.Hour, Success: h.Success, Failure: h.Failure})
	}

	out := make(map[string][StripDays]Day, len(view.Devices))
	for i := range view.Devices {
		d := &view.Devices[i]
		if d.Primary == state.StateRetired {
			continue
		}
		f := state.StripFacts{
			CustomerZone: locOf[d.CustomerID],
			DisplayZone:  defaultLoc,
			EnrolledAt:   enrolled[d.ID],
			PendingFirst: d.Primary == state.StatePendingFirst,
			Hours:        hoursOf[d.ID],
		}
		if d.Primary == state.StatePaused {
			// Unchanged pre-M3 rendering: a device paused now shows every cell as none.
			// (§4.7 asks only for the paused days; passing d.Since would do that.)
			f.PausedAt = time.Unix(0, 0)
		}
		out[d.ID] = state.TenDayStrip(f, now)
	}
	return out, nil
}
