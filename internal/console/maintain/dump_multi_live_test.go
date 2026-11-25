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
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// TestLiveDumpSeveralPaths is the P22-B live test (ADR 0028) with the pinned
// restic 0.19.1 and rclone 1.75.1 against a local rclone remote (skipped
// unless CRYO_RESTIC and CRYO_RCLONE are set, see live_test.go):
//
//   - three files and two folders (with a nested selection that collapses, a
//     sub-folder, an empty folder, a long non-ASCII name and a junction) come
//     through the admin-path socket as ONE zip whose entries match the source
//     by sha256;
//   - a restic run that fails mid-download (its data pack is gone) never
//     yields a readable zip;
//   - a restic run that stalls (it retries the missing pack without writing)
//     trips the idle watchdog, and the background `restic unlock` leaves no
//     lock in the repository;
//   - a dropped client cancels restic within 2 s;
//   - a 51st path is refused;
//   - nothing is written under the scratch directory, the harness directory or
//     restic's default cache directory, during or after.
func TestLiveDumpSeveralPaths(t *testing.T) {
	resticBin, rcloneBin := liveBinaries(t)

	remote := t.TempDir()
	conf := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(conf, []byte("[admin]\ntype = alias\nremote = "+filepath.ToSlash(remote)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, sum, err := hashFile(resticBin)
	if err != nil {
		t.Fatal(err)
	}
	e := newTenv(t, nil, func(c *Config) {
		c.ResticProgram, c.ResticSHA256 = resticBin, sum
		c.RcloneProgram, c.RcloneConfig = rcloneBin, conf
	})
	w, dID, ctx, rd := e.w, e.esc, e.ctx, dev(e.esc)
	if _, err := w.Init(ctx, adminpath.InitRequest{DeviceRequest: rd}); err != nil {
		t.Fatal(err)
	}

	// The source tree.
	rnd := rand.New(rand.NewSource(22))
	random := func(n int) string { b := make([]byte, n); _, _ = rnd.Read(b); return string(b) }
	long := strings.Repeat("lange-Ordnerbezeichnung-", 4) + "café üñï 日本.txt"
	tree := filepath.Join(t.TempDir(), "tree")
	files := map[string]string{
		"one.txt":              "one\n",
		"two.bin":              random(300 << 10),
		"drei café 日本.txt":     "drei\n",
		"folderA/a1.txt":       "a1\n",
		"folderA/sub/" + long:  "deep\n",
		"folderA/sub/s2.bin":   random(70 << 10),
		"folderB/b1.txt":       "b1\n",
		"folderB/big.bin":      random(3 << 20),
		"unselected/never.txt": "never\n",
	}
	for rel, body := range files {
		p := filepath.Join(tree, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(tree, "folderA", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	junction := false
	if runtime.GOOS == "windows" {
		out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(tree, "folderA", "jn"), filepath.Join(tree, "unselected")).CombinedOutput()
		junction = err == nil
		if !junction {
			t.Logf("live: no junction (mklink /J: %v %s); the skip count is tested by the unit tests", err, bytes.TrimSpace(out))
		}
	}

	repo, err := restic.AdminRepository("admin", e.cfg.Bucket, e.cfg.Prefix, dID)
	if err != nil {
		t.Fatal(err)
	}
	backup := func() {
		t.Helper()
		bcmd, err := restic.Backup(restic.BackupOptions{
			Common: restic.Common{Extra: restic.AdminOptions(filepath.ToSlash(rcloneBin), false)},
			Host:   dID, Roots: []string{tree},
		})
		if err != nil {
			t.Fatal(err)
		}
		benv, err := restic.Env{Repository: repo, Password: testPassword, CacheDir: filepath.Join(t.TempDir(), "agent-cache")}.Environ(nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, se, code := agentRestic(t, resticBin, append(benv, "RCLONE_CONFIG="+conf), bcmd.Args...); code != 0 {
			t.Fatalf("backup exit %d: %s", code, se)
		}
	}
	backup()
	snaps, err := w.Snapshots(ctx, rd)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("Snapshots = %d, %v", len(snaps), err)
	}
	snap1 := snaps[0].ID
	ts := snapPath(tree)

	// The admin-path socket, with the worker's Dump timed so the test sees when
	// the worker side (restic included) finished.
	tb := &timedBackend{Worker: w}
	sock := filepath.Join(shortTempDir(t), "ap.sock")
	sctx, stop := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- adminpath.Serve(sctx, sock, adminpath.Handler(tb)) }()
	defer func() { stop(); <-served }()
	waitSocket(t, sock)
	client := adminpath.NewClient(sock, adminpath.ClientOptions{})
	defer client.Close()

	userCache := ""
	if d, err := os.UserCacheDir(); err == nil {
		userCache = filepath.Join(d, "restic")
	}
	watched := []string{e.scratch, e.state, userCache}
	before := filesUnder(t, watched...)

	// 1. Three files and two folders, one zip, sha256-equal to the source.
	sel := []string{ts + "/one.txt", ts + "/two.bin", ts + "/drei café 日本.txt", ts + "/folderA", ts + "/folderB", ts + "/folderA/a1.txt"}
	var during map[string]int64
	zipOut := &watchWriter{onFirst: func() { during = filesUnder(t, watched...) }}
	res, err := client.Dump(ctx, adminpath.DumpRequest{DeviceRequest: rd, SnapshotID: snap1, Paths: sel}, zipOut)
	if err != nil {
		t.Fatalf("several-paths Dump: %v", err)
	}
	data := zipOut.Bytes()
	whole := sha256.Sum256(data)
	if res.Bytes != int64(len(data)) || res.SHA256 != hex.EncodeToString(whole[:]) {
		t.Fatalf("DumpResult %+v for %d bytes", res, len(data))
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("the download is not a readable zip: %v", err)
	}
	got := map[string]string{}
	var dirs []string
	skipped := ""
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/") {
			dirs = append(dirs, f.Name)
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("entry %q: %v", f.Name, err)
		}
		if f.Name == skippedEntryName {
			skipped = string(b)
			continue
		}
		got[f.Name] = string(b)
	}
	t.Logf("live: one zip, %d bytes, sha256 %s (matches the trailer)", len(data), res.SHA256)
	t.Logf("live: %-58s %10s  %-16s  %-16s  %s", "entry", "bytes", "sha256 source", "sha256 zip", "match")
	var wantNames []string
	for rel := range files {
		if !strings.HasPrefix(rel, "unselected/") {
			wantNames = append(wantNames, rel)
		}
	}
	sort.Strings(wantNames)
	for _, rel := range wantNames {
		src := sha256.Sum256([]byte(files[rel]))
		zb, ok := got[rel]
		zs := sha256.Sum256([]byte(zb))
		match := ok && src == zs
		t.Logf("live: %-58s %10d  %.16s  %.16s  %v", rel, len(files[rel]), hex.EncodeToString(src[:]), hex.EncodeToString(zs[:]), match)
		if !match {
			t.Errorf("zip entry %q missing or different from the source", rel)
		}
	}
	if len(got) != len(wantNames) {
		t.Errorf("zip has %d file entries, want %d (no unselected file, no link target as content)", len(got), len(wantNames))
	}
	sort.Strings(dirs)
	if strings.Join(dirs, "|") != "folderA/|folderA/empty/|folderA/sub/|folderB/" {
		t.Errorf("folder entries = %q", dirs)
	}
	if junction {
		if !strings.Contains(skipped, "symbolic links and junctions: 1") || strings.Contains(skipped, "jn") {
			t.Errorf("%s = %q, want the junction counted and not named", skippedEntryName, skipped)
		}
		t.Logf("live: junction left out and counted: %q", strings.Split(strings.TrimSpace(skipped), "\r\n"))
	}
	wantSkipped := adminpath.DumpSkipped{}
	if junction {
		wantSkipped.Symlinks = 1
	}
	if res.Skipped != wantSkipped {
		t.Errorf("DumpResult.Skipped = %+v, want %+v", res.Skipped, wantSkipped)
	}
	sameTree(t, "during the several-paths download", before, during)

	// 1b. One folder by Path (the single-path request) goes the same tar route
	// (ADR 0028): its own entry and its files, the junction left out and
	// counted, and no link-target file (restic's own `-a zip` writes a 47-byte
	// file holding the target).
	var one bytes.Buffer
	ores, err := client.Dump(ctx, adminpath.DumpRequest{DeviceRequest: rd, SnapshotID: snap1, Path: ts + "/folderA"}, &one)
	if err != nil {
		t.Fatalf("single-path Dump of a folder: %v", err)
	}
	ozr, err := zip.NewReader(bytes.NewReader(one.Bytes()), int64(one.Len()))
	if err != nil {
		t.Fatalf("the single-path download is not a readable zip: %v", err)
	}
	var oNames []string
	for _, f := range ozr.File {
		oNames = append(oNames, f.Name)
		if f.Name == "folderA/jn" || (f.UncompressedSize64 == 47 && f.Name != skippedEntryName) {
			t.Errorf("single-path zip holds %q (%d bytes): a link written as a file", f.Name, f.UncompressedSize64)
		}
	}
	wantOne := []string{"folderA/", "folderA/a1.txt", "folderA/empty/", "folderA/sub/", "folderA/sub/" + long, "folderA/sub/s2.bin"}
	if junction {
		wantOne = append(wantOne, skippedEntryName)
	}
	sort.Strings(oNames)
	sort.Strings(wantOne)
	if strings.Join(oNames, "|") != strings.Join(wantOne, "|") {
		t.Errorf("single-path folder entries\n got %q\nwant %q", oNames, wantOne)
	}
	if ores.Skipped != wantSkipped {
		t.Errorf("single-path DumpResult.Skipped = %+v, want %+v", ores.Skipped, wantSkipped)
	}
	t.Logf("live: single-path folder -> %d entries, skipped %+v, no link-target file", len(oNames), ores.Skipped)

	// 2. A 51st path is refused, by the client before sending and by the worker.
	many := make([]string, adminpath.MaxDumpPaths+1)
	for i := range many {
		many[i] = ts + "/p" + strings.Repeat("x", i)
	}
	var none bytes.Buffer
	_, err = client.Dump(ctx, adminpath.DumpRequest{DeviceRequest: rd, SnapshotID: snap1, Paths: many}, &none)
	wantCode(t, err, adminpath.CodeInvalidRequest, "")
	_, err = w.Dump(ctx, adminpath.DumpRequest{DeviceRequest: rd, SnapshotID: snap1, Paths: many}, &none)
	wantCode(t, err, adminpath.CodeInvalidRequest, "")
	if none.Len() != 0 {
		t.Fatal("bytes were sent for 51 paths")
	}
	t.Logf("live: 51 paths -> %s (client and worker)", adminpath.CodeInvalidRequest)

	// 3. A dropped client cancels restic within 2 s.
	drop := &failAfterWriter{n: 256 << 10}
	ended := tb.count()
	_, err = client.Dump(ctx, adminpath.DumpRequest{DeviceRequest: rd, SnapshotID: snap1, Paths: []string{ts + "/folderB"}}, drop)
	if !errors.Is(err, adminpath.ErrStreamWrite) {
		t.Fatalf("dropped client: err %v, want ErrStreamWrite", err)
	}
	workerDone := tb.waitDone(t, ended)
	drop.mu.Lock()
	failedAt := drop.failed
	drop.mu.Unlock()
	if d := workerDone.Sub(failedAt); d > 2*time.Second {
		t.Fatalf("the worker's restic ran %v after the client dropped", d)
	} else {
		t.Logf("live: dropped client -> worker Dump (restic run) ended %v after the failed write", d.Round(time.Millisecond))
	}

	// A second backup adds a file whose data lands in a new pack; folderB's
	// older file sorts before it, so restic streams part of folderB, then
	// needs the new pack.
	packs := dataPacks(t, remote)
	if err := os.WriteFile(filepath.Join(tree, "folderB", "zz-late.bin"), []byte(random(2<<20)), 0o644); err != nil {
		t.Fatal(err)
	}
	backup()
	snaps, err = w.Snapshots(ctx, rd)
	if err != nil || len(snaps) != 2 {
		t.Fatalf("Snapshots = %d, %v", len(snaps), err)
	}
	snap2 := snaps[0].ID
	if snaps[1].Time.After(snaps[0].Time) {
		snap2 = snaps[1].ID
	}
	newPack := largestNew(t, remote, packs)
	if err := os.Rename(newPack, newPack+".away"); err != nil {
		t.Fatal(err)
	}
	restorePack := func() {
		if _, err := os.Stat(newPack + ".away"); err == nil {
			_ = os.Rename(newPack+".away", newPack)
		}
	}
	defer restorePack()

	// 4. A restic run failing mid-download never yields a readable zip (the
	// watchdog stays at its default, so the failure is restic's own exit).
	var cut bytes.Buffer
	t0 := time.Now()
	_, err = client.Dump(ctx, adminpath.DumpRequest{DeviceRequest: rd, SnapshotID: snap2, Paths: []string{ts + "/two.bin", ts + "/folderB"}}, &cut)
	var ae *adminpath.Error
	if !errors.As(err, &ae) {
		t.Fatalf("a dump whose pack is gone: err %v, want an admin-path error", err)
	}
	if cut.Len() < 200<<10 {
		t.Fatalf("only %d bytes reached the client: the failure was meant to come mid-download", cut.Len())
	}
	if _, zerr := zip.NewReader(bytes.NewReader(cut.Bytes()), int64(cut.Len())); zerr == nil {
		t.Fatal("a failed download is a readable zip")
	}
	t.Logf("live: pack gone mid-download -> %s/%s after %v, %d bytes reached the client, not a readable zip",
		ae.Code, ae.Class, time.Since(t0).Round(time.Second), cut.Len())

	// 5. The same missing pack with a short watchdog: restic retries without
	// writing, the watchdog stops it, and the background unlock removes the
	// lock the killed restic left.
	shortIdle(t, 8*time.Second)
	var stall bytes.Buffer
	t0 = time.Now()
	_, err = client.Dump(ctx, adminpath.DumpRequest{DeviceRequest: rd, SnapshotID: snap2, Paths: []string{ts + "/folderB"}}, &stall)
	took := time.Since(t0)
	if !errors.As(err, &ae) || (ae.Code != adminpath.CodeTimeout && ae.Code != adminpath.CodeStorageUnreachable) {
		t.Fatalf("stalled restic: err %v, want timeout or storage_unreachable", err)
	}
	if !strings.Contains(e.logs.String(), "dump_idle") {
		t.Fatal("no dump_idle log line")
	}
	if _, zerr := zip.NewReader(bytes.NewReader(stall.Bytes()), int64(stall.Len())); zerr == nil {
		t.Fatal("a stalled download is a readable zip")
	}
	waitUnlockRun(t, e)
	locks := lockObjects(t, remote)
	if locks != 0 {
		t.Fatalf("%d lock objects left after the watchdog's unlock", locks)
	}
	t.Logf("live: stalled restic -> %s after %v (idle limit 8s, restic retries ~30s), unlock run recorded, %d lock objects left", ae.Code, took.Round(time.Second), locks)
	restorePack()

	sameTree(t, "after every download", before, filesUnder(t, watched...))
	for _, secret := range []string{"one.txt", "folderA", "folderB", "big.bin", tree, "café"} {
		if strings.Contains(e.logs.String(), secret) {
			t.Fatalf("a log line contains %q", secret)
		}
	}
}

// timedBackend is the worker with its Dump's end time recorded.
type timedBackend struct {
	*Worker
	mu   sync.Mutex
	done []time.Time
}

func (b *timedBackend) Dump(ctx context.Context, req adminpath.DumpRequest, w io.Writer) (adminpath.DumpResult, error) {
	res, err := b.Worker.Dump(ctx, req, w)
	b.mu.Lock()
	b.done = append(b.done, time.Now())
	b.mu.Unlock()
	return res, err
}

func (b *timedBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.done)
}

