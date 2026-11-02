package maintain

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

const (
	dumpTree   = "/C/fixture/tree"
	dumpSnapID = "c4709fdaf86559b31287cd581af7076550d83b73b567458d851dcbdbb8d975cd"
	lsDirJSON  = `{"message_type":"snapshot","id":"` + dumpSnapID + `"}` + "\n" +
		`{"message_type":"node","name":"tree","type":"dir","path":"/C/fixture/tree"}` + "\n"
	lsFileJSON = `{"message_type":"snapshot","id":"` + dumpSnapID + `"}` + "\n" +
		`{"message_type":"node","name":"report.docx","type":"file","path":"/C/fixture/tree/report.docx","mtime":"2026-10-20T10:00:00Z"}` + "\n"
	docxBody = "PK\x03\x04 this is a docx, which is itself a zip"
)

// realZip builds a zip of the given entries, like `restic dump -a zip` of a
// directory, and writes it to a file outside every directory the tests watch.
func realZip(t *testing.T, entries map[string]string) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(entries[n]))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "restic-stdout.zip")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p, buf.Bytes()
}

// filesUnder lists every file under the given directories with its size. The fake
// restic's own bookkeeping in the harness (scenario and call log) is not the
// worker's and is left out.
func filesUnder(t *testing.T, dirs ...string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, d := range dirs {
		_ = filepath.WalkDir(d, func(p string, de fs.DirEntry, err error) error {
			if err != nil || de.IsDir() {
				return nil
			}
			if b := filepath.Base(p); b == "fake.json" || b == "fake.calls" {
				return nil
			}
			fi, _ := de.Info()
			if fi != nil {
				out[p] = fi.Size()
			} else {
				out[p] = -1
			}
			return nil
		})
	}
	return out
}

func sameTree(t *testing.T, when string, before, after map[string]int64) {
	t.Helper()
	for p := range after {
		if _, ok := before[p]; !ok {
			t.Errorf("%s: a file appeared on disk: %s", when, p)
		}
	}
}

// watchWriter collects the stream and, on its first write, records the files
// on disk while the dump is running.
type watchWriter struct {
	bytes.Buffer
	onFirst func()
	done    bool
}

func (w *watchWriter) Write(p []byte) (int, error) {
	if !w.done && w.onFirst != nil {
		w.done = true
		w.onFirst()
	}
	return w.Buffer.Write(p)
}

func dumpArgs(t *testing.T, e *tenv) []string {
	t.Helper()
	for _, c := range e.repoCalls() {
		if c.Args[0] == "dump" {
			return c.Args
		}
	}
	t.Fatal("no dump call")
	return nil
}

// TestDumpStreamsADirectory: restic's zip is copied to the destination as it
// arrives, restic writes to stdout (no --target), and no file is created
// anywhere the worker writes, during or after the dump.
func TestDumpStreamsADirectory(t *testing.T) {
	src, want := realZip(t, map[string]string{"report.txt": "quarterly report\n", "notes.md": "# notes\n"})
	e := newTenv(t, []fakeRule{
		{Cmd: "ls", Stdout: "inline:" + lsDirJSON},
		{Cmd: "dump", StdoutFile: src},
	}, nil)
	before := filesUnder(t, e.scratch, e.state)
	var during map[string]int64
	out := &watchWriter{onFirst: func() { during = filesUnder(t, e.scratch, e.state) }}
	res, err := e.w.Dump(e.ctx, adminpath.DumpRequest{DeviceRequest: dev(e.esc), SnapshotID: "latest", Path: dumpTree}, out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("streamed %d bytes, want restic's %d", out.Len(), len(want))
	}
	zr, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil || len(zr.File) != 2 {
		t.Fatalf("not the directory's zip: %v", err)
	}
	if res.Outcome != restic.OutcomeSuccess {
		t.Fatalf("result = %+v", res)
	}
	args := strings.Join(dumpArgs(t, e), " ")
	if !strings.HasPrefix(args, "dump -a zip latest "+dumpTree) || strings.Contains(args, "--target") || !strings.Contains(args, "--no-cache") {
		t.Fatalf("dump args = %q: want dump -a zip to stdout with --no-cache", args)
	}
	sameTree(t, "during the dump", before, during)
	sameTree(t, "after the dump", before, filesUnder(t, e.scratch, e.state))
	if n := len(e.consoleRuns(e.esc)); n != 0 {
		t.Errorf("dump recorded %d console runs", n)
	}
}

