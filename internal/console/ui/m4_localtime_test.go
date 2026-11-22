package ui

import (
	"testing"
	"time"
)

// K2: resolveLocal reads the zone's own rules: a wall clock the zone passes once is that instant, one it
// passes twice is repeated, one it skips is missing. Chicago (both nights of 2026), London, a zone without
// daylight saving time and UTC.
func TestP20W3B_K2_ResolveLocalNeverGuesses(t *testing.T) {
	t.Parallel()
	zone := func(name string) *time.Location {
		l, err := time.LoadLocation(name)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	for _, c := range []struct {
		zone, raw string
		want      localResult
		utc       string // the instant, for localOnce
	}{
		{"America/Chicago", "2026-03-08T02:30", localMissing, ""},
		{"America/Chicago", "2026-03-08T01:59", localOnce, "2026-03-08T07:59:00Z"},
		{"America/Chicago", "2026-03-08T03:00", localOnce, "2026-03-08T08:00:00Z"},
		{"America/Chicago", "2026-11-01T01:30", localRepeated, ""},
		{"America/Chicago", "2026-11-01T00:59", localOnce, "2026-11-01T05:59:00Z"},
		{"America/Chicago", "2026-11-01T02:00", localOnce, "2026-11-01T08:00:00Z"},
		{"America/Chicago", "2026-07-04T12:00", localOnce, "2026-07-04T17:00:00Z"},
		{"Europe/London", "2026-03-29T01:30", localMissing, ""},
		{"Europe/London", "2026-10-25T01:30", localRepeated, ""},
		{"Europe/London", "2026-10-25T02:00", localOnce, "2026-10-25T02:00:00Z"},
		{"Europe/London", "2026-01-15T09:00", localOnce, "2026-01-15T09:00:00Z"},
		{"Asia/Riyadh", "2026-11-01T01:30", localOnce, "2026-10-31T22:30:00Z"},
		{"Asia/Riyadh", "2026-03-08T02:30", localOnce, "2026-03-07T23:30:00Z"},
		{"UTC", "2026-11-01T01:30", localOnce, "2026-11-01T01:30:00Z"},
		{"UTC", "2026-02-30T01:30", localInvalid, ""},
		{"UTC", "not a time", localInvalid, ""},
	} {
		got, res := resolveLocal(datetimeLocal, c.raw, zone(c.zone))
		if res != c.want {
			t.Errorf("%s %s: result %d, want %d", c.zone, c.raw, res, c.want)
			continue
		}
		if c.want == localOnce && got.UTC().Format(time.RFC3339) != c.utc {
			t.Errorf("%s %s: %s, want %s", c.zone, c.raw, got.UTC().Format(time.RFC3339), c.utc)
		}
	}
}
