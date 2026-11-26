//go:build windows

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// TestNoPathLeavesTheDeviceOnRestore extends TestNoPathLeavesTheDevice to the
// restore of ADR 0027: a selection whose every path carries the patient-like
// sentinel, a restic whose error line names a sentinel file with its absolute
// path, files placed under sentinel folders, a keep-mode skip, a guard
// refusal; then every byte the console received, the device log, the journal
// and every Event Log line are searched for the sentinel. Only restic's own
// run log (logs\runs\<id>.jsonl) may hold it.
func TestNoPathLeavesTheDeviceOnRestore(t *testing.T) {
	r := newRestoreRig(t, "restore_file_error")
	var evMu sync.Mutex
	var events []string
	r.o.eventHook = func(level string, id uint32, msg string) {
		evMu.Lock()
		events = append(events, msg)
		evMu.Unlock()
	}
	const id = "01JB0000000000000000000R20"
	dir := sentinel + "-Jane-Doe"
	paths := []string{
		"/C/Users/" + dir + "/" + sentinel + "-diagnosis.docx",
		"/C/Users/" + dir + "/" + sentinel + "-labs [2].pdf",
		"/C/Users/" + dir + "/" + sentinel + "-kept.txt",
		"/C/Users/" + dir + "/" + sentinel + "-blocked/n.txt",
	}
	r.fc.setRestore(id, restoreAnswer{sel: selection(id, api.DeviceRestoreOriginal, api.DeviceOverwriteNever, paths...)})
	home := filepath.Join(r.drives["C"], "Users", dir)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(home, sentinel+"-kept.txt"), []byte("existing"), 0o644)
	// A file where the restore needs a folder: a guard refusal.
	_ = os.WriteFile(filepath.Join(home, sentinel+"-blocked"), []byte("a file"), 0o644)

	run := start(t, r.o)
	r.fc.enqueue(restoreCommand(t, id))
	rec := r.waitClosed(t, id)
	run.stop()

	if rec.Outcome != restic.OutcomePartial || rec.Stats.FilesRestored != 2 || rec.Stats.FilesSkipped != 1 {
		t.Fatalf("record = %s / %+v", rec.Outcome, rec.Stats)
	}
	got := map[api.RestoreResultClass]int64{}
	for _, row := range rec.Stats.RestoreErrors {
		got[row.Class] = row.Count
	}
	if got[api.RestoreResultTargetLocked] != 1 || got[api.RestoreResultGuardRefused] != 1 {
		t.Fatalf("rows = %+v", rec.Stats.RestoreErrors)
	}
	if err := api.AssertPathFree(rec); err != nil {
		t.Fatal(err)
	}

	forbidden := []string{sentinel, "Jane-Doe", r.base, strings.ReplaceAll(r.base, `\`, `\\`), strings.ToLower(r.base)}
	bodies := r.fc.allBodies()
	if len(bodies) < 5 {
		t.Fatalf("only %d requests reached the console", len(bodies))
	}
	for _, b := range bodies {
		for _, bad := range forbidden {
			if strings.Contains(string(b.Body), bad) || strings.Contains(b.Path, bad) {
				t.Errorf("%s %s carried %q", b.Method, b.Path, bad)
			}
		}
	}
	layout := config.Layout{Root: r.dir}
	for _, file := range []string{layout.AgentLog(), layout.JournalFile(), layout.JournalFile() + "-wal"} {
		b, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		if strings.Contains(string(b), sentinel) {
			t.Errorf("%s holds the sentinel; only logs\\runs\\<id>.jsonl may", filepath.Base(file))
		}
	}
	evMu.Lock()
	for _, e := range events {
		if strings.Contains(e, sentinel) {
			t.Errorf("an Event Log line holds the sentinel: %q", e)
		}
	}
	evMu.Unlock()
	if log := readFile(t, layout.RunLog(id)); !strings.Contains(log, sentinel) {
		t.Error("the local run log lost restic's raw lines")
	}
	r.noIncludeFilesLeft(t)
}
