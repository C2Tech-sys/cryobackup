package state

import (
	"strconv"
	"strings"
)

// parseVersion reads a dotted version into a numeric tuple. Each dot-separated
// component must start with at least one digit; anything after the leading
// digits (a "-rc1" or "+build" suffix) is ignored, so a pre-release compares
// equal to its release. A leading "v" is allowed. ok is false for anything
// else: empty input, a component without a leading digit, or a number too
// large to hold. A version that does not parse is "garbled" and sorts below
// every version that does.
func parseVersion(s string) (tuple []uint64, ok bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	if s == "" {
		return nil, false
	}
	parts := strings.Split(s, ".")
	tuple = make([]uint64, 0, len(parts))
	for _, p := range parts {
		end := 0
		for end < len(p) && p[end] >= '0' && p[end] <= '9' {
			end++
		}
		if end == 0 {
			return nil, false
		}
		n, err := strconv.ParseUint(p[:end], 10, 64)
		if err != nil {
			return nil, false
		}
		tuple = append(tuple, n)
	}
	return tuple, true
}

// CompareVersions compares two dotted versions as numeric tuples, not strings:
// "2.5.9" is below "2.5.10" (A18). Missing trailing components count as zero,
// so "2.5" equals "2.5.0". A garbled version sorts below every parseable one
// and equal to another garbled one, so classification never panics and never
// reads a garbled agent version as current (A19). It returns -1, 0 or 1.
func CompareVersions(a, b string) int {
	ta, oka := parseVersion(a)
	tb, okb := parseVersion(b)
	switch {
	case !oka && !okb:
		return 0
	case !oka:
		return -1
	case !okb:
		return 1
	}
	n := len(ta)
	if len(tb) > n {
		n = len(tb)
	}
	for i := 0; i < n; i++ {
		var x, y uint64
		if i < len(ta) {
			x = ta[i]
		}
		if i < len(tb) {
			y = tb[i]
		}
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	return 0
}

// ExpectedVersion returns the highest of versions by tuple comparison (A23),
// as the original string. Garbled and empty entries are ignored; it returns ""
// when none parse, which means OUTDATED can never fire. Ties between equal
// tuples ("2.5" and "2.5.0") resolve to the lexically smaller string so the
// result does not depend on input order.
func ExpectedVersion(versions []string) string {
	best := ""
	found := false
	for _, v := range versions {
		if _, ok := parseVersion(v); !ok {
			continue
		}
		if !found {
			best, found = v, true
			continue
		}
		switch c := CompareVersions(v, best); {
		case c > 0, c == 0 && v < best:
			best = v
		}
	}
	return best
}
