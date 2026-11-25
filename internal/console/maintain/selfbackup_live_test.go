package maintain

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// TestLiveSelfBackup runs the self-backup with the real restic 0.19.1 and
// rclone 1.75.1 against a local rclone remote (an alias over a temp folder):
// a backup and a tested restore pass and the restored database equals the
// staged copy by sha256; the snapshot holds exactly the three staged files and
// none of the offline-kit files lying in the state directory; a staged copy
// corrupted after its manifest was written fails the next verification.
//
//	CRYO_RESTIC=<restic> CRYO_RCLONE=<rclone> go test -count=1 -run LiveSelfBackup -v ./internal/console/maintain
func TestLiveSelfBackup(t *testing.T) {
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
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(filepath.Join(stateDir, "tls"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The offline kit and other secrets an operator keeps beside the database:
	// none of them may reach the snapshot.
	decoys := []string{"master.key", "rclone-admin.conf", "rclone-gateway.conf", "maintain.env", ".env", "smtp-password", filepath.Join("tls", "gateway.key")}
	for _, d := range decoys {
		if err := os.WriteFile(filepath.Join(stateDir, d), []byte("SECRET-DECOY"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	confJSON := filepath.Join(stateDir, "config.json")
	if err := os.WriteFile(confJSON, []byte(`{"db_path":"/var/lib/cryobackup/console.db","data_dir":"/var/lib/cryobackup"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newTenv(t, nil, func(c *Config) {
		c.ResticProgram, c.ResticSHA256 = resticBin, sum
		c.RcloneProgram, c.RcloneConfig = rcloneBin, conf
		c.SelfPrefix = "console-self"
		c.StateDir = stateDir
		c.ConfigFile = confJSON
	})
	var pwb [32]byte
	if _, err := rand.Read(pwb[:]); err != nil {
		t.Fatal(err)
	}
	sk := &fakeSelfKeys{pw: hex.EncodeToString(pwb[:]), keyID: "live0123456789ab"}
	e.w.selfKeys = sk

	var stagedSHA, restoredSHA string
	corrupt := false
	e.w.selfBeforeBackup = func(dir string) {
		db := filepath.Join(dir, SelfBackupDBFile)
		if corrupt {
			b, err := os.ReadFile(db)
			if err != nil {
				t.Fatal(err)
			}
			for i := len(b) / 2; i < len(b)/2+64 && i < len(b); i++ {
				b[i] ^= 0xFF
			}
			if err := os.WriteFile(db, b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		_, stagedSHA, _ = hashFile(db)
	}
	e.w.selfAfterRestore = func(dir string) { _, restoredSHA, _ = hashFile(filepath.Join(dir, SelfBackupDBFile)) }

	// 1. Back up, verify, and compare.
	rep, err := e.w.SelfBackupStep(e.ctx)
	if err != nil || !rep.BackupOK || !rep.VerifyRan || !rep.VerifyVerdict || !rep.VerifyOK || !rep.EscrowChecked {
		t.Fatalf("first step = %+v, %v\nlog:\n%s", rep, err, e.logs.String())
	}
	st, err := e.st.SelfBackupState(e.ctx)
	if err != nil || st.Last == nil {
		t.Fatalf("state = %+v, %v", st, err)
	}
	if stagedSHA == "" || restoredSHA != stagedSHA || st.Last.DBSHA256 != stagedSHA {
		t.Fatalf("restored sha256 %s, staged %s, recorded %s", restoredSHA, stagedSHA, st.Last.DBSHA256)
	}
	t.Logf("live: backup snapshot %s, restored console.db sha256 %s equals the staged copy; verify ok, escrow checked %v",
		rep.SnapshotID[:12], restoredSHA[:16], rep.EscrowChecked)
	for _, d := range []string{SelfBackupStagingDir, SelfBackupVerifyDir} {
		if _, err := os.Lstat(filepath.Join(stateDir, d)); !os.IsNotExist(err) {
			t.Errorf("%s left behind (%v)", d, err)
		}
	}

	// 2. What the snapshot holds: exactly the three files.
	repo, err := restic.AdminRepository("admin", e.cfg.Bucket, "", "console-self")
	if err != nil {
		t.Fatal(err)
	}
	env := []string{"RESTIC_REPOSITORY=" + repo, "RESTIC_PASSWORD=" + sk.pw, "RCLONE_CONFIG=" + conf}
	opts := append([]string{"--no-cache"}, restic.AdminOptions(filepath.ToSlash(rcloneBin), false)...)
	out, serr, code := agentRestic(t, resticBin, env, append([]string{"ls", "--json", "latest"}, opts...)...)
	if code != 0 {
		t.Fatalf("restic ls exit %d: %s", code, serr)
	}
	var files []string
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var n struct {
			Type, Name, StructType string
			MessageType            string `json:"message_type"`
		}
		if json.Unmarshal(sc.Bytes(), &n) != nil {
			continue
		}
		if n.Type == "file" {
			files = append(files, n.Name)
		}
	}
	sort.Strings(files)
	if strings.Join(files, ",") != "config.json,console.db,manifest.json" {
		t.Fatalf("snapshot files = %v, want exactly config.json, console.db, manifest.json", files)
	}
	for _, d := range decoys {
		if strings.Contains(out, filepath.Base(d)) {
			t.Errorf("the snapshot lists %s", filepath.Base(d))
		}
	}
	if strings.Contains(out, "SECRET-DECOY") {
		t.Error("decoy content in the listing")
	}
	t.Logf("live: snapshot files %v; no offline-kit file listed", files)

	// 3. A staged copy corrupted after its manifest: the next verification fails.
	corrupt = true
	e.clk.Advance(SelfBackupVerifyEvery)
	rep, err = e.w.SelfBackupStep(e.ctx)
	if err != nil || !rep.BackupOK || !rep.VerifyRan || !rep.VerifyVerdict || rep.VerifyOK || rep.VerifyClass != SelfClassManifest {
		t.Fatalf("corrupted step = %+v, %v\nlog:\n%s", rep, err, e.logs.String())
	}
	f, err := e.st.SelfBackupFacts(e.ctx)
	if err != nil || !f.VerifyFailed {
		t.Fatalf("facts after a corrupted copy = %+v, %v", f, err)
	}
	js, err := e.st.GetJobState(e.ctx, SelfBackupVerifyJob)
	if err != nil || js.LastStatus != "error" || js.LastErrorClass != SelfClassManifest {
		t.Fatalf("verify row = %+v, %v", js, err)
	}
	t.Logf("live: corrupted staged copy -> verify %s (class %s), VerifyFailed %v", js.LastStatus, js.LastErrorClass, f.VerifyFailed)

	// Retention ran: forget kept both snapshots (inside 7 days), prune exited 0.
	out, serr, code = agentRestic(t, resticBin, env, append([]string{"snapshots", "--json"}, opts...)...)
	if code != 0 {
		t.Fatalf("restic snapshots exit %d: %s", code, serr)
	}
	var snaps []restic.Snapshot
	if err := json.Unmarshal([]byte(out), &snaps); err != nil || len(snaps) != 2 {
		t.Fatalf("snapshots = %d, %v", len(snaps), err)
	}
	for _, s := range snaps {
		if s.Hostname != SelfBackupHost {
			t.Errorf("snapshot host %q", s.Hostname)
		}
	}
}
