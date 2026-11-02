package agent

import (
	"context"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
	"github.com/c2tech-sys/cryobackup/internal/agent/supervisor"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// ---- r211: VSS per-volume fallback ------------------------------------------

func TestFillRecordReportsVSSFallback(t *testing.T) {
	snap := strings.Repeat("cd", 32)
	vssOnly := make(restic.ErrorCounts)
	vssOnly.Add(restic.ErrorEvent{During: "archival", Class: restic.ClassVSS})
	withLocked := make(restic.ErrorCounts)
	withLocked.Add(restic.ErrorEvent{During: "archival", Class: restic.ClassVSS})
	withLocked.Add(restic.ErrorEvent{During: "archival", Class: restic.ClassLocked})
	withLocked.Add(restic.ErrorEvent{During: "archival", Class: restic.ClassLocked})

	tests := []struct {
		name    string
		res     supervisor.Result
		outcome restic.Outcome
		class   restic.ErrorClass
		failed  int64
	}{
		// restic exits 0 when the fallback is the only problem: never a success.
		{"exit 0 with a fallback is partial/vss", supervisor.Result{ExitCode: 0, Restic: restic.Result{Outcome: restic.OutcomeSuccess},
			Summary: &restic.BackupSummary{SnapshotID: snap, TotalFilesProcessed: 5}, Counts: vssOnly,
			VSSFallbacks: 1, VSSFallbackVolumes: []string{"C"}}, restic.OutcomePartial, restic.ClassVSS, 0},
		// Locked files outnumber the fallback; the class still names the fallback.
		{"exit 3 with a fallback carries class vss", supervisor.Result{ExitCode: 3, Restic: restic.Result{Outcome: restic.OutcomePartial},
			Summary: &restic.BackupSummary{SnapshotID: snap, TotalFilesProcessed: 5}, Counts: withLocked, FailedFiles: 2,
			VSSFallbacks: 1, VSSFallbackVolumes: []string{"C"}}, restic.OutcomePartial, restic.ClassVSS, 2},
		// A run that failed outright keeps its own class.
		{"a failed run keeps its class", supervisor.Result{ExitCode: 1, Restic: restic.Result{Outcome: restic.OutcomeFailed, ErrorClass: restic.ClassStorage},
			Counts: vssOnly, VSSFallbacks: 1, VSSFallbackVolumes: []string{"C"}}, restic.OutcomeFailed, restic.ClassStorage, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fin := time.Now().UTC()
			rec := api.RunRecord{ID: newULID(), Kind: restic.KindBackup, Trigger: api.TriggerSchedule,
				StartedAt: fin.Add(-time.Minute), FinishedAt: &fin, Attempt: 1, PolicyVersion: 1, AgentVersion: "t"}
			fillRecord(&rec, tc.res)
			if rec.Outcome != tc.outcome || rec.ErrorClass != tc.class || rec.Stats.FailedFiles != tc.failed {
				t.Errorf("outcome %s class %s failed %d, want %s %s %d", rec.Outcome, rec.ErrorClass, rec.Stats.FailedFiles, tc.outcome, tc.class, tc.failed)
			}
			if !reflect.DeepEqual(rec.Stats.VSSFallbackVolumes, []string{"C"}) {
				t.Errorf("vss_fallback_volumes = %q", rec.Stats.VSSFallbackVolumes)
			}
			if err := rec.Validate(); err != nil {
				t.Fatalf("the record does not validate: %v", err)
			}
			if err := api.AssertPathFree(rec); err != nil {
				t.Fatalf("not path-free: %v", err)
			}
		})
	}
	// No fallback: nothing on the wire, success stays success.
	fin := time.Now().UTC()
	rec := api.RunRecord{ID: newULID(), Kind: restic.KindBackup, Trigger: api.TriggerSchedule, StartedAt: fin, FinishedAt: &fin, Attempt: 1, AgentVersion: "t"}
	fillRecord(&rec, supervisor.Result{Restic: restic.Result{Outcome: restic.OutcomeSuccess}, Summary: &restic.BackupSummary{SnapshotID: snap}})
	if rec.Outcome != restic.OutcomeSuccess || rec.Stats.VSSFallbackVolumes != nil || rec.ErrorClass != "" {
		t.Errorf("clean run = %s %q %q", rec.Outcome, rec.ErrorClass, rec.Stats.VSSFallbackVolumes)
	}
}

