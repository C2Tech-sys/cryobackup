package state

import "time"

// The ten-day strip (architecture §4.7, components.md §4.1): one cell per
// calendar day in the customer's zone, oldest first, today last.

// StripDays is the number of cells in the strip.
const StripDays = 10

// DayKind is the kind of one strip cell.
type DayKind string

// Cell kinds.
const (
	DayNone    DayKind = "none"    // before enrollment, paused, pending first backup, no schedule, or today with no run yet
	DaySuccess DayKind = "success" // at least one success and no failure that day
	DayMixed   DayKind = "mixed"   // a success and a failure
	DayFailed  DayKind = "failed"  // failures and no success
	DayMissed  DayKind = "missed"  // a past day a run was expected and none finished
)

// StripCell is one cell: a calendar day in the strip's zone.
type StripCell struct {
	Kind DayKind
	// Date is local midnight of the day (normalised by time.Date where a DST change skips midnight).
	Date    time.Time
	Success int
	Failed  int
}

// StripHour is one device's finished backup runs in one UTC hour, as the store
// groups them (the work is bounded by hours, not runs). Hour is truncated to the
// hour; in a zone with a half-hour offset a day edge can be off by that half hour.
type StripHour struct {
	Hour             time.Time
	Success, Failure int
}

// StripFacts is everything one device's strip needs. The engine loads no zone
// itself: the caller resolves the customer's display_tz.
type StripFacts struct {
	// CustomerZone is the customer's display_tz; nil falls back to DisplayZone, then UTC (§4.7, E03).
	CustomerZone, DisplayZone *time.Location
	// EnrolledAt is the device's (re)enrollment; days before its local date are none.
	EnrolledAt time.Time
	// PausedAt is when the current pause began; zero means not paused. Days from its local date on are none.
	PausedAt time.Time
	// PendingFirst marks a device still inside its first-backup grace: an empty day is none, not missed.
	PendingFirst bool
	// NoSchedule marks a device whose policy has no schedule: no day expects a run, so an empty day is none.
	NoSchedule bool
	// Hours are the device's hourly run counts, any order; hours outside the strip are ignored.
	Hours []StripHour
}

// StripZone is the zone a strip is drawn in: the customer's, else the display
// zone, else UTC (§4.7: the customer's display_tz, not the operator's).
func StripZone(customer, display *time.Location) *time.Location {
	switch {
	case customer != nil:
		return customer
	case display != nil:
		return display
	}
	return time.UTC
}

func localMidnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func dateKey(t time.Time) int {
	y, m, d := t.Date()
	return y*10000 + int(m)*100 + d
}

// TenDayStrip computes one device's strip at now. Runs are bucketed by the
// local calendar date of their hour, so a 23-hour or 25-hour DST day is one
// cell (E01, E02). Days before the (re)enrollment day and days from the pause
// day on are none whatever ran (E04). Other days are success, mixed or failed
// from their runs; a day without runs is missed only when a run was expected:
// not while the first backup is pending, not without a schedule, and not today
// (the day is not over).
func TenDayStrip(f StripFacts, now time.Time) [StripDays]StripCell {
	loc := StripZone(f.CustomerZone, f.DisplayZone)
	today := localMidnight(now.In(loc))
	var cells [StripDays]StripCell
	index := make(map[int]int, StripDays)
	for c := 0; c < StripDays; c++ {
		cells[c] = StripCell{Kind: DayNone, Date: today.AddDate(0, 0, c-(StripDays-1))}
		index[dateKey(cells[c].Date)] = c
	}
	for _, h := range f.Hours {
		hour := time.Unix(h.Hour.Unix()/3600*3600, 0).In(loc)
		if i, ok := index[dateKey(hour)]; ok {
			cells[i].Success += h.Success
			cells[i].Failed += h.Failure
		}
	}
	enrolDay := localMidnight(f.EnrolledAt.In(loc))
	paused := !f.PausedAt.IsZero()
	pausedDay := localMidnight(f.PausedAt.In(loc))
	for c := range cells {
		cell := &cells[c]
		switch {
		case paused && !cell.Date.Before(pausedDay), cell.Date.Before(enrolDay):
			// Runs before the (re)enrollment day belong to the previous enrollment or hardware.
			cell.Kind = DayNone
		case cell.Success > 0 && cell.Failed > 0:
			cell.Kind = DayMixed
		case cell.Success > 0:
			cell.Kind = DaySuccess
		case cell.Failed > 0:
			cell.Kind = DayFailed
		case cell.Date.Equal(today), f.PendingFirst, f.NoSchedule:
			cell.Kind = DayNone
		default:
			cell.Kind = DayMissed
		}
	}
	return cells
}
