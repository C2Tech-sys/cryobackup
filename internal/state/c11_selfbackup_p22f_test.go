package state

import (
	"strings"
	"testing"
	"time"
)

// P22-F: the self-backup causes of C11 once the loader supplies the worker's rows.
// Every other C11 input is healthy, so a banner here is the self-backup's alone.

func selfBackupC11(t *testing.T, now time.Time, sb *SelfBackupFacts) (Severity, string) {
	t.Helper()
	d := &DatabaseFacts{SelfBackup: sb, IntegrityCheckedAt: ms(now), IntegrityOK: true, MeasuredAt: ms(now), FreeBytes: 900, TotalBytes: 1000}
	got := databaseBanners(d, bannerCtx{now: now, loc: time.UTC})
	switch len(got) {
	case 0:
		return SeverityNone, ""
	case 1:
		if got[0].ID != "C11" {
			t.Fatalf("banner %+v, want C11", got[0])
		}
		return got[0].Severity, got[0].Text
	}
	t.Fatalf("banners %+v, want at most one", got)
	return SeverityNone, ""
}

func TestC11SelfBackupRules(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) Millis { return ms(now.Add(-d)) }
	const day = 24 * time.Hour
	cases := []struct {
		name string
		sb   SelfBackupFacts
		sev  Severity
		want []string // substrings of the text, in order of appearance not required
		not  []string
	}{
		// Rule 1: never written, the existing alarm; the job row's class says why.
		{name: "never_written", sb: SelfBackupFacts{}, sev: SeverityAlarm, want: []string{"no self-backup has been written"}, not: []string{"("}},
		{name: "never_written_not_configured", sb: SelfBackupFacts{JobClass: "not_configured"}, sev: SeverityAlarm,
			want: []string{"no self-backup has been written (not_configured: CRYO_MAINTAIN_SELF_PREFIX is not set)"}},
		{name: "never_written_no_slot", sb: SelfBackupFacts{JobClass: "no_slot"}, sev: SeverityAlarm,
			want: []string{"no self-backup has been written (no_slot: CRYO_MAINTAIN_MAX_STREAMS"}},
		{name: "never_written_network", sb: SelfBackupFacts{JobClass: "network"}, sev: SeverityAlarm,
			want: []string{"no self-backup has been written (latest run: network)"}},
		{name: "never_written_class_not_a_token", sb: SelfBackupFacts{JobClass: "C:\\Users\\x"}, sev: SeverityAlarm,
			want: []string{"(its latest run failed)"}, not: []string{"Users"}},
		// A never-written self-backup is not also "never verified": nothing to verify yet.
		{name: "never_written_not_stale_verify", sb: SelfBackupFacts{VerifyAttemptAt: ago(20 * day), VerifyAttemptClass: "no_slot"}, sev: SeverityAlarm,
			not: []string{"verified"}},
		// Rule 2: a failed verdict is critical at once, however fresh.
		{name: "verify_failed", sb: SelfBackupFacts{LastAt: ago(time.Hour), FirstBackupAt: ago(2 * day), VerifyAt: ago(time.Hour), VerifyAttemptAt: ago(time.Hour), VerifyFailed: true},
			sev: SeverityCritical, want: []string{"self-backup verify failed"}, not: []string{"not verified"}},
		// Rule 3: no verdict for more than 8 days, from the newest verdict.
		{name: "verdict_fresh", sb: SelfBackupFacts{LastAt: ago(time.Hour), FirstBackupAt: ago(30 * day), VerifyAt: ago(2 * day), VerifyAttemptAt: ago(2 * day)}, sev: SeverityNone},
		{name: "verdict_exactly_8_days", sb: SelfBackupFacts{LastAt: ago(time.Hour), FirstBackupAt: ago(30 * day), VerifyAt: ago(8 * day), VerifyAttemptAt: ago(8 * day)}, sev: SeverityNone},
		{name: "verdict_8_days_and_a_minute", sb: SelfBackupFacts{LastAt: ago(time.Hour), FirstBackupAt: ago(30 * day), VerifyAt: ago(8*day + time.Minute), VerifyAttemptAt: ago(8*day + time.Minute)},
			sev: SeverityCritical, want: []string{"self-backup not verified for 8 days"}, not: []string{"latest attempt"}},
		{name: "verdict_stale_attempt_class", sb: SelfBackupFacts{LastAt: ago(time.Hour), FirstBackupAt: ago(30 * day), VerifyAt: ago(9 * day), VerifyAttemptAt: ago(time.Hour), VerifyAttemptClass: "network"},
			sev: SeverityCritical, want: []string{"self-backup not verified for 9 days (latest attempt: network)"}},
		{name: "verdict_stale_attempt_no_slot", sb: SelfBackupFacts{LastAt: ago(time.Hour), FirstBackupAt: ago(30 * day), VerifyAt: ago(12 * day), VerifyAttemptAt: ago(time.Hour), VerifyAttemptClass: "no_slot"},
			sev: SeverityCritical, want: []string{"not verified for 12 days (latest attempt: no_slot)"}},
		{name: "verdict_stale_attempt_class_not_a_token", sb: SelfBackupFacts{LastAt: ago(time.Hour), FirstBackupAt: ago(30 * day), VerifyAt: ago(9 * day), VerifyAttemptAt: ago(time.Hour), VerifyAttemptClass: "/var/x"},
			sev: SeverityCritical, want: []string{"(the latest attempt reached no verdict)"}, not: []string{"/var"}},
		// An attempt older than the verdict is not named (it is not "later").
		{name: "verdict_stale_attempt_older", sb: SelfBackupFacts{LastAt: ago(time.Hour), FirstBackupAt: ago(30 * day), VerifyAt: ago(9 * day), VerifyAttemptAt: ago(10 * day), VerifyAttemptClass: "network"},
			sev: SeverityCritical, want: []string{"not verified for 9 days"}, not: []string{"latest attempt"}},
		// Rule 3, no verdict yet: counted from the first backup.
		{name: "never_verified_young", sb: SelfBackupFacts{LastAt: ago(time.Hour), FirstBackupAt: ago(7 * day)}, sev: SeverityNone},
		{name: "never_verified_old", sb: SelfBackupFacts{LastAt: ago(time.Hour), FirstBackupAt: ago(9 * day), VerifyAttemptAt: ago(2 * time.Hour), VerifyAttemptClass: "disk"},
			sev: SeverityCritical, want: []string{"self-backup never verified in the 9 days since the first backup (latest attempt: disk)"}},
		// First backup unknown (a row older than first_at): LastAt stands in.
		{name: "never_verified_first_unknown", sb: SelfBackupFacts{LastAt: ago(10 * day)}, sev: SeverityCritical,
			want: []string{"self-backup older than 26 h", "never verified in the 10 days"}},
		{name: "verify_ref_garbled", sb: SelfBackupFacts{LastAt: ago(time.Hour), FirstBackupAt: ago(30 * day), VerifyAt: ms(now.Add(time.Hour))},
			sev: SeverityAlarm, want: []string{"self-backup verify time is garbled or in the future"}},
		// Storage always unreachable: no new backup for 30 h is the existing 26 h alarm (not
		// critical while the verdict is fresh), naming the class of the failed runs.
		{name: "storage_unreachable", sb: SelfBackupFacts{LastAt: ago(30 * time.Hour), FirstBackupAt: ago(30 * day), VerifyAt: ago(3 * day), VerifyAttemptAt: ago(3 * day), JobClass: "network"},
			sev: SeverityAlarm, want: []string{"self-backup older than 26 h (latest run: network)"}, not: []string{"verified"}},
		// ... and it turns critical once the verdict, too, is more than 8 days old.
		{name: "storage_unreachable_for_9_days", sb: SelfBackupFacts{LastAt: ago(9 * day), FirstBackupAt: ago(30 * day), VerifyAt: ago(9 * day), VerifyAttemptAt: ago(time.Hour), VerifyAttemptClass: "network", JobClass: "network"},
			sev: SeverityCritical, want: []string{"self-backup older than 26 h (latest run: network)", "not verified for 9 days (latest attempt: network)"}},
		// A fresh backup with a failed newest run (one missed 6-hour run) raises nothing.
		{name: "fresh_with_failed_run", sb: SelfBackupFacts{LastAt: ago(7 * time.Hour), FirstBackupAt: ago(30 * day), VerifyAt: ago(time.Hour), VerifyAttemptAt: ago(time.Hour), JobClass: "network"}, sev: SeverityNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := tc.sb
			sev, text := selfBackupC11(t, now, &sb)
			if sev != tc.sev {
				t.Fatalf("severity %v (text %q), want %v", sev, text, tc.sev)
			}
			for _, w := range tc.want {
				if !strings.Contains(text, w) {
					t.Errorf("text %q lacks %q", text, w)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(text, n) {
					t.Errorf("text %q has %q", text, n)
				}
			}
		})
	}
}

// NotInBuild still silences every self-backup cause (facts built by hand).
func TestC11SelfBackupNotInBuildStaysQuiet(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	if sev, text := selfBackupC11(t, now, &SelfBackupFacts{NotInBuild: true, JobClass: "no_slot", VerifyFailed: true}); sev != SeverityNone {
		t.Fatalf("NotInBuild raised %v %q", sev, text)
	}
}

func TestClassToken(t *testing.T) {
	for s, want := range map[string]bool{
		"network": true, "no_slot": true, "wrong_password": true, "a1": true,
		"": false, "Network": false, "no-slot": false, "a b": false, "/tmp": false, strings.Repeat("a", 41): false,
	} {
		if got := classToken(s); got != want {
			t.Errorf("classToken(%q) = %v, want %v", s, got, want)
		}
	}
}
