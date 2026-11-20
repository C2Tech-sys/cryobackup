package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// P20-W3D: doctor's database-open causes and the heartbeat line.

// A missing -shm blames the directory only when doctor has tested that this account cannot create
// a file in it; a directory it can write in gets the neutral line. A -wal or -shm that is a
// directory is named as not a regular file.
func TestDoctorOpenFailureCausesW3D(t *testing.T) {
	unable := errors.New("store: open: unable to open database file: out of memory (14)")
	other := errors.New("store: open: disk I/O error (10)")
	for _, tc := range []struct {
		name     string
		openErr  error
		writable bool
		setup    func(t *testing.T, db string)
		want     string
		forbid   []string
	}{
		{"no -shm in a directory this account can write in", unable, true,
			func(t *testing.T, db string) { _ = os.Remove(db + "-shm") },
			"database could not be opened read-only", []string{"directory", "not writable"}},
		{"no -shm in a directory this account cannot write in", unable, false,
			func(t *testing.T, db string) { _ = os.Remove(db + "-shm") },
			"must create its -shm index file next to it, and this account cannot (the directory is not writable for it)", []string{"not readable"}},
		{"a -wal that is a directory", unable, true,
			func(t *testing.T, db string) { _ = os.Remove(db + "-wal"); mkdir(t, db+"-wal") },
			"database -wal file is not a regular file", []string{"not writable", "not readable"}},
		{"a -wal that is a directory, another open error", other, true,
			func(t *testing.T, db string) { _ = os.Remove(db + "-wal"); mkdir(t, db+"-wal") },
			"database -wal file is not a regular file", []string{"disk I/O"}},
		{"a -shm that is a directory, another open error", other, true,
			func(t *testing.T, db string) { _ = os.Remove(db + "-shm"); mkdir(t, db+"-shm") },
			"-shm index file next to it, and what stands at that name is not a regular file", []string{"disk I/O", "not writable"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, db := doctorRig(t)
			tc.setup(t, db)
			prevOpen, prevW := openNoMigrate, dirWritable
			t.Cleanup(func() { openNoMigrate, dirWritable = prevOpen, prevW })
			openNoMigrate = func(context.Context, store.Options) (*store.Store, []string, error) { return nil, nil, tc.openErr }
			var tested []string
			dirWritable = func(dir string) bool { tested = append(tested, dir); return tc.writable }
			code, out, _ := run(t, "", "doctor", "--config", cfgPath)
			if code != ExitFailed || !strings.Contains(out, doctorLine("FAIL", "database")) || !strings.Contains(out, tc.want) {
				t.Fatalf("%d\n%s", code, out)
			}
			for _, f := range tc.forbid {
				if strings.Contains(out, f) {
					t.Fatalf("the line says %q:\n%s", f, out)
				}
			}
			if strings.HasPrefix(tc.name, "no -shm") && len(tested) != 1 {
				t.Fatalf("the directory was tested %d times, want once", len(tested))
			}
		})
	}
}

// dirWritable itself: true for a directory this account can create a file in, and it leaves
// nothing behind; false for a directory that does not exist.
func TestDoctorDirWritableLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	if !dirWritable(dir) {
		t.Fatal("a temporary directory reads as not writable")
	}
	if ents, err := os.ReadDir(dir); err != nil || len(ents) != 0 {
		t.Fatalf("the test left %d entries (%v)", len(ents), err)
	}
	if dirWritable(dir + string(os.PathSeparator) + "missing") {
		t.Fatal("a missing directory reads as writable")
	}
}

// The heartbeat line prints an IPv6 host in brackets, as in a URL.
func TestDoctorHeartbeatLineIPv6(t *testing.T) {
	cfgPath, _ := doctorRig(t)
	setConfig(t, cfgPath, map[string]any{"healthcheck_ping": map[string]any{"url": "http://[::1]:9/ping/TOKENMARK-77a1", "method": "GET", "interval_s": 300}})
	_, out, _ := run(t, "", "doctor", "--config", cfgPath)
	want := dline("NOT CHECKED", "heartbeat ping", "configured: http://[::1], GET every 300 s; doctor does not ping it")
	if !strings.Contains(out, want) || strings.Contains(out, "TOKENMARK") {
		t.Fatalf("heartbeat line:\n%s", out)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
}