// TestVSSFallbackEndToEnd: restic's fallback line reaches the console as the
// volume letter, outcome partial, class vss, and no failed file.
func TestVSSFallbackEndToEnd(t *testing.T) {
	fc := newFakeConsole(t)
	dir := enroll(t, fc)
	fakerestic.Use(t, "vss_fallback")
	start(t, fakeOptions(t, dir))

	waitFor(t, "the closed run at the console", 30*time.Second, func() bool { return len(fc.closedRuns()) == 1 })
	run := fc.closedRuns()[0]
	if run.Outcome != restic.OutcomePartial || run.ErrorClass != restic.ClassVSS || run.ExitCode == nil || *run.ExitCode != 0 {
		t.Fatalf("run = %+v", run)
	}
	if !reflect.DeepEqual(run.Stats.VSSFallbackVolumes, []string{"C"}) || run.Stats.FailedFiles != 0 || len(run.SnapshotID) != 64 {
		t.Errorf("stats %+v snapshot %q", run.Stats, run.SnapshotID)
	}
	if len(run.ErrorCounts) != 1 || run.ErrorCounts[0] != (restic.ErrorProjection{During: "archival", Class: restic.ClassVSS, Count: 1}) {
		t.Errorf("error counts = %+v", run.ErrorCounts)
	}
	if err := api.AssertPathFree(run); err != nil {
		t.Errorf("the uploaded record is not path-free: %v", err)
	}
}

// ---- r209: memory cap -------------------------------------------------------

func TestAssembleSetsTheMemoryCap(t *testing.T) {
	ctx := context.Background()
	fc := newFakeConsole(t)
	dir := enroll(t, fc)
	fakerestic.Use(t, "backup_ok")
	a, err := New(ctx, fakeOptions(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	phys, known := supervisor.PhysicalMemoryMiB()
	auto := policy.Throttle{}.MemoryCapMiB(phys)
	if runtime.GOOS == "windows" && (!known || auto < policy.MemoryLimitAutoFloorMiB) {
		t.Fatalf("auto cap on Windows = %d (physical %d, %v)", auto, phys, known)
	}
	for _, tc := range []struct {
		setting, want int
	}{
		{policy.MemoryLimitAuto, auto},
		{policy.MemoryLimitNone, 0},
		{2048, 2048},
	} {
		pol := api.PolicyResponse{Policy: testPolicy(), PolicyID: testPolicyID, Version: 2, ETag: `"p2"`}
		pol.Policy.Throttle.MemoryLimitMiB = tc.setting
		_, spec, err := a.assemble(ctx, pol, newULID(), false)
		if err != nil {
			t.Fatal(err)
		}
		if spec.MemoryLimitMiB != tc.want {
			t.Errorf("memory_limit_mib %d: spec cap %d, want %d", tc.setting, spec.MemoryLimitMiB, tc.want)
		}
	}
}

// TestMemoryCapEndToEnd: a policy cap that the child exceeds ends the run as
// failed/memory at the console.
func TestMemoryCapEndToEnd(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the process memory cap is a Windows job-object limit")
	}
	fc := newFakeConsole(t)
	pol := testPolicy()
	pol.Throttle.MemoryLimitMiB = policy.MemoryLimitMinMiB
	fc.setPolicy(api.PolicyResponse{Policy: pol, PolicyID: testPolicyID, Version: 1, ETag: `"policy-v1"`, ServerTime: time.Now()})
	dir := enroll(t, fc)
	fakerestic.Use(t, "alloc")
	start(t, fakeOptions(t, dir))

	waitFor(t, "the failed run", 60*time.Second, func() bool { return len(fc.closedRuns()) == 1 })
	run := fc.closedRuns()[0]
	if run.Outcome != restic.OutcomeFailed || run.ErrorClass != restic.ClassMemory || run.SnapshotID != "" {
		t.Fatalf("run = %+v", run)
	}
}
