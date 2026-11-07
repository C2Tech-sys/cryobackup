package state

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// C17 and C17b (M3-T6, ADR 0016): a retired device's gateway login in the file the
// gateway reads. Table-driven like §4.8, over the measured fact: what a read of the
// file found. What cannot be determined is not checked, never ok.
func TestC17_LoginFileMeasurement(t *testing.T) {
	read := ago(30 * time.Second)
	for _, c := range []struct {
		name     string
		f        LoginFileFacts
		banner   string // "" = none
		severity Severity
		contains string
	}{
		{name: "C17_nothing_retired_no_banner", f: LoginFileFacts{}},
		{name: "C17_nothing_retired_a_failed_read_is_not_this_banner", f: LoginFileFacts{MeasuredAt: read, ReadFailed: "file_unreadable"}},
		{name: "C17_a_clean_read_without_the_login_clears", f: LoginFileFacts{Retired: 3, MeasuredAt: read}},
		{name: "C17_a_clean_read_with_the_login_is_critical", f: LoginFileFacts{Retired: 3, MeasuredAt: read, InFile: 1},
			banner: "C17", severity: SeverityCritical, contains: "the gateway may still accept it until the sync succeeds"},
		{name: "C17_a_failed_read_after_the_login_was_seen_does_not_clear", f: LoginFileFacts{Retired: 3, MeasuredAt: read, ReadFailed: "file_unreadable", InFile: 1},
			banner: "C17", severity: SeverityCritical},
		{name: "C17_an_old_read_that_found_the_login_stands", f: LoginFileFacts{Retired: 3, MeasuredAt: ago(day), InFile: 1},
			banner: "C17", severity: SeverityCritical},
		{name: "C17b_the_file_cannot_be_read", f: LoginFileFacts{Retired: 1, MeasuredAt: read, ReadFailed: "file_missing"},
			banner: "C17b", severity: SeverityAlarm, contains: "cannot be read (file_missing)"},
		{name: "C17b_never_read", f: LoginFileFacts{Retired: 1},
			banner: "C17b", severity: SeverityAlarm, contains: "has not been read yet"},
		{name: "C17b_not_read_lately", f: LoginFileFacts{Retired: 1, MeasuredAt: ago(day), Interval: time.Minute},
			banner: "C17b", severity: SeverityAlarm, contains: "has not been read for"},
		{name: "C17b_a_read_in_the_future", f: LoginFileFacts{Retired: 1, MeasuredAt: ahead(day)},
			banner: "C17b", severity: SeverityAlarm},
		{name: "C17b_a_garbled_read_time", f: LoginFileFacts{Retired: 1, MeasuredAt: -5},
			banner: "C17b", severity: SeverityAlarm},
		{name: "C17b_the_retired_devices_cannot_be_read", f: LoginFileFacts{NotRead: "the retired devices could not be read", MeasuredAt: read},
			banner: "C17b", severity: SeverityAlarm, contains: "the retired devices could not be read"},
		{name: "C17b_a_corrupt_retire_time_is_not_nothing", f: LoginFileFacts{Retired: 1, RetiredGarbled: 1, MeasuredAt: read},
			banner: "C17b", severity: SeverityAlarm, contains: "unusable retire time"},
		{name: "C17b_a_garbled_count", f: LoginFileFacts{Retired: -1, MeasuredAt: read},
			banner: "C17b", severity: SeverityAlarm},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := healthyFleet(1, 2)
			w.Probes = healthyProbes()
			f := c.f
			w.Probes.LoginFile = &f
			view := Evaluate(w, t0)
			var got *Banner
			for i, b := range view.Banners {
				if strings.HasPrefix(b.ID, "C17") {
					got = &view.Banners[i]
				}
			}
			if c.banner == "" {
				if got != nil {
					t.Fatalf("banner %+v, want none", *got)
				}
				if !view.AllClear() {
					t.Fatalf("not all clear: %v", bannerIDs(view))
				}
				return
			}
			if got == nil || got.ID != c.banner || got.Severity != c.severity {
				t.Fatalf("banner %+v, want %s %s (all: %v)", got, c.banner, c.severity, bannerIDs(view))
			}
			if !strings.Contains(got.Text, c.contains) {
				t.Errorf("text %q, want %q", got.Text, c.contains)
			}
			if view.AllClear() {
				t.Error("all clear with the banner up")
			}
		})
	}
}

// Wired but not supplied is not checked; not wired stays C00's business.
func TestC17_NotSuppliedWhileWiredIsNotChecked(t *testing.T) {
	w := healthyFleet(1, 2)
	w.Probes = healthyProbes()
	w.Probes.LoginFile = nil
	if got := bannerIDs(Evaluate(w, t0)); !reflect.DeepEqual(got, []string{"C17b"}) {
		t.Fatalf("banners %v, want C17b", got)
	}
}

// The banner clears by itself, on a clean read: only the measured fact changes.
func TestC17_ClearsOnACleanRead(t *testing.T) {
	w := healthyFleet(1, 2)
	w.Probes = healthyProbes()
	w.Probes.LoginFile = &LoginFileFacts{Retired: 1, MeasuredAt: ago(time.Minute), InFile: 1}
	if got := bannerIDs(Evaluate(w, t0)); !reflect.DeepEqual(got, []string{"C17"}) {
		t.Fatalf("banners %v", got)
	}
	w.Probes.LoginFile = &LoginFileFacts{Retired: 1, MeasuredAt: ago(time.Second)}
	if got := bannerIDs(Evaluate(w, t0)); len(got) != 0 {
		t.Fatalf("banners %v, want none after a clean read", got)
	}
}

// The default cadence applies when the fact names none.
func TestC17_StalenessUsesTheProberCadenceByDefault(t *testing.T) {
	w := healthyFleet(1, 2)
	w.Probes = healthyProbes() // ProberInterval one minute
	w.Probes.LoginFile = &LoginFileFacts{Retired: 1, MeasuredAt: ago(time.Duration(ProbeStaleIntervals)*time.Minute - time.Second)}
	if got := bannerIDs(Evaluate(w, t0)); len(got) != 0 {
		t.Fatalf("just inside the limit: %v", got)
	}
	w.Probes.LoginFile = &LoginFileFacts{Retired: 1, MeasuredAt: ago(time.Duration(ProbeStaleIntervals)*time.Minute + time.Second)}
	if got := bannerIDs(Evaluate(w, t0)); !reflect.DeepEqual(got, []string{"C17b"}) {
		t.Fatalf("just past the limit: %v", got)
	}
}
