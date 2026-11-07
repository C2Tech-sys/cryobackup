package adminapi

import (
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/health"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-HARDEN A8: the job table, the jobs row and banner C07 call the maintenance
// scheduler late at the same age. It ticks every minute; the table said OVERDUE at
// about 2 minutes while C07 waits 30.
func TestMaintenanceSchedulerIsLateWhenC07SaysSo(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	job := func(name string, age, every time.Duration) store.JobState {
		fin := now.Add(-age)
		next := fin.Add(every)
		return store.JobState{Name: name, LastStatus: "ok", LastFinishedAt: &fin, NextDueAt: &next}
	}
	rowFails := func(j store.JobState) bool {
		for _, a := range (healthFacts{Now: now, Jobs: []store.JobState{j}}).jobsAttempts() {
			if a.Result == failed {
				return true
			}
		}
		return false
	}
	for _, tc := range []struct {
		name string
		age  time.Duration
		late bool
	}{
		{"2 minutes", 2 * time.Minute, false},
		{"just before C07", state.MaintenanceSchedulerStale - time.Second, false},
		{"just past C07", state.MaintenanceSchedulerStale + time.Second, true},
	} {
		j := job(health.SchedulerJob, tc.age, time.Minute)
		if got := jobOverdue(j, now); got != tc.late {
			t.Errorf("%s: table overdue = %v, want %v", tc.name, got, tc.late)
		}
		if got := rowFails(j); got != tc.late {
			t.Errorf("%s: row failed = %v, want %v", tc.name, got, tc.late)
		}
	}
	// Every other job keeps its own limit.
	if !jobOverdue(job("usage-refresh", 5*time.Minute, time.Minute), now) {
		t.Error("another one-minute job is no longer late at 5 minutes")
	}
}

// M3-HARDEN A8: the jobs row names every flagged job when their ranks differ: an overdue
// job beside a young one that has not finished a first run.
func TestJobsRowNamesEveryFlaggedJobAcrossRanks(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	fin := now.Add(-time.Hour)
	next := fin.Add(5 * time.Minute)
	f := healthFacts{Now: now, Jobs: []store.JobState{{Name: "usage-refresh", LastStatus: "ok", LastFinishedAt: &fin, NextDueAt: &next}},
		FirstRunning: []string{"digest"}}
	for _, order := range [][]attempt{f.jobsAttempts(), reversed(f.jobsAttempts())} {
		st, detail, _ := judgeRow(now, order, "", false)
		if st == statusOK || st == statusNotChecked {
			t.Errorf("status = %s: a job is overdue", st)
		}
		for _, want := range []string{"usage-refresh", "digest"} {
			if !strings.Contains(detail, want) {
				t.Errorf("the row does not name %s: %q", want, detail)
			}
		}
	}
}

func reversed(a []attempt) []attempt {
	out := make([]attempt, 0, len(a))
	for i := len(a) - 1; i >= 0; i-- {
		out = append(out, a[i])
	}
	return out
}
