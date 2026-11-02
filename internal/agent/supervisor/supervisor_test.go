package supervisor

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

func TestMain(m *testing.M) {
	fakerestic.MaybeRun() // the test binary doubles as the fake restic
	os.Exit(m.Run())
}

const secretPassword = "correct-horse-battery-staple"

// fakeSpec builds a Spec that runs this test binary as restic.
func fakeSpec(t *testing.T, scenario string, kind restic.Kind) Spec {
	t.Helper()
	fakerestic.Use(t, scenario)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	var cmd restic.Command
	switch kind {
	case restic.KindBackup:
		cmd, err = restic.Backup(restic.BackupOptions{Host: "dev1", Roots: []string{`C:\fixture\tree`}})
	case restic.KindSnapshots:
		cmd, err = restic.Snapshots("dev1", restic.Common{})
	}
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	return Spec{
		Binary: exe, ExpectedSHA256: hex.EncodeToString(sum[:]), Command: cmd,
		Env:        restic.Env{Repository: "rest:https://dev1:pw@gw.example/dev1/", Password: secretPassword, CacheDir: filepath.Join(dir, "cache")},
		RunLogPath: filepath.Join(dir, "run.jsonl"), NoWindow: false,
	}
}

// verified returns a Supervisor that has already checked the binary, so a test
// that cancels at a fixed time cancels the run, not the first verification.
func verified(t *testing.T, spec Spec) *Supervisor {
	t.Helper()
	sup := New()
	if _, err := sup.Verify(context.Background(), spec.Binary, spec.ExpectedSHA256, spec.AllowUnpinned, spec.NoWindow); err != nil {
		t.Fatal(err)
	}
	return sup
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		out = append(out, strings.TrimRight(sc.Text(), "\r"))
	}
	return out
}

func fixtureLines(t *testing.T, names ...string) []string {
	t.Helper()
	var out []string
	for _, n := range names {
		if n == "" {
			continue
		}
		out = append(out, lines(t, filepath.Join(fakerestic.Dir(), n))...)
	}
	return out
}

