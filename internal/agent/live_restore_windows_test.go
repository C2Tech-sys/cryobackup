//go:build windows

package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/sys/windows"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// TestLiveRestore is the live check of ticket P22-D with the pinned binaries:
// restic 0.19.1 (CRYO_RESTIC) through rclone 1.75.1 (CRYO_RCLONE) serving
// "restic --append-only --private-repos" on loopback, the agent using its own
// device login and its DPAPI repository password (the enrolled gateway URL,
// the existing backup plumbing). A sample tree is backed up; then, through the
// agent's restore executor:
//
//  1. a file is deleted, restored original/keep: sha256 equal, the existing
//     files counted as skipped;
//  2. a file is modified, restored original/replace: content back;
//  3. side folder: the layout <side>\<id>\C\<path>, inherited descriptor;
//  4. a selection that matches nothing: FAILED nothing_matched.
//
// The placement writes at the tree's real original location on C:. The
// staging and side roots are moved into the test's folder and grant this user
// too (the lab trustee): a non-elevated test cannot use a root that only
// Administrators and SYSTEM may open. Everything else is production code.
func TestLiveRestore(t *testing.T) {
	resticExe, rcloneExe := os.Getenv("CRYO_RESTIC"), os.Getenv("CRYO_RCLONE")
	if resticExe == "" || rcloneExe == "" {
		t.Skip("CRYO_RESTIC and CRYO_RCLONE are not both set: the live restore needs restic 0.19.1 and rclone 1.75.1")
	}
	work := longName(t, t.TempDir())
	t.Cleanup(func() { unlockLeftovers(t, filepath.Join(work, ".cryobackup-restore")) })
	tree := filepath.Join(work, "tree")
	files := map[string]string{
		"a.txt":                       "alpha",
		filepath.Join("sub", "b.bin"): strings.Repeat("b", 70000),
		filepath.Join("sub", "brackets [1] $x.txt"): "special",
		filepath.Join("sub", "deep", "c.txt"):       "charlie",
	}
	for rel, body := range files {
		p := filepath.Join(tree, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before := map[string]string{}
	for rel := range files {
		before[rel] = sha(t, filepath.Join(tree, rel))
	}

	// The gateway: one device login, append-only, private repos.
	hash, err := bcrypt.GenerateFromPassword([]byte(testGatewaySec), 10)
	if err != nil {
		t.Fatal(err)
	}
	htpasswd := filepath.Join(work, "htpasswd")
	if err := os.WriteFile(htpasswd, []byte(testDeviceID+":"+string(hash)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gw := liveGateway(t, rcloneExe, htpasswd, filepath.Join(work, "store"))
	repo, err := restic.GatewayRepository(gw, testDeviceID, testGatewaySec, true)
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "RESTIC_REPOSITORY="+repo, "RESTIC_PASSWORD="+testRepoSecret, "RESTIC_CACHE_DIR="+filepath.Join(work, "cache"))
	resticRun := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command(resticExe, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("restic %s: %v\n%s", args[0], err, out)
		}
		return out
	}
	resticRun("init", "--quiet")
	resticRun("backup", "--quiet", "--host", testDeviceID, tree)
	var snaps []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resticRun("snapshots", "--json"), &snaps); err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots: %v %v", snaps, err)
	}
	snap := snaps[0].ID

	// The agent, enrolled against the fake console with the live gateway.
	fc := newFakeConsole(t)
	fc.gatewayURL = gw
	fc.setPolicy(api.PolicyResponse{Policy: testPolicy(), PolicyID: testPolicyID, Version: 1, ETag: `"policy-v1"`, Paused: true})
	dir := enroll(t, fc)
	o := fakeOptions(t, dir)
	o.ResticPath, o.ResticSHA256 = resticExe, fileSHA(t, resticExe)
	o.baseEnv = os.Environ()
	o.restoreStagingRoot = func(string) string { return filepath.Join(work, ".cryobackup-restore") }
	o.restoreSideRoot = filepath.Join(work, "CryoBackup-Restore")
	o.restoreLabTrustee = currentSID(t)
	o.restoreSweepLetters = func() []string { return nil }
	run := start(t, o)
	defer run.stop()
	t.Cleanup(func() {
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(dir, "logs", "agent.log"))
			t.Logf("agent.log:\n%s", b)
		}
	})

	snapPath := func(rel string) string { // /C/<tree below C:>/<rel>
		p := filepath.ToSlash(filepath.Join(tree, rel))
		return "/" + p[:1] + p[2:]
	}
	seq := 0
	restore := func(mode api.DeviceRestoreMode, ow api.DeviceRestoreOverwrite, paths ...string) api.RunRecord {
		t.Helper()
		seq++
		id := "01JB0000000000000000000Q" + string("0123456789"[seq]) + "1"
		fc.setRestore(id, restoreAnswer{sel: api.RestoreSelection{ID: id, SnapshotID: snap, Mode: mode, Overwrite: ow,
			Paths: paths, ExpiresAt: time.Now().Add(time.Hour)}})
		params, _ := json.Marshal(api.RestoreParams{RestoreRequestID: id})
		fc.enqueue(api.Command{ID: "01JB0000000000000000000M" + string("0123456789"[seq]) + "1", Type: api.CommandRestore,
			Params: params, CreatedAt: time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(time.Hour)})
		waitFor(t, "the live restore "+id, 4*time.Minute, func() bool {
			rec, ok := fc.run(id)
			return ok && !rec.Open()
		})
		rec, _ := fc.run(id)
		if err := rec.Validate(); err != nil {
			t.Fatalf("record does not validate: %v", err)
		}
		t.Logf("LIVE restore %s mode=%s overwrite=%s: outcome=%s result=%q restored=%d skipped=%d bytes=%d rows=%v",
			id, mode, ow, rec.Outcome, rec.Stats.RestoreResult, rec.Stats.FilesRestored, rec.Stats.FilesSkipped, rec.Stats.BytesRestored, rec.Stats.RestoreErrors)
		return rec
	}

	// okOutcome: elevated (SYSTEM, the agent's account), a clean restore is a
	// success. Run as an ordinary user, restic restores the snapshot's
	// descriptor of the folders above the tree (C:\Users and below) onto their
	// staging copies and then cannot set their timestamps: one error line per
	// run ("failed to restore timestamp of <staging>\Users: Access is denied"),
	// counted on the device as access_denied. That restic error is the only
	// difference allowed here.
	elevated := windows.GetCurrentProcessToken().IsElevated()
	okOutcome := func(rec api.RunRecord) bool {
		if rec.Outcome == restic.OutcomeSuccess {
			return true
		}
		if elevated || rec.Outcome != restic.OutcomePartial || len(rec.Stats.RestoreErrors) != 1 {
			return false
		}
		row := rec.Stats.RestoreErrors[0]
		return row.Class == api.RestoreResultAccessDenied && row.Count == 1
	}

	// 1. Delete a file, restore the tree's sub folder in keep mode.
	if err := os.Remove(filepath.Join(tree, "sub", "b.bin")); err != nil {
		t.Fatal(err)
	}
	rec := restore(api.DeviceRestoreOriginal, api.DeviceOverwriteNever, snapPath("sub"))
	// b.bin restored; the selected folder sub, brackets and deep\c.txt existed: skipped 3.
	if !okOutcome(rec) || rec.Stats.FilesRestored != 1 || rec.Stats.FilesSkipped != 3 {
		t.Errorf("keep restore = %s %+v", rec.Outcome, rec.Stats)
	}
	for rel := range files {
		got := sha(t, filepath.Join(tree, rel))
		t.Logf("LIVE sha256 keep    %-28s before=%s after=%s equal=%v", rel, before[rel][:16], got[:16], got == before[rel])
		if got != before[rel] {
			t.Errorf("%s differs after the keep restore", rel)
		}
	}

	// 2. Modify a file, restore it in replace mode.
	if err := os.WriteFile(filepath.Join(tree, "a.txt"), []byte("modified"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec = restore(api.DeviceRestoreOriginal, api.DeviceOverwriteIfChanged, snapPath("a.txt"))
	got := sha(t, filepath.Join(tree, "a.txt"))
	t.Logf("LIVE sha256 replace %-28s before=%s after=%s equal=%v", "a.txt", before["a.txt"][:16], got[:16], got == before["a.txt"])
	if !okOutcome(rec) || rec.Stats.FilesRestored != 1 || got != before["a.txt"] {
		t.Errorf("replace restore = %s %+v", rec.Outcome, rec.Stats)
	}

	// 3. Side folder.
	rec = restore(api.DeviceRestoreSide, api.DeviceOverwriteNever, snapPath(filepath.Join("sub", "brackets [1] $x.txt")), snapPath(filepath.Join("sub", "deep")))
	if !okOutcome(rec) || rec.Stats.FilesRestored != 3 {
		t.Errorf("side restore = %s %+v", rec.Outcome, rec.Stats)
	}
	sideID := "01JB0000000000000000000Q" + string("0123456789"[seq]) + "1"
	below := strings.TrimPrefix(filepath.Join(tree, "sub"), tree[:3])
	for _, rel := range []string{"brackets [1] $x.txt", filepath.Join("deep", "c.txt")} {
		p := filepath.Join(o.restoreSideRoot, sideID, "C", below, rel)
		gotSide := sha(t, p)
		want := before[filepath.Join("sub", rel)]
		t.Logf("LIVE sha256 side    %-28s before=%s after=%s equal=%v", filepath.Join("sub", rel), want[:16], gotSide[:16], gotSide == want)
		if gotSide != want {
			t.Errorf("side copy of %s differs", rel)
		}
		sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil || !strings.Contains(sd.String(), "ID;") || strings.Contains(sd.String(), "D:P") {
			t.Errorf("side descriptor of %s = %v %v", rel, sd, err)
		}
	}

	// 4. Nothing matched.
	rec = restore(api.DeviceRestoreOriginal, api.DeviceOverwriteNever, snapPath("no-such-file.txt"))
	if rec.Outcome != restic.OutcomeFailed || rec.Stats.RestoreResult != api.RestoreResultNothingMatched {
		t.Errorf("nothing-matched restore = %s %+v", rec.Outcome, rec.Stats)
	}
	if _, err := os.Lstat(filepath.Join(work, ".cryobackup-restore")); err == nil {
		if ents, _ := os.ReadDir(filepath.Join(work, ".cryobackup-restore")); len(ents) != 0 {
			// As SYSTEM the clean-up walk removes everything. As an ordinary
			// user it cannot remove the staging copy of C:\Users: restic gave
			// it the snapshot's read-only attribute and a DACL without write
			// attributes for this user, so it stays, the agent logs the ERROR
			// line, and the next start retries.
			if elevated {
				t.Errorf("staging left behind: %d entries", len(ents))
			} else {
				t.Logf("LIVE non-elevated: %d staging folders kept the read-only copy of the folders above the tree (removed as SYSTEM)", len(ents))
			}
		}
	}
	// Nothing path-like reached the console in what the agent sent.
	for _, b := range fc.allBodies() {
		if bytes.Contains(b.Body, []byte(work)) || bytes.Contains(b.Body, []byte(strings.ReplaceAll(work, `\`, `\\`))) ||
			bytes.Contains(b.Body, []byte("brackets")) || bytes.Contains(b.Body, []byte(`deep`)) {
			t.Errorf("%s carried a path", b.Path)
		}
	}
}

// longName expands 8.3 components (a TEMP of C:\Users\ABCDEF~1\...): restic
// stores the path it is given, and the placement compares names exactly.
func longName(t *testing.T, p string) string {
	t.Helper()
	in, _ := windows.UTF16PtrFromString(p)
	buf := make([]uint16, 1024)
	n, err := windows.GetLongPathName(in, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 {
		t.Fatalf("GetLongPathName: %v", err)
	}
	return windows.UTF16ToString(buf[:n])
}

func sha(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return strings.Repeat("-", 16) + "(" + err.Error() + ")"
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

var liveServingRE = regexp.MustCompile(`Serving restic REST API on \[?(https?://[^\]\s]+)`)

// liveGateway runs "rclone serve restic --append-only --private-repos" over a
// local folder on an ephemeral loopback port and returns its base URL.
func liveGateway(t *testing.T, rcloneExe, htpasswd, store string) string {
	t.Helper()
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(filepath.Dir(store), "rclone.conf")
	_ = os.WriteFile(conf, nil, 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, rcloneExe, "serve", "restic", "--addr", "127.0.0.1:0", "--htpasswd", htpasswd,
		"--private-repos", "--append-only", store)
	cmd.Env = append(os.Environ(), "RCLONE_CONFIG="+conf)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	urlCh := make(chan string, 1)
	var mu sync.Mutex
	var logb bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			mu.Lock()
			logb.WriteString(sc.Text() + "\n")
			mu.Unlock()
			if m := liveServingRE.FindStringSubmatch(sc.Text()); m != nil {
				select {
				case urlCh <- strings.TrimRight(m[1], "/"):
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
		return u
	case <-time.After(30 * time.Second):
		t.Fatal("rclone did not report a listen address")
		return ""
	}
}

// unlockLeftovers makes what a non-elevated run could not remove deletable
// again inside the test's own folder (this user owns it, so it may grant
// itself access and clear the read-only attribute), so the temp folder can be
// removed.
func unlockLeftovers(t *testing.T, root string) {
	if _, err := os.Lstat(root); err != nil {
		return
	}
	me := currentSID(t)
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if sd, e := windows.SecurityDescriptorFromString("D:(A;OICI;FA;;;" + me + ")"); e == nil {
			if dacl, _, e := sd.DACL(); e == nil {
				_ = windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
			}
		}
		if u, e := windows.UTF16PtrFromString(p); e == nil {
			attr := uint32(windows.FILE_ATTRIBUTE_NORMAL)
			if d.IsDir() {
				attr = windows.FILE_ATTRIBUTE_DIRECTORY
			}
			_ = windows.SetFileAttributes(u, attr)
		}
		return nil
	})
	_ = os.RemoveAll(root)
}
