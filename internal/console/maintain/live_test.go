package maintain

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/gateway"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// The live tests run the real restic 0.19.1 and rclone 1.75.1. They are skipped
// unless both CRYO_RESTIC and CRYO_RCLONE name the binaries:
//
//	CRYO_RESTIC=<restic> CRYO_RCLONE=<rclone> go test -count=1 -run Live -v ./internal/console/maintain
//
// Everything lives in test temp directories and on loopback; no storage
// credential, no network.

func liveBinaries(t *testing.T) (resticBin, rcloneBin string) {
	t.Helper()
	resticBin, rcloneBin = os.Getenv("CRYO_RESTIC"), os.Getenv("CRYO_RCLONE")
	if resticBin == "" || rcloneBin == "" {
		t.Skip("CRYO_RESTIC and CRYO_RCLONE are not both set")
	}
	for _, p := range []string{resticBin, rcloneBin} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s: %v", filepath.Base(p), err)
		}
	}
	return resticBin, rcloneBin
}

// snapPath turns a local path into restic's snapshot path form: forward slashes,
// and "C:\x" becomes "/C/x".
func snapPath(p string) string {
	p = filepath.ToSlash(p)
	if len(p) >= 2 && p[1] == ':' {
		p = "/" + p[:1] + p[2:]
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// agentRestic runs restic as the endpoint would (the agent is not part of this
// ticket), with the full parent environment plus the given extra variables, and
// returns stdout, stderr and the exit code.
func agentRestic(t *testing.T, bin string, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("restic did not start: %v", err)
		}
		code = ee.ExitCode()
	}
	return so.String(), se.String(), code
}

