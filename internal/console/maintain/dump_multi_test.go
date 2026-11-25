package maintain

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// Tests of the several-paths download (dump_multi.go, ADR 0028) with the fake
// restic. The tar streams are built here the way restic 0.19.1 writes them (P22
// spike, item 6): every entry named by its full snapshot path without the
// leading "/", no entry for the dumped folder, PAX records for long and
// non-ASCII names.

const multiTree = "/C/fixture/tree"

// longName is over 100 bytes and non-ASCII: it needs PAX records in a tar.
var longName = strings.Repeat("long-folder-name-", 7) + "café üñï 日本.txt"

// lsTreeJSON is `ls --json` of multiTree: the snapshot, the folder itself, then
// its children.
func lsTreeJSON(children ...string) string {
	s := `{"message_type":"snapshot","id":"` + dumpSnapID + `"}` + "\n" +
		`{"message_type":"node","name":"tree","type":"dir","path":"` + multiTree + `"}` + "\n"
	for _, c := range children {
		s += c + "\n"
	}
	return s
}

func lsNode(name, typ string) string {
	b, err := json.Marshal(map[string]string{
		"message_type": "node", "name": name, "type": typ, "path": multiTree + "/" + name, "mtime": "2026-10-20T10:00:00Z",
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// noise is n bytes that deflate cannot shrink, so they leave the zip writer's
// buffer at once (a fixed seed: the same bytes every run).
func noise(n int) string {
	b := make([]byte, n)
	r := rand.New(rand.NewSource(int64(n)))
	_, _ = r.Read(b)
	return string(b)
}

// rawFile writes data to a file outside every watched directory, for a fake
// restic's raw stdout.
func rawFile(t *testing.T, data string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "restic-stdout.bin")
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// tarEntry is one entry of a fake `dump -a tar` stream.
type tarEntry struct {
	name string
	typ  byte
	body string
	link string
}

// fakeTar writes the entries as restic would and returns the stream; cut > 0
// keeps only that many bytes (a stream that stopped). It is written to a file
// outside every directory the tests watch (the fake restic reads it from
// there).
func fakeTar(t *testing.T, entries []tarEntry, cut int) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o666, ModTime: time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC), Format: tar.FormatPAX}
		if e.typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if e.typ == tar.TypeSymlink {
			h.Linkname = e.link
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	if cut > 0 && cut < len(data) {
		data = data[:cut]
	}
	p := filepath.Join(t.TempDir(), "restic-stdout.tar")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// docsTar is `dump -a tar` of multiTree/docs: a file, a sub-folder with a long
// non-ASCII name inside, a junction (a symlink to restic) and a named pipe.
func docsTar(t *testing.T) string {
	p := strings.TrimPrefix(multiTree, "/") + "/docs/"
	return fakeTar(t, []tarEntry{
		{name: p + "a.txt", typ: tar.TypeReg, body: "alpha\n"},
		{name: p + "sub/", typ: tar.TypeDir},
		{name: p + "sub/" + longName, typ: tar.TypeReg, body: "deep\n"},
		{name: p + "zjunction1", typ: tar.TypeSymlink, link: `C:\Users\x`},
		{name: p + "zfifo1", typ: tar.TypeFifo},
	}, 0)
}

// emptyTar is `dump -a tar` of an empty folder: a valid empty archive.
func emptyTar(t *testing.T) string { return fakeTar(t, nil, 0) }

// zipEntries opens data as a zip and returns its entries in order with their
// bodies; it fails the test when data is not a readable zip.
func zipEntries(t *testing.T, data []byte) ([]string, map[string]string) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("not a readable zip: %v", err)
	}
	var names []string
	bodies := map[string]string{}
	for _, f := range zr.File {
		names = append(names, f.Name)
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("entry %q: %v", f.Name, err)
		}
		bodies[f.Name] = string(b)
	}
	return names, bodies
}

