package maintain

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// The bare-host restore against the fake restic (P22-F). The fake writes no
// files, so selfRestoreAfterRestore stands in for what `restic restore` puts in
// --out; everything else (the option checks, the version check, the derived
// password, the commands, the classes and the cleanup) is the real path.

// restoreStaging is the staging path the fake's snapshots list.
const restoreStaging = "/var/lib/cryobackup/self-backup-staging"

type restoreHarness struct {
	t       *testing.T
	repo    SelfRepo
	harness string
}

func newMasterKey(t *testing.T) *escrow.MasterKey {
	t.Helper()
	raw := make([]byte, escrow.MasterKeyLen)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	mk, err := escrow.NewMasterKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	return mk
}

// restoreSnapshots is the fake's listing: two snapshots of host console, the
// older one first (the engine sorts them).
func restoreSnapshots() string {
	b, _ := json.Marshal([]map[string]any{
		{"time": "2026-11-20T06:00:00Z", "id": strings.Repeat("ab", 32), "short_id": "abababab", "paths": []string{restoreStaging}, "hostname": "console"},
		{"time": "2026-11-25T06:00:00Z", "id": backupOKSnapshot, "short_id": backupOKSnapshot[:8], "paths": []string{restoreStaging}, "hostname": "console",
			"summary": map[string]any{"total_bytes_processed": 4096}},
	})
	return "inline:" + string(b)
}

func newRestoreHarness(t *testing.T, rules []fakeRule) *restoreHarness {
	t.Helper()
	exe, sum := fakeResticPath(t)
	harness := t.TempDir()
	fixtures, err := filepath.Abs(filepath.Join("..", "..", "restic", "testdata", "v0.19.1"))
	if err != nil {
		t.Fatal(err)
	}
	if rules == nil {
		rules = []fakeRule{
			{Cmd: "version", Stdout: "version.json"},
			{Cmd: "snapshots", Stdout: restoreSnapshots()},
			{Cmd: "restore", Stdout: "restore.stdout.jsonl"},
		}
	}
	sc, _ := json.Marshal(fakeScenario{Fixtures: fixtures, Rules: rules})
	if err := os.WriteFile(filepath.Join(harness, "fake.json"), sc, 0o600); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(harness, "rclone.conf")
	if err := os.WriteFile(conf, []byte("[admin]\ntype = alias\nremote = /nowhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &restoreHarness{t: t, harness: harness, repo: SelfRepo{
		ResticProgram: exe, ResticSHA256: sum, RcloneProgram: "rclone", RcloneConfig: conf,
		Remote: "admin", Bucket: "bucket", Prefix: "console-self", MasterKey: newMasterKey(t),
	}}
}

func (h *restoreHarness) calls() []fakeCall {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.harness, "fake.calls"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		h.t.Fatal(err)
	}
	var out []fakeCall
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var c fakeCall
		if err := json.Unmarshal([]byte(l), &c); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// selfCopy writes what a self-backup snapshot holds into dir: a VACUUM INTO