// waitDone waits until more than n Dumps have ended (the client may return
// before the worker does) and returns when the latest one did.
func (b *timedBackend) waitDone(t *testing.T, n int) time.Time {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		if len(b.done) > n {
			d := b.done[len(b.done)-1]
			b.mu.Unlock()
			return d
		}
		b.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the worker's Dump never ended")
	return time.Time{}
}

// dataPacks lists the pack files under the repository's data directory.
func dataPacks(t *testing.T, remote string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	_ = filepath.WalkDir(remote, func(p string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() || filepath.Base(filepath.Dir(filepath.Dir(p))) != "data" {
			return nil
		}
		if fi, err := de.Info(); err == nil {
			out[p] = fi.Size()
		}
		return nil
	})
	return out
}

// largestNew is the largest pack that is not in before: the data pack of the
// newly added file (its tree pack is a few hundred bytes).
func largestNew(t *testing.T, remote string, before map[string]int64) string {
	t.Helper()
	best, size := "", int64(-1)
	for p, n := range dataPacks(t, remote) {
		if _, old := before[p]; !old && n > size {
			best, size = p, n
		}
	}
	if best == "" {
		t.Fatal("the second backup wrote no new pack")
	}
	return best
}

// lockObjects counts the lock objects of every repository under remote.
func lockObjects(t *testing.T, remote string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(remote, func(p string, de fs.DirEntry, err error) error {
		if err == nil && !de.IsDir() && filepath.Base(filepath.Dir(p)) == "locks" {
			n++
		}
		return nil
	})
	return n
}
