package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/journal"
	"github.com/c2tech-sys/cryobackup/internal/agent/restoreplace"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// TestIncludeLineEscaping pins ADR 0027's include-file rules: every '[' is
// written "[[]", then every '$' is written "[$]"; nothing else is touched (no
// case folding, no normalisation, no trimming).
func TestIncludeLineEscaping(t *testing.T) {
	for in, want := range map[string]string{
		"/Users/x/a.txt":              "/Users/x/a.txt",
		"/data/bracket [ab].txt":      "/data/bracket [[]ab].txt",
		"/data/[x].txt":               "/data/[[]x].txt",
		"/data/x]y.txt":               "/data/x]y.txt",
		"/data/dollar $P22VAR.txt":    "/data/dollar [$]P22VAR.txt",
		"/data/both [$x].txt":         "/data/both [[][$]x].txt",
		"/data/percent %VAR% 100%.md": "/data/percent %VAR% 100%.md",
		"/Data/Café üñï 日本.TXT":       "/Data/Café üñï 日本.TXT",
		"/data/a{b,c}!.txt":           "/data/a{b,c}!.txt",
	} {
		if got := includeLine(in); got != want {
			t.Errorf("includeLine(%q) = %q, want %q", in, got, want)
		}
	}
	b := includeContent([]string{"/a [1].txt", "/b"})
	if !bytes.Equal(b, []byte("/a [1].txt\n/b\n")) {
		t.Fatalf("content = %q", b)
	}
	if bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}) || bytes.Contains(b, []byte("\r")) {
		t.Fatal("the include file has a BOM or a CR")
	}
}

// TestGroupSelection: one group per drive segment, exact case; the include
// lines are relative to the drive with a leading slash.
func TestGroupSelection(t *testing.T) {
	g := groupSelection([]string{"/D/data/x [1].txt", "/C/Users/x/Documents", "/C/Users/x/a$b.txt", "/c/lower.txt"})
	if len(g) != 3 || g[0].key != "C" || g[1].key != "D" || g[2].key != "c" || g[2].letter != "C" {
		t.Fatalf("groups = %+v", g)
	}
	if strings.Join(g[0].lines, "|") != "/Users/x/Documents|/Users/x/a[$]b.txt" || g[1].lines[0] != "/data/x [[]1].txt" {
		t.Fatalf("lines = %q / %q", g[0].lines, g[1].lines)
	}
	if strings.Join(g[0].rels[0], "|") != "Users|x|Documents" {
		t.Fatalf("rels = %q", g[0].rels)
	}
}

// TestIncludeFilesRemovedOnEveryExit: the include files are deleted when f
// returns, when it fails, and when it panics.
func TestIncludeFilesRemovedOnEveryExit(t *testing.T) {
	dir := t.TempDir()
	groups := groupSelection([]string{"/C/a.txt", "/D/b.txt"})
	left := func() []string {
		m, _ := filepath.Glob(filepath.Join(dir, "restore-*.include"))
		return m
	}
	var seen []string
	_ = withIncludeFiles(dir, "01JB00000000000000000000R1", groups, func(files []string) error {
		seen = files
		for _, f := range files {
			if _, err := os.Stat(f); err != nil {
				t.Errorf("include file missing during the run: %v", err)
			}
		}
		return nil
	})
	if len(seen) != 2 || len(left()) != 0 {
		t.Fatalf("files %d, left after return %v", len(seen), left())
	}
	_ = withIncludeFiles(dir, "01JB00000000000000000000R2", groups, func([]string) error { return errors.New("boom") })
	if len(left()) != 0 {
		t.Fatalf("left after an error: %v", left())
	}
	func() {
		defer func() { _ = recover() }()
		_ = withIncludeFiles(dir, "01JB00000000000000000000R3", groups, func([]string) error { panic("simulated") })
	}()
	if len(left()) != 0 {
		t.Fatalf("left after a panic: %v", left())
	}
}

// restoreRecord closes an open restore record with an outcome.
func restoreRecord(o restoreOutcome) api.RunRecord {
	start := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	fin := start.Add(time.Minute)
	outcome, st := restoreVerdict(o)
	return api.RunRecord{
		ID: "01JB0000000000000000000R01", Kind: restic.KindRestore, Trigger: api.TriggerManual, StartedAt: start,
		FinishedAt: &fin, Outcome: outcome, Stats: st, Attempt: 1, AgentVersion: "0.4.0",
	}
}

