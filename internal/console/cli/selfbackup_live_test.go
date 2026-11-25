package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/maintain"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// TestLiveSelfBackupCLI runs `cryo-console self-backup` end to end with the real
// restic 0.19.1 and rclone 1.75.1 against a local rclone remote (an alias over a
// temp folder): run writes a snapshot through the worker's own step, verify
// passes, list shows it; then, with the console's data directory moved away
// (no database) and the worker's variables cleared, restore with only the
// rclone file and a copy of the master key reproduces console.db sha256-equal
// to the copy the engine backed up and passes the manifest check. A wrong
// master key fails with exit 1 and writes nothing; no secret is printed.
//
//	CRYO_RESTIC=<restic> CRYO_RCLONE=<rclone> go test -count=1 -run LiveSelfBackupCLI -v ./internal/console/cli
func TestLiveSelfBackupCLI(t *testing.T) {
	resticBin, rcloneBin := os.Getenv("CRYO_RESTIC"), os.Getenv("CRYO_RCLONE")
	if resticBin == "" || rcloneBin == "" {
		t.Skip("CRYO_RESTIC and CRYO_RCLONE are not both set")
	}
	clearSelfEnv(t)
	dir := t.TempDir()
	if code, out, errb := run(t, "", initArgs(dir, "--master-key-copy-confirmed")...); code != 0 {
		t.Fatalf("init: %d\n%s\n%s", code, out, errb)
	}
	cfgPath := filepath.Join(dir, "config.json")
	cfg, err := config.Read(cfgPath, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	remote := t.TempDir()
	kit := t.TempDir() // the offline kit: the admin rclone file and the master key
	conf := filepath.Join(kit, "rclone-admin.conf")
	if err := os.WriteFile(conf, []byte("[admin]\ntype = alias\nremote = "+filepath.ToSlash(remote)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keyCopy := filepath.Join(kit, "master.key")
	keyBytes, err := os.ReadFile(cfg.MasterKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyCopy, keyBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	mk, err := escrow.LoadMasterKey(keyCopy)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := maintain.SelfBackupPasswordFrom(mk.Bytes())
	secrets := []string{password, strings.TrimSpace(string(keyBytes)), base64.StdEncoding.EncodeToString(mk.Bytes())}
	noSecret := func(what, s string) {
		t.Helper()
		for _, sec := range secrets {
			if sec != "" && strings.Contains(s, sec) {
				t.Fatalf("%s printed a secret", what)
			}
		}
	}

	// The worker's environment, as maintain.env and the unit give it.
	t.Setenv("CRYO_MAINTAIN_RESTIC", resticBin)
	t.Setenv("CRYO_MAINTAIN_RCLONE", rcloneBin)
	t.Setenv("RCLONE_CONFIG", conf)
	t.Setenv("CRYO_MAINTAIN_REMOTE", "admin")
	t.Setenv("CRYO_MAINTAIN_BUCKET", "bucket")
	t.Setenv("CRYO_MAINTAIN_PREFIX", "cb")
	t.Setenv("CRYO_MAINTAIN_SELF_PREFIX", "console-self")
	t.Setenv("CRYO_MAINTAIN_SCRATCH_DIR", filepath.Join(dir, "scratch"))
	t.Setenv("CRYO_MAINTAIN_MAX_STREAMS", "")

	// 1. run: one backup now, through the worker's step.
	code, out, errb := run(t, "", "self-backup", "run", "--config", cfgPath)
	noSecret("run", out+errb)
	if code != ExitOK || !strings.Contains(out, "self-backup run: ok") {
		t.Fatalf("run: %d\nstdout %s\nstderr %s", code, out, errb)
	}
	t.Logf("live: %s", lastLine(out))

	// 2. verify: the weekly tested restore now.
	code, out, errb = run(t, "", "self-backup", "verify", "--config", cfgPath)
	noSecret("verify", out+errb)
	if code != ExitOK || !strings.Contains(out, "self-backup verify: verdict ok") {
		t.Fatalf("verify: %d\nstdout %s\nstderr %s", code, out, errb)
	}
	t.Logf("live: %s", lastLine(out))

	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: cfg.DBPath})
	if err != nil {
		t.Fatal(err)
	}
	state, err := st.SelfBackupState(ctx)
	st.Close()
	if err != nil || state.Last == nil || state.Last.DBSHA256 == "" || state.Verify == nil || !state.Verify.OK {
		t.Fatalf("self-backup rows: %+v %v", state, err)
	}
	backedUp := state.Last.DBSHA256

	// 3. list (the worker's variables give the defaults; the key from the kit).
	code, out, errb = run(t, "", "self-backup", "list", "--master-key", keyCopy)
	noSecret("list", out+errb)
	if code != ExitOK || !strings.Contains(out, state.Last.SnapshotID) || !strings.Contains(out, "TIME (UTC)") {
		t.Fatalf("list: %d\nstdout %s\nstderr %s", code, out, errb)
	}
	t.Logf("live: list\n%s", out)

	// 4. A bare host: no database (the data directory is gone), no worker
	// variables, only the rclone file and the master key.
	if err := os.Rename(cfg.DataDir, cfg.DataDir+".gone"); err != nil {
		t.Fatal(err)
	}
	clearSelfEnv(t)
	for _, v := range []string{"CRYO_MAINTAIN_PREFIX", "CRYO_MAINTAIN_SCRATCH_DIR", "CRYO_DB_PATH", "CRYO_DATA_DIR"} {
		t.Setenv(v, "")
	}
	bare := func(key, outDir string) (int, string, string) {
		return run(t, "", "self-backup", "restore", "--rclone-config", conf, "--remote", "admin", "--bucket", "bucket",
			"--prefix", "console-self", "--master-key", key, "--restic", resticBin, "--rclone", rcloneBin, "--out", outDir)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	code, out, errb = bare(keyCopy, restored)
	noSecret("restore", out+errb)
	if code != ExitOK || !strings.Contains(out, "self-backup restore: ok") || !strings.Contains(out, "equal to its manifest") {
		t.Fatalf("restore: %d\nstdout %s\nstderr %s", code, out, errb)
	}
	sum := sha256File(t, filepath.Join(restored, maintain.SelfBackupDBFile))
	if sum != backedUp {
		t.Fatalf("restored console.db sha256 %s, backed up %s", sum, backedUp)
	}
	for _, f := range []string{maintain.SelfBackupConfigFile, maintain.SelfBackupManifestFile} {
		if _, err := os.Stat(filepath.Join(restored, f)); err != nil {
			t.Fatalf("%s not restored: %v", f, err)
		}
	}
	facts, err := store.InspectDatabaseFile(ctx, filepath.Join(restored, maintain.SelfBackupDBFile))
	if err != nil || facts.Integrity != "ok" {
		t.Fatalf("restored database: %+v %v", facts, err)
	}
	t.Logf("live: bare-host restore, console.db sha256 %s equals the backed-up copy; integrity ok; schema %d", sum[:16], facts.SchemaVersion)
	t.Logf("live: restore output\n%s", out)

	// 5. A wrong master key: exit 1, nothing written (an absent --out stays
	// absent, an empty one stays empty).
	wrongKey := filepath.Join(kit, "other.key")
	if _, err := escrow.GenerateMasterKey(wrongKey, rand.Reader); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(t.TempDir(), "never")
	code, out, errb = bare(wrongKey, absent)
	noSecret("wrong key", out+errb)
	if code != ExitFailed || !strings.Contains(errb, "class wrong_password") {
		t.Fatalf("wrong key: %d\nstdout %s\nstderr %s", code, out, errb)
	}
	if _, err := os.Lstat(absent); !os.IsNotExist(err) {
		t.Fatalf("--out exists after a wrong key: %v", err)
	}
	empty := filepath.Join(t.TempDir(), "empty") // alone in its parent, so the session folder would show
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	if code, _, _ = bare(wrongKey, empty); code != ExitFailed {
		t.Fatalf("wrong key into an empty folder: %d", code)
	}
	if ents, _ := os.ReadDir(empty); len(ents) != 0 {
		t.Fatalf("an empty --out holds %d entries after a wrong key", len(ents))
	}
	if ents, _ := os.ReadDir(filepath.Dir(empty)); len(ents) != 1 {
		t.Fatalf("beside --out: %d entries (the session folder must be gone)", len(ents))
	}
	t.Logf("live: wrong master key -> exit %d, class wrong_password, nothing written", code)

	// 6. Restoring into the folder that now holds the restore is refused.
	if code, _, errb = bare(keyCopy, restored); code != ExitUsage || !strings.Contains(errb, "class out_not_empty") {
		t.Fatalf("non-empty --out: %d %s", code, errb)
	}
}

func sha256File(t *testing.T, p string) string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
