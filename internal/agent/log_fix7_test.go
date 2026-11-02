package agent

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
	"github.com/c2tech-sys/cryobackup/internal/agent/internal/fakerestic"
)

// L-LOG-1: agent.log stayed at 0 bytes (as dir and Explorer list it) on the
// lab VM while the service ran for hours. New opens the device log the same
// way in service and console mode (openDeviceLog, flushing every line:
// logs.TestLinesAreVisibleWhileTheLogIsOpen has the NTFS case). Here it runs
// on a state directory whose agent.log already exists, empty, as an earlier
// process leaves it, and while the agent still holds the file open its lines
// are in the file and the directory listing shows their size.
func TestAgentLogLinesAreVisibleWhileTheAgentRuns(t *testing.T) {
	fc := newFakeConsole(t)
	dir := enroll(t, fc)
	fakerestic.Use(t, "backup_ok")
	layout := config.Layout{Root: dir}
	if err := os.WriteFile(layout.AgentLog(), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	a, err := New(context.Background(), fakeOptions(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for i := 0; i < 40; i++ {
		a.log.Info("backup finished", "run", "01JB0000000000000000000001", "outcome", "success", "pad", strings.Repeat("x", 120))
	}

	// The listing first: closing another handle to the file (the read
	// below) refreshes the directory entry and would hide a stale size.
	var listed int64 = -1
	entries, err := os.ReadDir(layout.LogsDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "agent.log" {
			info, err := e.Info()
			if err != nil {
				t.Fatal(err)
			}
			listed = info.Size()
		}
	}
	b, err := os.ReadFile(layout.AgentLog())
	if err != nil || !strings.Contains(string(b), `"msg":"agent started"`) {
		t.Fatalf("agent.log holds %q (%v), want the start line", b, err)
	}
	if listed != int64(len(b)) {
		t.Fatalf("a directory listing shows agent.log as %d bytes while the open file holds %d", listed, len(b))
	}
}
