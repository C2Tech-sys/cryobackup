package agent

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
	"github.com/c2tech-sys/cryobackup/internal/agent/diag"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// TestLiveAgentLoop is the lab loop of ticket P1-T7: enroll against a fake
// console, one scheduled backup with the REAL pinned restic into a real
// repository on disk, the run uploaded, the heartbeat idle, and
// "cryo-agent status" printing the device state. Gated on CRYO_RESTIC (the
// path of restic 0.19.1); it runs on Windows because the policy's roots are
// Windows paths and restic's flags are the Windows ones.
func TestLiveAgentLoop(t *testing.T) {
	resticExe := os.Getenv("CRYO_RESTIC")
	if resticExe == "" {
		t.Skip("CRYO_RESTIC is not set: the live loop needs the pinned restic 0.19.1 binary")
	}
	if runtime.GOOS != "windows" {
		t.Skip("the live loop backs up Windows paths; run it on Windows")
	}
	ctx := context.Background()
	work := t.TempDir()
	tree, repo := filepath.Join(work, "tree"), filepath.Join(work, "repo")
	for name, body := range map[string]string{
		"a.txt": "alpha", filepath.Join("sub", "b.docx"): strings.Repeat("b", 4096), filepath.Join("sub", "deeper", "c.bin"): strings.Repeat("c", 100000),
	} {
		p := filepath.Join(tree, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// The repository is created with the binary itself ("restic init" is not an agent function).
	initCmd := exec.Command(resticExe, "init", "--quiet")
	initCmd.Env = append(os.Environ(),
		"RESTIC_REPOSITORY="+repo, "RESTIC_PASSWORD="+testRepoSecret, "RESTIC_CACHE_DIR="+filepath.Join(work, "initcache"))
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("restic init: %v\n%s", err, out)
	}

	fc := newFakeConsole(t)
	pol := testPolicy()
	pol.Selection.Roots = []string{tree}
	pol.Selection.VSS.Enabled = false // VSS needs an elevated process; the lab loop is unprivileged
	fc.setPolicy(api.PolicyResponse{Policy: pol, PolicyID: testPolicyID, Version: 1, ETag: `"policy-v1"`})
	dir := enroll(t, fc)

	o := fakeOptions(t, dir)
	o.ResticPath, o.ResticSHA256 = resticExe, fileSHA(t, resticExe) // the pin is checked against the real file
	o.insecureLocalRepo = func(string) string { return repo }       // GatewayRepository is replaced by a local repository
	o.environment = func() policy.Environment { return policy.Environment{} }
	r := start(t, o)

	waitFor(t, "the closed run at the console", 120*time.Second, func() bool { return len(fc.closedRuns()) == 1 })
	run := fc.closedRuns()[0]
	t.Logf("uploaded run: outcome=%s exit=%d snapshot=%s files=%d new=%d added=%d B duration=%.1fs trigger=%s attempt=%d",
		run.Outcome, *run.ExitCode, run.SnapshotID, run.Stats.TotalFilesProcessed, run.Stats.FilesNew, run.Stats.DataAdded, run.Stats.TotalDurationS, run.Trigger, run.Attempt)
	if run.Outcome != restic.OutcomeSuccess || *run.ExitCode != 0 {
		t.Fatalf("run = %+v", run)
	}
	if !hex64.MatchString(run.SnapshotID) {
		t.Errorf("snapshot id %q is not 64 lower-case hex characters", run.SnapshotID)
	}
	if run.Stats.TotalFilesProcessed != 3 || run.Stats.FilesNew != 3 || run.Stats.FailedFiles != 0 || run.Stats.DataAdded == 0 {
		t.Errorf("stats = %+v, want the three files of the tree", run.Stats)
	}
	if err := api.AssertPathFree(run); err != nil {
		t.Errorf("the uploaded record is not path-free: %v", err)
	}

	// The snapshot list follows, with the configured root and the run's own snapshot.
	waitFor(t, "the snapshot upload", 60*time.Second, func() bool { return len(fc.snapshotUploads()) >= 1 })
	found := false
	for _, s := range fc.snapshotUploads()[0].Snapshots {
		if s.ID == run.SnapshotID {
			found = true
			if s.Username != "" || len(s.Paths) != 1 || s.Hostname != testDeviceID {
				t.Errorf("snapshot as uploaded = %+v", s)
			}
			// The real restic stored the real ids as tags (no placeholders).
			tags := strings.Join(s.Tags, " ")
			for _, want := range []string{"cb.customer=" + testCustomerID, "cb.policy=" + testPolicyID + ".1", "cb.run=" + run.ID} {
				if !strings.Contains(tags, want) {
					t.Errorf("snapshot tags %q lack %q", tags, want)
				}
			}
		}
	}
	if !found {
		t.Error("the new snapshot is not in the uploaded list")
	}

	// Heartbeat sequence: idle once the run is over; every one valid and from this device.
	waitFor(t, "an idle heartbeat after the run", 60*time.Second, func() bool {
		hbs := fc.heartbeatList()
		return len(hbs) >= 2 && hbs[len(hbs)-1].ServiceState == api.ServiceIdle
	})
	hbs := fc.heartbeatList()
	states := make([]string, len(hbs))
	for i, h := range hbs {
		states[i] = string(h.ServiceState)
		if h.ResticVersion != "0.19.1" || len(h.HWFingerprint) != 64 {
			t.Errorf("heartbeat %d = %+v", i, h)
		}
	}
	t.Logf("heartbeat service states: %s", strings.Join(states, " "))
	if hbs[len(hbs)-1].LastErrorClass != "" {
		t.Errorf("last heartbeat reports error class %q after a clean run", hbs[len(hbs)-1].LastErrorClass)
	}

	// Nothing path-like went to the console in the run or heartbeat bodies.
	for _, b := range fc.allBodies() {
		if strings.HasSuffix(b.Path, "/snapshots") {
			continue // roots only, by design (§13.3)
		}
		if strings.Contains(string(b.Body), work) || strings.Contains(string(b.Body), strings.ReplaceAll(work, `\`, `\\`)) {
			t.Errorf("%s carried the work directory path", b.Path)
		}
	}

	// The run log is restic's own JSON stream, verbatim, on the device.
	layout := config.Layout{Root: dir}
	if log := readFile(t, layout.RunLog(run.ID)); !strings.Contains(log, `"message_type":"summary"`) || !strings.Contains(log, run.SnapshotID) {
		t.Errorf("run log does not look like restic's JSON stream:\n%.400s", log)
	}
	jr := openJournalRO(t, dir)
	waitFor(t, "delivered_at", 30*time.Second, func() bool {
		row, ok, _ := jr.Run(ctx, run.ID)
		return ok && row.DeliveredAt != nil
	})

	r.stop()

	// "cryo-agent status" prints the device state.
	rep, err := diag.Status(ctx, dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	rep.Render(&buf)
	t.Logf("cryo-agent status:\n%s", buf.String())
	for _, want := range []string{testDeviceID, "success", "last heartbeat:", "policy:"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("status lacks %q", want)
		}
	}
	t.Logf("LIVE LOOP RESULT: enroll -> policy -> scheduled backup -> run uploaded (%s, snapshot %s) -> heartbeat idle -> status printed", run.Outcome, run.SnapshotID[:12])
}
