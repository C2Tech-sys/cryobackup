package logs

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// L-LOG-1: agent.log existed (empty) before the service opened it, and the
// service keeps it open for days, writing a line now and then. Every line
// must be in the file and visible in a directory listing while the handle is
// still open (listedSizes: on NTFS an unflushed file's listed size lagged
// behind, or stayed 0, until the handle was closed).
func TestLinesAreVisibleWhileTheLogIsOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil { // left by an earlier process
		t.Fatal(err)
	}
	r, err := OpenRotating(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	l := NewLogger(r, slog.LevelInfo)
	l.Info("agent started", "service_mode", true)
	// A service writes now and then, not in one burst.
	for i := 0; i < 40; i++ {
		l.Info("backup finished", "run", "01JB0000000000000000000001", "outcome", "success", "pad", strings.Repeat("x", 120))
		time.Sleep(20 * time.Millisecond)
	}

	// The listing comes first: closing any other handle to the file (the
	// read below) makes NTFS refresh the directory entry, which would hide
	// the problem.
	listed := listedSizes(t, dir, "agent.log")
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), `"msg":"agent started"`) {
		t.Fatalf("agent.log holds %q (%v), want the lines", b, err)
	}
	for how, got := range listed {
		if got != int64(len(b)) {
			t.Errorf("%s shows agent.log as %d bytes while it holds %d", how, got, len(b))
		}
	}
}
