package policy

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseWindowValid(t *testing.T) {
	tests := []struct {
		in, canon string
	}{
		{"*/0-24", "*/0-24"},
		{"1-7/0-24", "*/0-24"},
		{"Mon-Sun/0-24", "*/0-24"},
		{"Mon-Fri/8-18;Sat,Sun/0-24", "Mon-Fri/8-18;Sat,Sun/0-24"},
		{"mon-fri/8-18", "Mon-Fri/8-18"},
		{"MON/8-9", "Mon/8-9"},
		{"1-5/8-18", "Mon-Fri/8-18"},
		{" Mon - Fri / 8 - 18 ", "Mon-Fri/8-18"},
		{"Mon/8:00-9:30", "Mon/8-9:30"},
		{"Mon-Fri/8:00-9:00, 19:30-20:30;Sat,Sun/0-24", "Mon-Fri/8-9,19:30-20:30;Sat,Sun/0-24"},
		{"Sun/20-6", "Mon/0-6;Sun/20-24"},
		{"*/20-6", "*/0-6,20-24"},
		{"Fri-Mon/0-24", "Mon,Fri-Sun/0-24"},
		{"Tue-Mon/8-9", "*/8-9"},
		{"Mon,Wed,Fri/9-17", "Mon,Wed,Fri/9-17"},
		{"Mon-Wed/9-17", "Mon-Wed/9-17"},
		{"Mon-Tue/9-17", "Mon,Tue/9-17"},
		{"Mon/0-0", "Mon/0-24"},
		{"Mon/8-8", "Mon/8-24;Tue/0-8"},
		{"Mon/0-24:00", "Mon/0-24"},
		{"7/22-23", "Sun/22-23"},
		{"Mon/8-12,13-17", "Mon/8-12,13-17"},
		{"Mon/8:05-8:10", "Mon/8:05-8:10"},
		{"Mon/08-09", "Mon/8-9"},
		{"Mon/8-18;Mon/20-22", "Mon/8-18,20-22"},
		{"Mon/8-18 ; Tue/8-18", "Mon,Tue/8-18"},
		{"Sat,Sun/0-24;Mon-Fri/8-18", "Mon-Fri/8-18;Sat,Sun/0-24"},
		{"Mon/12-12:30,12:30-13", "Mon/12-13"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			w, err := ParseWindow(tc.in)
			if err != nil {
				t.Fatalf("ParseWindow(%q): %v", tc.in, err)
			}
			if got := w.String(); got != tc.canon {
				t.Fatalf("String() = %q, want %q", got, tc.canon)
			}
			again, err := ParseWindow(w.String())
			if err != nil {
				t.Fatalf("canonical form %q does not parse: %v", w.String(), err)
			}
			if again != w {
				t.Fatalf("canonical form %q does not re-parse to an equal Window", w.String())
			}
		})
	}
	if len(tests) < 25 {
		t.Fatalf("only %d valid cases, want >= 25", len(tests))
	}
}

func TestParseWindowErrors(t *testing.T) {
	tests := []struct {
		in     string
		offset int
		msg    string
	}{
		{"", 0, "expected day"},
		{"   ", 3, "expected day"},
		{"Mon", 3, "expected '/'"},
		{"Mon/", 4, "expected hour"},
		{"Mon/8", 5, "expected '-'"},
		{"Mon/8-", 6, "expected hour"},
		{"Mon/-18", 4, "expected hour"},
		{"Mon/8-18;", 9, "expected day"},
		{"Mon/8-18;;Tue/1-2", 9, "expected day"},
		{"Foo/8-18", 0, "unknown day"},
		{"Monday/8-18", 0, "unknown day"},
		{"8/8-18", 0, "out of range"},
		{"0/8-18", 0, "out of range"},
		{"1-8/8-9", 2, "out of range"},
		{"Mon-/8-18", 4, "expected day"},
		{"Mon,/8-18", 4, "expected day"},
		{"Mon/8:5-9", 6, "two-digit minutes"},
		{"Mon/8:-9", 6, "two-digit minutes"},
		{"Mon/8:75-9", 6, "minutes"},
		{"Mon/25-26", 4, "out of range"},
		{"Mon/008-9", 4, "out of range"},
		{"Mon/24:30-1", 4, "24"},
		{"Mon/24-6", 4, "cannot start at 24"},
		{"Mon/8-18x", 8, "expected ';'"},
		{"*/0-24 extra", 7, "expected ';'"},
		{"Mon/8-9,", 8, "expected hour"},
		{"Mon/8–18", 5, "expected '-'"},
		{"Mon/8-18;Tue/9", 14, "expected '-'"},
		{"Mon-Fri/8-18;Sat,Sun", 20, "expected '/'"},
		{"Mon Tue/8-9", 4, "expected '/'"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			_, err := ParseWindow(tc.in)
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("ParseWindow(%q) error = %v, want *ParseError", tc.in, err)
			}
			if pe.Offset != tc.offset {
				t.Errorf("offset = %d, want %d (%v)", pe.Offset, tc.offset, err)
			}
			if !strings.Contains(pe.Msg, tc.msg) {
				t.Errorf("message %q does not contain %q", pe.Msg, tc.msg)
			}
			if want := "at " + strconv.Itoa(tc.offset) + ": "; !strings.HasPrefix(err.Error(), want) {
				t.Errorf("Error() = %q, want prefix %q", err.Error(), want)
			}
		})
	}
}