func TestRunTable(t *testing.T) {
	cases := []struct {
		scenario string
		exit     int
		outcome  restic.Outcome
		class    restic.ErrorClass
		deferred restic.DeferredReason
		summary  bool
		failed   int64
		counted  bool
	}{
		{"backup_ok", 0, restic.OutcomeSuccess, "", "", true, 0, false},
		{"backup_partial", 3, restic.OutcomePartial, "", "", true, 0, true},
		{"backup_locked", 3, restic.OutcomePartial, "", "", true, 1, true},
		{"exit10", 10, restic.OutcomeFailed, restic.ClassRepoMissing, "", false, 0, false},
		{"exit11", 11, restic.OutcomeDeferred, restic.ClassLock, restic.DeferredLock, false, 0, false},
		{"exit12", 12, restic.OutcomeFailed, restic.ClassWrongPassword, "", false, 0, false},
		{"exit_gateway", 1, restic.OutcomeDeferred, restic.ClassNetwork, restic.DeferredGateway, false, 0, false},
		{"exit_unknown", 99, restic.OutcomeFailed, restic.ClassUnknown, "", false, 0, false},
		{"no_summary", 0, restic.OutcomeFailed, restic.ClassParse, "", false, 0, false},
	}
	for _, c := range cases {
		t.Run(c.scenario, func(t *testing.T) {
			spec := fakeSpec(t, c.scenario, restic.KindBackup)
			res, err := New().Run(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			if res.ExitCode != c.exit {
				t.Errorf("exit = %d, want %d", res.ExitCode, c.exit)
			}
			if res.Restic.Outcome != c.outcome || res.Restic.ErrorClass != c.class || res.Restic.DeferredReason != c.deferred {
				t.Errorf("result = %+v, want outcome %s class %q deferred %q", res.Restic, c.outcome, c.class, c.deferred)
			}
			if (res.Summary != nil) != c.summary {
				t.Errorf("summary seen = %v, want %v", res.Summary != nil, c.summary)
			}
			if c.summary && len(res.Summary.SnapshotID) != 64 {
				t.Errorf("snapshot id %q is not 64 hex characters", res.Summary.SnapshotID)
			}
			if res.FailedFiles != c.failed {
				t.Errorf("failed files = %d, want %d", res.FailedFiles, c.failed)
			}
			if c.counted && len(res.Counts) == 0 {
				t.Errorf("a partial run counted no error, want at least one path-free count")
			}
		})
	}
}

func TestRunLogIsVerbatim(t *testing.T) {
	spec := fakeSpec(t, "backup_locked", restic.KindBackup)
	if _, err := New().Run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	got := lines(t, spec.RunLogPath)
	want := fixtureLines(t, "backup_locked.stdout.jsonl", "backup_locked.stderr.jsonl")
	if len(got) != len(want) {
		t.Fatalf("run log has %d lines, the fixtures have %d", len(got), len(want))
	}
	in := map[string]int{}
	for _, l := range got {
		in[l]++
	}
	for _, l := range want {
		if in[l] == 0 {
			t.Fatalf("a fixture line is missing or altered in the run log: %.80s", l)
		}
		in[l]--
	}
	// The log keeps the path the counts drop.
	if !strings.Contains(strings.Join(got, "\n"), `locked.docx`) {
		t.Error("the local run log should carry the file name (it is path-bearing by design)")
	}
}

func TestCountsAreProjectedWithoutPaths(t *testing.T) {
	spec := fakeSpec(t, "backup_locked", restic.KindBackup)
	res, err := New().Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	rows := res.Counts.Projection()
	if len(rows) != 1 || rows[0].During != "archival" || rows[0].Ext != "docx" || rows[0].Count != 1 {
		t.Fatalf("projection = %+v", rows)
	}
	if strings.ContainsAny(string(rows[0].Class)+rows[0].During+rows[0].Ext, `\/:`) {
		t.Errorf("a separator survived the projection: %+v", rows[0])
	}
}

func TestProgressIsSampled(t *testing.T) {
	spec := fakeSpec(t, "slow", restic.KindBackup)
	spec.ProgressInterval = 40 * time.Millisecond
	var samples atomic.Int32
	var maxPct atomic.Uint64
	spec.OnProgress = func(p Progress) {
		samples.Add(1)
		if p.Kind != restic.KindBackup {
			t.Errorf("progress kind = %q", p.Kind)
		}
		maxPct.Store(uint64(p.PercentDone * 100))
	}
	sup := New()
	done := make(chan Result, 1)
	go func() {
		res, _ := sup.Run(context.Background(), spec)
		done <- res
	}()
	sawLive := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !sawLive {
		if p, ok := sup.Progress(); ok && p.TotalFiles == 8 {
			sawLive = true
		}
		time.Sleep(10 * time.Millisecond)
	}
	res := <-done
	if res.Restic.Outcome != restic.OutcomeSuccess {
		t.Fatalf("outcome = %s", res.Restic.Outcome)
	}
	if samples.Load() < 2 {
		t.Errorf("got %d progress samples, want several", samples.Load())
	}
	if !sawLive {
		t.Error("Progress() never showed the run in flight")
	}
	if _, ok := sup.Progress(); ok {
		t.Error("Progress() should report nothing after the run ends")
	}
}

func TestBinaryChecks(t *testing.T) {
	t.Run("hash mismatch refuses and never starts the child", func(t *testing.T) {
		spec := fakeSpec(t, "backup_ok", restic.KindBackup)
		out := filepath.Join(t.TempDir(), "child-ran")
		t.Setenv(fakerestic.Out, out)
		spec.ExpectedSHA256 = strings.Repeat("0", 64)
		_, err := New().Run(context.Background(), spec)
		if !errors.Is(err, ErrHashMismatch) {
			t.Fatalf("err = %v, want ErrHashMismatch", err)
		}
		if _, err := os.Stat(out); err == nil {
			t.Error("the child ran although the hash did not match (not even the version query is allowed)")
		}
	})
	t.Run("no pin and no unpinned flag refuses", func(t *testing.T) {
		spec := fakeSpec(t, "backup_ok", restic.KindBackup)
		spec.ExpectedSHA256 = ""
		if _, err := New().Run(context.Background(), spec); !errors.Is(err, ErrNotPinned) {
			t.Fatalf("err = %v, want ErrNotPinned", err)
		}
	})
	t.Run("unpinned lab mode still checks the version", func(t *testing.T) {
		spec := fakeSpec(t, "backup_ok", restic.KindBackup)
		spec.ExpectedSHA256, spec.AllowUnpinned = "", true
		res, err := New().Run(context.Background(), spec)
		if err != nil || res.Restic.Outcome != restic.OutcomeSuccess {
			t.Fatalf("unpinned lab run: %v %+v", err, res.Restic)
		}
		spec = fakeSpec(t, "version_bad", restic.KindBackup)
		spec.ExpectedSHA256, spec.AllowUnpinned = "", true
		if _, err := New().Run(context.Background(), spec); !errors.Is(err, ErrVersionMismatch) {
			t.Fatalf("err = %v, want ErrVersionMismatch", err)
		}
	})
	t.Run("missing binary", func(t *testing.T) {
		spec := fakeSpec(t, "backup_ok", restic.KindBackup)
		spec.Binary = filepath.Join(t.TempDir(), "nope.exe")
		if _, err := New().Run(context.Background(), spec); !errors.Is(err, ErrStart) {
			t.Fatalf("err = %v, want ErrStart", err)
		}
	})
}

func TestSecretsReachTheChildOnlyByEnvironment(t *testing.T) {
	spec := fakeSpec(t, "env_dump", restic.KindBackup)
	out := filepath.Join(t.TempDir(), "dump.txt")
	t.Setenv(fakerestic.Out, out)
	if _, err := New().Run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var args, env []string
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimRight(l, "\r")
		switch {
		case strings.HasPrefix(l, "ARGS "):
			args = append(args, l)
		case strings.HasPrefix(l, "ENV "):
			env = append(env, l)
		}
	}
	for _, a := range args {
		if strings.Contains(a, secretPassword) || strings.Contains(a, "pw@gw.example") {
			t.Errorf("a secret is on the command line: %q", a)
		}
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "ENV RESTIC_PASSWORD="+secretPassword) {
		t.Error("RESTIC_PASSWORD did not reach the child's environment")
	}
	if !strings.Contains(joined, "ENV RESTIC_REPOSITORY=rest:https://dev1:pw@gw.example/dev1/") {
		t.Error("RESTIC_REPOSITORY did not reach the child's environment")
	}
	if !strings.Contains(joined, "RESTIC_CACHE_DIR=") {
		t.Error("RESTIC_CACHE_DIR is not set (restic would use the SYSTEM profile)")
	}
}