// TestDumpWrapsASingleFileOnTheFly: restic writes a file's raw bytes; the worker
// wraps them into a one-entry zip while streaming, never through a temp file.
func TestDumpWrapsASingleFileOnTheFly(t *testing.T) {
	e := newTenv(t, []fakeRule{
		{Cmd: "ls", Stdout: "inline:" + lsFileJSON},
		{Cmd: "dump", Stdout: "inline:" + docxBody},
	}, nil)
	before := filesUnder(t, e.scratch, e.state)
	var during map[string]int64
	out := &watchWriter{onFirst: func() { during = filesUnder(t, e.scratch, e.state) }}
	if _, err := e.w.Dump(e.ctx, adminpath.DumpRequest{DeviceRequest: dev(e.esc), SnapshotID: "latest", Path: dumpTree + "/report.docx"}, out); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatalf("the stream is not a zip: %v", err)
	}
	if len(zr.File) != 1 || zr.File[0].Name != "report.docx" || !zr.File[0].Modified.Equal(time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("entries = %+v", zr.File)
	}
	rc, _ := zr.File[0].Open()
	body, _ := io.ReadAll(rc)
	rc.Close()
	if string(body) != docxBody {
		t.Fatalf("entry body = %q", body)
	}
	if strings.Contains(strings.Join(dumpArgs(t, e), " "), "--target") {
		t.Fatal("a single file must not be dumped to a file")
	}
	sameTree(t, "during the dump", before, during)
	sameTree(t, "after the dump", before, filesUnder(t, e.scratch, e.state))
}

func TestDumpSnapshotRootSkipsTheLookup(t *testing.T) {
	src, _ := realZip(t, map[string]string{"a": "b"})
	e := newTenv(t, []fakeRule{{Cmd: "dump", StdoutFile: src}}, nil)
	if _, err := e.w.Dump(e.ctx, adminpath.DumpRequest{DeviceRequest: dev(e.esc), SnapshotID: "latest", Path: "/"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if calls := e.repoCalls(); len(calls) != 1 || calls[0].Args[0] != "dump" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestDumpNotInTheSnapshot(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "ls", Stdout: "inline:" + `{"message_type":"snapshot","id":"` + dumpSnapID + `"}` + "\n"}}, nil)
	var out bytes.Buffer
	_, err := e.w.Dump(e.ctx, adminpath.DumpRequest{DeviceRequest: dev(e.esc), SnapshotID: "latest", Path: "/nope"}, &out)
	wantCode(t, err, adminpath.CodeNotFound, "")
	if out.Len() != 0 {
		t.Fatal("bytes were streamed for a path that is not there")
	}
}

// TestDumpFailureNeverEndsInAValidZip: restic fails after part of a file was
// streamed; the error is a failure, the one-entry archive is never closed, so
// what reached the destination is not a well-formed zip, and nothing is on disk.
func TestDumpFailureNeverEndsInAValidZip(t *testing.T) {
	e := newTenv(t, []fakeRule{
		{Cmd: "ls", Stdout: "inline:" + lsFileJSON},
		{Cmd: "dump", Exit: 1, Stdout: "inline:" + strings.Repeat("half a file ", 1000), Stderr: "inline:Fatal: pack not found\n"},
	}, nil)
	before := filesUnder(t, e.scratch, e.state)
	var out bytes.Buffer
	_, err := e.w.Dump(e.ctx, adminpath.DumpRequest{DeviceRequest: dev(e.esc), SnapshotID: "latest", Path: dumpTree + "/report.docx"}, &out)
	wantCode(t, err, adminpath.CodeResticFailed, "")
	if _, zerr := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len())); zerr == nil {
		t.Fatal("a failed dump produced a well-formed zip")
	}
	sameTree(t, "after a failed dump", before, filesUnder(t, e.scratch, e.state))
}

// brokenDest accepts nothing.
type brokenDest struct{}

func (brokenDest) Write([]byte) (int, error) { return 0, errors.New("browser went away") }

// TestDumpDestinationFailureStopsRestic: when the destination fails, restic is
// cancelled at once instead of reading the rest of the snapshot.
func TestDumpDestinationFailureStopsRestic(t *testing.T) {
	src, _ := realZip(t, map[string]string{"big": strings.Repeat("x", 1<<20)})
	e := newTenv(t, []fakeRule{
		{Cmd: "ls", Stdout: "inline:" + lsDirJSON},
		{Cmd: "dump", StdoutFile: src, SleepAfterMS: 20000},
	}, nil)
	t0 := time.Now()
	_, err := e.w.Dump(e.ctx, adminpath.DumpRequest{DeviceRequest: dev(e.esc), SnapshotID: "latest", Path: dumpTree}, brokenDest{})
	if err == nil {
		t.Fatal("a dump into a failed destination succeeded")
	}
	if d := time.Since(t0); d > 15*time.Second {
		t.Fatalf("restic kept running for %v after the destination failed", d)
	}
}

