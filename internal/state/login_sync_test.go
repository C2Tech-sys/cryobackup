package state

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// C17 and C17b (M3-T6, ADR 0016): a retired device's gateway login may still be in
// the file the gateway reads. Table-driven like §4.8: the job's result against the
// newest retire, and what cannot be determined is not checked, never ok.
func TestC17_LoginSyncAgainstTheNewestRetire(t *testing.T) {
	retired := ago(10 * time.Minute)
	for _, c := range []struct {
		name     string
		f        LoginSyncFacts
		status   LoginSyncStatus
		class    string
		banner   string // "" = none
		severity Severity
		contains string
	}{
		{name: "C17_nothing_retired_no_banner", f: LoginSyncFacts{}, status: LoginRemoved},
		{name: "C17_nothing_retired_a_failing_job_is_not_this_banner", f: LoginSyncFacts{LastStartedAt: ago(time.Minute), LastStatus: "error", LastErrorClass: "io"}, status: LoginRemoved},
		{name: "C17_sync_passed_after_the_retire_clears", f: LoginSyncFacts{NewestRetireAt: retired, LastStartedAt: ago(9 * time.Minute), LastStatus: "ok"}, status: LoginRemoved},
		{name: "C17_sync_started_in_the_same_millisecond_counts", f: LoginSyncFacts{NewestRetireAt: retired, LastStartedAt: retired, LastStatus: "ok"}, status: LoginRemoved},
		{name: "C17_sync_failed_after_the_retire_is_critical_with_its_class", f: LoginSyncFacts{NewestRetireAt: retired, LastStartedAt: ago(9 * time.Minute), LastStatus: "error", LastErrorClass: "io"},
			status: LoginPending, class: "io", banner: "C17", severity: SeverityCritical, contains: "the gateway may still accept it until the sync succeeds (the newest login sync failed: io)"},
		{name: "C17_sync_failed_without_a_class_still_fails", f: LoginSyncFacts{NewestRetireAt: retired, LastStartedAt: ago(9 * time.Minute), LastStatus: "error"},
			status: LoginPending, class: LoginSyncFailed, banner: "C17", severity: SeverityCritical},
		{name: "C17_a_pass_from_before_the_retire_proves_nothing", f: LoginSyncFacts{NewestRetireAt: retired, LastStartedAt: ago(11 * time.Minute), LastStatus: "ok"},
			status: LoginPending, class: LoginNoSyncSinceRetire, banner: "C17", severity: SeverityCritical, contains: "no login sync has run since the retire"},
		{name: "C17_a_skipped_run_says_nothing_about_the_file", f: LoginSyncFacts{NewestRetireAt: retired, LastStartedAt: ago(time.Minute), LastStatus: "skipped"},
			status: LoginPending, class: LoginSyncSkipped, banner: "C17", severity: SeverityCritical},
		{name: "C17b_the_job_never_ran", f: LoginSyncFacts{NewestRetireAt: retired},
			status: LoginUnchecked, class: LoginNotChecked, banner: "C17b", severity: SeverityAlarm, contains: "the login sync has no recorded run"},
		{name: "C17b_not_read", f: LoginSyncFacts{NotRead: "the newest retire could not be read"},
			status: LoginUnchecked, class: LoginNotChecked, banner: "C17b", severity: SeverityAlarm, contains: "the newest retire could not be read"},
		{name: "C17b_a_retire_in_the_future", f: LoginSyncFacts{NewestRetireAt: ahead(day), LastStartedAt: ago(time.Minute), LastStatus: "ok"},
			status: LoginUnchecked, class: LoginNotChecked, banner: "C17b", severity: SeverityAlarm},
		{name: "C17b_a_run_in_the_future", f: LoginSyncFacts{NewestRetireAt: retired, LastStartedAt: ahead(day), LastStatus: "ok"},
			status: LoginUnchecked, class: LoginNotChecked, banner: "C17b", severity: SeverityAlarm},
		{name: "C17b_a_garbled_retire", f: LoginSyncFacts{NewestRetireAt: -5, LastStartedAt: ago(time.Minute), LastStatus: "ok"},
			status: LoginUnchecked, class: LoginNotChecked, banner: "C17b", severity: SeverityAlarm},
		{name: "C17b_an_unknown_status", f: LoginSyncFacts{NewestRetireAt: retired, LastStartedAt: ago(time.Minute), LastStatus: "running"},
			status: LoginUnchecked, class: LoginNotChecked, banner: "C17b", severity: SeverityAlarm},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := JudgeLoginSync(c.f, t0)
			if v.Status != c.status || v.Class != c.class {
				t.Fatalf("verdict %+v, want %s %q", v, c.status, c.class)
			}
			w := healthyFleet(1, 2)
			w.Probes = healthyProbes()
			f := c.f
			w.Probes.LoginSync = &f
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
	w.Probes.LoginSync = nil
	if got := bannerIDs(Evaluate(w, t0)); !reflect.DeepEqual(got, []string{"C17b"}) {
		t.Fatalf("banners %v, want C17b", got)
	}
}

// The banner clears by itself: only the facts change, and the engine holds no state.
func TestC17_ClearsWhenASyncPasses(t *testing.T) {
	w := healthyFleet(1, 2)
	w.Probes = healthyProbes()
	w.Probes.LoginSync = &LoginSyncFacts{NewestRetireAt: ago(time.Hour), LastStartedAt: ago(time.Minute), LastStatus: "error", LastErrorClass: "io"}
	if got := bannerIDs(Evaluate(w, t0)); !reflect.DeepEqual(got, []string{"C17"}) {
		t.Fatalf("banners %v", got)
	}
	w.Probes.LoginSync = &LoginSyncFacts{NewestRetireAt: ago(time.Hour), LastStartedAt: ago(time.Second), LastStatus: "ok"}
	if got := bannerIDs(Evaluate(w, t0)); len(got) != 0 {
		t.Fatalf("banners %v, want none after the sync passed", got)
	}
}