func TestLiveAdminPathLocal(t *testing.T) {
	resticBin, rcloneBin := liveBinaries(t)

	remote := t.TempDir()
	conf := filepath.Join(t.TempDir(), "rclone.conf")
	// An alias remote over a local directory: no S3, and "admin:bucket/prefix/id"
	// resolves under the directory instead of the process's working directory.
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
	w, dID := e.w, e.esc
	ctx := e.ctx
	rd := dev(dID)

	// init, cat config, and init again (DH17).
	ires, err := w.Init(ctx, adminpath.InitRequest{DeviceRequest: rd})
	if err != nil || !ires.Created || ires.RepositoryID == "" || ires.Outcome != restic.OutcomeSuccess {
		t.Fatalf("Init = %+v, %v", ires, err)
	}
	cfg, err := w.CatConfig(ctx, rd)
	if err != nil || cfg.ID != ires.RepositoryID {
		t.Fatalf("CatConfig = %+v, %v (repository id %s)", cfg, err, ires.RepositoryID)
	}
	again, err := w.Init(ctx, adminpath.InitRequest{DeviceRequest: rd})
	if err != nil || again.Created || again.RepositoryID != ires.RepositoryID || again.Outcome != restic.OutcomeSuccess {
		t.Fatalf("second Init = %+v, %v (want existing repository %s, Created false)", again, err, ires.RepositoryID)
	}
	t.Logf("live: init created=%v, cat config matches, second init created=%v exit=%d", ires.Created, again.Created, again.ExitCode)

	// A backup with the restic binary directly, through the same rclone: backend.
	tree := filepath.Join(t.TempDir(), "tree")
	for rel, body := range map[string]string{
		"hello.txt":        "hello world\n",
		"docs/report.txt":  "quarterly report\n",
		"docs/notes.md":    "# notes\n",
		"media/empty.data": "",
	} {
		p := filepath.Join(tree, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := restic.AdminRepository("admin", e.cfg.Bucket, e.cfg.Prefix, dID)
	if err != nil {
		t.Fatal(err)
	}
	bcmd, err := restic.Backup(restic.BackupOptions{
		Common: restic.Common{Extra: restic.AdminOptions(filepath.ToSlash(rcloneBin), false)}, // restic splits rclone.program: forward slashes only
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

	// snapshots
	snaps, err := w.Snapshots(ctx, rd)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("Snapshots = %d, %v", len(snaps), err)
	}
	snap := snaps[0].ID
	t.Logf("live: snapshots=%d", len(snaps))

	// ls "/" (roots) and ls of the tree and a subdirectory: one level each.
	treeSnap := snapPath(tree)
	first := strings.Split(strings.TrimPrefix(treeSnap, "/"), "/")[0]
	roots, err := w.Ls(ctx, adminpath.LsRequest{DeviceRequest: rd, SnapshotID: snap, Dir: "/"})
	if err != nil || len(roots.Nodes) == 0 {
		t.Fatalf("Ls(/) = %+v, %v", roots, err)
	}
	foundRoot := false
	for _, n := range roots.Nodes {
		foundRoot = foundRoot || n.Name == first
	}
	if !foundRoot || roots.Snapshot.ID != snap {
		t.Fatalf("Ls(/) lacks root %q or the snapshot header (id %s)", first, roots.Snapshot.ID)
	}
	level, err := w.Ls(ctx, adminpath.LsRequest{DeviceRequest: rd, SnapshotID: snap, Dir: treeSnap})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, n := range level.Nodes {
		names[n.Name] = n.Type
	}
	if len(names) != 2+1 || names["hello.txt"] != "file" || names["docs"] != "dir" || names["media"] != "dir" {
		t.Fatalf("Ls(tree) = %v; want hello.txt, docs, media and nothing deeper or the directory itself", names)
	}
	sub, err := w.Ls(ctx, adminpath.LsRequest{DeviceRequest: rd, SnapshotID: "latest", Dir: treeSnap + "/docs", Limit: 1})
	if err != nil || len(sub.Nodes) != 1 || !sub.Truncated {
		t.Fatalf("Ls(docs, limit 1) = %d nodes, truncated %v, %v", len(sub.Nodes), sub.Truncated, err)
	}
	t.Logf("live: ls roots=%d nodes, tree level=%d nodes, limit 1 truncated=%v", len(roots.Nodes), len(level.Nodes), sub.Truncated)

	// find
	hits, err := w.Find(ctx, adminpath.FindRequest{DeviceRequest: rd, Pattern: "report*"})
	if err != nil || len(hits) != 1 || len(hits[0].Matches) != 1 || !strings.HasSuffix(hits[0].Matches[0].Path, "/docs/report.txt") {
		t.Fatalf("Find = %+v, %v", hits, err)
	}
	t.Logf("live: find hits=%d", hits[0].Hits)

	// stats raw-data
	st, err := w.Stats(ctx, adminpath.StatsRequest{DeviceRequest: rd, Mode: restic.StatsRawData})
	if err != nil || st.TotalSize == 0 || st.SnapshotsCount != 1 {
		t.Fatalf("Stats = %+v, %v", st, err)
	}
	t.Logf("live: stats raw-data total_size>0 snapshots=%d blobs=%d", st.SnapshotsCount, st.TotalBlobCount)

	// dump (ADR 0008): a directory (restic's own zip) and a single file (wrapped on
	// the fly) stream through the admin-path socket; nothing appears on disk in
	// the scratch space, the harness or restic's default cache, during or after.
	userCache := ""
	if d, err := os.UserCacheDir(); err == nil {
		userCache = filepath.Join(d, "restic")
	}
	watched := []string{e.scratch, e.state, userCache}
	before := filesUnder(t, watched...)
	sock := filepath.Join(shortTempDir(t), "ap.sock")
	sctx, stop := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- adminpath.Serve(sctx, sock, adminpath.Handler(w)) }()
	defer func() { stop(); <-served }()
	waitSocket(t, sock)
	client := adminpath.NewClient(sock, adminpath.ClientOptions{})
	defer client.Close()
	var during map[string]int64
	dirOut := &watchWriter{onFirst: func() { during = filesUnder(t, watched...) }}
	dd, err := client.Dump(ctx, adminpath.DumpRequest{DeviceRequest: rd, SnapshotID: snap, Path: treeSnap + "/docs"}, dirOut)
	if err != nil {
		t.Fatalf("Dump(dir): %v", err)
	}
	checkStream(t, dirOut.Bytes(), dd, map[string]string{"report.txt": "quarterly report\n", "notes.md": "# notes\n"})
	sameTree(t, "during the directory dump", before, during)
	var fileOut bytes.Buffer
	df, err := client.Dump(ctx, adminpath.DumpRequest{DeviceRequest: rd, SnapshotID: snap, Path: treeSnap + "/hello.txt"}, &fileOut)
	if err != nil {
		t.Fatalf("Dump(file): %v", err)
	}
	checkStream(t, fileOut.Bytes(), df, map[string]string{"hello.txt": "hello world\n"})
	sameTree(t, "after both dumps", before, filesUnder(t, watched...))
	t.Logf("live: dump dir bytes=%d, dump file bytes=%d, both valid zips with matching sha256, no file on disk", dd.Bytes, df.Bytes)
	if _, err := w.Dump(ctx, adminpath.DumpRequest{DeviceRequest: rd, SnapshotID: snap, Path: treeSnap + "/nope.txt"}, io.Discard); err == nil {
		t.Fatal("Dump of a path that is not in the snapshot succeeded")
	} else {
		wantCode(t, err, adminpath.CodeNotFound, "")
	}

	// unlock, stale only and --remove-all for a quiesced device; both recorded.
	ul, err := w.Unlock(ctx, adminpath.UnlockRequest{DeviceRequest: rd})
	if err != nil || ul.Outcome != restic.OutcomeSuccess || ul.ExitCode != 0 {
		t.Fatalf("Unlock = %+v, %v", ul, err)
	}
	ua, err := w.Unlock(ctx, adminpath.UnlockRequest{DeviceRequest: rd, RemoveAll: true, Quiesced: true})
	if err != nil || ua.Outcome != restic.OutcomeSuccess {
		t.Fatalf("Unlock --remove-all = %+v, %v", ua, err)
	}
	if runs := e.consoleRuns(dID); len(runs) != 2 || runs[0].Kind != "unlock" || runs[1].Kind != "unlock" {
		t.Fatalf("console runs = %+v", runs)
	}
	t.Logf("live: unlock exit=%d, unlock --remove-all exit=%d, 2 unlock runs recorded", ul.ExitCode, ua.ExitCode)

	// A wrong key: cat config is wrong_password; init on the existing repository reports it as a result.
	e.keys.pw = "not-the-repository-password"
	_, err = w.CatConfig(ctx, rd)
	wantCode(t, err, adminpath.CodeWrongPassword, restic.ClassWrongPassword)
	bad, err := w.Init(ctx, adminpath.InitRequest{DeviceRequest: rd})
	if err != nil || bad.Created || bad.Outcome != restic.OutcomeFailed || bad.ErrorClass != restic.ClassWrongPassword || bad.ExitCode != 12 {
		t.Fatalf("Init with the wrong key = %+v, %v", bad, err)
	}
	e.keys.pw = testPassword
	t.Logf("live: wrong key -> %s, init with wrong key -> %s exit=%d", adminpath.CodeWrongPassword, bad.ErrorClass, bad.ExitCode)

	// A missing device is not_found; a device without a repository is repo_missing;
	// a customer-held device is refused without touching restic.
	_, err = w.CatConfig(ctx, dev("01NODEVICE000000000000000"))
	wantCode(t, err, adminpath.CodeNotFound, "")
	_, err = w.CatConfig(ctx, dev(e.deviceID("LAB-RETINA-02")))
	wantCode(t, err, adminpath.CodeRepoMissing, restic.ClassRepoMissing)
	_, err = w.CatConfig(ctx, dev(e.held))
	wantCode(t, err, adminpath.CodeCustodyCustomerHeld, "")
	t.Logf("live: missing device -> %s, no repository -> %s, customer-held -> %s", adminpath.CodeNotFound, adminpath.CodeRepoMissing, adminpath.CodeCustodyCustomerHeld)

	// The scheduler's usage pass against the real repository.
	e.onlyDevice(dID)
	rep, err := w.RunOnce(ctx)
	if err != nil || rep.Sampled != 1 || rep.Failed != 0 {
		t.Fatalf("RunOnce = %+v, %v", rep, err)
	}
	if n, b := usageRows(t, e, dID); n != 1 || b != int64(st.TotalSize) {
		t.Fatalf("usage_samples rows = %d, repo_bytes = %d (stats said %d)", n, b, st.TotalSize)
	}
	t.Logf("live: RunOnce sampled=%d repo_bytes=%d", rep.Sampled, st.TotalSize)

	// Nothing the worker printed contains a file name from the tree.
	for _, secret := range []string{"report.txt", "hello.txt", "notes.md", tree} {
		if strings.Contains(e.logs.String(), secret) {
			t.Fatalf("a log line contains %q", secret)
		}
	}
}

// checkStream verifies the DumpResult against the streamed bytes and that the
// zip contains each wanted entry (matched by base name) with the wanted body.
func checkStream(t *testing.T, data []byte, res adminpath.DumpResult, want map[string]string) {
	t.Helper()
	sum := sha256.Sum256(data)
	if res.Bytes != int64(len(data)) || res.SHA256 != hex.EncodeToString(sum[:]) || res.Bytes == 0 || res.Outcome != restic.OutcomeSuccess {
		t.Fatalf("DumpResult = %+v, stream has %d bytes sha256 %x", res, len(data), sum)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("the stream is not a zip: %v", err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		got[filepath.Base(f.Name)] = string(b)
	}
	for name, body := range want {
		if got[name] != body {
			t.Errorf("zip entry %q = %q, want %q (entries: %d)", name, got[name], body, len(got))
		}
	}
}

// rcloneGateway is a running `rclone serve restic`.
type rcloneGateway struct {
	base string // http(s)://127.0.0.1:port, no trailing slash
}

var servingRE = regexp.MustCompile(`Serving restic REST API on \[?(https?://[^\]\s]+)`)

// startGateway runs rclone serve restic on an ephemeral loopback port over a
// local directory, with the given htpasswd file, always with --private-repos and
// with --append-only as a command-line flag when appendOnly is set (never through
// the environment, architecture 7.1). extra flags (for example the TLS flags) are
// added before the remote. It is killed at cleanup.
func startGateway(t *testing.T, rcloneBin, htpasswd string, appendOnly bool, extra ...string) rcloneGateway {
	t.Helper()
	remote := t.TempDir()
	emptyConf := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(emptyConf, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"serve", "restic", "--addr", "127.0.0.1:0", "--htpasswd", htpasswd, "--private-repos"}
	if appendOnly {
		args = append(args, "--append-only")
	}
	args = append(args, extra...)
	args = append(args, remote)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, rcloneBin, args...)
	cmd.Env = append(os.Environ(), "RCLONE_CONFIG="+emptyConf)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var logb bytes.Buffer
	urlCh := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			mu.Lock()
			logb.WriteString(sc.Text() + "\n")
			mu.Unlock()
			if m := servingRE.FindStringSubmatch(sc.Text()); m != nil {
				select {
				case urlCh <- m[1]:
				default:
				}
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
		<-done
		if t.Failed() {
			mu.Lock()
			t.Logf("rclone output:\n%s", logb.String())
			mu.Unlock()
		}
	})
	select {
	case u := <-urlCh:
		return rcloneGateway{base: strings.TrimRight(u, "/")}
	case <-time.After(30 * time.Second):
		t.Fatal("rclone did not report a listen address within 30s")
		return rcloneGateway{}
	}
}

// httpStatus requests path as user:password and returns the status code.
func httpStatus(t *testing.T, base, path, user, password string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(user, password)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

type gwLogin struct{ id, pw string }

// writeHtpasswd renders the logins (plus a probe login) with the gateway package
// and deploys the file the way the console does.
func writeHtpasswd(t *testing.T, logins []gwLogin) string {
	t.Helper()
	var ls []gateway.Login
	for _, l := range logins {
		h, err := gateway.HashPassword(l.pw)
		if err != nil {
			t.Fatal(err)
		}
		ls = append(ls, gateway.Login{Username: l.id, Hash: h})
	}
	probePW, err := gateway.ProbePassword(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	probeHash, err := gateway.HashPassword(probePW)
	if err != nil {
		t.Fatal(err)
	}
	content := gateway.Render(ls, probeHash, 1)
	if content == nil {
		t.Fatal("htpasswd rendering failed")
	}
	dir := t.TempDir()
	if err := gateway.Sync(dir, content); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, gateway.FileName)
}

// TestLiveAppendOnlyGateway is L-APPEND-1 with the real binaries (architecture
// 12.2, engine 3.2): with a device login against rclone serve restic
// --append-only --private-repos, forget, forget --prune, rewrite --forget and
// key remove / key passwd must fail with 403, while backup, snapshots and unlock
// succeed, and a second login gets 403 on the first login's repository. A second
// subtest runs deploy/rclone/append-only-test.sh against this gateway (must pass)
// and against a gateway started WITHOUT --append-only (must fail, proving the gate
// has teeth).
func TestLiveAppendOnlyGateway(t *testing.T) {
	resticBin, rcloneBin := liveBinaries(t)
	newPW := func() string {
		p, err := gateway.NewPassword(nil)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	devA, devB, devC := gwLogin{"01LIVEDEVICE000000000000A", newPW()}, gwLogin{"01LIVEDEVICE000000000000B", newPW()}, gwLogin{"01LIVEDEVICE000000000000C", newPW()}
	htpasswd := writeHtpasswd(t, []gwLogin{devA, devB, devC})
	gw := startGateway(t, rcloneBin, htpasswd, true)

	const repoPW = "live-append-only-repo-password"
	envFor := func(l gwLogin) []string {
		repo, err := restic.GatewayRepository(gw.base, l.id, l.pw, true)
		if err != nil {
			t.Fatal(err)
		}
		return []string{"RESTIC_REPOSITORY=" + repo, "RESTIC_PASSWORD=" + repoPW, "RESTIC_CACHE_DIR=" + filepath.Join(t.TempDir(), "cache")}
	}
	envA := envFor(devA)
	run := func(args ...string) (string, string, int) { return agentRestic(t, resticBin, envA, args...) }

	// Authentication and confinement at the HTTP level.
	if c := httpStatus(t, gw.base, "/"+devA.id+"/config", devA.id, devA.pw); c != 404 && c != 200 {
		t.Fatalf("own repository, before init: %d", c)
	}
	if c := httpStatus(t, gw.base, "/"+devA.id+"/config", devA.id, devA.pw+"x"); c != 401 {
		t.Fatalf("wrong password: %d, want 401", c)
	}

	// Must succeed: create the repository, back up, list, unlock.
	if _, se, code := run("init", "--json"); code != 0 {
		t.Fatalf("init through the gateway: exit %d: %s", code, se)
	}
	tree := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "one.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, se, code := run("backup", "--host", devA.id, "--json", "--", tree); code != 0 {
		t.Fatalf("backup: exit %d: %s", code, se)
	}
	so, se, code := run("snapshots", "--json")
	if code != 0 {
		t.Fatalf("snapshots: exit %d: %s", code, se)
	}
	snaps, err := restic.DecodeSnapshots(strings.NewReader(so))
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots decode: %d, %v", len(snaps), err)
	}
	snap := snaps[0].ID
	if _, se, code := run("unlock"); code != 0 {
		t.Fatalf("unlock: exit %d: %s", code, se)
	}
	t.Logf("L-APPEND-1 PASS backup, snapshots, unlock succeed (snapshot %.8s)", snap)

	// Must fail with 403.
	forbidden := func(name string, args ...string) {
		t.Helper()
		_, se, code := run(args...)
		if code == 0 {
			t.Errorf("L-APPEND-1 FAIL %s SUCCEEDED", name)
			return
		}
		if !strings.Contains(se, "403") && !strings.Contains(strings.ToLower(se), "forbidden") {
			t.Errorf("L-APPEND-1 FAIL %s failed (exit %d) but not with 403: %s", name, code, strings.TrimSpace(se))
			return
		}
		t.Logf("L-APPEND-1 PASS %s refused (exit %d, 403)", name, code)
	}
	forbidden("forget", "forget", snap)
	forbidden("forget --prune", "forget", "--prune", snap)
	forbidden("rewrite --forget --exclude", "rewrite", "--forget", "--exclude", "one.txt", snap)

	// A plain rewrite (no --forget) only adds a tagged snapshot: recorded, not asserted.
	if _, _, code := run("rewrite", "--exclude", "one.txt", snap); code == 0 {
		t.Logf("L-APPEND-1 INFO rewrite --exclude without --forget adds a tagged snapshot (allowed by design)")
	}

	// key remove needs a second key (restic refuses to remove its own key regardless of the server).
	pwFile := filepath.Join(t.TempDir(), "second")
	if err := os.WriteFile(pwFile, []byte(repoPW+"-second"), 0o600); err != nil {
		t.Fatal(err)
	}
	so, se, code = run("key", "add", "--new-password-file", pwFile)
	if code != 0 {
		t.Fatalf("key add: exit %d: %s (cannot build the key remove test)", code, se)
	}
	m := regexp.MustCompile(`ID ([0-9a-f]{64})`).FindStringSubmatch(so)
	if m == nil {
		t.Fatalf("key add printed no key id")
	}
	forbidden("key remove", "key", "remove", m[1])
	forbidden("key passwd", "key", "passwd", "--new-password-file", pwFile)

	// History survived the attacks.
	so, se, code = run("snapshots", "--json")
	if code != 0 {
		t.Fatalf("snapshots after the attacks: exit %d: %s", code, se)
	}
	after, _ := restic.DecodeSnapshots(strings.NewReader(so))
	if len(after) < 1 {
		t.Fatal("the snapshot is gone")
	}
	found := false
	for _, s := range after {
		found = found || s.ID == snap
	}
	if !found {
		t.Fatal("the original snapshot is gone after the attacks")
	}

	// --private-repos: a second login gets 403 on the first login's repository.
	if c := httpStatus(t, gw.base, "/"+devA.id+"/config", devB.id, devB.pw); c != http.StatusForbidden {
		t.Fatalf("second login on the first login's repository (HTTP): %d, want 403", c)
	}
	if c := httpStatus(t, gw.base, "/"+devA.id+"/config", devA.id, devA.pw); c != 200 {
		t.Fatalf("own repository: %d, want 200", c)
	}
	crossRepo := "rest:http://" + devB.id + ":" + devB.pw + "@" + strings.TrimPrefix(gw.base, "http://") + "/" + devA.id + "/"
	_, se, code = agentRestic(t, resticBin, []string{"RESTIC_REPOSITORY=" + crossRepo, "RESTIC_PASSWORD=" + repoPW, "RESTIC_CACHE_DIR=" + filepath.Join(t.TempDir(), "cache")}, "snapshots", "--json")
	if code == 0 || !strings.Contains(se, "403") {
		t.Fatalf("restic as the second login on the first login's repository: exit %d, stderr %q (want failure with 403)", code, strings.TrimSpace(se))
	}
	t.Logf("L-APPEND-1 PASS --private-repos: second login gets 403 on the first login's repository (HTTP and restic)")

	// The shell script, against this gateway and against one without --append-only.
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Logf("bash not found: script subtests skipped")
		return
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "deploy", "rclone", "append-only-test.sh"))
	if err != nil {
		t.Fatal(err)
	}
	runScript := func(base string, l gwLogin) (string, int) {
		cmd := exec.Command(bash, script, base)
		cmd.Env = append(os.Environ(), "DEVICE_ID="+l.id, "GATEWAY_PASSWORD="+l.pw, "RESTIC_PASSWORD="+repoPW, "RESTIC="+resticBin)
		out, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			ee, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("script did not start: %v", err)
			}
			code = ee.ExitCode()
		}
		return string(out), code
	}
	t.Run("script against append-only", func(t *testing.T) {
		out, code := runScript(gw.base, devC)
		t.Logf("script output:\n%s", out)
		if code != 0 || !strings.Contains(out, "RESULT: PASS") {
			t.Fatalf("script exit %d against an append-only gateway", code)
		}
		if strings.Contains(out, devC.pw) || strings.Contains(out, repoPW) {
			t.Fatal("the script printed a secret")
		}
	})
	t.Run("script against a gateway without append-only", func(t *testing.T) {
		open := startGateway(t, rcloneBin, htpasswd, false)
		out, code := runScript(open.base, devC)
		t.Logf("script output:\n%s", out)
		if code != 1 {
			t.Fatalf("script exit %d against a gateway WITHOUT --append-only, want 1", code)
		}
		for _, step := range []string{"FAIL forget <id> (SUCCEEDED", "FAIL forget --prune <id> (SUCCEEDED", "RESULT: FAIL"} {
			if !strings.Contains(out, step) {
				t.Errorf("script output lacks %q", step)
			}
		}
	})
}