func notAZip(t *testing.T, data []byte) {
	t.Helper()
	if _, err := zip.NewReader(bytes.NewReader(data), int64(len(data))); err == nil {
		t.Fatalf("a failed download of %d bytes is a readable zip", len(data))
	}
}

func multiReq(e *tenv, snap string, paths ...string) adminpath.DumpRequest {
	return adminpath.DumpRequest{DeviceRequest: dev(e.esc), SnapshotID: snap, Paths: paths}
}

// treeRules are the fake restic's answers for the standard tree.
func treeRules(t *testing.T) []fakeRule {
	return []fakeRule{
		{Cmd: "ls", Contains: " " + multiTree + " --", Stdout: "inline:" + lsTreeJSON(
			lsNode("docs", "dir"), lsNode("empty", "dir"), lsNode("zlink1", "symlink"), lsNode("report.docx", "file"))},
		{Cmd: "dump", Contains: "-a tar " + dumpSnapID + " " + multiTree + "/docs --", StdoutFile: docsTar(t)},
		{Cmd: "dump", Contains: "-a tar " + dumpSnapID + " " + multiTree + "/empty --", StdoutFile: emptyTar(t)},
		{Cmd: "dump", Contains: "dump " + dumpSnapID + " " + multiTree + "/report.docx --", Stdout: "inline:" + docxBody},
		{Cmd: "unlock"},
	}
}

// TestDumpMultiOneZipOfFilesAndFolders: three selections that are streamed (a
// folder with a sub-folder, a file, an empty folder), one symlink and one
// nested selection become ONE zip: entries named relative to each selection's
// parent, the folders' own entries present, the PAX name intact, the symlink
// and the named pipe left out and counted without names, one `ls` for the one
// parent, the snapshot "latest" pinned to the id restic reported, a raw dump
// for the file and `dump -a tar` for the folders, nothing on disk.
func TestDumpMultiOneZipOfFilesAndFolders(t *testing.T) {
	e := newTenv(t, treeRules(t), nil)
	before := filesUnder(t, e.scratch, e.state)
	var during map[string]int64
	out := &watchWriter{onFirst: func() { during = filesUnder(t, e.scratch, e.state) }}
	res, err := e.w.Dump(e.ctx, multiReq(e, "latest",
		multiTree+"/docs", multiTree+"/report.docx", multiTree+"/zlink1", multiTree+"/empty", multiTree+"/docs/a.txt"), out)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != restic.OutcomeSuccess {
		t.Fatalf("result %+v", res)
	}
	names, bodies := zipEntries(t, out.Bytes())
	want := []string{"docs/", "docs/a.txt", "docs/sub/", "docs/sub/" + longName, "report.docx", "empty/", skippedEntryName}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Fatalf("entries\n got %q\nwant %q", names, want)
	}
	if bodies["docs/a.txt"] != "alpha\n" || bodies["docs/sub/"+longName] != "deep\n" || bodies["report.docx"] != docxBody {
		t.Fatalf("bodies = %q", bodies)
	}
	skipped := bodies[skippedEntryName]
	for _, s := range []string{"left 3 item(s)", "symbolic links and junctions: 2", "named pipes: 1"} {
		if !strings.Contains(skipped, s) {
			t.Errorf("%s lacks %q:\n%s", skippedEntryName, s, skipped)
		}
	}
	for _, name := range []string{"zjunction1", "zfifo1", "zlink1", `C:\Users`} {
		if strings.Contains(skipped, name) {
			t.Errorf("%s names a skipped entry (%q)", skippedEntryName, name)
		}
	}
	var ls, dumps []string
	for _, c := range e.repoCalls() {
		j := strings.Join(c.Args, " ")
		if !strings.Contains(j, "--no-cache") || strings.Contains(j, "--target") {
			t.Errorf("call without --no-cache or with --target: %q", j)
		}
		switch c.Args[0] {
		case "ls":
			ls = append(ls, j)
		case "dump":
			dumps = append(dumps, j)
		}
	}
	if len(ls) != 1 {
		t.Fatalf("%d ls calls, want one for the one parent: %q", len(ls), ls)
	}
	wantDumps := []string{
		"dump -a tar " + dumpSnapID + " " + multiTree + "/docs ",
		"dump " + dumpSnapID + " " + multiTree + "/report.docx ",
		"dump -a tar " + dumpSnapID + " " + multiTree + "/empty ",
	}
	if len(dumps) != len(wantDumps) {
		t.Fatalf("dumps = %q", dumps)
	}
	for i, d := range dumps {
		if !strings.HasPrefix(d, wantDumps[i]) {
			t.Errorf("dump %d = %q, want prefix %q (latest pinned to the id ls reported)", i, d, wantDumps[i])
		}
	}
	sameTree(t, "during the download", before, during)
	sameTree(t, "after the download", before, filesUnder(t, e.scratch, e.state))
	for _, secret := range []string{"report.docx", "docs", longName, multiTree} {
		if strings.Contains(e.logs.String(), secret) {
			t.Fatalf("a log line contains %q", secret)
		}
	}
}

