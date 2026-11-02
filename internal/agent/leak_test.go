package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

const sentinel = "PHI-sentinel"

// TestNoPathLeavesTheDevice is the HIPAA check of architecture §8.4 item 7 on
// the agent: a tree whose file names carry a patient-like sentinel, a restic
// whose error lines name every one of those files, a run that is uploaded; then
// every byte the console received is searched for the sentinel (and for the
// tree's own path), while the local run log must still hold it (DH15).
func TestNoPathLeavesTheDevice(t *testing.T) {
	fc := newFakeConsole(t)
	dir := enroll(t, fc)

	tree := filepath.Join(t.TempDir(), sentinel+"-Jane-Doe")
	for rel, body := range map[string]string{
		sentinel + "-diagnosis.docx":               "x",
		sentinel + "-labs.pdf":                     "x",
		filepath.Join(sentinel+"-clinic", "n.txt"): "x",
	} {
		p := filepath.Join(tree, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fakerestic.Use(t, "pathy")
	t.Setenv(fakerestic.Tree, tree)

	r := start(t, fakeOptions(t, dir))
	waitFor(t, "the uploaded run", 30*time.Second, func() bool { return len(fc.closedRuns()) == 1 })
	waitFor(t, "the snapshot upload", 30*time.Second, func() bool { return len(fc.snapshotUploads()) >= 1 })
	waitFor(t, "an idle heartbeat", 30*time.Second, func() bool {
		hbs := fc.heartbeatList()
		return len(hbs) > 1 && hbs[len(hbs)-1].ServiceState == api.ServiceIdle
	})
	run := fc.closedRuns()[0]
	r.stop()

	// What the console did receive is the projection, and it is there.
	if run.Outcome != restic.OutcomePartial || run.Stats.FailedFiles != 3 {
		t.Fatalf("run = outcome %s, failed files %d", run.Outcome, run.Stats.FailedFiles)
	}
	exts := map[string]int{}
	for _, row := range run.ErrorCounts {
		if row.During == "archival" {
			exts[row.Ext] += row.Count
		}
		if strings.ContainsAny(row.During+string(row.Class)+row.Ext, `\/:`) {
			t.Errorf("a separator survived the projection: %+v", row)
		}
	}
	if exts["docx"] != 1 || exts["pdf"] != 1 || exts["txt"] != 1 {
		t.Errorf("error counts by extension = %v, want one each of docx, pdf, txt", exts)
	}

	// Not one byte the console received carries the sentinel, the tree path or the account name.
	forbidden := []string{
		sentinel, "Jane-Doe", tree, strings.ReplaceAll(tree, `\`, `\\`), strings.ToLower(tree),
		"fixture-user", // the snapshot fixture's account name must have been blanked
	}
	bodies := fc.allBodies()
	if len(bodies) < 5 {
		t.Fatalf("only %d requests reached the console", len(bodies))
	}
	for _, b := range bodies {
		for _, bad := range forbidden {
			if strings.Contains(string(b.Body), bad) {
				t.Errorf("%s %s carried %q", b.Method, b.Path, bad)
			}
		}
	}
	// ... and every body the console received passes the guard, except the two documented exemptions.
	for _, rr := range fc.snapshotUploads() {
		for _, s := range rr.Snapshots {
			if s.Username != "" {
				t.Errorf("snapshot username %q left the device", s.Username)
			}
		}
	}

	// The raw detail stayed on the device: the run log has the paths, and only under the state dir.
	layout := config.Layout{Root: dir}
	log := readFile(t, layout.RunLog(run.ID))
	if !strings.Contains(log, sentinel) {
		t.Fatal("the local run log lost the path detail it exists to keep")
	}
	if !strings.Contains(log, `"during":"archival"`) {
		t.Error("the local run log does not hold the raw error lines")
	}
	// Nothing else on the device logs restic's text or the journal's content: only the run log has it.
	for _, file := range []string{layout.AgentLog(), layout.JournalFile(), layout.JournalFile() + "-wal", layout.PolicyFile(), layout.ConfigFile()} {
		b, err := os.ReadFile(file)
		if err != nil {
			continue // the WAL may already be checkpointed away
		}
		if strings.Contains(string(b), sentinel) {
			t.Errorf("%s holds the sentinel; only logs\\runs\\<id>.jsonl may", filepath.Base(file))
		}
	}
}
