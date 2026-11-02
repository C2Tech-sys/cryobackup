package agent

import (
	"context"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
	"github.com/c2tech-sys/cryobackup/internal/agent/supervisor"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// L-STATS-1: a run that ends without a restic summary reports the last
// progress values the agent saw, not zeros (lab: 3.4 GB uploaded, stats 0).
func TestRunWithoutSummaryReportsItsLastProgress(t *testing.T) {
	prog := supervisor.Progress{Kind: restic.KindBackup, FilesDone: 41_000, BytesDone: 3_400_000_000, SecondsElapsed: 1190, UpdatedAt: time.Now()}
	for _, tc := range []struct {
		name string
		res  supervisor.Result
	}{
		{"interrupted by a service stop", supervisor.Result{ExitCode: 130, Restic: restic.Result{Outcome: restic.OutcomeInterrupted}, Progress: prog, Elapsed: 20 * time.Minute}},
		{"cancelled", supervisor.Result{ExitCode: 130, Restic: restic.Result{Outcome: restic.OutcomeCancelled}, Progress: prog, Elapsed: 20 * time.Minute}},
		{"a fatal error before the summary", supervisor.Result{ExitCode: 1, Restic: restic.Result{Outcome: restic.OutcomeFailed, ErrorClass: restic.ClassStorage}, Progress: prog, Elapsed: 20 * time.Minute}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fin := time.Now().UTC()
			rec := api.RunRecord{ID: newULID(), Kind: restic.KindBackup, Trigger: api.TriggerSchedule,
				StartedAt: fin.Add(-20 * time.Minute), FinishedAt: &fin, Attempt: 1, PolicyVersion: 1, AgentVersion: "t"}
			fillRecord(&rec, tc.res)
			if rec.Stats.TotalFilesProcessed != 41_000 || rec.Stats.TotalBytesProcessed != 3_400_000_000 {
				t.Fatalf("stats = %+v, want the last progress (41000 files, 3.4 GB)", rec.Stats)
			}
			if rec.Stats.TotalDurationS != (20 * time.Minute).Seconds() {
				t.Errorf("duration = %v, want the measured 1200 s", rec.Stats.TotalDurationS)
			}
			if err := rec.Validate(); err != nil {
				t.Fatalf("the record does not validate: %v", err)
			}
		})
	}

	// A summary wins: its totals are restic's own.
	fin := time.Now().UTC()
	rec := api.RunRecord{ID: newULID(), Kind: restic.KindBackup, Trigger: api.TriggerSchedule,
		StartedAt: fin.Add(-time.Minute), FinishedAt: &fin, Attempt: 1, PolicyVersion: 1, AgentVersion: "t"}
	fillRecord(&rec, supervisor.Result{Restic: restic.Result{Outcome: restic.OutcomeSuccess}, Progress: prog,
		Summary: &restic.BackupSummary{TotalFilesProcessed: 50_000, TotalBytesProcessed: 4_000_000_000}})
	if rec.Stats.TotalFilesProcessed != 50_000 || rec.Stats.TotalBytesProcessed != 4_000_000_000 {
		t.Fatalf("stats with a summary = %+v, want the summary's", rec.Stats)
	}

	// No sample was ever published: nothing is invented.
	var st api.RunStats
	applyProgress(&st, supervisor.Progress{FilesDone: 5})
	if st.TotalFilesProcessed != 0 || st.TotalBytesProcessed != 0 || st.TotalDurationS != 0 {
		t.Fatalf("an unpublished sample changed the stats: %+v", st)
	}
}

// End to end: restic reports progress (1 file, 100 bytes), the service stops,
// the run is closed as interrupted with those values, and the console gets
// them.
func TestServiceStopCarriesTheProgressIntoTheRun(t *testing.T) {
	ctx := context.Background()
	fc := newFakeConsole(t)
	dir := enroll(t, fc)
	fakerestic.Use(t, "hang_ignore")
	r := start(t, fakeOptions(t, dir))
	waitFor(t, "the progress of the running backup", 30*time.Second, func() bool {
		p, ok := r.a.sup.Progress()
		return ok && p.FilesDone == 1
	})
	r.stop()

	closed := fc.closedRuns()
	if len(closed) != 1 || closed[0].Outcome != restic.OutcomeInterrupted {
		t.Fatalf("closed runs = %+v", closed)
	}
	if st := closed[0].Stats; st.TotalFilesProcessed != 1 || st.TotalBytesProcessed != 100 {
		t.Fatalf("the interrupted run's stats at the console = %+v, want 1 file and 100 bytes", st)
	}
	jr := openJournalRO(t, dir)
	row, ok, err := jr.Run(ctx, closed[0].ID)
	if err != nil || !ok || row.Stats.TotalBytesProcessed != 100 {
		t.Fatalf("journal row = %+v (%v, %v)", row.Stats, ok, err)
	}
}
