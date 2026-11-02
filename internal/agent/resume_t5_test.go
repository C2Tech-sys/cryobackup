package agent

import (
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// The live tests of M1-T5 run the REAL pinned restic (CRYO_RESTIC, Windows,
// like TestLiveAgentLoop) through the real agent against a local REST gateway
// (restgw_t5_test.go) that they stop in the middle of the upload and
// restart. What they establish about restic 0.19.1, measured here:
//
//   - A refused connection is not an exit. restic retries every request with a
//     growing backoff (up to a minute and a half between tries) for up to 15
//     minutes per backend operation (internal/global/global.go:
//     retry.New(be, 15*time.Minute, ...)): the data save gave up after about
//     15 minutes, but the lock refresh kept retrying and the process did not
//     exit. Left alone, a gateway that is down mid-run would keep the run open
//     and "running" until max_runtime. The agent's gateway watchdog
//     (gateway_watchdog.go, M1-FIX4) ends it: no progress and no gateway for
//     throttle.gateway_down_grace_minutes (15 by default) stops restic and
//     journals the run deferred / gateway. The live tests below end the run
//     sooner with the operator's cancel (cancelled), and neither is a failure.
//   - A gateway that is back within restic's retries costs nothing: the run
//     finishes in the same attempt.
//   - What an interrupted attempt leaves behind is only usable by the next
//     attempt once restic has saved an index that lists it. restic saves one
//     when the in-memory index is ten minutes old or holds 3 x 50,000 blobs
//     (compressed repositories), checked each time a pack is stored; a short
//     run never gets there, so its packs are orphans and the next attempt
//     uploads the data again. That is the "resume from index checkpoints" of
//     architecture 6.2, and TestT5LiveResumeFromAnIndexCheckpoint measures it
//     with a run long enough to have one.

// liveRig is a real-restic agent run against a stoppable gateway.
type liveRig struct {
	t     *testing.T
	exe   string
	work  string
	tree  string
	total int64 // bytes in the tree
	g     *restGateway
	fc    *fakeConsole
	dir   string
	env   []string
}

func newLiveRig(t *testing.T, files, size int, tune func(*policy.Policy, *Options)) *liveRig {
	t.Helper()
	exe := os.Getenv("CRYO_RESTIC")
	if exe == "" {
		t.Skip("CRYO_RESTIC is not set: the live tests need the pinned restic 0.19.1 binary")
	}
	if runtime.GOOS != "windows" {
		t.Skip("the live loop backs up Windows paths; run it on Windows")
	}
	l := &liveRig{t: t, exe: exe, work: t.TempDir(), total: int64(files) * int64(size)}
	l.tree = filepath.Join(l.work, "tree")
	if err := os.MkdirAll(l.tree, 0o755); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, size)
	for i := 0; i < files; i++ {
		if _, err := rand.Read(b); err != nil { // incompressible: the repository grows as much as the tree
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(l.tree, fmt.Sprintf("f%02d.bin", i)), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	l.g = newRestGateway(t)
	l.env = append(os.Environ(), "RESTIC_REPOSITORY="+l.g.Repository(), "RESTIC_PASSWORD="+testRepoSecret,
		"RESTIC_CACHE_DIR="+filepath.Join(l.work, "toolcache"))
	if out, err := l.restic("init", "--quiet"); err != nil {
		t.Fatalf("restic init: %v\n%s", err, out)
	}

	l.fc = newFakeConsole(t)
	pol := testPolicy()
	pol.Selection.Roots = []string{l.tree}
	pol.Selection.VSS.Enabled = false // VSS needs an elevated process
	o := fakeOptions(t, "")
	if tune != nil {
		tune(&pol, &o)
	}
	l.fc.setPolicy(api.PolicyResponse{Policy: pol, PolicyID: testPolicyID, Version: 1, ETag: `"policy-v1"`})
	l.dir = enroll(t, l.fc)
	o.StateDir = l.dir
	o.ResticPath, o.ResticSHA256 = exe, fileSHA(t, exe)
	o.insecureLocalRepo = func(string) string { return l.g.Repository() }
	o.environment = func() policy.Environment { return policy.Environment{} }
	start(t, o)
	return l
}

// restic runs the binary against the gateway, outside the agent.
func (l *liveRig) restic(args ...string) (string, error) {
	c := exec.Command(l.exe, args...)
	c.Env = l.env
	out, err := c.CombinedOutput()
	return string(out), err
}

// checkRepository runs "restic check" over the whole repository. It takes no
// lock: the interrupted attempt could not remove its own lock while the
// gateway was down, and until it is 30 minutes old (or an unlock) restic's
// exclusive lock for check would be refused with exit 11.
func (l *liveRig) checkRepository() {
	l.t.Helper()
	if out, err := l.restic("check", "--read-data", "--no-lock"); err != nil {
		l.t.Fatalf("restic check after the interrupted and the resumed run: %v\n%s", err, out)
	}
}

// waitStopped waits for the gateway to have gone away on its own trigger.
func (l *liveRig) waitStopped() {
	l.t.Helper()
	waitFor(l.t, "the gateway to go away mid-run", 20*time.Minute, func() bool { return l.g.stopped.Load() })
	time.Sleep(2 * time.Second)
	if n := len(l.fc.closedRuns()); n != 0 {
		l.t.Fatalf("the run was closed within seconds of the outage: %+v", l.fc.closedRuns()[0])
	}
	l.fc.mu.Lock()
	open := len(l.fc.runOrder)
	l.fc.mu.Unlock()
	if open != 1 {
		l.t.Fatalf("%d runs known to the console during the outage, want the one open run", open)
	}
}

// waitStalled waits for a heartbeat that reports the open run as stalled while
// the device is still "running": the console is told, nothing is silent.
func (l *liveRig) waitStalled() {
	l.t.Helper()
	waitFor(l.t, "a heartbeat that reports the run stalled", 5*time.Minute, func() bool {
		for _, hb := range l.fc.heartbeatList() {
			if hb.ServiceState == api.ServiceRunning && hb.Progress != nil && hb.Progress.Stalled {
				return true
			}
		}
		return false
	})
}

// interruptAndResume ends the open run with the cancel command (the effect of
// max_runtime or an operator while the gateway is down), restarts the gateway,
// asks for the next attempt and returns both runs.
func (l *liveRig) interruptAndResume() (first, second api.RunRecord) {
	l.t.Helper()
	l.fc.enqueue(command(1, api.CommandCancel, ""))
	waitFor(l.t, "the interrupted run to be closed", 3*time.Minute, func() bool { return len(l.fc.closedRuns()) == 1 })
	first = l.fc.closedRuns()[0]
	l.g.Start()
	l.fc.enqueue(command(2, api.CommandBackupNow, ""))
	waitFor(l.t, "the next attempt to finish", 5*time.Minute, func() bool { return len(l.fc.closedRuns()) == 2 })
	return first, l.fc.closedRuns()[1]
}

// TestT5LiveOutageWithinResticsRetriesIsBridged: the gateway is stopped after
// two packs and restarted seconds later. The run was never closed while the
// gateway was away, and it ends as one success holding every file.
func TestT5LiveOutageWithinResticsRetriesIsBridged(t *testing.T) {
	l := newLiveRig(t, 12, 8<<20, nil)
	l.g.StopAfterData.Store(2)
	l.waitStopped()
	l.g.Start()

	waitFor(t, "the run to finish after the gateway came back", 5*time.Minute, func() bool { return len(l.fc.closedRuns()) == 1 })
	run := l.fc.closedRuns()[0]
	if run.Outcome != restic.OutcomeSuccess || run.Attempt != 1 || len(run.SnapshotID) != 64 || run.ErrorClass != "" {
		t.Fatalf("run = %+v", run)
	}
	if run.Stats.TotalFilesProcessed != 12 || run.Stats.FailedFiles != 0 || int64(run.Stats.TotalBytesProcessed) != l.total {
		t.Errorf("stats = %+v", run.Stats)
	}
	l.checkRepository()
}

// TestT5LiveInterruptedRunIsNotFailedAndTheNextAttemptCompletes: the gateway
// is stopped mid-run and the console is told the run is stalled; the run is
// ended while the gateway is down (cancel command); the
// gateway comes back and the next attempt completes against it. The interrupted
// run is cancelled, never failed; the next attempt is a full success, and the
// repository is consistent. A run this short has no index checkpoint (see the
// comment at the top), so the next attempt uploads the data again: it must not
// add more than the data set.
func TestT5LiveInterruptedRunIsNotFailedAndTheNextAttemptCompletes(t *testing.T) {
	l := newLiveRig(t, 12, 8<<20, func(p *policy.Policy, o *Options) {
		p.SLA.StallAfterMinutes = 1
		o.cancelGrace = 10 * time.Second
	})
	l.g.StopAfterData.Store(2)
	l.waitStopped()
	l.waitStalled()
	first, second := l.interruptAndResume()

	if first.Outcome != restic.OutcomeCancelled || first.SnapshotID != "" {
		t.Fatalf("interrupted run = %+v, want cancelled without a snapshot (never failed)", first)
	}
	if second.Outcome != restic.OutcomeSuccess || second.Attempt != 2 || len(second.SnapshotID) != 64 || second.Stats.TotalFilesProcessed != 12 {
		t.Fatalf("next attempt = %+v", second)
	}
	if limit := uint64(l.total + l.total/50); second.Stats.DataAdded > limit {
		t.Errorf("the next attempt added %d bytes for a %d-byte tree", second.Stats.DataAdded, l.total)
	}
	t.Logf("short run, no checkpoint: the next attempt added %d of %d bytes", second.Stats.DataAdded, l.total)
	l.checkRepository()
}

// TestT5LiveResumeFromAnIndexCheckpoint is the resume of architecture 6.2 /
// M1 acceptance: a backup that has run long enough for restic to have saved
// an index checkpoint, interrupted by the loss of the gateway, is resumed by
// the next attempt, which adds less than the whole data set. "Long enough" is
// ten minutes (restic's index age limit), so the upload is throttled through
// the policy (throttle.upload_kibps) and the gateway is stopped the moment the
// checkpoint index arrives. About thirteen minutes; set CRYO_T5_SLOW=1.
// (Measured with restic 0.19.1: the checkpoint index arrived after eleven
// minutes with all seven packs listed, and the next attempt added 3,771 bytes
// of a 100,663,296-byte tree; without a checkpoint it adds the whole tree.)
func TestT5LiveResumeFromAnIndexCheckpoint(t *testing.T) {
	if os.Getenv("CRYO_T5_SLOW") == "" {
		t.Skip("takes about thirteen minutes (restic saves an index checkpoint only after ten): set CRYO_T5_SLOW=1")
	}
	l := newLiveRig(t, 12, 8<<20, func(p *policy.Policy, o *Options) {
		p.Throttle.Upload = 130 // KiB/s: about twelve minutes for the 96 MiB tree
		o.cancelGrace = 10 * time.Second
	})
	var indexes, dataAtIndex atomic.Int64
	l.g.OnSave = func(key string) {
		if strings.HasPrefix(key, "index/") && indexes.Add(1) == 1 {
			dataAtIndex.Store(l.g.DataSaves.Load())
			if l.g.stopped.CompareAndSwap(false, true) {
				go l.g.Stop()
			}
		}
	}
	l.waitStopped()
	t.Logf("checkpoint index stored with %d data packs; the gateway is down", dataAtIndex.Load())
	first, second := l.interruptAndResume()

	if first.Outcome != restic.OutcomeCancelled {
		t.Fatalf("interrupted run = %+v", first)
	}
	if second.Outcome != restic.OutcomeSuccess || second.Attempt != 2 || second.Stats.TotalFilesProcessed != 12 {
		t.Fatalf("next attempt = %+v", second)
	}
	t.Logf("the next attempt added %d of %d bytes", second.Stats.DataAdded, l.total)
	if second.Stats.DataAdded >= uint64(l.total)*9/10 {
		t.Errorf("the next attempt added %d of %d bytes: nothing was resumed from the checkpoint", second.Stats.DataAdded, l.total)
	}
	l.checkRepository()
}
