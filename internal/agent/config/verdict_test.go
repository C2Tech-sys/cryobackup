package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// One verdict id per state directory: 16 hex digits, the same for every
// spelling of one directory, different for two directories, path-free.
func TestVerdictID(t *testing.T) {
	base := t.TempDir()
	a := VerdictID(filepath.Join(base, "State"))
	if len(a) != 16 || strings.Trim(a, "0123456789abcdef") != "" {
		t.Fatalf("VerdictID = %q, want 16 lower-case hex digits", a)
	}
	for _, same := range []string{
		filepath.Join(base, "STATE"),
		filepath.Join(base, "x", "..", "state") + string(filepath.Separator),
		filepath.Join(base, "state", "."),
	} {
		if got := VerdictID(same); got != a {
			t.Errorf("VerdictID(%s) = %s, want %s (same directory)", same, got, a)
		}
	}
	if b := VerdictID(filepath.Join(base, "other")); b == a {
		t.Error("two directories share a verdict id")
	}
	if strings.Contains(VerdictLocation(filepath.Join(base, "State")), base) {
		t.Errorf("VerdictLocation names the path: %s", VerdictLocation(filepath.Join(base, "State")))
	}
}

// The stored form of the verdict, on every platform.
func TestVerdictFormatAndParse(t *testing.T) {
	at := time.Date(2026, 10, 22, 15, 30, 0, 0, time.FixedZone("x", 3600))
	s := formatVerdict(Verdict{Since: at, Reason: "secrets: owner S-1-5-21-1\r\nsecond line"})
	if s != "2026-10-22T14:30:00Z|secrets: owner S-1-5-21-1  second line" {
		t.Errorf("stored form %q (want a UTC stamp, '|', the reason on one line)", s)
	}
	v := parseVerdict(s)
	if !v.Since.Equal(at) || v.Since.Location() != time.UTC || v.Reason != "secrets: owner S-1-5-21-1  second line" {
		t.Errorf("round trip: %+v", v)
	}
	if got := v.Describe(); got != VerdictSource+" since 2026-10-22T14:30:00Z: secrets: owner S-1-5-21-1  second line" {
		t.Errorf("Describe = %q", got)
	}

	long := formatVerdict(Verdict{Since: at, Reason: strings.Repeat("é", maxVerdictReason)})
	if r := parseVerdict(long).Reason; len(r) > maxVerdictReason || !strings.HasPrefix(r, "é") || strings.ContainsRune(r, '�') {
		t.Errorf("a long reason is not bounded to valid UTF-8: %d bytes", len(r))
	}

	// Whatever is stored stands, even when it does not parse.
	for in, want := range map[string]string{
		"garbage":         "unparseable stored verdict",
		"not-a-stamp|why": "why",
		"":                "unparseable stored verdict",
	} {
		v := parseVerdict(in)
		if !v.Since.IsZero() || v.Reason != want {
			t.Errorf("parseVerdict(%q) = %+v, want reason %q and no stamp", in, v, want)
		}
	}
	if got := (Verdict{}).Describe(); got != VerdictSource+" since an unknown time: no reason recorded" {
		t.Errorf("Describe of an empty verdict = %q", got)
	}
}
