package scheduler

import (
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/journal"
	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// L-VSS-2: the lab run. VSS was off, restic read C: live (exit 0, no failed
// file); the agent reports partial, class vss, vss_fallback_volumes [C]. The
// console rules it over tolerance (PARTIAL alarm), so the agent must owe a
// retry on the normal backoff, not wait a full interval.
func TestVSSFallbackPartialIsRetried(t *testing.T) {
	pol := policy.Default(policy.ClassDesktop)
	start := t0.Add(-time.Hour)
	r := row("V1", start, restic.OutcomePartial, func(r *journal.RunRow) {
		r.ErrorClass = restic.ClassVSS
		r.Stats.FailedFiles, r.Stats.TotalFilesProcessed = 0, 5000
		r.Stats.VSSFallbackVolumes = []string{"C"}
		r.ErrorCounts = []restic.ErrorProjection{{During: "archival", Class: restic.ClassVSS, Count: 1}}
	})
	if Completed(r, pol.SLA.PartialTolerance) {
		t.Fatal("a partial with a VSS fallback counts as complete; the console rules it over tolerance")
	}
	f := FactsFromRuns([]journal.RunRow{r}, pol)
	if !f.LastSuccess.IsZero() || !f.Retry.Pending || f.Attempts != 1 {
		t.Fatalf("facts = %+v, want a pending retry after one attempt and no success", f)
	}
	want := RetryDelay(pol.Schedule.Retries, 1, false, stableFraction("V1"))
	if got := f.Retry.NotBefore.Sub(*r.FinishedAt); got != want {
		t.Fatalf("retry after %v, want the normal backoff %v", got, want)
	}
}

// The partial rule is the console's (internal/state withinTolerance): each
// case is one of its conditions. Tolerance: 20 files or 0.5 %.
func TestCompletedMirrorsTheConsoleTolerance(t *testing.T) {
	tol := policy.PartialTolerance{MaxFailedFiles: 20, MaxFailedPct: 0.5}
	locked := []restic.ErrorProjection{{During: "archival", Class: restic.ClassLocked, Count: 3}}
	cases := []struct {
		name   string
		mutate func(*journal.RunRow)
		want   bool
	}{
		{"locked files under the count", func(r *journal.RunRow) {
			r.ErrorClass, r.ErrorCounts, r.Stats.FailedFiles, r.Stats.TotalFilesProcessed = restic.ClassLocked, locked, 3, 1000
		}, true},
		{"permission and unsupported_file are tolerable too", func(r *journal.RunRow) {
			r.ErrorClass, r.Stats.FailedFiles = restic.ClassPermission, 2
			r.ErrorCounts = []restic.ErrorProjection{{Class: restic.ClassPermission, Count: 1}, {Class: restic.ClassUnsupportedFile, Count: 1}}
		}, true},
		{"no failed file and no class", func(r *journal.RunRow) { r.Stats.TotalFilesProcessed = 10 }, true},
		{"a VSS fallback, even with nothing failed", func(r *journal.RunRow) {
			r.ErrorClass, r.ErrorCounts, r.Stats.FailedFiles = restic.ClassLocked, locked, 3
			r.Stats.VSSFallbackVolumes = []string{"D"}
		}, false},
		{"class vss", func(r *journal.RunRow) { r.ErrorClass = restic.ClassVSS }, false},
		{"missing_source among the counts", func(r *journal.RunRow) {
			r.ErrorClass, r.Stats.FailedFiles = restic.ClassLocked, 2
			r.ErrorCounts = []restic.ErrorProjection{{Class: restic.ClassLocked, Count: 1}, {Class: restic.ClassMissingSource, Count: 1}}
		}, false},
		{"failed files without any class", func(r *journal.RunRow) { r.Stats.FailedFiles, r.Stats.TotalFilesProcessed = 3, 1000 }, false},
		{"negative counts", func(r *journal.RunRow) { r.ErrorClass, r.Stats.FailedFiles = restic.ClassLocked, -1 }, false},
		{"over the count, under the share of processed plus failed", func(r *journal.RunRow) {
			r.ErrorClass, r.ErrorCounts, r.Stats.FailedFiles, r.Stats.TotalFilesProcessed = restic.ClassLocked, locked, 25, 9975 // 25/10000 = 0.25 %
		}, true},
		{"over the count and the share", func(r *journal.RunRow) {
			r.ErrorClass, r.ErrorCounts, r.Stats.FailedFiles, r.Stats.TotalFilesProcessed = restic.ClassLocked, locked, 25, 975 // 2.5 %
		}, false},
		{"the share is of processed plus failed (0.5 % of 5000, not of 4975)", func(r *journal.RunRow) {
			r.ErrorClass, r.ErrorCounts, r.Stats.FailedFiles, r.Stats.TotalFilesProcessed = restic.ClassLocked, locked, 25, 4975 // 25/5000 = 0.5 %
		}, true},
		{"over the count with no files processed (25 of 25 is 100 %)", func(r *journal.RunRow) {
			r.ErrorClass, r.ErrorCounts, r.Stats.FailedFiles = restic.ClassLocked, locked, 25
		}, false},
	}
	for _, c := range cases {
		r := row("T", t0, restic.OutcomePartial, c.mutate)
		if got := Completed(r, tol); got != c.want {
			t.Errorf("%s: Completed = %v, want %v", c.name, got, c.want)
		}
	}
}