// TestDumpMultiRefusesHostileEntryNames is the zip-slip guard: any tar entry
// whose name could land outside the selected folder refuses the whole download
// loudly (a guard_refused failure and a log line with a class, never the name),
// and what was already sent is not a readable zip.
func TestDumpMultiRefusesHostileEntryNames(t *testing.T) {
	p := strings.TrimPrefix(multiTree, "/") + "/docs/"
	for name, hostile := range map[string]string{
		"dot-dot":           p + "../../../evil.txt",
		"absolute":          "/etc/evil",
		"outside":           strings.TrimPrefix(multiTree, "/") + "/other/evil.txt",
		"look-alike prefix": strings.TrimPrefix(multiTree, "/") + "/docsX/evil.txt",
		"backslash":         p + `a\..\..\evil.txt`,
		"drive":             p + "C:evil.txt",
		"dot component":     p + "x/./evil.txt",
		"empty component":   p + "x//evil.txt",
		"folder as a file":  strings.TrimPrefix(multiTree, "/") + "/docs",
	} {
		t.Run(name, func(t *testing.T) {
			src := fakeTar(t, []tarEntry{
				{name: p + "fine.txt", typ: tar.TypeReg, body: strings.Repeat("fine ", 2000)},
				{name: hostile, typ: tar.TypeReg, body: "evil\n"},
			}, 0)
			e := newTenv(t, []fakeRule{
				{Cmd: "ls", Stdout: "inline:" + lsTreeJSON(lsNode("docs", "dir"))},
				{Cmd: "dump", StdoutFile: src},
			}, nil)
			var out bytes.Buffer
			_, err := e.w.Dump(e.ctx, multiReq(e, "latest", multiTree+"/docs"), &out)
			wantCode(t, err, adminpath.CodeResticFailed, restic.ClassGuardRefused)
			notAZip(t, out.Bytes())
			logs := e.logs.String()
			if !strings.Contains(logs, "dump_unsafe_name") {
				t.Fatalf("no loud log line for the refusal:\n%s", logs)
			}
			if strings.Contains(logs, "evil") || strings.Contains(logs, "fine.txt") {
				t.Fatal("the log names an entry")
			}
		})
	}
	t.Run("selected name", func(t *testing.T) {
		e := newTenv(t, []fakeRule{{Cmd: "ls", Stdout: "inline:" + lsTreeJSON(lsNode(`a\..\b`, "file"))}}, nil)
		var out bytes.Buffer
		_, err := e.w.Dump(e.ctx, multiReq(e, "latest", multiTree+`/a\..\b`), &out)
		wantCode(t, err, adminpath.CodeResticFailed, restic.ClassGuardRefused)
		if out.Len() != 0 {
			t.Fatal("bytes were sent for a refused selection")
		}
	})
}

