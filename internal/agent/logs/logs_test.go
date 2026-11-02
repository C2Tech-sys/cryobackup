package logs

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "agent.log")
	r, err := OpenRotating(path, 100, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	line := strings.Repeat("x", 59) + "\n" // 60 bytes: two lines do not fit in 100
	for i := 0; i < 7; i++ {
		if _, err := r.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"agent.log", "agent.log.1", "agent.log.2"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), name)); err != nil {
			t.Errorf("%s is missing: %v", name, err)
		}
	}
	if _, err := os.Stat(path + ".3"); err == nil {
		t.Error("more files than the retention allows")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("x")); err == nil {
		t.Error("write after close succeeded")
	}
}

func TestAppendsToAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	for i := 0; i < 2; i++ {
		r, err := OpenRotating(path, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = r.Write([]byte("line\n"))
		_ = r.Close()
	}
	if b, _ := os.ReadFile(path); string(b) != "line\nline\n" {
		t.Errorf("file = %q", b)
	}
}

func TestLoggerWritesJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	r, _ := OpenRotating(path, 0, 0)
	defer r.Close()
	NewLogger(r, slog.LevelInfo).Info("hello", "run", "ID1")
	NewLogger(r, slog.LevelInfo).Debug("hidden")
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"msg":"hello"`) || !strings.Contains(string(b), `"run":"ID1"`) || strings.Contains(string(b), "hidden") {
		t.Errorf("log = %q", b)
	}
	Discard().Info("nothing")
}

func TestPruneRunLogs(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old, fresh, other := filepath.Join(dir, "old.jsonl"), filepath.Join(dir, "fresh.jsonl"), filepath.Join(dir, "keep.txt")
	for _, p := range []string{old, fresh, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	long := now.Add(-40 * 24 * time.Hour)
	for _, p := range []string{old, other} {
		if err := os.Chtimes(p, long, long); err != nil {
			t.Fatal(err)
		}
	}
	n, err := PruneRunLogs(dir, now, RunLogRetention)
	if err != nil || n != 1 {
		t.Fatalf("pruned %d: %v", n, err)
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("the old run log survived")
	}
	for _, p := range []string{fresh, other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed", filepath.Base(p))
		}
	}
	if n, err := PruneRunLogs(filepath.Join(dir, "missing"), now, RunLogRetention); n != 0 || err != nil {
		t.Errorf("missing directory: %d %v", n, err)
	}
}
