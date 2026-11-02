package journal

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// L-STATS-1: a run killed with its process (a reboot without a clean stop, a
// crash) is closed by the next start; it reports the last progress values the
// agent stored, not zeros.
func TestStaleRunKeepsItsLastProgress(t *testing.T) {
	ctx := context.Background()
	c := newClock()
	j := open(t, filepath.Join(t.TempDir(), "journal.db"), c)
	if err := j.StartRun(ctx, openRec(id(8), c.now())); err != nil {
		t.Fatal(err)
	}
	c.advance(20 * time.Minute)
	progress := api.RunStats{TotalFilesProcessed: 41_000, TotalBytesProcessed: 3_400_000_000, TotalDurationS: 1200,
		Lease: api.LeaseGranted}
	if err := j.TouchProgress(ctx, id(8), progress); err != nil {
		t.Fatal(err)
	}
	last := c.now()
	c.advance(time.Hour)

	closedRuns, err := j.OpenRunsOnStart(ctx, true, "test")
	if err != nil || len(closedRuns) != 1 {
		t.Fatalf("closed = %+v, %v", closedRuns, err)
	}
	got := closedRuns[0]
	if got.Outcome != restic.OutcomeInterrupted || !got.FinishedAt.Equal(last) {
		t.Fatalf("closed run = %+v, want interrupted at the last progress", got)
	}
	if got.Stats.TotalFilesProcessed != 41_000 || got.Stats.TotalBytesProcessed != 3_400_000_000 || got.Stats.TotalDurationS != 1200 || got.Stats.Lease != api.LeaseGranted {
		t.Fatalf("stats = %+v, want the last progress values", got.Stats)
	}
	row, ok, err := j.Run(ctx, id(8))
	if err != nil || !ok || row.Stats.TotalBytesProcessed != 3_400_000_000 {
		t.Fatalf("journal row stats = %+v (%v, %v)", row.Stats, ok, err)
	}

	// A closed row is not touched any more.
	if err := j.TouchProgress(ctx, id(8), api.RunStats{TotalFilesProcessed: 1}); err != nil {
		t.Fatal(err)
	}
	if row, _, _ := j.Run(ctx, id(8)); row.Stats.TotalFilesProcessed != 41_000 {
		t.Fatalf("TouchProgress changed a closed row: %+v", row.Stats)
	}
}