func counts(restored, skipped int64, errs map[restoreplace.Class]int64) restoreplace.Counts {
	c := restoreplace.Counts{Restored: restored, Skipped: skipped, BytesRestored: uint64(restored) * 10, BytesSkipped: uint64(skipped) * 5}
	for k, v := range errs {
		c.Add(k, v)
	}
	return c
}

// TestEveryRestoreCouplingRowValidates: every record shape the agent can
// produce passes api.RunRecord.Validate (a rejected record is journaled as
// refused and never sent: the console would never learn the outcome), with
// the outcome and class each row of the coupling table demands.
func TestEveryRestoreCouplingRowValidates(t *testing.T) {
	gr := restoreplace.ClassGuardRefused
	for _, tc := range []struct {
		name    string
		o       restoreOutcome
		outcome restic.Outcome
		class   api.RestoreResultClass
		rows    int
	}{
		{"success", restoreOutcome{runs: 1, counts: counts(3, 1, nil)}, restic.OutcomeSuccess, "", 0},
		{"success-skipped-only", restoreOutcome{runs: 1, counts: counts(0, 2, nil)}, restic.OutcomeSuccess, "", 0},
		{"partial-errors", restoreOutcome{runs: 1, counts: counts(2, 0, map[restoreplace.Class]int64{gr: 1, restoreplace.ClassTargetLocked: 2})}, restic.OutcomePartial, api.RestoreResultPartial, 2},
		{"partial-one-run-nothing", restoreOutcome{runs: 2, nothing: 1, counts: counts(1, 0, nil)}, restic.OutcomePartial, api.RestoreResultPartial, 1},
		{"nothing-matched", restoreOutcome{runs: 2, nothing: 2}, restic.OutcomeFailed, api.RestoreResultNothingMatched, 0},
		{"failed-disk", restoreOutcome{fatal: api.RestoreResultDisk}, restic.OutcomeFailed, api.RestoreResultDisk, 0},
		{"failed-guard-root", restoreOutcome{fatal: api.RestoreResultGuardRefused}, restic.OutcomeFailed, api.RestoreResultGuardRefused, 0},
		{"failed-auth", restoreOutcome{fatal: api.RestoreResultAuth}, restic.OutcomeFailed, api.RestoreResultAuth, 0},
		{"failed-unreachable", restoreOutcome{fatal: api.RestoreResultRepoUnreachable}, restic.OutcomeFailed, api.RestoreResultRepoUnreachable, 0},
		{"failed-other", restoreOutcome{fatal: api.RestoreResultOther}, restic.OutcomeFailed, api.RestoreResultOther, 0},
		{"failed-auth-after-a-volume", restoreOutcome{fatal: api.RestoreResultAuth, runs: 1, counts: counts(2, 0, map[restoreplace.Class]int64{gr: 1})}, restic.OutcomeFailed, api.RestoreResultAuth, 1},
		{"failed-dominant", restoreOutcome{runs: 1, counts: counts(0, 0, map[restoreplace.Class]int64{gr: 3, restoreplace.ClassAccessDenied: 1})}, restic.OutcomeFailed, api.RestoreResultGuardRefused, 2},
		{"failed-dominant-with-nothing", restoreOutcome{runs: 2, nothing: 1, counts: counts(0, 0, map[restoreplace.Class]int64{restoreplace.ClassOther: 1})}, restic.OutcomeFailed, api.RestoreResultOther, 2},
		{"failed-nothing-placed", restoreOutcome{runs: 1}, restic.OutcomeFailed, api.RestoreResultOther, 0},
		{"cancelled", restoreOutcome{fatal: api.RestoreResultCancelled}, restic.OutcomeCancelled, api.RestoreResultCancelled, 0},
		{"cancelled-after-a-volume", restoreOutcome{fatal: api.RestoreResultCancelled, runs: 1, counts: counts(4, 0, nil)}, restic.OutcomeCancelled, api.RestoreResultCancelled, 0},
		{"interrupted", restoreOutcome{fatal: api.RestoreResultInterrupted, runs: 1, counts: counts(1, 0, map[restoreplace.Class]int64{gr: 1})}, restic.OutcomeInterrupted, api.RestoreResultInterrupted, 1},
	} {
		rec := restoreRecord(tc.o)
		if err := rec.Validate(); err != nil {
			t.Errorf("%s: record does not validate: %v (%+v)", tc.name, err, rec.Stats)
			continue
		}
		if rec.Outcome != tc.outcome || rec.Stats.RestoreResult != tc.class || len(rec.Stats.RestoreErrors) != tc.rows {
			t.Errorf("%s: outcome %s class %q rows %v", tc.name, rec.Outcome, rec.Stats.RestoreResult, rec.Stats.RestoreErrors)
		}
		if err := api.AssertPathFree(rec); err != nil {
			t.Errorf("%s: not path-free: %v", tc.name, err)
		}
	}
	// The open record the agent writes before anything runs.
	open := api.RunRecord{ID: "01JB0000000000000000000R01", Kind: restic.KindRestore, Trigger: api.TriggerManual,
		StartedAt: time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC), Attempt: 1, AgentVersion: "0.4.0"}
	if err := open.Validate(); err != nil {
		t.Errorf("open restore record: %v", err)
	}
}

