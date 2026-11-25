package maintain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// The self-backup unit tests run the engine against the fake restic. The fake
// cannot write a folder, so two seams stand in for the repository: the staged
// files are copied aside when `restic backup` is about to run
// (selfBeforeBackup), and copied into the verify folder when `restic restore`
// has exited 0 (selfAfterRestore). Everything else (the commands, the
// environment, the exit codes, the job rows and settings) is the real path.

const (
	testSelfPrefix = "console-self"
	testSelfPW     = "self-backup-password-0123456789abcdef"
	testSelfKeyID  = "0123456789abcdef"
	// backupOKSnapshot is the snapshot id in backup_ok.stdout.jsonl.
	backupOKSnapshot = "d108a6a97862287861963bf188f282666208e1f99f2fe0f39a9f6763e6e9e6f4"
)

// fakeSelfKeys is a SelfBackupKeyer with a fixed password and key id.
type fakeSelfKeys struct {
	mu        sync.Mutex
	pw, keyID string
	verifyErr error
	verified  []string
}

func (k *fakeSelfKeys) SelfBackupPassword() (string, error) { return k.pw, nil }
func (k *fakeSelfKeys) MasterKeyID() string                 { return k.keyID }
func (k *fakeSelfKeys) VerifyEscrow(_ context.Context, st *store.Store, deviceID string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.verified = append(k.verified, deviceID)
	if _, err := st.ActiveEscrow(context.Background(), store.ScopeAll(), deviceID); err != nil {
		return err // the row must really be in the restored copy
	}
	return k.verifyErr
}

// selfEnv is a tenv with the self-backup configured and the two seams wired.
type selfEnv struct {
	*tenv
	sk       *fakeSelfKeys
	repo     string   // the staged files as last "backed up"
	staged   []string // names in the staging folder at the last backup
	stateDir string
	confFile string
}

// selfRules are the fake restic's answers for a clean first step: the
// repository is missing (cat config exit 10), init creates it, and every later
// command succeeds. staging is the staging folder the snapshot lists.
func selfRules(staging string) []fakeRule {
	snaps, _ := json.Marshal([]map[string]any{
		{"time": "2026-10-22T13:42:10Z", "id": backupOKSnapshot, "short_id": backupOKSnapshot[:8], "paths": []string{staging}, "hostname": "console"},
		{"time": "2026-10-21T13:42:10Z", "id": strings.Repeat("ab", 32), "short_id": "abababab", "paths": []string{"/elsewhere"}, "hostname": "console"},
	})
	return []fakeRule{
		{Cmd: "cat", Exit: 10, Stderr: "exit10_repo_missing.stderr.jsonl"},
		{Cmd: "init", Stdout: "init.json"},
		{Cmd: "backup", Stdout: "backup_ok.stdout.jsonl"},
		{Cmd: "forget", Stdout: "forget.json"},
		{Cmd: "prune", Stdout: "prune.stdout.txt"},
		{Cmd: "snapshots", Stdout: "inline:" + string(snaps)},
		{Cmd: "restore", Stdout: "restore.stdout.jsonl"},
		{Cmd: "check", Stdout: "check.stdout.jsonl"},
		{Cmd: "unlock"},
	}
}