// TestDumpMultiSymlinkEntries: a folder of nothing but links (junctions come
// out of restic as symlinks) gives the folder's entry and the count, and no
// link target ever becomes file content (`dump -a zip` would do that).
func TestDumpMultiSymlinkEntries(t *testing.T) {
	p := strings.TrimPrefix(multiTree, "/") + "/docs/"
	src := fakeTar(t, []tarEntry{
		{name: p + "j1", typ: tar.TypeSymlink, link: `C:\secret\target-one`},
		{name: p + "j2", typ: tar.TypeSymlink, link: `C:\secret\target-two`},
		{name: p + "hard", typ: tar.TypeLink, link: p + "j1"},
		{name: p + "dev", typ: tar.TypeChar},
	}, 0)
	e := newTenv(t, []fakeRule{
		{Cmd: "ls", Stdout: "inline:" + lsTreeJSON(lsNode("docs", "dir"))},
		{Cmd: "dump", StdoutFile: src},
	}, nil)
	var out bytes.Buffer
	if _, err := e.w.Dump(e.ctx, multiReq(e, "latest", multiTree+"/docs"), &out); err != nil {
		t.Fatal(err)
	}
	names, bodies := zipEntries(t, out.Bytes())
	if strings.Join(names, "|") != "docs/|"+skippedEntryName {
		t.Fatalf("entries = %q", names)
	}
	s := bodies[skippedEntryName]
	for _, want := range []string{"left 4 item(s)", "symbolic links and junctions: 2", "hard links: 1", "devices: 1"} {
		if !strings.Contains(s, want) {
			t.Errorf("skipped file lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(out.String(), "target-one") || strings.Contains(s, "j1") {
		t.Fatal("a link target or a link name reached the zip")
	}
}

// TestDumpMultiPartialTarIsAFailure: restic exits 0 but its tar stops in the
// middle of an entry: the download fails (parse) and is not a readable zip.
func TestDumpMultiPartialTarIsAFailure(t *testing.T) {
	p := strings.TrimPrefix(multiTree, "/") + "/docs/"
	src := fakeTar(t, []tarEntry{
		{name: p + "one.txt", typ: tar.TypeReg, body: strings.Repeat("1", 3000)},
		{name: p + "two.txt", typ: tar.TypeReg, body: strings.Repeat("2", 9000)},
	}, 6000)
	e := newTenv(t, []fakeRule{
		{Cmd: "ls", Stdout: "inline:" + lsTreeJSON(lsNode("docs", "dir"))},
		{Cmd: "dump", StdoutFile: src},
	}, nil)
	var out bytes.Buffer
	_, err := e.w.Dump(e.ctx, multiReq(e, "latest", multiTree+"/docs"), &out)
	wantCode(t, err, adminpath.CodeResticFailed, restic.ClassParse)
	notAZip(t, out.Bytes())
}

// TestDumpMultiChildExitsMidStream: the first selection streamed completely,
// the second restic run fails half-way through its tar (exit 1): the download
// fails with restic's class and the bytes sent are not a readable zip.
func TestDumpMultiChildExitsMidStream(t *testing.T) {
	p := strings.TrimPrefix(multiTree, "/") + "/docs/"
	cut := fakeTar(t, []tarEntry{{name: p + "big.bin", typ: tar.TypeReg, body: strings.Repeat("b", 20000)}}, 9000)
	e := newTenv(t, []fakeRule{
		{Cmd: "ls", Stdout: "inline:" + lsTreeJSON(lsNode("docs", "dir"), lsNode("report.docx", "file"))},
		{Cmd: "dump", Contains: "/report.docx", StdoutFile: rawFile(t, noise(64<<10))},
		{Cmd: "dump", Contains: "/docs", Exit: 1, StdoutFile: cut, Stderr: "inline:Fatal: cannot dump file: ReadFull(<data/391aec1893>): <data/391aec1893> does not exist\n"},
	}, nil)
	var out bytes.Buffer
	_, err := e.w.Dump(e.ctx, multiReq(e, "latest", multiTree+"/report.docx", multiTree+"/docs"), &out)
	var ae *adminpath.Error
	if !errors.As(err, &ae) || ae.Code != adminpath.CodeResticFailed {
		t.Fatalf("err = %v, want restic_failed", err)
	}
	if out.Len() < 32<<10 {
		t.Fatalf("only %d bytes went out: the failure was meant to come mid-download", out.Len())
	}
	notAZip(t, out.Bytes())
}

// TestDumpMultiUnreachable: the storage cannot be reached, before the first
// byte (the listing) and in the middle (a dump run): both are
// storage_unreachable with the class, neither leaves a readable zip.
func TestDumpMultiUnreachable(t *testing.T) {
	refused := "inline:Fatal: unable to open repository: dial tcp 127.0.0.1:9: connect: connection refused\n"
	t.Run("listing", func(t *testing.T) {
		e := newTenv(t, []fakeRule{{Cmd: "ls", Exit: 1, Stderr: refused}}, nil)
		var out bytes.Buffer
		_, err := e.w.Dump(e.ctx, multiReq(e, "latest", multiTree+"/docs"), &out)
		wantCode(t, err, adminpath.CodeStorageUnreachable, restic.ClassNetwork)
		if out.Len() != 0 {
			t.Fatal("bytes were streamed")
		}
	})
	t.Run("mid-stream", func(t *testing.T) {
		e := newTenv(t, []fakeRule{
			{Cmd: "ls", Stdout: "inline:" + lsTreeJSON(lsNode("docs", "dir"), lsNode("report.docx", "file"))},
			{Cmd: "dump", Contains: "/report.docx", StdoutFile: rawFile(t, noise(64<<10))},
			{Cmd: "dump", Contains: "/docs", Exit: 1, Stderr: refused},
		}, nil)
		var out bytes.Buffer
		_, err := e.w.Dump(e.ctx, multiReq(e, "latest", multiTree+"/report.docx", multiTree+"/docs"), &out)
		wantCode(t, err, adminpath.CodeStorageUnreachable, restic.ClassNetwork)
		notAZip(t, out.Bytes())
	})
}

// shortIdle shortens the idle watchdog for one test.
func shortIdle(t *testing.T, d time.Duration) {
	t.Helper()
	old := dumpIdleOverride.Load()
	dumpIdleOverride.Store(int64(d))
	t.Cleanup(func() { dumpIdleOverride.Store(old) })
}

// waitUnlockRun waits for the background stale-lock cleanup to be recorded and
// closed, and returns its row.
func waitUnlockRun(t *testing.T, e *tenv) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range e.consoleRuns(e.esc) {
			if r.Kind == "unlock" && r.FinishedAt != nil {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("no unlock run was recorded after the watchdog stopped restic")
}

// TestDumpMultiStalledChildTripsTheWatchdog: restic sends part of a tar and
// then nothing (as with its storage gone, it retries without writing): the
// watchdog stops it after the idle limit, the download fails and is not a
// readable zip, one log line with a class says so, and `restic unlock` runs
// through the admin path for the lock the killed restic left.
func TestDumpMultiStalledChildTripsTheWatchdog(t *testing.T) {
	shortIdle(t, 3*time.Second)
	p := strings.TrimPrefix(multiTree, "/") + "/docs/"
	part := fakeTar(t, []tarEntry{{name: p + "big.bin", typ: tar.TypeReg, body: strings.Repeat("s", 50000)}}, 20000)
	e := newTenv(t, []fakeRule{
		{Cmd: "ls", Stdout: "inline:" + lsTreeJSON(lsNode("docs", "dir"))},
		{Cmd: "dump", StdoutFile: part, SleepAfterMS: 60000},
		{Cmd: "unlock"},
	}, nil)
	var out bytes.Buffer
	t0 := time.Now()
	_, err := e.w.Dump(e.ctx, multiReq(e, "latest", multiTree+"/docs"), &out)
	took := time.Since(t0)
	wantCode(t, err, adminpath.CodeTimeout, "")
	if took > 45*time.Second {
		t.Fatalf("the stalled restic ran for %v (idle limit 3s; it would sleep 60s)", took)
	}
	notAZip(t, out.Bytes())
	waitUnlockRun(t, e)
	unlocks := 0
	for _, c := range e.repoCalls() {
		if c.Args[0] == "unlock" {
			unlocks++
			if strings.Contains(strings.Join(c.Args, " "), "--remove-all") {
				t.Fatal("the watchdog's cleanup removed every lock, not only stale ones")
			}
		}
	}
	if unlocks != 1 {
		t.Fatalf("%d unlock calls, want 1", unlocks)
	}
	logs := e.logs.String()
	idleLines := 0
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "dump_idle") && !strings.Contains(line, "dump_idle_unlock") {
			idleLines++
		}
	}
	if idleLines != 1 {
		t.Fatalf("want exactly one dump_idle line, got %d:\n%s", idleLines, logs)
	}
	if strings.Contains(logs, "big.bin") || strings.Contains(logs, multiTree) {
		t.Fatal("the watchdog's log names a path")
	}
}

// TestIdleReaderCountsOnlyWaitsForRestic: the idle clock runs only inside
// Read. A caller that spends longer than the limit between reads (writing to a
// slow browser) never trips it; a Read that waits longer than the limit does.
func TestIdleReaderCountsOnlyWaitsForRestic(t *testing.T) {
	var tripped atomic.Bool
	ir := newIdleReader(strings.NewReader("abcdef"), 50*time.Millisecond, func() { tripped.Store(true) })
	buf := make([]byte, 2)
	for {
		_, err := ir.Read(buf)
		if err != nil {
			break
		}
		time.Sleep(150 * time.Millisecond) // a slow browser, three times the limit
	}
	ir.stop()
	time.Sleep(100 * time.Millisecond)
	if tripped.Load() {
		t.Fatal("time spent outside Read tripped the watchdog")
	}

	pr, pw := io.Pipe()
	defer pw.Close()
	done := make(chan struct{})
	stall := newIdleReader(pr, 50*time.Millisecond, func() { close(done); pw.CloseWithError(errDumpIdle) })
	if _, err := stall.Read(buf); !errors.Is(err, errDumpIdle) {
		t.Fatalf("a Read with nothing to read for the limit returned %v", err)
	}
	<-done
}

// failAfterWriter accepts n bytes, then fails, and records when it failed.
type failAfterWriter struct {
	mu     sync.Mutex
	n      int
	failed time.Time
}

func (f *failAfterWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n <= 0 {
		if f.failed.IsZero() {
			f.failed = time.Now()
		}
		return 0, errors.New("browser went away")
	}
	f.n -= len(p)
	return len(p), nil
}

// TestDumpMultiDroppedClientCancelsRestic: the destination fails mid-download;
// restic (which would otherwise keep going for 20 s) is stopped and Dump
// returns within 2 s of the failed write.
func TestDumpMultiDroppedClientCancelsRestic(t *testing.T) {
	p := strings.TrimPrefix(multiTree, "/") + "/docs/"
	big := fakeTar(t, []tarEntry{{name: p + "big.bin", typ: tar.TypeReg, body: noise(1 << 20)}}, 0)
	e := newTenv(t, []fakeRule{
		{Cmd: "ls", Stdout: "inline:" + lsTreeJSON(lsNode("docs", "dir"))},
		{Cmd: "dump", StdoutFile: big, SleepAfterMS: 20000},
	}, nil)
	dst := &failAfterWriter{n: 64 << 10}
	_, err := e.w.Dump(e.ctx, multiReq(e, "latest", multiTree+"/docs"), dst)
	done := time.Now()
	if err == nil {
		t.Fatal("a download into a failed destination succeeded")
	}
	dst.mu.Lock()
	failed := dst.failed
	dst.mu.Unlock()
	if failed.IsZero() {
		t.Fatal("the destination never failed")
	}
	if d := done.Sub(failed); d > 2*time.Second {
		t.Fatalf("restic kept running for %v after the destination failed", d)
	}
}

// TestDumpMulti51PathsRefused: the 51st path is refused before restic runs;
// fifty are within the contract.
func TestDumpMulti51PathsRefused(t *testing.T) {
	e := newTenv(t, nil, nil)
	paths := make([]string, adminpath.MaxDumpPaths+1)
	for i := range paths {
		paths[i] = multiTree + "/f" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	var out bytes.Buffer
	_, err := e.w.Dump(e.ctx, multiReq(e, "latest", paths...), &out)
	wantCode(t, err, adminpath.CodeInvalidRequest, "")
	if len(e.repoCalls()) != 0 || out.Len() != 0 {
		t.Fatal("restic ran or bytes were sent for 51 paths")
	}
	if err := multiReq(e, "latest", paths[:adminpath.MaxDumpPaths]...).Validate(); err != nil {
		t.Fatalf("50 paths refused: %v", err)
	}
}

// TestDumpMultiMissingSelection: a selection that is not in the snapshot is
// not_found before any dump runs or any byte is sent.
func TestDumpMultiMissingSelection(t *testing.T) {
	e := newTenv(t, treeRules(t), nil)
	var out bytes.Buffer
	_, err := e.w.Dump(e.ctx, multiReq(e, "latest", multiTree+"/report.docx", multiTree+"/nope.txt"), &out)
	wantCode(t, err, adminpath.CodeNotFound, "")
	if out.Len() != 0 {
		t.Fatal("bytes were sent")
	}
	for _, c := range e.repoCalls() {
		if c.Args[0] == "dump" {
			t.Fatal("a dump ran although a selection is missing")
		}
	}
}

// TestDumpMultiRepeatedNames: selections with the same base name from
// different folders keep apart (compared without case, as Windows extracts).
func TestDumpMultiRepeatedNames(t *testing.T) {
	lsOf := func(dir, name string) string {
		return `{"message_type":"snapshot","id":"` + dumpSnapID + `"}` + "\n" +
			`{"message_type":"node","name":"` + name + `","type":"file","path":"` + dir + "/" + name + `"}` + "\n"
	}
	e := newTenv(t, []fakeRule{
		{Cmd: "ls", Contains: " /C/a --", Stdout: "inline:" + lsOf("/C/a", "report.txt")},
		{Cmd: "ls", Contains: " /C/b --", Stdout: "inline:" + lsOf("/C/b", "report.txt")},
		{Cmd: "ls", Contains: " /C/c --", Stdout: "inline:" + lsOf("/C/c", "REPORT.txt")},
		{Cmd: "ls", Contains: " /C/d --", Stdout: "inline:" + lsOf("/C/d", "cryobackup-skipped.txt")},
		{Cmd: "dump", Contains: "/C/a/", Stdout: "inline:A"},
		{Cmd: "dump", Contains: "/C/b/", Stdout: "inline:B"},
		{Cmd: "dump", Contains: "/C/c/", Stdout: "inline:C"},
		{Cmd: "dump", Contains: "/C/d/", Stdout: "inline:D"},
	}, nil)
	var out bytes.Buffer
	if _, err := e.w.Dump(e.ctx, multiReq(e, "latest", "/C/a/report.txt", "/C/b/report.txt", "/C/c/REPORT.txt", "/C/d/cryobackup-skipped.txt"), &out); err != nil {
		t.Fatal(err)
	}
	names, bodies := zipEntries(t, out.Bytes())
	want := "report.txt|report (2).txt|REPORT (3).txt|cryobackup-skipped (2).txt"
	if strings.Join(names, "|") != want {
		t.Fatalf("entries = %q, want %q", names, want)
	}
	if bodies["report (2).txt"] != "B" || bodies["REPORT (3).txt"] != "C" {
		t.Fatalf("bodies = %q", bodies)
	}
}

// TestDumpMultiOverTheSocket: the console-side Client receives the zip with
// the size and SHA-256 of what crossed the socket.
func TestDumpMultiOverTheSocket(t *testing.T) {
	e := newTenv(t, treeRules(t), nil)
	sock := filepath.Join(shortTempDir(t), "ap.sock")
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- adminpath.Serve(ctx, sock, adminpath.Handler(e.w)) }()
	defer func() { stop(); <-served }()
	waitSocket(t, sock)
	c := adminpath.NewClient(sock, adminpath.ClientOptions{})
	defer c.Close()
	var out bytes.Buffer
	res, err := c.Dump(context.Background(), multiReq(e, "latest", multiTree+"/docs", multiTree+"/report.docx"), &out)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(out.Bytes())
	if res.Bytes != int64(out.Len()) || res.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("result %+v for %d bytes", res, out.Len())
	}
	names, _ := zipEntries(t, out.Bytes())
	if len(names) != 6 || names[0] != "docs/" || names[4] != "report.docx" {
		t.Fatalf("entries = %q", names)
	}
	_, err = c.Dump(context.Background(), multiReq(e, "latest", append(make([]string, 0), strings.Split(strings.Repeat(multiTree+"/x,", 51), ",")[:51]...)...), &out)
	wantCode(t, err, adminpath.CodeInvalidRequest, "")
}