// TestJournalClosesAnOpenRestoreAsInterrupted: a restore left open by a
// previous process is closed as interrupted (never abandoned, whatever the stop
// marker says), and its record validates.
func TestJournalClosesAnOpenRestoreAsInterrupted(t *testing.T) {
	ctx := context.Background()
	for _, marker := range []bool{false, true} {
		jr, err := journal.Open(ctx, journal.Options{Path: filepath.Join(t.TempDir(), "j.db")})
		if err != nil {
			t.Fatal(err)
		}
		rec := api.RunRecord{ID: "01JB0000000000000000000R01", Kind: restic.KindRestore, Trigger: api.TriggerManual,
			StartedAt: time.Now().UTC().Add(-time.Minute), Attempt: 1, AgentVersion: "0.4.0"}
		if err := jr.StartRun(ctx, rec); err != nil {
			t.Fatal(err)
		}
		closed, err := jr.OpenRunsOnStart(ctx, marker, "0.4.0")
		_ = jr.Close()
		if err != nil || len(closed) != 1 {
			t.Fatalf("closed = %v, err %v", closed, err)
		}
		c := closed[0]
		if c.Outcome != restic.OutcomeInterrupted || c.Stats.RestoreResult != api.RestoreResultInterrupted {
			t.Errorf("marker %v: closed as %s / %s", marker, c.Outcome, c.Stats.RestoreResult)
		}
		if err := c.Validate(); err != nil {
			t.Errorf("marker %v: %v", marker, err)
		}
	}
}

// TestRestoreClassesMatchTheWire: the placement package's classes are the
// countable wire words, copied without a table.
func TestRestoreClassesMatchTheWire(t *testing.T) {
	for _, c := range []restoreplace.Class{restoreplace.ClassGuardRefused, restoreplace.ClassAccessDenied, restoreplace.ClassTargetLocked, restoreplace.ClassOther} {
		w := api.RestoreResultClass(c)
		if !w.Valid() || !w.Countable() {
			t.Errorf("%s is not a countable wire class", c)
		}
	}
}

func TestMatchedNothingAndResticErrors(t *testing.T) {
	if !matchedNothing(0, &restic.RestoreSummary{}) || matchedNothing(1, &restic.RestoreSummary{}) ||
		matchedNothing(0, &restic.RestoreSummary{TotalFiles: 3, FilesRestored: 3}) || matchedNothing(0, nil) {
		t.Fatal("matchedNothing")
	}
	ec := restic.ErrorCounts{
		{During: "restore", Class: restic.ClassLocked}:     2,
		{During: "restore", Class: restic.ClassPermission}: 1,
		{During: "restore", Class: restic.ClassUnknown}:    1,
		{During: "", Class: restic.ClassUnknown}:           5, // plain text: not an item
	}
	var c restoreplace.Counts
	resticRestoreErrors(ec, &c)
	if c.Errors[restoreplace.ClassTargetLocked] != 2 || c.Errors[restoreplace.ClassAccessDenied] != 1 || c.Errors[restoreplace.ClassOther] != 1 {
		t.Fatalf("counts = %+v", c.Errors)
	}
	for class, want := range map[restic.ErrorClass]api.RestoreResultClass{
		restic.ClassAuth: api.RestoreResultAuth, restic.ClassNetwork: api.RestoreResultRepoUnreachable,
		restic.ClassTLS: api.RestoreResultRepoUnreachable, restic.ClassRepoMissing: api.RestoreResultRepoUnreachable,
		restic.ClassWrongPassword: api.RestoreResultOther, restic.ClassUnknown: api.RestoreResultOther,
	} {
		if got := fatalOfRestic(class); got != want {
			t.Errorf("fatalOfRestic(%s) = %s, want %s", class, got, want)
		}
	}
}