// TestEveryResticCallRunsWithoutACache (ADR 0008): every restic invocation,
// the start-up version check included, carries --no-cache, none gets a
// RESTIC_CACHE_DIR, and temp files go to the scratch directory.
func TestEveryResticCallRunsWithoutACache(t *testing.T) {
	e := newTenv(t, []fakeRule{
		{Cmd: "cat", Stdout: "cat_config.json"},
		{Cmd: "snapshots", Stdout: "inline:[]"},
		{Cmd: "ls", Stdout: "inline:" + lsDirJSON},
	}, nil)
	if _, err := e.w.CatConfig(e.ctx, dev(e.esc)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.w.Snapshots(e.ctx, dev(e.esc)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.w.Ls(e.ctx, adminpath.LsRequest{DeviceRequest: dev(e.esc), SnapshotID: "latest", Dir: dumpTree}); err != nil {
		t.Fatal(err)
	}
	calls := e.calls()
	if len(calls) < 4 {
		t.Fatalf("%d restic calls, want version, cat, snapshots and ls", len(calls))
	}
	for _, c := range calls {
		joined := strings.Join(c.Args, " ")
		has := false
		for _, a := range c.Args {
			has = has || a == "--no-cache"
		}
		if !has {
			t.Errorf("%s ran without --no-cache: %q", c.Args[0], joined)
		}
		if c.CacheSet {
			t.Errorf("%s got RESTIC_CACHE_DIR", c.Args[0])
		}
		if !strings.HasPrefix(c.TmpDir, e.scratch) {
			t.Errorf("%s has TMPDIR %q outside the scratch directory", c.Args[0], c.TmpDir)
		}
	}
}

// shortTempDir is a temporary directory with a short path (unix socket paths
// are limited to about 100 bytes).
func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "cm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// waitSocket waits until the admin-path socket answers a ping.
func waitSocket(t *testing.T, sock string) {
	t.Helper()
	c := adminpath.NewClient(sock, adminpath.ClientOptions{})
	defer c.Close()
	var err error
	for i := 0; i < 100; i++ {
		if _, err = c.Ping(context.Background()); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the admin-path socket never answered: %v", err)
}

// TestDumpOverTheSocket runs the whole worker half of a technician download
// through the admin-path socket with the fake restic: the console-side Client
// receives restic's zip byte for byte, with the size and SHA-256 of what
// crossed the socket, and no file appears on disk.
func TestDumpOverTheSocket(t *testing.T) {
	src, want := realZip(t, map[string]string{"report.txt": "quarterly report\n"})
	e := newTenv(t, []fakeRule{
		{Cmd: "ls", Stdout: "inline:" + lsDirJSON},
		{Cmd: "dump", StdoutFile: src},
	}, nil)
	sock := filepath.Join(shortTempDir(t), "ap.sock")
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- adminpath.Serve(ctx, sock, adminpath.Handler(e.w)) }()
	defer func() { stop(); <-served }()
	waitSocket(t, sock)
	before := filesUnder(t, e.scratch, e.state)
	c := adminpath.NewClient(sock, adminpath.ClientOptions{})
	defer c.Close()
	var out bytes.Buffer
	res, err := c.Dump(context.Background(), adminpath.DumpRequest{DeviceRequest: dev(e.esc), SnapshotID: "latest", Path: dumpTree}, &out)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(want)
	if !bytes.Equal(out.Bytes(), want) || res.Bytes != int64(len(want)) || res.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("result %+v for %d bytes, want %d bytes sha256 %x", res, out.Len(), len(want), sum)
	}
	sameTree(t, "after a dump over the socket", before, filesUnder(t, e.scratch, e.state))
}

// TestScratchIsPreparedAndEmptied: New creates the scratch directory 0700 and
// empties what a dead worker left in its tmp and rclone subdirectories, and
// touches nothing else.
func TestScratchIsPreparedAndEmptied(t *testing.T) {
	e := newTenv(t, nil, nil)
	left := filepath.Join(e.scratch, "tmp", "restic-temp-pack-123")
	cached := filepath.Join(e.scratch, "rclone", "vfs", "object")
	other := filepath.Join(e.scratch, "operator-note")
	for _, p := range []string{left, cached, other} {
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := New(e.cfg, e.st, e.keys, e.clk.Now); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{left, cached} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("leftover %s survived New", filepath.Base(p))
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("New removed a file it does not own")
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(e.scratch)
		if err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("scratch mode = %v, %v; want 0700", fi.Mode().Perm(), err)
		}
	}
}

// TestScratchOnDiskIsALoudWarning: on Linux the worker can tell a tmpfs from a
// disk; a scratch directory on disk is logged (fail loudly) but does not stop it.
func TestScratchOnDiskIsALoudWarning(t *testing.T) {
	e := newTenv(t, nil, nil)
	inMemory, known := onMemoryFS(e.scratch)
	logs := e.logs.String()
	switch {
	case !known:
		if strings.Contains(logs, "scratch_not_tmpfs") {
			t.Fatal("warned without knowing")
		}
		t.Skip("this platform cannot tell a tmpfs from a disk")
	case inMemory:
		if strings.Contains(logs, "scratch_not_tmpfs") {
			t.Fatal("warned for a tmpfs")
		}
	default:
		if !strings.Contains(logs, "scratch_not_tmpfs") || !strings.Contains(logs, "is not a tmpfs") {
			t.Fatalf("no warning for a scratch directory on disk:\n%s", logs)
		}
	}
}
