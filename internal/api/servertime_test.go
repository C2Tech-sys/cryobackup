package api

import (
	"testing"
	"time"
)

func TestServerTimeHelpers(t *testing.T) {
	in := time.Date(2026, 10, 22, 14, 3, 9, 512_999_999, time.UTC)
	got := FormatServerTime(in)
	if got != "2026-10-22T14:03:09.512Z" {
		t.Errorf("FormatServerTime = %q", got)
	}
	// a non-UTC input is converted, and zero milliseconds are still printed
	local := time.Date(2026, 10, 22, 9, 3, 9, 0, time.FixedZone("CDT", -5*3600))
	if got := FormatServerTime(local); got != "2026-10-22T14:03:09.000Z" {
		t.Errorf("FormatServerTime(local) = %q", got)
	}
	back, err := ParseServerTime(FormatServerTime(local))
	if err != nil || !back.Equal(local) || back.Location() != time.UTC {
		t.Errorf("ParseServerTime = %v, %v", back, err)
	}
	for _, in := range []string{"2026-10-22T14:03:09Z", "2026-10-22T14:03:09.5Z", "2026-10-22T14:03:09.512345Z", "2026-10-22T09:03:09.512-05:00"} {
		if _, err := ParseServerTime(in); err != nil {
			t.Errorf("ParseServerTime(%q): %v", in, err)
		}
	}
	for _, in := range []string{"", "yesterday", "2026-10-22", "2026-10-22 14:03:09", "1666447389"} {
		if _, err := ParseServerTime(in); err == nil {
			t.Errorf("ParseServerTime(%q) succeeded", in)
		}
	}
	// the round trip of a formatted time loses only sub-millisecond precision
	rt, _ := ParseServerTime(FormatServerTime(in))
	if !rt.Equal(in.Truncate(time.Millisecond)) {
		t.Errorf("round trip = %v", rt)
	}
}

func TestClockOffset(t *testing.T) {
	server := time.Date(2026, 10, 22, 14, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		device time.Time
		want   int64
	}{
		{"in sync", server, 0},
		{"device ahead 3 h", server.Add(3 * time.Hour), 3 * 3600 * 1000},
		{"device behind 1.5 s", server.Add(-1500 * time.Millisecond), -1500},
		{"sub-millisecond is truncated toward zero", server.Add(1999 * time.Microsecond), 1},
		{"other zone, same instant", server.In(time.FixedZone("X", 7200)), 0},
	}
	for _, c := range cases {
		if got := ClockOffset(c.device, server); got != c.want {
			t.Errorf("%s: ClockOffset = %d, want %d", c.name, got, c.want)
		}
	}
}