// copy of a fresh console database, config.json and its manifest under mk.
func selfCopy(t *testing.T, dir string, mk *escrow.MasterKey) SelfBackupManifest {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "live.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	db := filepath.Join(dir, SelfBackupDBFile)
	if err := st.VacuumInto(ctx, db); err != nil {
		t.Fatal(err)
	}
	facts, err := store.InspectDatabaseFile(ctx, db)
	if err != nil || facts.Integrity != "ok" {
		t.Fatalf("copy facts %+v %v", facts, err)
	}
	size, sum, err := hashFile(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, SelfBackupConfigFile), []byte(`{"db_path":"/var/lib/cryobackup/console.db"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m := SelfBackupManifest{Format: SelfBackupManifestFormat, CreatedAt: time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC), SchemaVersion: facts.SchemaVersion,
		Build: "test", Database: SelfBackupFile{Name: SelfBackupDBFile, Bytes: size, SHA256: sum}, Tables: facts.Tables,
		MasterKeyID: mk.KeyID(), Bucket: "bucket", Prefix: "console-self", DevicePrefix: "cb", ConfigIncluded: true}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, SelfBackupManifestFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return m
}

// seamFrom makes the fake "restore" the files of src into --out.
func seamFrom(t *testing.T, src string, mutate func(out string)) {
	t.Helper()
	selfRestoreAfterRestore = func(out string) {
		ents, _ := os.ReadDir(src)
		for _, e := range ents {
			b, _ := os.ReadFile(filepath.Join(src, e.Name()))
			_ = os.WriteFile(filepath.Join(out, e.Name()), b, 0o600)
		}
		if mutate != nil {
			mutate(out)
		}
	}
	t.Cleanup(func() { selfRestoreAfterRestore = nil })
}

func wantSelfErr(t *testing.T, err error, class string, usage bool) {
	t.Helper()
	var se *SelfRestoreError
	if !errors.As(err, &se) || se.Class != class || se.Usage != usage {
		t.Fatalf("error %v, want class %s usage %v", err, class, usage)
	}
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// A good restore: the newest snapshot, restored with "<id>:<staging path>"
// into --out, with the password derived from the master key; console.db is
// byte-equal to the backed-up copy and passes the manifest check; the session
// folder beside --out is gone.
func TestSelfRestoreGood(t *testing.T) {
	h := newRestoreHarness(t, nil)
	src := t.TempDir()
	m := selfCopy(t, src, h.repo.MasterKey)
	seamFrom(t, src, nil)
	parent := t.TempDir()
	out := filepath.Join(parent, "restored")

	res, err := RestoreSelfBackup(context.Background(), h.repo, "", out)
	if err != nil {
		t.Fatal(err)
	}
	if res.Snapshot.ID != backupOKSnapshot || res.Manifest.Database.SHA256 != m.Database.SHA256 || !res.ConfigIncluded || res.EscrowChecked {
		t.Fatalf("result %+v", res)
	}
	if _, sum, _ := hashFile(filepath.Join(out, SelfBackupDBFile)); sum != m.Database.SHA256 {
		t.Fatalf("restored console.db sha256 %s, manifest %s", sum, m.Database.SHA256)
	}
	if got := strings.Join(dirEntries(t, out), ","); got != "config.json,console.db,manifest.json" {
		t.Fatalf("--out holds %s", got)
	}
	if got := dirEntries(t, parent); len(got) != 1 || got[0] != "restored" {
		t.Fatalf("beside --out: %v (the session folder must be removed)", got)
	}
	pw, _ := SelfBackupPasswordFrom(h.repo.MasterKey.Bytes())
	var restore *fakeCall
	for i, c := range h.calls() {
		if c.Args[0] != "version" && (c.Password != pw || c.Repo != "rclone:admin:bucket/console-self" || c.CacheSet || c.RClone != h.repo.RcloneConfig) {
			t.Errorf("call %v: repo %q, password derived %v, cache set %v", c.Args, c.Repo, c.Password == pw, c.CacheSet)
		}
		if c.Args[0] == "restore" {
			restore = &h.calls()[i]
		}
	}
	if restore == nil || !argsHave(*restore, backupOKSnapshot+":"+restoreStaging, "--target", out, restic.NoCacheFlag) {
		t.Fatalf("restore call %+v", restore)
	}
}

// --snapshot takes a unique id prefix; an unknown one fails, an ambiguous one is
// the caller's to fix.
func TestPickSnapshot(t *testing.T) {
	snaps := []restic.Snapshot{{ID: "aa11"}, {ID: "aa22"}, {ID: "bb33"}}
	if s, err := pickSnapshot(snaps, ""); err != nil || s.ID != "aa11" {
		t.Fatalf("newest: %+v %v", s, err)
	}
	if s, err := pickSnapshot(snaps, "bb"); err != nil || s.ID != "bb33" {
		t.Fatalf("prefix: %+v %v", s, err)
	}
	_, err := pickSnapshot(snaps, "cc")
	wantSelfErr(t, err, SelfClassSnapshotUnknown, false)
	_, err = pickSnapshot(snaps, "aa")
	wantSelfErr(t, err, SelfClassSnapshotAmbiguous, true)
	_, err = pickSnapshot(nil, "")
	wantSelfErr(t, err, SelfClassNoSnapshot, false)
	for id, ok := range map[string]bool{"abcd": true, backupOKSnapshot: true, "abc": false, "ABCD": false, "abcg": false, strings.Repeat("a", 65): false} {
		if ValidSnapshotPrefix(id) != ok {
			t.Errorf("ValidSnapshotPrefix(%q) != %v", id, ok)
		}
	}
}

func TestSelfRestoreNamedSnapshot(t *testing.T) {
	h := newRestoreHarness(t, nil)
	src := t.TempDir()
	selfCopy(t, src, h.repo.MasterKey)
	seamFrom(t, src, nil)
	res, err := RestoreSelfBackup(context.Background(), h.repo, "abab", filepath.Join(t.TempDir(), "out"))
	if err != nil || res.Snapshot.ID != strings.Repeat("ab", 32) {
		t.Fatalf("named: %+v %v", res.Snapshot, err)
	}
	_, err = RestoreSelfBackup(context.Background(), h.repo, "cdcd", filepath.Join(t.TempDir(), "out"))
	wantSelfErr(t, err, SelfClassSnapshotUnknown, false)
	_, err = RestoreSelfBackup(context.Background(), h.repo, "xyz!", filepath.Join(t.TempDir(), "out"))
	wantSelfErr(t, err, SelfClassConfig, true)
}

// A non-empty --out, a file, or a symlink is refused before restic runs; the
// folder's files are untouched.
func TestSelfRestoreRefusesOut(t *testing.T) {
	h := newRestoreHarness(t, nil)
	out := t.TempDir()
	keep := filepath.Join(out, "keep.txt")
	if err := os.WriteFile(keep, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := RestoreSelfBackup(context.Background(), h.repo, "", out)
	wantSelfErr(t, err, SelfClassOutNotEmpty, true)
	if b, err := os.ReadFile(keep); err != nil || string(b) != "mine" {
		t.Fatalf("the operator's file: %q %v", b, err)
	}
	_, err = RestoreSelfBackup(context.Background(), h.repo, "", keep)
	wantSelfErr(t, err, SelfClassOutDir, true)
	_, err = RestoreSelfBackup(context.Background(), h.repo, "", filepath.Join(t.TempDir(), "no", "parent"))
	wantSelfErr(t, err, SelfClassOutDir, true)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(t.TempDir(), link); err == nil {
		_, err = RestoreSelfBackup(context.Background(), h.repo, "", link)
		wantSelfErr(t, err, SelfClassOutDir, true)
	}
	if c := h.calls(); len(c) != 0 {
		t.Fatalf("restic ran for a refused --out: %v", c)
	}
}

// Missing options and an unloaded key are usage errors; a restic that is not
// the pinned version or not the pinned binary is refused.
func TestSelfRestoreConfigErrors(t *testing.T) {
	h := newRestoreHarness(t, nil)
	out := filepath.Join(t.TempDir(), "out")
	for name, mut := range map[string]func(*SelfRepo){
		"bucket":        func(r *SelfRepo) { r.Bucket = "" },
		"prefix":        func(r *SelfRepo) { r.Prefix = "/" },
		"remote":        func(r *SelfRepo) { r.Remote = "" },
		"remote_colon":  func(r *SelfRepo) { r.Remote = "a:b" },
		"rclone_config": func(r *SelfRepo) { r.RcloneConfig = filepath.Join(h.harness, "absent.conf") },
		"rclone":        func(r *SelfRepo) { r.RcloneProgram = "" },
		"rclone_space":  func(r *SelfRepo) { r.RcloneProgram = "/opt/my tools/rclone" }, // restic splits rclone.program
		"rclone_quote":  func(r *SelfRepo) { r.RcloneProgram = `/opt/"rclone"` },
	} {
		r := h.repo
		mut(&r)
		_, err := RestoreSelfBackup(context.Background(), r, "", out)
		var se *SelfRestoreError
		if !errors.As(err, &se) || !se.Usage || se.Class != SelfClassConfig {
			t.Errorf("%s: %v", name, err)
		}
	}
	r := h.repo
	r.MasterKey = nil
	_, err := RestoreSelfBackup(context.Background(), r, "", out)
	wantSelfErr(t, err, SelfClassNoKey, true)
	r = h.repo
	r.ResticSHA256 = strings.Repeat("1", 64)
	_, err = RestoreSelfBackup(context.Background(), r, "", out)
	wantSelfErr(t, err, SelfClassResticBinary, true)
	r = h.repo
	r.ResticProgram = filepath.Join(h.harness, "no-restic")
	_, err = RestoreSelfBackup(context.Background(), r, "", out)
	wantSelfErr(t, err, SelfClassResticBinary, true)

	old := newRestoreHarness(t, []fakeRule{{Cmd: "version", Stdout: `inline:{"message_type":"version","version":"0.18.0","go_version":"go1.24","go_os":"linux","go_arch":"amd64"}`}})
	_, err = RestoreSelfBackup(context.Background(), old.repo, "", out)
	wantSelfErr(t, err, SelfClassResticVersion, true)
	if _, err := os.Lstat(out); !os.IsNotExist(err) {
		t.Fatalf("--out created on a refused run: %v", err)
	}
}

// A wrong master key: restic refuses the derived password; the run fails (not
// usage) and nothing is written: an absent --out stays absent, an empty one
// stays empty.
func TestSelfRestoreWrongMasterKey(t *testing.T) {
	h := newRestoreHarness(t, []fakeRule{
		{Cmd: "version", Stdout: "version.json"},
		{Cmd: "snapshots", Exit: 12, Stderr: "exit12_wrong_password.stderr.jsonl"},
	})
	absent := filepath.Join(t.TempDir(), "out")
	_, err := RestoreSelfBackup(context.Background(), h.repo, "", absent)
	wantSelfErr(t, err, string(restic.ClassWrongPassword), false)
	if _, err := os.Lstat(absent); !os.IsNotExist(err) {
		t.Fatalf("--out exists after a wrong key: %v", err)
	}
	empty := t.TempDir()
	_, err = RestoreSelfBackup(context.Background(), h.repo, "", empty)
	wantSelfErr(t, err, string(restic.ClassWrongPassword), false)
	if got := dirEntries(t, empty); len(got) != 0 {
		t.Fatalf("--out holds %v after a wrong key", got)
	}
	_, err = ListSelfSnapshots(context.Background(), h.repo)
	wantSelfErr(t, err, string(restic.ClassWrongPassword), false)
}

// A restored copy that does not match its manifest (or a restore that wrote
// nothing, or a copy made under another master key) fails, and --out is emptied
// again (removed when the restore created it).
func TestSelfRestoreMismatchWipes(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(out string)
		noSeam bool
		key    bool // the copy is made under another master key
		class  string
	}{
		"corrupt_db": {mutate: func(out string) {
			p := filepath.Join(out, SelfBackupDBFile)
			b, _ := os.ReadFile(p)
			b[len(b)/2] ^= 0xFF
			_ = os.WriteFile(p, b, 0o600)
		}, class: SelfClassManifest},
		"no_manifest": {mutate: func(out string) { _ = os.Remove(filepath.Join(out, SelfBackupManifestFile)) }, class: SelfClassManifest},
		"config_lost": {mutate: func(out string) { _ = os.Remove(filepath.Join(out, SelfBackupConfigFile)) }, class: SelfClassManifest},
		"nothing":     {noSeam: true, class: SelfClassManifest},
		"other_key":   {key: true, class: SelfClassMasterKey},
	} {
		t.Run(name, func(t *testing.T) {
			h := newRestoreHarness(t, nil)
			src := t.TempDir()
			mk := h.repo.MasterKey
			if tc.key {
				mk = newMasterKey(t)
			}
			selfCopy(t, src, mk)
			if !tc.noSeam {
				seamFrom(t, src, tc.mutate)
			}
			parent := t.TempDir()
			created := filepath.Join(parent, "out")
			_, err := RestoreSelfBackup(context.Background(), h.repo, "", created)
			wantSelfErr(t, err, tc.class, false)
			if got := dirEntries(t, parent); len(got) != 0 {
				t.Fatalf("left beside and in --out: %v", got)
			}
			empty := t.TempDir()
			_, err = RestoreSelfBackup(context.Background(), h.repo, "", empty)
			wantSelfErr(t, err, tc.class, false)
			if got := dirEntries(t, empty); len(got) != 0 {
				t.Fatalf("--out holds %v", got)
			}
		})
	}
}

// The storage unreachable and a restore that restic fails: classes, nothing left.
func TestSelfRestoreRestoreFails(t *testing.T) {
	h := newRestoreHarness(t, []fakeRule{
		{Cmd: "version", Stdout: "version.json"},
		{Cmd: "snapshots", Stdout: restoreSnapshots()},
		{Cmd: "restore", Exit: 1, Stdout: "restore_error.stdout.jsonl", Stderr: "restore_error.stderr.jsonl"},
	})
	out := filepath.Join(t.TempDir(), "out")
	_, err := RestoreSelfBackup(context.Background(), h.repo, "", out)
	var se *SelfRestoreError
	if !errors.As(err, &se) || se.Usage {
		t.Fatalf("failed restore: %v", err)
	}
	if _, err := os.Lstat(out); !os.IsNotExist(err) {
		t.Fatalf("--out left after a failed restore: %v", err)
	}
	down := newRestoreHarness(t, []fakeRule{
		{Cmd: "version", Stdout: "version.json"},
		{Cmd: "snapshots", Exit: 1, Stderr: `inline:{"message_type":"exit_error","code":1,"message":"Fatal: read tcp: connection reset by peer"}` + "\n"},
	})
	_, err = ListSelfSnapshots(context.Background(), down.repo)
	wantSelfErr(t, err, string(restic.ClassNetwork), false)
}

// The listing is newest first and carries the summary's size.
func TestListSelfSnapshotsNewestFirst(t *testing.T) {
	h := newRestoreHarness(t, nil)
	snaps, err := ListSelfSnapshots(context.Background(), h.repo)
	if err != nil || len(snaps) != 2 || snaps[0].ID != backupOKSnapshot || snaps[0].Summary == nil || snaps[0].Summary.TotalBytesProcessed != 4096 {
		t.Fatalf("snapshots %+v %v", snaps, err)
	}
	for _, c := range h.calls() {
		if c.Args[0] == "snapshots" && !argsHave(c, "--host", SelfBackupHost) {
			t.Errorf("listing not limited to host console: %v", c.Args)
		}
	}
}

// P22-R2b B6: a drill run (SelfRepo.Drill) reads the live self-backup repository with --no-lock and no
// --retry-lock, for the snapshot listing and for the restore, so it leaves no lock object in the
// production repository; a normal run keeps --retry-lock and no --no-lock. The restore writes only to
// --out. Killed mutants: Drill ignored (argv keeps --retry-lock), --no-lock dropped from the restore.
func TestSelfRestoreDrillReadsRunWithNoLock(t *testing.T) {
	for _, drill := range []bool{false, true} {
		h := newRestoreHarness(t, nil)
		h.repo.Drill = drill
		src := t.TempDir()
		selfCopy(t, src, h.repo.MasterKey)
		seamFrom(t, src, nil)
		if _, err := ListSelfSnapshots(context.Background(), h.repo); err != nil {
			t.Fatal(err)
		}
		if _, err := RestoreSelfBackup(context.Background(), h.repo, "", filepath.Join(t.TempDir(), "restored")); err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, c := range h.calls() {
			if c.Args[0] == "version" {
				continue
			}
			seen[c.Args[0]] = true
			if got := argsHave(c, "--no-lock"); got != drill {
				t.Errorf("drill=%v: %s argv %v: --no-lock = %v", drill, c.Args[0], c.Args, got)
			}
			if got := argsHave(c, "--retry-lock"); got == drill {
				t.Errorf("drill=%v: %s argv %v: --retry-lock = %v", drill, c.Args[0], c.Args, got)
			}
		}
		if !seen["snapshots"] || !seen["restore"] {
			t.Fatalf("drill=%v: calls seen %v", drill, seen)
		}
	}
}