func TestWindowContains(t *testing.T) {
	chi := chicago(t)
	office := mustWindow(t, "Mon-Fri/8-18;Sat,Sun/0-24")
	night := mustWindow(t, "Sat/22-4")
	sunSpill := mustWindow(t, "Sun/20-6")
	tests := []struct {
		name string
		w    Window
		t    time.Time
		want bool
	}{
		{"mon 08:00 open", office, at(chi, 2026, 3, 9, 8, 0), true},
		{"mon 07:59 closed", office, at(chi, 2026, 3, 9, 7, 59), false},
		{"mon 17:59 open", office, at(chi, 2026, 3, 9, 17, 59), true},
		{"mon 18:00 closed", office, at(chi, 2026, 3, 9, 18, 0), false},
		{"sat 03:00 open", office, at(chi, 2026, 3, 7, 3, 0), true},
		{"seconds ignored", office, time.Date(2026, 3, 9, 17, 59, 59, 999, chi), true},
		{"sat 23:00 night open", night, at(chi, 2026, 3, 7, 23, 0), true},
		{"sat 21:59 night closed", night, at(chi, 2026, 3, 7, 21, 59), false},
		{"sun 03:59 spill open", night, at(chi, 2026, 3, 8, 3, 59), true},
		{"sun 04:00 spill closed", night, at(chi, 2026, 3, 8, 4, 0), false},
		{"sun 20:00 into monday", sunSpill, at(chi, 2026, 3, 8, 20, 0), true},
		{"mon 05:59 spill open", sunSpill, at(chi, 2026, 3, 9, 5, 59), true},
		{"mon 06:00 spill closed", sunSpill, at(chi, 2026, 3, 9, 6, 0), false},
		{"mon 20:00 no spill", sunSpill, at(chi, 2026, 3, 9, 20, 0), false},
		{"zero window never open", Window{}, at(chi, 2026, 3, 9, 9, 0), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.w.Contains(tc.t); got != tc.want {
				t.Fatalf("Contains(%v) = %v, want %v", tc.t, got, tc.want)
			}
		})
	}
}

// openMinutes counts the real minutes of the local calendar day of date that
// the window is open, by stepping instants (so a 23- or 25-hour day is counted
// as the zone shows it).
func openMinutes(w Window, loc *time.Location, y int, mo time.Month, d int) int {
	start := time.Date(y, mo, d, 0, 0, 0, 0, loc)
	end := time.Date(y, mo, d+1, 0, 0, 0, 0, loc)
	n := 0
	for t := start; t.Before(end); t = t.Add(time.Minute) {
		if w.Contains(t) {
			n++
		}
	}
	return n
}

