package jobs

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/maintain"
	"github.com/c2tech-sys/cryobackup/internal/restic"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// M3-STORE1 E (operator ruling 2026-11-05): a worker that cannot read its own
// database is critical. The storage row still learned nothing, so it stays C02b
// and never C02; the database row raises C11 critical after two in a row.

// workerStoreDown is the worker's answer on both paths that reach the settings
// row: after a clean list + cat config, and on the exit 10 branch.
var workerStoreDown = map[string]error{adminpath.MethodCatConfig: adminpath.NewError(adminpath.CodeUnavailable, maintain.ProbeRecordClass)}

// tick runs one tick a minute later under errs and waits for every probe.
func (r *proberRig) tick(t *testing.T, errs map[string]error) {
	t.Helper()
	r.clk.Advance(time.Minute)
	r.be.Errors = errs
	r.run(t)
	r.waitAll(t)
}

// c11 is the C11 banner, or the zero Banner when it is not raised.
func (r *proberRig) c11() state.Banner {
	p := r.hp.Facts()
	now := state.Millis(r.clk.Now().UnixMilli())
	p.Evaluator = &state.JobFacts{LastFinishedAt: now, Period: time.Minute}
	p.Maintain.Scheduler = &state.JobFacts{LastFinishedAt: now}
	p.Database.SelfBackup = &state.SelfBackupFacts{NotInBuild: true}
	p.Digest, p.Quota, p.Release = &state.DigestFacts{NotInBuild: true}, &state.QuotaFacts{NotInBuild: true}, &state.ReleaseFacts{NotInBuild: true}
	v := state.Evaluate(state.World{Probes: p, LoadComplete: true, PreviousDeviceCount: -1, LoadedAt: r.clk.Now()}, r.clk.Now())
	for _, b := range v.Banners {
		if b.ID == "C11" {
			return b
		}
	}
	return state.Banner{}
}

func TestC17WorkerStoreStreakRaisesC11Critical(t *testing.T) { // M3-STORE1 E
	r := newProberRig(t, false)

	r.tick(t, workerStoreDown) // one failure: storage not checked, nobody paged
	got := r.evaluate()
	if !slices.Contains(got, "C02b") || slices.Contains(got, "C02") {
		t.Fatalf("one failure: %v, want C02b and no C02", got)
	}
	if b := r.c11(); b.Severity >= state.SeverityCritical {
		t.Fatalf("one failure raised %+v, want no critical", b)
	}

	r.tick(t, workerStoreDown) // two failures: critical, and still never a storage failure
	got = r.evaluate()
	if !slices.Contains(got, "C02b") || slices.Contains(got, "C02") {
		t.Fatalf("two failures: %v, want C02b and no C02", got)
	}
	b := r.c11()
	if b.Severity != state.SeverityCritical || !strings.Contains(b.Text, "the maintenance worker cannot read or write the console database") {
		t.Fatalf("two failures raised %+v, want C11 critical with its cause", b)
	}
	if f := r.hp.Facts(); f.Storage.ConsecutiveFailures != 0 || f.Storage.LastRunAt != 0 {
		t.Fatalf("worker store failures reached the storage facts: %+v", f.Storage)
	}
	first := r.clk.Now().Add(-time.Minute).UnixMilli()
	if d := r.hp.Facts().Database; d.WorkerStoreFailures != 2 || int64(d.WorkerStoreFailingSince) != first {
		t.Fatalf("database facts %+v, want a streak of 2 since the first failure (%d)", d, first)
	}
	if j := r.row(t, ProbeRowStorage); j.LastStatus != "skipped" || j.LastErrorClass != ClassWorkerStore {
		t.Fatalf("storage row %+v, want skipped / %s", j, ClassWorkerStore)
	}

	r.tick(t, nil) // a clean probe clears both
	got = r.evaluate()
	if slices.Contains(got, "C02b") || slices.Contains(got, "C02") {
		t.Fatalf("after a clean probe: %v, want no storage banner", got)
	}
	if b := r.c11(); b.ID != "" {
		t.Fatalf("after a clean probe C11 is still raised: %+v", b)
	}
	if d := r.hp.Facts().Database; d.WorkerStoreFailures != 0 || d.WorkerStoreFailingSince != 0 {
		t.Fatalf("after a clean probe: %+v", d)
	}
}

