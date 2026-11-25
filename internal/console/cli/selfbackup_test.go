package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/maintain"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// `cryo-console self-backup` (P22-F): argument checks and exit codes. The
// restore engine has its own tests (maintain/selfbackup_restore_test.go) and
// the whole path runs live in selfbackup_live_test.go.

// clearSelfEnv empties the variables list and restore take their defaults from.
func clearSelfEnv(t *testing.T) {
	t.Helper()
	for _, v := range []string{"RCLONE_CONFIG", "CRYO_MAINTAIN_REMOTE", "CRYO_MAINTAIN_BUCKET", "CRYO_MAINTAIN_SELF_PREFIX",
		"CRYO_MASTER_KEY_PATH", "CRYO_MAINTAIN_RESTIC", "CRYO_MAINTAIN_RESTIC_SHA256", "CRYO_MAINTAIN_RCLONE", "CRYO_CONFIG"} {
		t.Setenv(v, "")
	}
}

func TestSelfBackupUsage(t *testing.T) {
	clearSelfEnv(t)
	if code, _, errb := run(t, "", "self-backup"); code != ExitUsage || !strings.Contains(errb, "usage: cryo-console self-backup") {
		t.Fatalf("no command: %d %q", code, errb)
	}
	if code, out, _ := run(t, "", "self-backup", "help"); code != ExitOK || !strings.Contains(out, "restore  restore a self-backup") {
		t.Fatalf("help: %d %q", code, out)
	}
	if code, _, errb := run(t, "", "self-backup", "frob"); code != ExitUsage || !strings.Contains(errb, "unknown command") {
		t.Fatalf("unknown: %d %q", code, errb)
	}
	if code, out, _ := run(t, "", "help"); code != ExitOK || !strings.Contains(out, "self-backup") {
		t.Fatalf("the command summary does not list self-backup: %q", out)
	}
	for _, cmd := range []string{"run", "verify", "list", "restore"} {
		if code, _, _ := run(t, "", "self-backup", cmd, "extra-arg"); code != ExitUsage {
			t.Errorf("%s with a stray argument: %d", cmd, code)
		}
		if code, _, _ := run(t, "", "self-backup", cmd, "--no-such-flag"); code != ExitUsage {
			t.Errorf("%s with an unknown flag: %d", cmd, code)
		}
	}
}