// cancelWhenReady cancels with cause once the child has printed its first
// status line. The fake restic prints it only after it has installed its
// interrupt handler, so the cancel never lands while the child is still
// starting: a fixed sleep lost that race under -race on a loaded CI runner
// (run 39858363680), and the default handler then ended the child before the
// scenario could swallow the interrupt.
func cancelWhenReady(spec *Spec, cancel context.CancelCauseFunc, cause error) {
	var once sync.Once
	spec.ProgressInterval = 20 * time.Millisecond
	spec.OnProgress = func(Progress) { once.Do(func() { cancel(cause) }) }
}

func TestCancellation(t *testing.T) {
	t.Run("graceful or terminated, always cancelled", func(t *testing.T) {
		spec := fakeSpec(t, "hang_graceful", restic.KindBackup)
		spec.CancelGrace = 2 * time.Second
		ctx, cancel := context.WithCancelCause(context.Background())
		go func() {
			time.Sleep(500 * time.Millisecond)
			cancel(ErrCancelled)
		}()
		start := time.Now()
		res, err := verified(t, spec).Run(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		if res.Restic.Outcome != restic.OutcomeCancelled || res.ExitCode != 130 || res.Cancel != restic.CancelAgent {
			t.Fatalf("result = %+v exit %d cancel %v", res.Restic, res.ExitCode, res.Cancel)
		}
		t.Logf("cancel mode: %s (L-CANCEL-1: %s)", res.CancelMode, runtime.GOOS)
		if runtime.GOOS != "windows" && res.CancelMode != CancelGraceful {
			t.Errorf("on %s SIGINT must end the child gracefully, got %q", runtime.GOOS, res.CancelMode)
		}
		if time.Since(start) > 8*time.Second {
			t.Errorf("cancel took %v", time.Since(start))
		}
	})
	t.Run("a child that ignores the interrupt is terminated after the grace period", func(t *testing.T) {
		spec := fakeSpec(t, "hang_ignore", restic.KindBackup)
		spec.CancelGrace = 400 * time.Millisecond
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		cancelWhenReady(&spec, cancel, ErrCancelled)
		start := time.Now()
		res, err := verified(t, spec).Run(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		if res.Restic.Outcome != restic.OutcomeCancelled || res.CancelMode != CancelTerminated {
			t.Fatalf("result = %+v mode %q", res.Restic, res.CancelMode)
		}
		if time.Since(start) > 15*time.Second {
			t.Errorf("termination took %v", time.Since(start))
		}
	})
	t.Run("a service stop is interrupted, not cancelled", func(t *testing.T) {
		spec := fakeSpec(t, "hang_ignore", restic.KindBackup)
		spec.CancelGrace = 300 * time.Millisecond
		ctx, cancel := context.WithCancelCause(context.Background())
		go func() {
			time.Sleep(400 * time.Millisecond)
			cancel(ErrServiceStop)
		}()
		res, err := verified(t, spec).Run(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		if res.Restic.Outcome != restic.OutcomeInterrupted || res.Cancel != restic.CancelService {
			t.Fatalf("result = %+v cancel %v", res.Restic, res.Cancel)
		}
	})
	t.Run("a child that exits 1 on its own while a cancel is pending has failed, not been cancelled", func(t *testing.T) {
		spec := fakeSpec(t, "exit1_racing_cancel", restic.KindBackup)
		spec.CancelGrace = 20 * time.Second // the supervisor must not be the one that ends it
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		cancelWhenReady(&spec, cancel, ErrCancelled)
		res, err := verified(t, spec).Run(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		if res.CancelMode == CancelTerminated {
			t.Skip("the interrupt could not be delivered here (no console shared with the child), so the supervisor terminated the child itself and cancelled is correct; the race needs a deliverable interrupt")
		}
		if res.ExitCode != 1 || res.Restic.Outcome != restic.OutcomeFailed {
			t.Fatalf("exit %d outcome %s: an exit 1 that races a cancel must keep restic.Map's result", res.ExitCode, res.Restic.Outcome)
		}
		if res.Cancel != restic.CancelAgent {
			t.Errorf("cancel cause = %v; the pending cancel is still reported", res.Cancel)
		}
		log := strings.Join(lines(t, spec.RunLogPath), "\n")
		if !strings.Contains(log, `"message_type":"cryo_supervisor_note"`) || !strings.Contains(log, `"cancel_cause":"agent"`) || !strings.Contains(log, `"exit_code":1`) {
			t.Errorf("the run log does not record the pending cancel:\n%s", log)
		}
	})
	t.Run("max runtime cancels and says so", func(t *testing.T) {
		spec := fakeSpec(t, "hang_ignore", restic.KindBackup)
		spec.CancelGrace, spec.MaxRuntime = 300*time.Millisecond, 400*time.Millisecond
		res, err := verified(t, spec).Run(context.Background(), spec)
		if err != nil {
			t.Fatal(err)
		}
		if !res.LongRun || res.Restic.Outcome != restic.OutcomeCancelled {
			t.Fatalf("result = %+v long run %v", res.Restic, res.LongRun)
		}
	})
}

func TestCancelBeforeTheChildExists(t *testing.T) {
	spec := fakeSpec(t, "backup_ok", restic.KindBackup)
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrServiceStop)
	if _, err := New().Run(ctx, spec); !errors.Is(err, ErrCancelledBeforeStart) {
		t.Fatalf("err = %v, want ErrCancelledBeforeStart", err)
	}
}

func TestDocumentCommandCapturesStdout(t *testing.T) {
	spec := fakeSpec(t, "backup_ok", restic.KindSnapshots)
	spec.RunLogPath = ""
	res, err := New().Run(context.Background(), spec)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("snapshots: %v exit %d", err, res.ExitCode)
	}
	snaps, err := restic.DecodeSnapshots(strings.NewReader(string(res.Stdout)))
	if err != nil || len(snaps) == 0 {
		t.Fatalf("decode snapshots: %v (%d)", err, len(snaps))
	}
}
