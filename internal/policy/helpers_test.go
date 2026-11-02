package policy

import (
	"errors"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // so time.LoadLocation works on Windows CI without a system zoneinfo
)

// chicago returns America/Chicago, the zone the DST tests use. In 2026 the
// spring-forward day is Sunday 8 March (02:00 CST -> 03:00 CDT) and the
// fall-back day is Sunday 1 November (02:00 CDT -> 01:00 CST).
func chicago(t testing.TB) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load America/Chicago: %v", err)
	}
	return loc
}

// at builds a wall-clock time in loc.
func at(loc *time.Location, y int, mo time.Month, d, h, m int) time.Time {
	return time.Date(y, mo, d, h, m, 0, 0, loc)
}

// mustWindow parses s or fails the test.
func mustWindow(t testing.TB, s string) Window {
	t.Helper()
	w, err := ParseWindow(s)
	if err != nil {
		t.Fatalf("ParseWindow(%q): %v", s, err)
	}
	return w
}

// hasProblem reports whether err is a *ValidationError containing a problem at
// path whose message contains sub.
func hasProblem(err error, path, sub string) bool {
	var ve *ValidationError
	if !errors.As(err, &ve) {
		return false
	}
	for _, p := range ve.Problems {
		if p.Path == path && strings.Contains(p.Message, sub) {
			return true
		}
	}
	return false
}