// list and restore: every required option is named when missing; a key file
// that is not a master key, a malformed --snapshot and a missing --out are
// usage errors; none of them prints a path or runs restic.
func TestSelfBackupRepoFlags(t *testing.T) {
	clearSelfEnv(t)
	dir := t.TempDir()
	conf := filepath.Join(dir, "rclone.conf")
	badKey := filepath.Join(dir, "not-a-key")
	for _, f := range []string{conf, badKey} {
		if err := os.WriteFile(f, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	full := []string{"--rclone-config", conf, "--bucket", "b", "--prefix", "console-self", "--master-key", badKey, "--restic", filepath.Join(dir, "no-restic")}
	drop := func(flag string) []string {
		var out []string
		for i := 0; i < len(full); i += 2 {
			if full[i] != flag {
				out = append(out, full[i], full[i+1])
			}
		}
		return out
	}
	for _, cmd := range []string{"list", "restore"} {
		extra := []string{}
		if cmd == "restore" {
			extra = []string{"--out", filepath.Join(dir, "out")}
		}
		for _, flag := range []string{"--rclone-config", "--bucket", "--prefix", "--master-key"} {
			code, _, errb := run(t, "", append(append([]string{"self-backup", cmd}, drop(flag)...), extra...)...)
			if code != ExitUsage || !strings.Contains(errb, flag+" is required") {
				t.Errorf("%s without %s: %d %q", cmd, flag, code, errb)
			}
		}
		code, _, errb := run(t, "", append(append([]string{"self-backup", cmd}, full...), extra...)...)
		if code != ExitUsage || !strings.Contains(errb, "--master-key file could not be read or is not a console master key") || strings.Contains(errb, dir) {
			t.Errorf("%s with a bad key file: %d %q", cmd, code, errb)
		}
	}
	if code, _, errb := run(t, "", append([]string{"self-backup", "restore"}, full...)...); code != ExitUsage || !strings.Contains(errb, "--out is required") {
		t.Errorf("restore without --out: %d %q", code, errb)
	}
	if code, _, errb := run(t, "", append([]string{"self-backup", "restore", "--out", filepath.Join(dir, "o"), "--snapshot", "XYZ"}, full...)...); code != ExitUsage || !strings.Contains(errb, "--snapshot must be") {
		t.Errorf("restore with a bad --snapshot: %d %q", code, errb)
	}
	if _, err := os.Lstat(filepath.Join(dir, "out")); !os.IsNotExist(err) {
		t.Fatalf("a refused restore created --out: %v", err)
	}
}

// A real master key, but a non-empty --out: refused (exit 2) before restic runs;
// the operator's file stays.
func TestSelfBackupRestoreRefusesNonEmptyOut(t *testing.T) {
	clearSelfEnv(t)
	dir := t.TempDir()
	if code, out, errb := run(t, "", initArgs(dir, "--master-key-copy-confirmed")...); code != 0 {
		t.Fatalf("init: %d\n%s\n%s", code, out, errb)
	}
	cfg, err := config.Read(filepath.Join(dir, "config.json"), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "rclone.conf")
	if err := os.WriteFile(conf, []byte("[admin]\ntype = alias\nremote = /nowhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "keep"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errb := run(t, "", "self-backup", "restore", "--rclone-config", conf, "--bucket", "b", "--prefix", "console-self",
		"--master-key", cfg.MasterKeyPath, "--restic", filepath.Join(dir, "no-restic"), "--out", out)
	if code != ExitUsage || !strings.Contains(errb, "class out_not_empty") {
		t.Fatalf("non-empty --out: %d %q", code, errb)
	}
	if b, err := os.ReadFile(filepath.Join(out, "keep")); err != nil || string(b) != "mine" {
		t.Fatalf("the operator's file: %q %v", b, err)
	}
	// The binary check comes next: a restic that is not there is a usage error too.
	code, _, errb = run(t, "", "self-backup", "restore", "--rclone-config", conf, "--bucket", "b", "--prefix", "console-self",
		"--master-key", cfg.MasterKeyPath, "--restic", filepath.Join(dir, "no-restic"), "--out", filepath.Join(dir, "fresh"))
	if code != ExitUsage || !strings.Contains(errb, "class restic_binary") {
		t.Fatalf("missing restic: %d %q", code, errb)
	}
}

// redirectTo makes link a directory that redirects to target: a symlink, or on Windows, where creating
// one needs a privilege that a developer machine often lacks, a junction (Go reports both as
// ModeSymlink). It skips the test, saying why, when neither can be made.
func redirectTo(t *testing.T, target, link string) {
	t.Helper()
	err := os.Symlink(target, link)
	if err == nil {
		return
	}
	if runtime.GOOS == "windows" {
		if out, jerr := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); jerr == nil {
			return
		} else {
			t.Skipf("cannot make a symlink (%v) or a junction (%v: %s)", err, jerr, strings.TrimSpace(string(out)))
		}
	}
	t.Skipf("cannot make a symlink: %v", err)
}

// --out that is a symlink (or a junction) to an empty folder is refused, exit 2 with class out_dir,
// before restic runs: a restore never follows a link out of the folder the operator named, and the
// folder behind the link stays empty.
func TestSelfBackupRestoreRefusesLinkedOut(t *testing.T) {
	clearSelfEnv(t)
	dir := t.TempDir()
	if code, out, errb := run(t, "", initArgs(dir, "--master-key-copy-confirmed")...); code != 0 {
		t.Fatalf("init: %d\n%s\n%s", code, out, errb)
	}
	cfg, err := config.Read(filepath.Join(dir, "config.json"), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "rclone.conf")
	if err := os.WriteFile(conf, []byte("[admin]\ntype = alias\nremote = /nowhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	behind := filepath.Join(dir, "behind")
	if err := os.Mkdir(behind, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	redirectTo(t, behind, link)
	code, _, errb := run(t, "", "self-backup", "restore", "--rclone-config", conf, "--bucket", "b", "--prefix", "console-self",
		"--master-key", cfg.MasterKeyPath, "--restic", filepath.Join(dir, "no-restic"), "--out", link)
	if code != ExitUsage || !strings.Contains(errb, "class out_dir") {
		t.Fatalf("--out through a link: %d %q", code, errb)
	}
	if entries, err := os.ReadDir(behind); err != nil || len(entries) != 0 {
		t.Fatalf("the folder behind the link: %v %v", entries, err)
	}
}

func TestSelfRestoreFailureExitCodes(t *testing.T) {
	var errb strings.Builder
	sio := IO{Stdout: io.Discard, Stderr: &errb}
	if code := selfRestoreFailure("restore", &maintain.SelfRestoreError{Class: "wrong_password"}, sio); code != ExitFailed ||
		!strings.Contains(errb.String(), "the master key does not open this repository") || !strings.Contains(errb.String(), "nothing was left in --out") {
		t.Fatalf("wrong key: %d %q", code, errb.String())
	}
	errb.Reset()
	if code := selfRestoreFailure("list", &maintain.SelfRestoreError{Class: maintain.SelfClassConfig, Usage: true}, sio); code != ExitUsage {
		t.Fatalf("usage: %d %q", code, errb.String())
	}
	errb.Reset()
	if code := selfRestoreFailure("restore", context.Canceled, sio); code != ExitFailed {
		t.Fatalf("other error: %d", code)
	}
}

// The self-backup line of `maintain --once`, judged for run and for verify.
func TestSelfStepOutcome(t *testing.T) {
	pass := "maintain: considered=0 sampled=0 preflight=0 deferred=0 unlocked=0 failed=0 m5_due=0 skipped=false\n"
	for _, tc := range []struct {
		what, line string
		code       int
		want       string
	}{
		{"run", "backup=ok verify=not_due", ExitOK, "ok: a new snapshot was written"},
		{"run", "backup=ok verify=failed:manifest", ExitOK, "ok"},
		{"run", "backup=error:network verify=not_due", ExitFailed, "FAILED, class network"},
		{"run", "backup=not_due verify=ok", ExitFailed, "did not run"},
		{"run", "backup=ok verify=not_due lease_lost", ExitFailed, "took over the self-backup's lease"},
		{"run", "not_configured", ExitUsage, "class not_configured"},
		{"run", "no_slot", ExitUsage, "class no_slot"},
		{"run", "skipped", ExitFailed, "another process holds the self-backup's lease"},
		{"run", "nothing_due", ExitFailed, "nothing was due"},
		{"run", "", ExitFailed, "stopped before the self-backup step"},
		{"verify", "backup=not_due verify=ok", ExitOK, "verdict ok"},
		{"verify", "backup=error:disk verify=ok", ExitOK, "verdict ok"},
		{"verify", "backup=not_due verify=failed:manifest", ExitFailed, "verdict FAILED, class manifest"},
		{"verify", "backup=not_due verify=inconclusive:network", ExitFailed, "no verdict, class network"},
		{"verify", "backup=not_due verify=not_due", ExitFailed, "no verification ran"},
		{"verify", "no_slot", ExitUsage, "class no_slot"},
	} {
		out := pass
		if tc.line != "" {
			out += "maintain: self-backup " + tc.line + "\n"
		}
		msg, code := selfStepOutcome(tc.what, out)
		if code != tc.code || !strings.Contains(msg, tc.want) || !strings.HasPrefix(msg, "self-backup "+tc.what+": ") {
			t.Errorf("%s %q: %d %q, want %d %q", tc.what, tc.line, code, msg, tc.code, tc.want)
		}
	}
}

// run and verify make their job due without touching its history, then run
// exactly `maintain --config <file> --once`; the worker's usage exit is passed on.
func TestSelfBackupStepMakesDueAndRunsOnce(t *testing.T) {
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
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: cfg.DBPath})
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(5 * time.Hour)
	now := time.Now()
	for _, job := range []string{maintain.SelfBackupJob, maintain.SelfBackupVerifyJob} {
		if err := st.RecordJobRun(ctx, job, store.JobRun{Status: "ok", StartedAt: now, FinishedAt: now, NextDueAt: &later}); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	var gotArgs []string
	line := ""
	rc := maintain.ExitOK
	old := selfBackupMaintain
	selfBackupMaintain = func(args []string, stdout, _ io.Writer) int {
		gotArgs = args
		_, _ = io.WriteString(stdout, "maintain: self-backup "+line+"\n")
		return rc
	}
	t.Cleanup(func() { selfBackupMaintain = old })

	jobRow := func(name string) store.JobState {
		st, err := store.Open(ctx, store.Options{Path: cfg.DBPath})
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		js, err := st.GetJobState(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return js
	}

	line = "backup=ok verify=not_due"
	code, out, errb := run(t, "", "self-backup", "run", "--config", cfgPath)
	if code != ExitOK || !strings.Contains(out, "self-backup run: ok") || strings.Join(gotArgs, " ") != "--config "+cfgPath+" --once" {
		t.Fatalf("run: %d %q %q, maintain args %v", code, out, errb, gotArgs)
	}
	if js := jobRow(maintain.SelfBackupJob); js.NextDueAt != nil || js.LastStatus != "ok" || js.RunCount != 1 {
		t.Fatalf("backup job row after run: %+v", js)
	}
	if js := jobRow(maintain.SelfBackupVerifyJob); js.NextDueAt == nil {
		t.Fatalf("run made the verify due too: %+v", js)
	}

	line = "backup=not_due verify=failed:manifest"
	t.Setenv("CRYO_CONFIG", cfgPath) // the default config path
	code, _, errb = run(t, "", "self-backup", "verify")
	if code != ExitFailed || !strings.Contains(errb, "verdict FAILED, class manifest") {
		t.Fatalf("verify: %d %q", code, errb)
	}
	if js := jobRow(maintain.SelfBackupVerifyJob); js.NextDueAt != nil || js.RunCount != 1 {
		t.Fatalf("verify job row after verify: %+v", js)
	}

	rc = maintain.ExitUsage
	if code, _, _ := run(t, "", "self-backup", "run"); code != ExitUsage {
		t.Fatalf("worker usage exit not passed on: %d", code)
	}
	// A config file that is not there is a usage error before the worker runs.
	gotArgs = nil
	if code, _, _ := run(t, "", "self-backup", "run", "--config", filepath.Join(dir, "absent.json")); code != ExitUsage || gotArgs != nil {
		t.Fatalf("absent config: %d, maintain ran %v", code, gotArgs)
	}
}

// healthySelfBackup writes what a working maintenance worker's self-backup
// leaves at at (ADR 0029): a snapshot, a passing verdict and both job rows ok.
// Since P22-F the console judges the self-backup, so a healthy console in a
// test needs these rows as it needs the scheduler's (r3MaintainPass).
func healthySelfBackup(t *testing.T, st *store.Store, at time.Time) {
	t.Helper()
	ctx := context.Background()
	ms := at.UnixMilli()
	if err := st.RecordSelfBackup(ctx, store.SelfBackupLast{At: ms, FirstAt: ms, SnapshotID: "0123abcd"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordSelfBackupVerify(ctx, store.SelfBackupVerify{At: ms, OK: true, SnapshotID: "0123abcd", AttemptAt: ms}); err != nil {
		t.Fatal(err)
	}
	for _, job := range []string{maintain.SelfBackupJob, maintain.SelfBackupVerifyJob} {
		if err := st.RecordJobRun(ctx, job, store.JobRun{Status: "ok", StartedAt: at, FinishedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
}