// On a console whose storage already passed, the storage row's last pass stays
// and ages into C02b on its own schedule; C11 does not wait for it.
func TestC17WorkerStoreStreakAfterAPassDoesNotWaitForTheStorageRow(t *testing.T) { // M3-STORE1 E
	r := newProberRig(t, false)
	r.tick(t, nil)
	r.tick(t, workerStoreDown)
	if b := r.c11(); b.ID != "" {
		t.Fatalf("one failure after a pass raised %+v", b)
	}
	r.tick(t, workerStoreDown)
	if b := r.c11(); b.Severity != state.SeverityCritical {
		t.Fatalf("two failures after a pass raised %+v, want C11 critical", b)
	}
	got := r.evaluate()
	if slices.Contains(got, "C02") {
		t.Fatalf("two failures after a pass: %v, must never be C02", got)
	}
	r.tick(t, workerStoreDown)
	r.tick(t, workerStoreDown)
	got = r.evaluate()
	if !slices.Contains(got, "C02b") || slices.Contains(got, "C02") {
		t.Fatalf("after the last pass aged out: %v, want C02b and no C02", got)
	}
}

// One blip, then a clean probe, then another blip: not two in a row.
func TestC17WorkerStoreBlipsSeparatedByACleanProbeDoNotPage(t *testing.T) { // M3-STORE1 E
	r := newProberRig(t, false)
	for range 3 {
		r.tick(t, workerStoreDown)
		if b := r.c11(); b.Severity >= state.SeverityCritical {
			t.Fatalf("a single failure raised %+v", b)
		}
		r.tick(t, nil)
	}
}

// Answers that never reached the database say nothing about it: they neither
// extend the streak nor end it.
func TestC17WorkerStoreStreakIgnoresAnswersThatNeverReachedTheDatabase(t *testing.T) { // M3-STORE1 E
	silent := []struct {
		name string
		err  error
	}{
		{"storage unreachable", adminpath.NewError(adminpath.CodeStorageUnreachable, restic.ClassNetwork)},
		{"worker busy", adminpath.NewError(adminpath.CodeBusy, "")},
		{"worker down", adminpath.ErrUnavailable},
	}
	for _, s := range silent {
		t.Run(s.name, func(t *testing.T) {
			r := newProberRig(t, false)
			errs := map[string]error{adminpath.MethodCatConfig: s.err}
			r.tick(t, errs)
			if d := r.hp.Facts().Database; d.WorkerStoreFailures != 0 {
				t.Fatalf("%v alone counted: %+v", s.err, d)
			}
			r.tick(t, workerStoreDown)
			r.tick(t, errs)
			if d := r.hp.Facts().Database; d.WorkerStoreFailures != 1 {
				t.Fatalf("%v changed the streak: %+v, want 1", s.err, d)
			}
			r.tick(t, workerStoreDown)
			if b := r.c11(); b.Severity != state.SeverityCritical {
				t.Fatalf("two failures around %v raised %+v, want C11 critical", s.err, b)
			}
		})
	}
}

// A repo_missing verdict is a real storage failure (C02), not a database fact,
// and it is not the clean probe that clears the streak.
func TestC17RepoMissingVerdictLeavesTheStreakAlone(t *testing.T) { // M3-STORE1 E
	r := newProberRig(t, false)
	missing := map[string]error{adminpath.MethodCatConfig: adminpath.NewError(adminpath.CodeRepoMissing, restic.ClassRepoMissing)}
	r.tick(t, workerStoreDown)
	r.tick(t, missing)
	if d := r.hp.Facts().Database; d.WorkerStoreFailures != 1 {
		t.Fatalf("a repo_missing verdict changed the streak: %+v, want 1", d)
	}
	if f := r.hp.Facts().Storage; f.ConsecutiveFailures != 1 || f.LastErrorClass != string(restic.ClassRepoMissing) {
		t.Fatalf("storage %+v, want one repo_missing failure", f)
	}
}
