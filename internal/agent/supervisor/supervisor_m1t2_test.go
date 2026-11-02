package supervisor

import (
	"context"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// TestVSSFallbackIsReportedByVolume: restic's per-volume fallback line
// (fakerestic.VSSFallbackLine, the text of restic 0.19.1) is reported as the
// drive letter, counted as class vss and not as a failed file; the verbatim
// line stays in the local run log.
func TestVSSFallbackIsReportedByVolume(t *testing.T) {
	cases := []struct {
		scenario string
		exit     int
		outcome  restic.Outcome
		failed   int64
		counts   map[restic.ErrorClass]int
	}{
		// Exit 0: the fallback alone does not make restic 0.19.1 exit 3.
		{"vss_fallback", 0, restic.OutcomeSuccess, 0, map[restic.ErrorClass]int{restic.ClassVSS: 1}},
		// Exit 3: a locked file that the live read could not open.
		{"vss_fallback_partial", 3, restic.OutcomePartial, 1, map[restic.ErrorClass]int{restic.ClassVSS: 1, restic.ClassLocked: 1}},
	}
	for _, c := range cases {
		t.Run(c.scenario, func(t *testing.T) {
			spec := fakeSpec(t, c.scenario, restic.KindBackup)
			res, err := New().Run(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			if res.ExitCode != c.exit || res.Restic.Outcome != c.outcome {
				t.Errorf("exit %d outcome %s, want %d %s", res.ExitCode, res.Restic.Outcome, c.exit, c.outcome)
			}
			if res.VSSFallbacks != 1 || !reflect.DeepEqual(res.VSSFallbackVolumes, []string{"C"}) {
				t.Errorf("fallbacks %d volumes %q, want 1 [C]", res.VSSFallbacks, res.VSSFallbackVolumes)
			}
			if res.FailedFiles != c.failed {
				t.Errorf("failed files = %d, want %d (a volume is not a file)", res.FailedFiles, c.failed)
			}
			if got := res.Counts.ByClass(); !reflect.DeepEqual(got, c.counts) {
				t.Errorf("counts by class = %v, want %v", got, c.counts)
			}
			found := false
			for _, l := range lines(t, spec.RunLogPath) {
				if l == fakerestic.VSSFallbackLine {
					found = true
				}
			}
			if !found {
				t.Error("the run log does not hold the verbatim fallback line")
			}
		})
	}
}

// TestNoFallbackNoVolumes: a clean run reports no fallback.
func TestNoFallbackNoVolumes(t *testing.T) {
	res, err := New().Run(context.Background(), fakeSpec(t, "backup_ok", restic.KindBackup))
	if err != nil {
		t.Fatal(err)
	}
	if res.VSSFallbacks != 0 || res.VSSFallbackVolumes != nil {
		t.Errorf("fallbacks %d volumes %q on a clean run", res.VSSFallbacks, res.VSSFallbackVolumes)
	}
}

// TestVSSFilterPassesEveryOtherLine: lines that are not a fallback reach the
// decoder byte for byte, including a line longer than the reader's buffer and
// a last line without a newline.
func TestVSSFilterPassesEveryOtherLine(t *testing.T) {
	long := `{"message_type":"status","current_files":["` + strings.Repeat("x", 200<<10) + `"]}`
	in := long + "\n" + fakerestic.VSSFallbackLine + "\nplain text line\n" + fakerestic.VSSFallbackLine + "\r\n" + `{"message_type":"exit_error","code":3,"message":"Warning"}`
	var vols []string
	f := newVSSFilter(strings.NewReader(in), func(v string) { vols = append(vols, v) })
	var out strings.Builder
	buf := make([]byte, 777)
	for {
		n, err := f.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			break
		}
	}
	want := long + "\nplain text line\n" + `{"message_type":"exit_error","code":3,"message":"Warning"}`
	if out.String() != want {
		t.Errorf("filtered stream differs: got %d bytes, want %d", out.Len(), len(want))
	}
	if !reflect.DeepEqual(vols, []string{"C", "C"}) {
		t.Errorf("volumes = %q", vols)
	}
}

// TestMemoryCapKillClassifiesAsMemory runs a helper that commits memory past a
// small job-object cap: the child dies on its own (the Go runtime cannot commit
// the next arena) and the run is failed/memory, not unknown, internal or
// interrupted.
func TestMemoryCapKillClassifiesAsMemory(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the process memory cap is a Windows job-object limit")
	}
	spec := fakeSpec(t, "alloc", restic.KindBackup)
	spec.MemoryLimitMiB = 256
	res, err := verified(t, spec).Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !res.MemoryLimitHit {
		t.Errorf("the job object's memory-limit message was not seen (exit %d)", res.ExitCode)
	}
	if res.ExitCode == 0 || res.ExitCode == 130 {
		t.Errorf("exit = %d, want the child's own failure", res.ExitCode)
	}
	if res.Restic.Outcome != restic.OutcomeFailed || res.Restic.ErrorClass != restic.ClassMemory {
		t.Errorf("result = %+v, want failed/memory", res.Restic)
	}
	if res.CancelMode != "" || res.Cancel != restic.CancelNone {
		t.Errorf("cancel mode %q cause %d: the supervisor did not stop this child", res.CancelMode, res.Cancel)
	}
}

// TestMemoryCapLeavesAnOrdinaryRunAlone: a run well under its cap is a plain
// success, with no memory-limit report.
func TestMemoryCapLeavesAnOrdinaryRunAlone(t *testing.T) {
	spec := fakeSpec(t, "backup_ok", restic.KindBackup)
	spec.MemoryLimitMiB = 1024
	res, err := New().Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.MemoryLimitHit || res.Restic.Outcome != restic.OutcomeSuccess {
		t.Errorf("memory hit %v result %+v", res.MemoryLimitHit, res.Restic)
	}
}

// TestPhysicalMemory: on Windows the machine's RAM is known and plausible;
// elsewhere it is reported as unknown.
func TestPhysicalMemory(t *testing.T) {
	mib, ok := PhysicalMemoryMiB()
	if runtime.GOOS != "windows" {
		if ok {
			t.Errorf("PhysicalMemoryMiB = %d, true off Windows", mib)
		}
		return
	}
	if !ok || mib < 512 || mib > 64<<20 {
		t.Errorf("PhysicalMemoryMiB = %d, %v", mib, ok)
	}
}