func TestWindowContainsAcrossDST(t *testing.T) {
	chi := chicago(t)
	tests := []struct {
		name   string
		window string
		y      int
		mo     time.Month
		d      int
		want   int
	}{
		{"normal sunday 1-3", "Sun/1-3", 2026, 3, 15, 120},
		{"spring forward sunday 1-3 loses the skipped hour", "Sun/1-3", 2026, 3, 8, 60},
		{"spring forward sunday 2-3 never opens", "Sun/2-3", 2026, 3, 8, 0},
		{"fall back sunday 1-3 gains the repeated hour", "Sun/1-3", 2026, 11, 1, 180},
		{"fall back sunday 1-2 opens twice", "Sun/1-2", 2026, 11, 1, 120},
		{"spring forward whole day is 23h", "Sun/0-24", 2026, 3, 8, 23 * 60},
		{"fall back whole day is 25h", "Sun/0-24", 2026, 11, 1, 25 * 60},
		{"wrap window spill, normal", "Sat/22-4", 2026, 3, 15, 240},
		{"wrap window spill, spring forward", "Sat/22-4", 2026, 3, 8, 180},
		{"wrap window spill, fall back", "Sat/22-4", 2026, 11, 1, 300},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := mustWindow(t, tc.window)
			if got := openMinutes(w, chi, tc.y, tc.mo, tc.d); got != tc.want {
				t.Fatalf("open minutes = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestWindowNextOpen(t *testing.T) {
	chi := chicago(t)
	office := mustWindow(t, "Mon-Fri/8-18")
	utc := func(y int, mo time.Month, d, h, m int) time.Time {
		return time.Date(y, mo, d, h, m, 0, 0, time.UTC)
	}
	tests := []struct {
		name   string
		w      Window
		from   time.Time
		want   time.Time
		wantOK bool
	}{
		{"inside returns from unchanged", office, time.Date(2026, 3, 9, 9, 30, 15, 7, chi), time.Date(2026, 3, 9, 9, 30, 15, 7, chi), true},
		{"before opening same day", office, at(chi, 2026, 3, 9, 6, 10), at(chi, 2026, 3, 9, 8, 0), true},
		{"after closing next day", office, at(chi, 2026, 3, 10, 18, 0), at(chi, 2026, 3, 11, 8, 0), true},
		{"seconds floor to a minute boundary", office, time.Date(2026, 3, 9, 7, 59, 30, 0, chi), at(chi, 2026, 3, 9, 8, 0), true},
		{"friday evening to monday morning across spring forward", office, at(chi, 2026, 3, 6, 18, 30), at(chi, 2026, 3, 9, 8, 0), true},
		{"friday evening to monday morning across fall back", office, at(chi, 2026, 10, 30, 18, 30), at(chi, 2026, 11, 2, 8, 0), true},
		{"opens in the skipped hour's aftermath", mustWindow(t, "Sun/2-4"), at(chi, 2026, 3, 8, 0, 30), at(chi, 2026, 3, 8, 3, 0), true},
		{"never: zero window", Window{}, at(chi, 2026, 3, 9, 6, 0), time.Time{}, false},
		{"second 01:30 of fall back does not step backwards", mustWindow(t, "Sun/2-3"),
			utc(2026, 11, 1, 7, 30).In(chi), utc(2026, 11, 1, 8, 0).In(chi), true},
		{"first 01:30 of fall back reaches 02:00 CST", mustWindow(t, "Sun/2-3"),
			utc(2026, 11, 1, 6, 30).In(chi), utc(2026, 11, 1, 8, 0).In(chi), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.w.NextOpen(tc.from)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("NextOpen = %v, want %v", got, tc.want)
			}
			if ok && got.Before(tc.from) {
				t.Fatalf("NextOpen %v is before from %v", got, tc.from)
			}
		})
	}
}

func TestWindowGridMinutesAlways(t *testing.T) {
	w := mustWindow(t, "Mon/8:30-10;Tue/0-24")
	g := w.Grid()
	if g[0][8] {
		t.Error("Monday 08:00 hour is only half open; Grid must be false")
	}
	if !g[0][9] {
		t.Error("Monday 09:00 hour is fully open; Grid must be true")
	}
	if g[0][10] {
		t.Error("Monday 10:00 hour is closed")
	}
	for h := 0; h < 24; h++ {
		if !g[1][h] {
			t.Errorf("Tuesday hour %d must be open", h)
		}
	}
	m := w.Minutes()
	if m[0][8*60+29] || !m[0][8*60+30] || !m[0][9*60+59] || m[0][10*60] {
		t.Error("Monday minute boundaries wrong")
	}
	if w.IsAlways() {
		t.Error("partial window reported as always")
	}
	for _, s := range []string{"*/0-24", "1-7/0-24", "Mon-Sun/0-24", "*/8-8"} {
		if !mustWindow(t, s).IsAlways() {
			t.Errorf("%q should be always", s)
		}
	}
	if (Window{}).IsAlways() || (Window{}).String() != "" {
		t.Error("zero window must be neither always nor have a string form")
	}
}

func TestWindowStringRoundTripRandom(t *testing.T) {
	// A deterministic walk over many generated windows: String always re-parses
	// to an equal Window.
	days := []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun", "*", "Fri-Mon", "1-3"}
	hours := []string{"0-24", "8-18", "20-6", "6:15-7:45", "23-1", "0-0", "12-12:30"}
	for i, d := range days {
		for j, h := range hours {
			s := d + "/" + h + ";" + days[(i+3)%len(days)] + "/" + hours[(j+2)%len(hours)]
			w, err := ParseWindow(s)
			if err != nil {
				t.Fatalf("ParseWindow(%q): %v", s, err)
			}
			again, err := ParseWindow(w.String())
			if err != nil || again != w {
				t.Fatalf("round trip of %q via %q failed: %v", s, w.String(), err)
			}
		}
	}
}
