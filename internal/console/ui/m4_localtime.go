package ui

import (
	"time"
)

// A local date and time typed into a form names one instant only when the display zone passes
// through that wall-clock time exactly once. On the night the clocks go back it passes twice; on the
// night they go forward it never does. time.ParseInLocation then picks an offset on its own, which
// is a guess: a snooze could end an hour early or late with nobody told. resolveLocal decides by the
// zone's own rules instead, with no date or zone written into the code (P20-W3B K2, foreman ruling).

// localResult says what a wall-clock date-time is in a zone.
type localResult int

const (
	localOnce     localResult = iota // exactly one instant: use it
	localRepeated                    // happens twice (the clocks go back over it): refuse
	localMissing                     // never happens (the clocks go forward over it): refuse
	localInvalid                     // not a date-time of the layout at all
)

// resolveLocal returns the instant that raw (in layout, read as a wall clock in loc) names, and
// what it is. It reads the zone's offsets on either side of the wall time and tries each: an offset
// that maps back to the same wall clock is a real occurrence. One occurrence is the answer; two mean
// the time is repeated, none that it is skipped. Transitions are at most a few hours from the wall
// time read as UTC, and every offset in use is within 14 hours of UTC, so offsets read a day and a
// half either side cover every transition that can touch it; each candidate is checked by the round
// trip, so an offset from an unrelated transition is never taken.
func resolveLocal(layout, raw string, loc *time.Location) (time.Time, localResult) {
	naive, err := time.Parse(layout, raw) // the wall clock's fields, read as UTC
	if err != nil {
		return time.Time{}, localInvalid
	}
	offsets := map[int]bool{}
	for _, d := range []time.Duration{-36 * time.Hour, -12 * time.Hour, 0, 12 * time.Hour, 36 * time.Hour} {
		_, off := naive.Add(d).In(loc).Zone()
		offsets[off] = true
	}
	var found []time.Time
	for off := range offsets {
		c := naive.Add(-time.Duration(off) * time.Second)
		if c.In(loc).Format(layout) != raw {
			continue
		}
		dup := false
		for _, f := range found {
			dup = dup || f.Equal(c)
		}
		if !dup {
			found = append(found, c)
		}
	}
	switch len(found) {
	case 0:
		return time.Time{}, localMissing
	case 1:
		return found[0], localOnce
	}
	return time.Time{}, localRepeated
}