// TestSafeEntryName pins the zip-slip guard.
func TestSafeEntryName(t *testing.T) {
	for _, ok := range []string{"a", "a/b", "docs/sub/" + longName, "x.y.z", "a b/c", "...", "..a"} {
		if !safeEntryName(ok) {
			t.Errorf("refused %q", ok)
		}
	}
	for _, bad := range []string{"", "/a", "a/", "a//b", "./a", "a/.", "..", "../a", "a/../b", `a\b`, "C:", "a/C:b", "a\x00b", "a\nb", "a\x7fb"} {
		if safeEntryName(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

// TestLsWindow: Ls pages restic's stream: the window of Offset and Limit,
// Total over every child, Truncated when children follow, and consecutive
// windows that add up to the stream in its order.
func TestLsWindow(t *testing.T) {
	var kids []string
	var order []string
	for _, n := range []string{"a.txt", "b", "c.doc", "d", "e.bin", "f", "g.txt"} {
		typ := "file"
		if !strings.Contains(n, ".") {
			typ = "dir"
		}
		kids = append(kids, lsNode(n, typ))
		order = append(order, n)
	}
	e := newTenv(t, []fakeRule{{Cmd: "ls", Stdout: "inline:" + lsTreeJSON(kids...)}}, nil)
	ls := func(off, lim int) adminpath.LsResult {
		t.Helper()
		r, err := e.w.Ls(e.ctx, adminpath.LsRequest{DeviceRequest: dev(e.esc), SnapshotID: "latest", Dir: multiTree, Offset: off, Limit: lim})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	var got []string
	for off := 0; off < 7; off += 3 {
		r := ls(off, 3)
		if r.Total != 7 {
			t.Fatalf("offset %d: total %d, want 7", off, r.Total)
		}
		if wantTrunc := off+3 < 7; r.Truncated != wantTrunc {
			t.Fatalf("offset %d: truncated %v", off, r.Truncated)
		}
		for _, n := range r.Nodes {
			got = append(got, n.Name)
		}
	}
	if strings.Join(got, ",") != strings.Join(order, ",") {
		t.Fatalf("windows = %v, want the stream order %v", got, order)
	}
	if r := ls(10, 3); len(r.Nodes) != 0 || r.Total != 7 || r.Truncated {
		t.Fatalf("offset past the end = %+v", r)
	}
	if r := ls(0, 0); len(r.Nodes) != 7 || r.Total != 7 || r.Truncated || r.Snapshot.ID != dumpSnapID {
		t.Fatalf("default window = %d nodes, total %d, truncated %v", len(r.Nodes), r.Total, r.Truncated)
	}
	if r := ls(int(^uint(0)>>1), 5); len(r.Nodes) != 0 || r.Truncated || r.Total != 7 {
		t.Fatalf("a huge offset = %+v", r)
	}
}
