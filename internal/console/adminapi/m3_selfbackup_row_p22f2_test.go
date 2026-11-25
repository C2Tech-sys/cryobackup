package adminapi

import (
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// P22-F2: the System page's "Database self-backup" row reads the same facts and the same rules as C11
// (state.SelfBackupVerifyCause), so it is never ok while C11 is critical for the self-backup.

// selfBackupRowFacts is fullEvidence with the self-backup facts replaced and C11 evaluated by the real
// engine from the same probe facts, as the console's page does.
func selfBackupRowFacts(now time.Time, sb *state.SelfBackupFacts) healthFacts {
	ran, next := now.Add(-time.Minute), now.Add(time.Hour)
	f := fullEvidence(now, &ran, &next)
	f.Probes.Database.SelfBackup = sb
	f.Probes.Database.FreeBytes, f.Probes.Database.TotalBytes = 80<<30, 100<<30 // C11 would otherwise raise "disk not measured"
	view := state.Evaluate(state.World{LoadComplete: true, LoadedAt: now, PreviousDeviceCount: -1, Probes: f.Probes}, now)
	f.Banners = view.Banners
	return f
}

func c11Of(f healthFacts) (state.Banner, bool) {
	for _, b := range f.Banners {
		if b.ID == "C11" {
			return b, true
		}
	}
	return state.Banner{}, false
}

func TestSelfBackupRowTellsTheTruth(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) state.Millis { return ms(now.Add(-d)) }
	const day = 24 * time.Hour
	for _, tc := range []struct {
		name       string
		sb         state.SelfBackupFacts
		status     string
		detail     []string // substrings of the row's detail
		notDetail  []string
		c11        state.Severity // what the C11 banner is, none when it is not raised
		c11Contain []string
	}{
		// The verifier's scratch probe: verified ok 20 days ago, attempts since with class network.
		{name: "verified_20_days_ago_attempts_failing", sb: state.SelfBackupFacts{LastAt: ago(time.Hour), FirstBackupAt: ago(60 * day), VerifyAt: ago(20 * day), VerifyAttemptAt: ago(time.Hour), VerifyAttemptClass: "network"},
			status: statusFailed, detail: []string{"not verified for 20 days", "latest attempt: network"},
			c11: state.SeverityCritical, c11Contain: []string{"not verified for 20 days (latest attempt: network)"}},
		{name: "verdict_exactly_8_days", sb: state.SelfBackupFacts{LastAt: ago(time.Hour), FirstBackupAt: ago(30 * day), VerifyAt: ago(8 * day), VerifyAttemptAt: ago(8 * day)},
			status: statusOK, detail: []string{"last backup 1 h ago", "last restore test 8 d ago"}},
		{name: "verdict_8_days_and_a_minute", sb: state.SelfBackupFacts{LastAt: ago(time.Hour), FirstBackupAt: ago(30 * day), VerifyAt: ago(8*day + time.Minute), VerifyAttemptAt: ago(8*day + time.Minute)},
			status: statusFailed, detail: []string{"not verified for 8 days"}, notDetail: []string{"latest attempt"},
			c11: state.SeverityCritical},
		{name: "never_verified_old", sb: state.SelfBackupFacts{LastAt: ago(time.Hour), FirstBackupAt: ago(9 * day), VerifyAttemptAt: ago(time.Hour), VerifyAttemptClass: "disk"},
			status: statusFailed, detail: []string{"never verified in the 9 days since the first backup", "latest attempt: disk"},
			c11: state.SeverityCritical},
		{name: "never_verified_young", sb: state.SelfBackupFacts{LastAt: ago(time.Hour), FirstBackupAt: ago(2 * day)},
			status: statusOK, detail: []string{"last backup 1 h ago", "no restore test has finished yet"}},
		{name: "verify_failed", sb: state.SelfBackupFacts{LastAt: ago(time.Hour), FirstBackupAt: ago(2 * day), VerifyAt: ago(time.Hour), VerifyAttemptAt: ago(time.Hour), VerifyFailed: true},
			status: statusFailed, detail: []string{"tested restore of the self-backup failed"}, c11: state.SeverityCritical},
		{name: "healthy", sb: state.SelfBackupFacts{LastAt: ago(time.Hour), FirstBackupAt: ago(30 * day), VerifyAt: ago(2 * day), VerifyAttemptAt: ago(2 * day)},
			status: statusOK, detail: []string{"last backup 1 h ago", "last restore test 2 d ago"}},
		// The job's class is named while nothing was ever written, in the row as in C11.
		{name: "never_written_not_configured", sb: state.SelfBackupFacts{JobClass: "not_configured"},
			status: statusNotChecked, detail: []string{"no self-backup has been written", "not_configured", "CRYO_MAINTAIN_SELF_PREFIX"},
			c11: state.SeverityAlarm, c11Contain: []string{"not_configured"}},
		{name: "never_written_no_slot", sb: state.SelfBackupFacts{JobClass: "no_slot"},
			status: statusNotChecked, detail: []string{"no self-backup has been written", "no_slot"},
			c11: state.SeverityAlarm, c11Contain: []string{"no_slot"}},
		{name: "never_written_network", sb: state.SelfBackupFacts{JobClass: "network"},
			status: statusNotChecked, detail: []string{"no self-backup has been written (latest run: network)"}, c11: state.SeverityAlarm},
		{name: "never_written_class_not_a_token", sb: state.SelfBackupFacts{JobClass: `C:\Users\x`},
			status: statusNotChecked, detail: []string{"(its latest run failed)"}, notDetail: []string{"Users"}, c11: state.SeverityAlarm},
		// A never-written backup is not also "never verified" or "garbled", whatever a stray verdict says.
		{name: "never_written_with_stale_attempts", sb: state.SelfBackupFacts{VerifyAt: ago(20 * day), VerifyAttemptAt: ago(time.Hour), VerifyAttemptClass: "network"},
			status: statusNotChecked, detail: []string{"no self-backup has been written"}, notDetail: []string{"verified", "garbled", "latest attempt"}, c11: state.SeverityAlarm},
		// The 26 h rule is unchanged: a stale backup with a fresh verdict is not_checked, never ok.
		{name: "stale_backup_fresh_verdict", sb: state.SelfBackupFacts{LastAt: ago(30 * time.Hour), FirstBackupAt: ago(30 * day), VerifyAt: ago(3 * day), VerifyAttemptAt: ago(3 * day), JobClass: "network"},
			status: statusNotChecked, c11: state.SeverityAlarm},
		// ... and critical once the verdict is old too.
		{name: "stale_backup_old_verdict", sb: state.SelfBackupFacts{LastAt: ago(9 * day), FirstBackupAt: ago(30 * day), VerifyAt: ago(9 * day), VerifyAttemptAt: ago(time.Hour), VerifyAttemptClass: "network", JobClass: "network"},
			status: statusFailed, detail: []string{"not verified for 9 days (latest attempt: network)"}, c11: state.SeverityCritical},
		// A garbled verdict time is loud, not quiet: C11 alarm, row warn.
		{name: "verify_time_garbled", sb: state.SelfBackupFacts{LastAt: ago(time.Hour), FirstBackupAt: ago(30 * day), VerifyAt: ms(now.Add(time.Hour))},
			status: statusWarn, detail: []string{"verify time is garbled or in the future"}, c11: state.SeverityAlarm},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := tc.sb
			f := selfBackupRowFacts(now, &sb)
			row := rowsOf(f)["self_backup"]
			if row.Status != tc.status {
				t.Fatalf("row = %+v, want status %s", row, tc.status)
			}
			for _, w := range tc.detail {
				if !strings.Contains(row.Detail, w) {
					t.Errorf("detail %q lacks %q", row.Detail, w)
				}
			}
			for _, n := range tc.notDetail {
				if strings.Contains(row.Detail, n) {
					t.Errorf("detail %q has %q", row.Detail, n)
				}
			}
			b, raised := c11Of(f)
			switch {
			case tc.c11 == state.SeverityNone && raised:
				t.Fatalf("C11 raised %+v, want none", b)
			case tc.c11 != state.SeverityNone && (!raised || b.Severity != tc.c11):
				t.Fatalf("C11 = %+v (raised %v), want severity %v", b, raised, tc.c11)
			}
			for _, w := range tc.c11Contain {
				if !strings.Contains(b.Text, w) {
					t.Errorf("C11 text %q lacks %q", b.Text, w)
				}
			}
			// The property the row exists for.
			if raised && b.Severity == state.SeverityCritical && row.Status == statusOK {
				t.Fatalf("row ok while C11 is critical: row %+v, C11 %+v", row, b)
			}
		})
	}
}

// With the self-backup facts not in the build, or not supplied at all, the row keeps its words.
func TestSelfBackupRowOutsideCases(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	if row := rowsOf(selfBackupRowFacts(now, &state.SelfBackupFacts{NotInBuild: true}))["self_backup"]; row.Status != statusNotInBuild {
		t.Errorf("not in build: %+v", row)
	}
	if row := rowsOf(selfBackupRowFacts(now, nil))["self_backup"]; row.Status != statusNotChecked {
		t.Errorf("not supplied: %+v", row)
	}
}
