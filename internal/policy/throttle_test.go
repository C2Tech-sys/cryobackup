package policy

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseRate(t *testing.T) {
	tests := []struct {
		in      string
		want    Rate
		wantErr string
	}{
		{"0", 0, ""},
		{"unlimited", 0, ""},
		{"UNLIMITED", 0, ""},
		{"60", 60, ""},
		{" 60 ", 60, ""},
		{"60k", 60, ""},
		{"60K", 60, ""},
		{"2m", 2048, ""},
		{"1M", 1024, ""},
		{"0k", 0, ""},
		{"1073741824", 1 << 30, ""},
		{"50%", 0, "percent rates are not supported"},
		{"50 %", 0, "percent rates are not supported"},
		{"100%", 0, "percent rates are not supported"},
		{"", 0, "empty rate"},
		{"k", 0, "missing number"},
		{"m", 0, "missing number"},
		{"-5", 0, "invalid rate"},
		{"1.5m", 0, "invalid rate"},
		{"fast", 0, "invalid rate"},
		{"10g", 0, "invalid rate"},
		{"1073741825", 0, "too large"},
		{"99999999999999999999", 0, "too large"},
		{"2000000m", 0, "too large"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseRate(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParseRate(%q) error = %v, want %q", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("ParseRate(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
			}
		})
	}
}

func TestParseThrottleSpec(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want Throttle
	}{
		{"default only", "60", Throttle{Upload: 60, Download: 60}},
		{"unlimited default", "unlimited", Throttle{}},
		{"recon example", "60;10@Mon-Fri/8-18;5@Sat,Sun/0-24", Throttle{
			Upload: 60, Download: 60,
			Windows: []ThrottleWindow{{When: "Mon-Fri/8-18", Up: 10, Down: 10}, {When: "Sat,Sun/0-24", Up: 5, Down: 5}},
		}},
		{"whitespace and units", " 1m ; 0 @ 1-5/8-18 ; 512k@Sat/0-24", Throttle{
			Upload: 1024, Download: 1024,
			Windows: []ThrottleWindow{{When: "1-5/8-18", Up: 0, Down: 0}, {When: "Sat/0-24", Up: 512, Down: 512}},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseThrottleSpec(tc.in)
			if err != nil {
				t.Fatalf("ParseThrottleSpec(%q): %v", tc.in, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseThrottleSpecErrors(t *testing.T) {
	tests := []struct {
		in     string
		offset int
		msg    string
	}{
		{"", 0, "empty rate"},
		{"50%", 0, "percent rates are not supported"},
		{"60;50%@Mon/8-18", 3, "percent rates are not supported"},
		{"60;10", 3, "expected <rate>@<window>"},
		{"60;10@", 6, "expected day"},
		{"60;10@Mon/8-18;Sat,Sun/0-24", 15, "expected <rate>@<window>"},
		{"60;x@Mon/8-18", 3, "invalid rate"},
		{"60;10@Foo/8-18", 6, "unknown day"},
		{"60;10@Mon/8-18;5@Sat/8-25", 23, "out of range"},
		{"60; 10@ Mon/8:5-9", 14, "two-digit minutes"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			_, err := ParseThrottleSpec(tc.in)
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("error = %v, want *ParseError", err)
			}
			if pe.Offset != tc.offset || !strings.Contains(pe.Msg, tc.msg) {
				t.Fatalf("got offset %d %q, want offset %d containing %q", pe.Offset, pe.Msg, tc.offset, tc.msg)
			}
		})
	}
}

func TestThrottleEffective(t *testing.T) {
	chi := chicago(t)
	th := Throttle{
		Upload: 60, Download: 80,
		Windows: []ThrottleWindow{
			{When: "Mon-Fri/8-18", Up: 10, Down: 20},
			{When: "Mon-Fri/12-13", Up: 0, Down: 5}, // narrower, listed later: wins on overlap
			{When: "Sat,Sun/0-24", Up: 5, Down: 5},
			{When: "not a window", Up: 1, Down: 1}, // invalid: ignored, never throttles
		},
	}
	tests := []struct {
		name         string
		now          time.Time
		wantUp, want Rate
	}{
		{"weekday morning", at(chi, 2026, 3, 9, 9, 0), 10, 20},
		{"weekday lunch: later window wins", at(chi, 2026, 3, 9, 12, 30), 0, 5},
		{"weekday evening: defaults", at(chi, 2026, 3, 9, 18, 0), 60, 80},
		{"weekday 07:59: defaults", at(chi, 2026, 3, 9, 7, 59), 60, 80},
		{"saturday", at(chi, 2026, 3, 7, 3, 0), 5, 5},
		{"sunday night", at(chi, 2026, 3, 8, 23, 59), 5, 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			up, down := th.Effective(tc.now)
			if up != tc.wantUp || down != tc.want {
				t.Fatalf("Effective = %d/%d, want %d/%d", up, down, tc.wantUp, tc.want)
			}
		})
	}
	up, down := Throttle{Upload: 7, Download: 9}.Effective(at(chi, 2026, 3, 9, 9, 0))
	if up != 7 || down != 9 {
		t.Fatalf("no windows: got %d/%d", up, down)
	}
}

func TestThrottleNextBoundary(t *testing.T) {
	chi := chicago(t)
	th := Throttle{
		Upload: 60, Download: 60,
		Windows: []ThrottleWindow{{When: "Mon-Fri/8-18", Up: 10, Down: 10}},
	}
	tests := []struct {
		name   string
		th     Throttle
		from   time.Time
		want   time.Time
		wantOK bool
	}{
		{"before opening", th, at(chi, 2026, 3, 9, 6, 0), at(chi, 2026, 3, 9, 8, 0), true},
		{"inside: next is closing", th, at(chi, 2026, 3, 9, 9, 30), at(chi, 2026, 3, 9, 18, 0), true},
		{"after closing: next morning", th, at(chi, 2026, 3, 10, 19, 0), at(chi, 2026, 3, 11, 8, 0), true},
		{"friday evening to monday across spring forward", th, at(chi, 2026, 3, 6, 19, 0), at(chi, 2026, 3, 9, 8, 0), true},
		{"seconds are floored", th, time.Date(2026, 3, 9, 7, 59, 59, 0, chi), at(chi, 2026, 3, 9, 8, 0), true},
		{"no windows", Throttle{Upload: 5}, at(chi, 2026, 3, 9, 6, 0), time.Time{}, false},
		{"always-open window never changes", Throttle{Upload: 5, Windows: []ThrottleWindow{{When: "*/0-24", Up: 5, Down: 0}}},
			at(chi, 2026, 3, 9, 6, 0), time.Time{}, false},
		{"window equal to defaults changes nothing", Throttle{Upload: 5, Download: 5, Windows: []ThrottleWindow{{When: "Mon/8-18", Up: 5, Down: 5}}},
			at(chi, 2026, 3, 9, 6, 0), time.Time{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.th.NextBoundary(tc.from)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got %v)", ok, tc.wantOK, got)
			}
			if ok && !got.Equal(tc.want) {
				t.Fatalf("NextBoundary = %v, want %v", got, tc.want)
			}
			if !ok && !got.IsZero() {
				t.Fatalf("not ok must return the zero time, got %v", got)
			}
		})
	}
}