func newSelfEnv(t *testing.T, rules func(staging string) []fakeRule, mut func(*Config)) *selfEnv {
	t.Helper()
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(conf, []byte(`{"db_path":"/var/lib/cryobackup/console.db"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(stateDir, SelfBackupStagingDir)
	if rules == nil {
		rules = selfRules
	}
	e := newTenv(t, rules(staging), func(c *Config) {
		c.SelfPrefix = testSelfPrefix
		c.StateDir = stateDir
		c.ConfigFile = conf
		if mut != nil {
			mut(c)
		}
	})
	s := &selfEnv{tenv: e, sk: &fakeSelfKeys{pw: testSelfPW, keyID: testSelfKeyID}, repo: filepath.Join(t.TempDir(), "repo"),
		stateDir: stateDir, confFile: conf}
	e.w.selfKeys = s.sk
	e.w.freeDisk = func(string) (uint64, bool, error) { return 1 << 40, true, nil }
	e.w.selfBeforeBackup = func(dir string) {
		s.staged = nil
		_ = os.RemoveAll(s.repo)
		_ = os.MkdirAll(s.repo, 0o700)
		ents, _ := os.ReadDir(dir)
		for _, en := range ents {
			s.staged = append(s.staged, en.Name())
			b, err := os.ReadFile(filepath.Join(dir, en.Name()))
			if err != nil {
				t.Errorf("staged %s unreadable: %v", en.Name(), err)
				continue
			}
			_ = os.WriteFile(filepath.Join(s.repo, en.Name()), b, 0o600)
		}
		sort.Strings(s.staged)
	}
	e.w.selfAfterRestore = func(target string) {
		ents, _ := os.ReadDir(s.repo)
		for _, en := range ents {
			b, _ := os.ReadFile(filepath.Join(s.repo, en.Name()))
			_ = os.WriteFile(filepath.Join(target, en.Name()), b, 0o600)
		}
	}
	return s
}

func (s *selfEnv) step() SelfBackupReport {
	s.t.Helper()
	rep, err := s.w.SelfBackupStep(s.ctx)
	if err != nil {
		s.t.Fatalf("SelfBackupStep: %v", err)
	}
	return rep
}

func (s *selfEnv) job(name string) store.JobState {
	s.t.Helper()
	js, err := s.st.GetJobState(s.ctx, name)
	if err != nil {
		s.t.Fatalf("job %s: %v", name, err)
	}
	return js
}

func (s *selfEnv) noJob(name string) {
	s.t.Helper()
	if js, err := s.st.GetJobState(s.ctx, name); !errors.Is(err, store.ErrNotFound) {
		s.t.Fatalf("job %s exists: %+v (%v)", name, js, err)
	}
}

func (s *selfEnv) state() store.SelfBackupState {
	s.t.Helper()
	st, err := s.st.SelfBackupState(s.ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	return st
}

// absent fails when a staging or verify folder is left in the state directory.
func (s *selfEnv) absent() {
	s.t.Helper()
	for _, d := range []string{SelfBackupStagingDir, SelfBackupVerifyDir} {
		if _, err := os.Lstat(filepath.Join(s.stateDir, d)); !errors.Is(err, os.ErrNotExist) {
			s.t.Errorf("%s left behind (%v)", d, err)
		}
	}
}

func (s *selfEnv) callsOf(cmd string) []fakeCall {
	var out []fakeCall
	for _, c := range s.repoCalls() {
		if c.Args[0] == cmd {
			out = append(out, c)
		}
	}
	return out
}

func argsHave(c fakeCall, want ...string) bool {
	j := " " + strings.Join(c.Args, " ") + " "
	for _, w := range want {
		if !strings.Contains(j, " "+w+" ") {
			return false
		}
	}
	return true
}

// TestSelfBackupNotConfigured: without CRYO_MAINTAIN_SELF_PREFIX nothing runs,
// the job row says not_configured, and it is not rewritten until it is due.
func TestSelfBackupNotConfigured(t *testing.T) {
	e := newTenv(t, nil, nil)
	rep, err := e.w.SelfBackupStep(e.ctx)
	if err != nil || !rep.NotConfigured || rep.BackupRan || rep.VerifyRan {
		t.Fatalf("report = %+v, %v", rep, err)
	}
	js, err := e.st.GetJobState(e.ctx, SelfBackupJob)
	if err != nil || js.LastStatus != "error" || js.LastErrorClass != SelfClassNotConfigured || js.RunCount != 1 ||
		js.NextDueAt == nil || !js.NextDueAt.Equal(t0.Add(SelfBackupEvery)) || js.LeaseHolder != "" {
		t.Fatalf("job row = %+v, %v", js, err)
	}
	if _, err := e.st.GetJobState(e.ctx, SelfBackupVerifyJob); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("verify row written: %v", err)
	}
	if n := len(e.repoCalls()); n != 0 {
		t.Fatalf("restic ran %d times", n)
	}
	if !strings.Contains(e.logs.String(), "CRYO_MAINTAIN_SELF_PREFIX") {
		t.Errorf("the warning does not name the variable: %s", e.logs.String())
	}
	if rep, _ := e.w.SelfBackupStep(e.ctx); !rep.NothingDue {
		t.Fatalf("second step = %+v, want nothing due", rep)
	}
	e.clk.Advance(SelfBackupEvery)
	if rep, _ := e.w.SelfBackupStep(e.ctx); !rep.NotConfigured {
		t.Fatalf("step after 6 h = %+v", rep)
	}
	if js, _ := e.st.GetJobState(e.ctx, SelfBackupJob); js.RunCount != 2 {
		t.Fatalf("run count = %d, want 2", js.RunCount)
	}
}

// TestSelfBackupRunAndVerify is the clean first step: init once, the backup of
// exactly three staged files, the C4 retention, the tested restore with the
// manifest checks and one escrow unwrap, check --read-data, both job rows ok,
// both settings rows, and no folder left behind.
func TestSelfBackupRunAndVerify(t *testing.T) {
	s := newSelfEnv(t, nil, nil)
	rep := s.step()
	if !rep.BackupRan || !rep.BackupOK || rep.SnapshotID != backupOKSnapshot || !rep.VerifyRan || !rep.VerifyVerdict || !rep.VerifyOK || !rep.EscrowChecked {
		t.Fatalf("report = %+v", rep)
	}
	s.absent()
	if want := []string{SelfBackupConfigFile, SelfBackupDBFile, SelfBackupManifestFile}; strings.Join(s.staged, ",") != strings.Join(want, ",") {
		t.Fatalf("staged %v, want exactly %v", s.staged, want)
	}

	// The commands, in order, and what they carried.
	var kinds []string
	for _, c := range s.repoCalls() {
		kinds = append(kinds, c.Args[0])
		if c.Repo != "rclone:admin:test-bucket/"+testSelfPrefix || c.Password != testSelfPW || c.CacheSet {
			t.Errorf("%s: repo %q, password ok %v, cache set %v", c.Args[0], c.Repo, c.Password == testSelfPW, c.CacheSet)
		}
		if !argsHave(c, "--no-cache") {
			t.Errorf("%s without --no-cache: %v", c.Args[0], c.Args)
		}
	}
	if got := strings.Join(kinds, " "); got != "cat init backup forget prune snapshots restore check" {
		t.Fatalf("commands = %s", got)
	}
	staging := filepath.Join(s.stateDir, SelfBackupStagingDir)
	if b := s.callsOf("backup")[0]; !argsHave(b, "--host", SelfBackupHost, "--", staging) {
		t.Errorf("backup args = %v", b.Args)
	}
	if f := s.callsOf("forget")[0]; !argsHave(f, "--host", SelfBackupHost, "--group-by", "host", "--keep-daily", "30", "--keep-monthly", "12", "--keep-within", "7d") {
		t.Errorf("forget args = %v", f.Args)
	}
	if r := s.callsOf("restore")[0]; !argsHave(r, "--target", filepath.Join(s.stateDir, SelfBackupVerifyDir), backupOKSnapshot+":"+snapshotTreePath(staging)) {
		t.Errorf("restore args = %v (the newest snapshot, its staging path as subfolder)", r.Args)
	}
	if c := s.callsOf("check")[0]; !argsHave(c, "--read-data") {
		t.Errorf("check args = %v", c.Args)
	}

	// The manifest describes the staged copy.
	raw, err := os.ReadFile(filepath.Join(s.repo, SelfBackupManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var m SelfBackupManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	_, sum, _ := hashFile(filepath.Join(s.repo, SelfBackupDBFile))
	if m.Format != SelfBackupManifestFormat || m.Database.SHA256 != sum || m.MasterKeyID != testSelfKeyID || m.Bucket != "test-bucket" ||
		m.Prefix != testSelfPrefix || m.DevicePrefix != "pre" || !m.ConfigIncluded || m.SchemaVersion == 0 || m.Tables["devices"] == 0 ||
		m.CreatedAt.Location() != time.UTC || !m.CreatedAt.Equal(t0) {
		t.Fatalf("manifest = %+v", m)
	}
	if b, _ := os.ReadFile(filepath.Join(s.repo, SelfBackupConfigFile)); !bytes.Equal(b, mustRead(t, s.confFile)) {
		t.Errorf("config.json is not the console's")
	}

	// Rows.
	if js := s.job(SelfBackupJob); js.LastStatus != "ok" || !js.NextDueAt.Equal(t0.Add(SelfBackupEvery)) || js.LeaseHolder != "" {
		t.Errorf("backup row = %+v", js)
	}
	if js := s.job(SelfBackupVerifyJob); js.LastStatus != "ok" || !js.NextDueAt.Equal(t0.Add(SelfBackupVerifyEvery)) {
		t.Errorf("verify row = %+v", js)
	}
	st := s.state()
	if st.Repo == nil || st.Repo.Prefix != testSelfPrefix || st.Last == nil || st.Last.SnapshotID != backupOKSnapshot || st.Last.DBSHA256 != sum ||
		st.Verify == nil || !st.Verify.OK || st.Verify.At != t0.UnixMilli() || !st.Verify.EscrowChecked {
		t.Fatalf("settings = %+v %+v %+v", st.Repo, st.Last, st.Verify)
	}
	if f, err := s.st.SelfBackupFacts(s.ctx); err != nil || int64(f.LastAt) != pms(t0) || f.VerifyFailed || f.NotInBuild {
		t.Fatalf("facts = %+v, %v", f, err)
	}
	if len(s.sk.verified) != 1 {
		t.Fatalf("escrow unwrapped %d times", len(s.sk.verified))
	}

	// Six hours later: a backup, no cat config or init (the guard), no verify.
	s.clk.Advance(SelfBackupEvery)
	before := len(s.repoCalls())
	rep = s.step()
	if !rep.BackupOK || rep.VerifyRan {
		t.Fatalf("second report = %+v", rep)
	}
	var second []string
	for _, c := range s.repoCalls()[before:] {
		second = append(second, c.Args[0])
	}
	if got := strings.Join(second, " "); got != "backup forget prune" {
		t.Fatalf("second step commands = %s", got)
	}
	s.absent()
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func pms(t time.Time) int64 { return t.UnixMilli() }

// TestSelfBackupStorageOutage: a storage that cannot be reached fails the run
// with the network class, retried in an hour, with nothing recorded as written.
func TestSelfBackupStorageOutage(t *testing.T) {
	const down = `{"message_type":"exit_error","code":1,"message":"Fatal: unable to open repository: dial tcp: lookup s3.example.invalid: no such host"}` + "\n"
	s := newSelfEnv(t, func(string) []fakeRule {
		return []fakeRule{{Cmd: "cat", Exit: 1, Stderr: "inline:" + down}}
	}, nil)
	rep := s.step()
	if !rep.BackupRan || rep.BackupOK || rep.BackupClass != string(restic.ClassNetwork) || rep.VerifyRan {
		t.Fatalf("report = %+v", rep)
	}
	js := s.job(SelfBackupJob)
	if js.LastStatus != "error" || js.LastErrorClass != string(restic.ClassNetwork) || !js.NextDueAt.Equal(t0.Add(selfBackupRetry)) {
		t.Fatalf("job row = %+v", js)
	}
	if st := s.state(); st.Repo != nil || st.Last != nil || st.Verify != nil {
		t.Fatalf("settings written on an outage: %+v", st)
	}
	s.noJob(SelfBackupVerifyJob)
	s.absent()
	if !strings.Contains(s.logs.String(), "storage_host") {
		t.Errorf("the outage line does not name the storage host: %s", s.logs.String())
	}

	// The same outage on the backup itself, once the repository is known.
	if err := s.st.PutSelfBackupRepo(s.ctx, store.SelfBackupRepoMarker{CreatedAt: 1, Bucket: "test-bucket", Prefix: testSelfPrefix}); err != nil {
		t.Fatal(err)
	}
	s.scenario([]fakeRule{{Cmd: "backup", Exit: 1, Stderr: "inline:" + down}})
	s.clk.Advance(selfBackupRetry)
	rep = s.step()
	if rep.BackupClass != string(restic.ClassNetwork) || len(s.callsOf("cat")) != 1 {
		t.Fatalf("report = %+v (cat config calls %d: the guard must skip it)", rep, len(s.callsOf("cat")))
	}
	s.absent()
}

// TestSelfBackupDiskTooSmall: less than twice the database plus the margin
// free in the state directory: class disk, nothing staged, no backup.
func TestSelfBackupDiskTooSmall(t *testing.T) {
	s := newSelfEnv(t, nil, nil)
	s.w.freeDisk = func(string) (uint64, bool, error) { return SelfBackupDiskMargin, true, nil }
	rep := s.step()
	if rep.BackupOK || rep.BackupClass != SelfClassDisk {
		t.Fatalf("report = %+v", rep)
	}
	if n := len(s.callsOf("backup")); n != 0 {
		t.Fatalf("restic backup ran %d times", n)
	}
	if js := s.job(SelfBackupJob); js.LastErrorClass != SelfClassDisk {
		t.Fatalf("job row = %+v", js)
	}
	s.absent()
	// A free-disk figure that cannot be read is the same failure, never a pass.
	s.w.freeDisk = func(string) (uint64, bool, error) { return 0, true, errors.New("statfs failed") }
	s.clk.Advance(selfBackupRetry)
	if rep := s.step(); rep.BackupClass != SelfClassDisk {
		t.Fatalf("unreadable free disk = %+v", rep)
	}
}

// TestSelfBackupVerifyFailures: a restored copy that does not match its
// manifest, whose escrowed key does not unwrap, or a repository check that
// finds damage is a verdict: verify failed (C11 critical).
func TestSelfBackupVerifyFailures(t *testing.T) {
	cases := map[string]struct {
		mut   func(s *selfEnv)
		class string
	}{
		"manifest sha256": {func(s *selfEnv) {
			after := s.w.selfAfterRestore
			s.w.selfAfterRestore = func(target string) {
				after(target)
				p := filepath.Join(target, SelfBackupManifestFile)
				b := mustRead(s.t, p)
				var m SelfBackupManifest
				_ = json.Unmarshal(b, &m)
				m.Database.SHA256 = strings.Repeat("0", 64)
				b, _ = json.Marshal(m)
				_ = os.WriteFile(p, b, 0o600)
			}
		}, SelfClassManifest},
		"row counts": {func(s *selfEnv) {
			after := s.w.selfAfterRestore
			s.w.selfAfterRestore = func(target string) {
				after(target)
				p := filepath.Join(target, SelfBackupManifestFile)
				var m SelfBackupManifest
				_ = json.Unmarshal(mustRead(s.t, p), &m)
				m.Tables["devices"]++
				b, _ := json.Marshal(m)
				_ = os.WriteFile(p, b, 0o600)
			}
		}, SelfClassManifest},
		"database damaged": {func(s *selfEnv) {
			after := s.w.selfAfterRestore
			s.w.selfAfterRestore = func(target string) {
				after(target)
				_ = os.WriteFile(filepath.Join(target, SelfBackupDBFile), []byte("garbage"), 0o600)
			}
		}, SelfClassManifest},
		"another master key": {func(s *selfEnv) {
			before := s.w.selfBeforeBackup
			s.w.selfBeforeBackup = func(dir string) { before(dir); s.sk.keyID = "fedcba9876543210" }
		}, SelfClassMasterKey},
		"escrow does not unwrap": {func(s *selfEnv) { s.sk.verifyErr = errors.New("cannot decrypt") }, SelfClassEscrow},
		"check finds damage": {func(s *selfEnv) {
			rules := selfRules(filepath.Join(s.stateDir, SelfBackupStagingDir))
			for i := range rules {
				if rules[i].Cmd == "check" {
					rules[i] = fakeRule{Cmd: "check", Exit: 1, Stdout: "check_damaged.stdout.jsonl", Stderr: "check_damaged.stderr.jsonl"}
				}
			}
			s.scenario(rules)
		}, SelfClassCheck},
		"no snapshot": {func(s *selfEnv) {
			rules := selfRules(filepath.Join(s.stateDir, SelfBackupStagingDir))
			for i := range rules {
				if rules[i].Cmd == "snapshots" {
					rules[i] = fakeRule{Cmd: "snapshots", Stdout: "inline:[]"}
				}
			}
			s.scenario(rules)
		}, SelfClassNoSnapshot},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := newSelfEnv(t, nil, nil)
			tc.mut(s)
			rep := s.step()
			if !rep.BackupOK || !rep.VerifyRan || !rep.VerifyVerdict || rep.VerifyOK || rep.VerifyClass != tc.class {
				t.Fatalf("report = %+v, want verify failed with %s", rep, tc.class)
			}
			if js := s.job(SelfBackupVerifyJob); js.LastStatus != "error" || js.LastErrorClass != tc.class || !js.NextDueAt.Equal(t0.Add(selfVerifyRetry)) {
				t.Fatalf("verify row = %+v", js)
			}
			st := s.state()
			if st.Verify == nil || st.Verify.OK || st.Verify.Class != tc.class || st.Verify.At == 0 {
				t.Fatalf("verify setting = %+v", st.Verify)
			}
			if f, _ := s.st.SelfBackupFacts(s.ctx); !f.VerifyFailed {
				t.Fatalf("facts = %+v, want VerifyFailed", f)
			}
			s.absent()
		})
	}
}

// TestSelfBackupVerifyInconclusiveKeepsVerdict: a verification that cannot
// reach the storage says nothing about the backup: the job row fails with the
// class, the previous verdict stays.
func TestSelfBackupVerifyInconclusiveKeepsVerdict(t *testing.T) {
	s := newSelfEnv(t, nil, nil)
	if rep := s.step(); !rep.VerifyOK {
		t.Fatalf("first step = %+v", rep)
	}
	rules := selfRules(filepath.Join(s.stateDir, SelfBackupStagingDir))
	for i := range rules {
		if rules[i].Cmd == "restore" {
			rules[i] = fakeRule{Cmd: "restore", Exit: 1, Stderr: `inline:{"message_type":"exit_error","code":1,"message":"Fatal: read tcp: connection reset by peer"}` + "\n"}
		}
	}
	s.scenario(rules)
	s.clk.Advance(SelfBackupVerifyEvery)
	rep := s.step()
	if !rep.VerifyRan || rep.VerifyVerdict || rep.VerifyClass != string(restic.ClassNetwork) {
		t.Fatalf("report = %+v", rep)
	}
	st := s.state()
	if st.Verify == nil || !st.Verify.OK || st.Verify.At != pms(t0) || st.Verify.AttemptClass != string(restic.ClassNetwork) || st.Verify.AttemptAt != pms(s.clk.Now()) {
		t.Fatalf("verify setting = %+v", st.Verify)
	}
	if js := s.job(SelfBackupVerifyJob); js.LastStatus != "error" || js.LastErrorClass != string(restic.ClassNetwork) {
		t.Fatalf("verify row = %+v", js)
	}
	s.absent()
}

// TestSelfBackupStagingSweptAfterKill: a worker killed mid-run leaves its
// staging and verify folders; the next step sweeps them before it stages, so
// the snapshot holds only the three files and nothing is left afterwards. A run
// stopped mid-backup removes its staging folder on the way out.
func TestSelfBackupStagingSweptAfterKill(t *testing.T) {
	s := newSelfEnv(t, nil, nil)
	for _, d := range []string{SelfBackupStagingDir, SelfBackupVerifyDir} {
		p := filepath.Join(s.stateDir, d)
		if err := os.MkdirAll(filepath.Join(p, "sub"), 0o700); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"console.db", "leftover.tmp", filepath.Join("sub", "x")} {
			if err := os.WriteFile(filepath.Join(p, f), []byte("from the killed run"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	rep := s.step()
	if !rep.BackupOK || !rep.VerifyOK {
		t.Fatalf("report = %+v", rep)
	}
	if want := []string{SelfBackupConfigFile, SelfBackupDBFile, SelfBackupManifestFile}; strings.Join(s.staged, ",") != strings.Join(want, ",") {
		t.Fatalf("staged %v after a killed run, want exactly %v", s.staged, want)
	}
	s.absent()

	// Stopped mid-backup: the backup sleeps, the context ends.
	rules := selfRules(filepath.Join(s.stateDir, SelfBackupStagingDir))
	for i := range rules {
		if rules[i].Cmd == "backup" {
			rules[i].SleepMS = 20000
		}
	}
	s.scenario(rules)
	s.clk.Advance(SelfBackupEvery)
	ctx, cancel := context.WithCancel(s.ctx)
	staged := make(chan struct{})
	before := s.w.selfBeforeBackup
	s.w.selfBeforeBackup = func(dir string) {
		before(dir)
		close(staged)
	}
	go func() {
		<-staged
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	rep, err := s.w.SelfBackupStep(ctx)
	if err != nil || rep.BackupOK || rep.BackupClass != SelfClassInterrupted {
		t.Fatalf("stopped step = %+v, %v", rep, err)
	}
	s.absent()
}

// TestSelfBackupOnlyLeaseHolderWrites: with the db-self-backup lease held by
// another process the step runs nothing and writes nothing; a lease taken over
// mid-run stops the run and leaves the job rows as they were.
func TestSelfBackupOnlyLeaseHolderWrites(t *testing.T) {
	s := newSelfEnv(t, nil, nil)
	if ok, err := s.st.AcquireJobLease(s.ctx, SelfBackupJob, "another-maintain", time.Hour); err != nil || !ok {
		t.Fatalf("lease: %v %v", ok, err)
	}
	rep := s.step()
	if !rep.Skipped || rep.BackupRan {
		t.Fatalf("report = %+v", rep)
	}
	js := s.job(SelfBackupJob)
	if js.LastStatus != "" || js.LastFinishedAt != nil || js.RunCount != 0 || js.LeaseHolder != "another-maintain" {
		t.Fatalf("a non-holder wrote the job row: %+v", js)
	}
	s.noJob(SelfBackupVerifyJob)
	if st := s.state(); st.Repo != nil || st.Last != nil || st.Verify != nil {
		t.Fatalf("a non-holder wrote settings: %+v", st)
	}
	if n := len(s.repoCalls()); n != 0 {
		t.Fatalf("a non-holder ran restic %d times", n)
	}

	// Not configured is no different: only the holder writes not_configured.
	e := newTenv(t, nil, nil)
	if ok, _ := e.st.AcquireJobLease(e.ctx, SelfBackupJob, "another-maintain", time.Hour); !ok {
		t.Fatal("lease")
	}
	if rep, _ := e.w.SelfBackupStep(e.ctx); !rep.Skipped {
		t.Fatalf("not-configured non-holder = %+v", rep)
	}
	if js, _ := e.st.GetJobState(e.ctx, SelfBackupJob); js.LastStatus != "" || js.RunCount != 0 {
		t.Fatalf("not-configured non-holder wrote: %+v", js)
	}

	// Taken over mid-run.
	s2 := newSelfEnv(t, func(staging string) []fakeRule {
		r := selfRules(staging)
		for i := range r {
			if r[i].Cmd == "backup" {
				r[i].SleepMS = 20000
			}
		}
		return r
	}, nil)
	s2.w.selfLeaseRenew = 50 * time.Millisecond
	before := s2.w.selfBeforeBackup
	s2.w.selfBeforeBackup = func(dir string) {
		before(dir)
		if err := s2.st.Write(s2.ctx, func(tx *store.Tx) error {
			_, err := tx.Exec(`UPDATE job_state SET lease_holder = 'thief', lease_until = ? WHERE name = ?`,
				s2.clk.Now().Add(time.Hour).UnixMilli(), SelfBackupJob)
			return err
		}); err != nil {
			t.Error(err)
		}
	}
	start := time.Now()
	rep = s2.step()
	if !rep.LeaseLost || rep.BackupOK {
		t.Fatalf("taken-over report = %+v", rep)
	}
	if time.Since(start) > 15*time.Second {
		t.Fatalf("the run was not stopped when the lease was lost (%v)", time.Since(start))
	}
	js = s2.job(SelfBackupJob)
	if js.LastStatus != "" || js.RunCount != 0 || js.LeaseHolder != "thief" {
		t.Fatalf("job row after a lost lease = %+v", js)
	}
	if st := s2.state(); st.Last != nil || st.Verify != nil {
		t.Fatalf("settings after a lost lease: %+v %+v", st.Last, st.Verify)
	}
	s2.absent()
}

// TestSelfBackupOneAtATime: a second step in the same process while one runs
// is skipped.
func TestSelfBackupOneAtATime(t *testing.T) {
	s := newSelfEnv(t, nil, nil)
	s.w.selfRunning.Store(true)
	if rep := s.step(); !rep.Skipped || rep.BackupRan {
		t.Fatalf("report = %+v", rep)
	}
	s.w.selfRunning.Store(false)
	if rep := s.step(); !rep.BackupOK {
		t.Fatalf("report = %+v", rep)
	}
}

// TestSelfBackupNoKey: without the self-backup key the run fails loudly.
func TestSelfBackupNoKey(t *testing.T) {
	s := newSelfEnv(t, nil, nil)
	s.w.selfKeys = nil
	if rep := s.step(); rep.BackupClass != SelfClassNoKey {
		t.Fatalf("report = %+v", rep)
	}
	if n := len(s.repoCalls()); n != 0 {
		t.Fatalf("restic ran %d times without a key", n)
	}
}

// TestCheckSelfPrefix is the prefix rule (ticket P22-C, spike P22-S1 item 8.2).
func TestCheckSelfPrefix(t *testing.T) {
	cases := []struct {
		device, self string
		want         error
	}{
		{"cb", "cb", errSelfPrefixGateway},
		{"cb", "cb/x", errSelfPrefixGateway},
		{"cb", "cb-console", errSelfPrefixGateway},
		{"cb", "cbx", errSelfPrefixGateway},
		{"/cb/", "/cb-console/", errSelfPrefixGateway},
		{"tenant/cb", "tenant/cb-console", errSelfPrefixGateway},
		{"", "console", errSelfPrefixRoot},
		{"/", "anything/at/all", errSelfPrefixRoot},
		{"a/b", "a", errSelfPrefixContains},
		{"cb", "x/../y", errSelfPrefixForm},
		{"cb", "has space", errSelfPrefixForm},
		{"cb", ".", errSelfPrefixForm},
		{"cb", "a//b", errSelfPrefixForm},
		{"cb", "console-cb", nil},
		{"cb", "c", nil},
		{"cb", "backups/console", nil},
		{"tenant/cb", "tenant/console", nil},
		{"a/b", "a-console", nil},
	}
	for _, tc := range cases {
		err := checkSelfPrefix(tc.device, tc.self)
		switch {
		case tc.want == nil && err != nil:
			t.Errorf("device %q self %q refused: %v", tc.device, tc.self, err)
		case tc.want != nil && (!errors.Is(err, tc.want) || !errors.Is(err, errConfig)):
			t.Errorf("device %q self %q = %v, want %v", tc.device, tc.self, err, tc.want)
		}
	}
	for _, e := range []error{errSelfPrefixGateway, errSelfPrefixRoot} {
		if !strings.Contains(configText(e), "IAM") {
			t.Errorf("%q does not name the IAM reason", configText(e))
		}
	}
	if !strings.Contains(configText(errSelfPrefixRoot), "device repositories must live under a prefix for self-backup to be safe") {
		t.Errorf("root refusal = %q", configText(errSelfPrefixRoot))
	}
	// New refuses it; an unset self prefix is fine.
	exe, sum := fakeResticPath(t)
	st, err := store.OpenMemory(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = New(Config{ResticProgram: exe, ResticSHA256: sum, Bucket: "b", Prefix: "cb", SelfPrefix: "cb-console",
		ScratchDir: filepath.Join(t.TempDir(), "scratch")}, st, &fakeKeys{pw: "x"}, nil)
	if !errors.Is(err, errSelfPrefixGateway) {
		t.Fatalf("New = %v, want the gateway refusal", err)
	}
}

// TestRunRefusesSelfPrefix: `maintain` exits with the usage code and the IAM
// sentence for a self prefix the gateway can reach.
func TestRunRefusesSelfPrefix(t *testing.T) {
	dir := t.TempDir()
	exe, sum := fakeResticPath(t)
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	sc, _ := json.Marshal(fakeScenario{Rules: []fakeRule{{Cmd: "version", Stdout: `inline:{"message_type":"version","version":"0.19.1"}`}}})
	if err := os.WriteFile(filepath.Join(state, "fake.json"), sc, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.json")
	body := `{"db_path":` + jsonStr(filepath.Join(dir, "c.db")) + `,"master_key_path":` + jsonStr(filepath.Join(dir, "master.key")) + `}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRYO_MAINTAIN_RESTIC", exe)
	t.Setenv("CRYO_MAINTAIN_RESTIC_SHA256", sum)
	t.Setenv("RCLONE_CONFIG", filepath.Join(state, "rclone.conf"))
	t.Setenv("CRYO_MAINTAIN_SCRATCH_DIR", filepath.Join(dir, "scratch"))
	t.Setenv("CRYO_MAINTAIN_BUCKET", "bucket")
	t.Setenv("CRYO_MAINTAIN_PREFIX", "cb")
	t.Setenv("CRYO_MAINTAIN_SELF_PREFIX", "cb-console")
	keys := func(string, *store.Store) (KeyUnwrapper, error) { return &fakeKeys{pw: "x"}, nil }
	var out, errb bytes.Buffer
	if code := Run([]string{"--config", cfg, "--once"}, &out, &errb, keys); code != ExitUsage {
		t.Fatalf("exit %d, want %d (stderr %q)", code, ExitUsage, errb.String())
	}
	if !strings.Contains(errb.String(), "IAM") || !strings.Contains(errb.String(), "CRYO_MAINTAIN_SELF_PREFIX") {
		t.Fatalf("stderr = %q", errb.String())
	}
	if strings.Contains(errb.String(), dir) {
		t.Fatalf("stderr names a path: %q", errb.String())
	}
}

func TestSnapshotTreePath(t *testing.T) {
	for in, want := range map[string]string{
		`C:\dev\state\self-backup-staging`:     "/C/dev/state/self-backup-staging",
		"/var/lib/cryobackup/self-backup-stag": "/var/lib/cryobackup/self-backup-stag",
		`D:\`:                                  "/D/",
	} {
		if got := snapshotTreePath(in); got != want {
			t.Errorf("snapshotTreePath(%q) = %q, want %q", in, got, want)
		}
	}
}
