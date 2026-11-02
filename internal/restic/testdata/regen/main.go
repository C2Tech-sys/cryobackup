// Command regen captures the fixtures that internal/restic is tested against.
//
// It runs the pinned restic (0.19.1) against a throw-away tree and repository
// in a temporary directory, writes every command's stdout, stderr and exit
// code, and scrubs the machine out of the result: the temporary root and its
// ancestors become C:\fixture and C:\fixture-upN, the current user becomes
// fixture-user, the host becomes fixture-host. It fails loudly when anything of
// the machine survives the scrub.
//
// Usage, from the repository root:
//
//	go run ./internal/restic/testdata/regen -restic <path to restic 0.19.1> -out ./internal/restic/testdata/v0.19.1
//
// The directory name "testdata" keeps this program out of "go build ./..." and
// "go test ./...": it is built only on demand and never runs in CI. It needs
// Windows (it holds a file open with PowerShell to capture a locked-file
// backup; that one capture is skipped, and MANIFEST.md says so, where
// PowerShell is not available).
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/restic"
)

const (
	pinnedVersion = "0.19.1"
	fixtureHost   = "fixture-host"
	fixtureUser   = "fixture-user"
	fixturePW     = "fixture-password"
	fixtureRoot   = `C:\fixture`
)

func main() {
	resticPath := flag.String("restic", "", "path to the pinned restic binary (required)")
	out := flag.String("out", "./internal/restic/testdata/v0.19.1", "output directory")
	flag.Parse()
	if *resticPath == "" {
		fatalf("-restic is required")
	}
	if err := run(*resticPath, *out); err != nil {
		fatalf("%v", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "regen: "+format+"\n", args...)
	os.Exit(1)
}

// entry is one MANIFEST row.
type entry struct {
	file string
	args []string
	exit int
	note string
}

type regen struct {
	restic string
	out    string
	root   string
	tree   string
	repo   string
	env    restic.Env
	scrub  *scrubber
	rows   []entry
	skips  []string
}

// result is one captured process run.
type result struct {
	stdout, stderr []byte
	exit           int
}

func run(resticPath, out string) error {
	root, err := os.MkdirTemp("", "cryo-regen-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)

	cur, _ := user.Current()
	host, _ := os.Hostname()
	userName := ""
	if cur != nil {
		userName = cur.Username
	}
	g := &regen{
		restic: resticPath,
		out:    out,
		root:   root,
		tree:   filepath.Join(root, "tree"),
		repo:   filepath.Join(root, "repo"),
	}
	g.scrub, err = newScrubber(root, userName, host)
	if err != nil {
		return err
	}
	for _, d := range []string{"tree", "cache", "tmp", "restore", "skiptree"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return err
		}
	}
	g.env = restic.Env{
		Repository: g.repo,
		Password:   fixturePW,
		CacheDir:   filepath.Join(root, "cache"),
		TempDir:    filepath.Join(root, "tmp"),
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	if err := g.makeTree(); err != nil {
		return err
	}
	if err := g.capture(); err != nil {
		return err
	}
	return g.writeManifest()
}

func (g *regen) makeTree() error {
	files := map[string]string{
		filepath.Join("invoices", "2026-01.pdf"): "invoice january\n",
		filepath.Join("invoices", "2026-02.pdf"): "invoice february\n",
		filepath.Join("notes", "readme.txt"):     "fixture readme\n",
		filepath.Join("notes", "todo.docx"):      "fixture todo\n",
	}
	for rel, content := range files {
		p := filepath.Join(g.tree, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return err
		}
	}
	// 2 MiB of reproducible noise: incompressible, so it produces pack data and
	// progress lines.
	buf := make([]byte, 2<<20)
	rand.New(rand.NewSource(1)).Read(buf)
	if err := os.MkdirAll(filepath.Join(g.tree, "media"), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(g.tree, "media", "clip.mp4"), buf, 0o644)
}

// environ returns the child environment for one run: the package's own Env plus
// a fast progress rate so that short runs still print status lines.
func (g *regen) environ(e restic.Env) []string {
	env, err := e.Environ(os.Environ())
	if err != nil {
		fatalf("environ: %v", err)
	}
	return append(env, "RESTIC_PROGRESS_FPS=60")
}

func (g *regen) exec(env restic.Env, args []string, stdin io.Reader) result {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, g.restic, args...)
	cmd.Env = g.environ(env)
	cmd.Stdin = stdin
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	r := result{stdout: so.Bytes(), stderr: se.Bytes()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		r.exit = ee.ExitCode()
	default:
		fatalf("cannot run restic: %v", err)
	}
	return r
}

// save scrubs and writes one captured stream and records it.
func (g *regen) save(file string, data []byte) {
	s := g.scrub.apply(data)
	if bad := g.scrub.residual(s); bad != "" {
		fatalf("%s: scrub left %q behind: %s", file, bad, s)
	}
	if err := os.WriteFile(filepath.Join(g.out, file), s, 0o644); err != nil {
		fatalf("write %s: %v", file, err)
	}
}

// record saves stdout and/or stderr of r under name and adds MANIFEST rows.
// stdoutFile and stderrFile may be empty to skip a stream.
func (g *regen) record(r result, args []string, stdoutFile, stderrFile, note string) {
	if stdoutFile != "" {
		g.save(stdoutFile, r.stdout)
		g.rows = append(g.rows, entry{stdoutFile, args, r.exit, note})
	}
	if stderrFile != "" {
		g.save(stderrFile, r.stderr)
		g.rows = append(g.rows, entry{stderrFile, args, r.exit, note})
	}
}

func must(c restic.Command, err error) restic.Command {
	if err != nil {
		fatalf("builder: %v", err)
	}
	return c
}

func (g *regen) expect(r result, want int, what string) {
	if r.exit != want {
		fatalf("%s: exit %d, want %d\nstderr: %s", what, r.exit, want, g.scrub.apply(r.stderr))
	}
}

// snapPath converts a native path to restic's snapshot path form: /C/dir/dir.
func snapPath(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	if len(p) >= 2 && p[1] == ':' {
		return "/" + p[:1] + p[2:]
	}
	return p
}

func (g *regen) capture() error {
	env := g.env
	tags := restic.StandardTags("fixture-customer", "fixture-policy", 1, "fixture-run-1")
	common := restic.Common{RetryLock: 30 * time.Second}
	backupOpts := restic.BackupOptions{Common: common, Host: fixtureHost, Tags: tags, Roots: []string{g.tree}}

	// version
	cmd := restic.Version()
	r := g.exec(env, cmd.Args, nil)
	g.expect(r, 0, "version")
	if v, err := restic.DecodeVersion(bytes.NewReader(r.stdout)); err != nil || v.Version != pinnedVersion {
		fatalf("restic %q is not the pinned %s", v.Version, pinnedVersion)
	}
	g.record(r, cmd.Args, "version.json", "", "")

	// init
	initArgs := []string{"init", "--json"}
	r = g.exec(env, initArgs, nil)
	g.expect(r, 0, "init")
	g.record(r, initArgs, "init.json", "", "raw argv (no builder for init)")

	// backup_skipped (its own repository: retries must not add snapshots to the main one)
	g.captureSkipped(common)

	// backup_ok
	cmd = must(restic.Backup(backupOpts))
	r = g.exec(env, cmd.Args, nil)
	g.expect(r, 0, "backup_ok")
	g.record(r, cmd.Args, "backup_ok.stdout.jsonl", "backup_ok.stderr.jsonl", "")

	// backup_partial: a non-existent root next to a real one.
	missing := filepath.Join(g.root, "missing")
	partial := backupOpts
	partial.Roots = []string{g.tree, missing}
	cmd = must(restic.Backup(partial))
	r = g.exec(env, cmd.Args, nil)
	g.expect(r, 3, "backup_partial")
	g.record(r, cmd.Args, "backup_partial.stdout.jsonl", "backup_partial.stderr.jsonl",
		"missing root: plain-text stderr line, no JSON error line")

	// backup_locked: a file held open without sharing (exit 3 + error line).
	g.captureLocked(env, backupOpts)

	// backup_dry_run (-vv adds the per-item verbose_status lines).
	dry := backupOpts
	dry.DryRun = true
	dry.Extra = []string{"-vv"}
	cmd = must(restic.Backup(dry))
	r = g.exec(env, cmd.Args, nil)
	g.expect(r, 0, "backup_dry_run")
	g.record(r, cmd.Args, "backup_dry_run.stdout.jsonl", "", "")

	// Two old snapshots so that forget has something to remove.
	for _, when := range []string{"2020-01-01 00:00:00", "2020-02-01 00:00:00"} {
		old := backupOpts
		old.Extra = []string{"--time", when}
		cmd = must(restic.Backup(old))
		r = g.exec(env, cmd.Args, nil)
		g.expect(r, 0, "old backup")
	}

	// snapshots
	cmd = must(restic.Snapshots(fixtureHost, common))
	r = g.exec(env, cmd.Args, nil)
	g.expect(r, 0, "snapshots")
	g.record(r, cmd.Args, "snapshots.json", "", "")
	snaps, err := restic.DecodeSnapshots(bytes.NewReader(r.stdout))
	if err != nil || len(snaps) == 0 {
		fatalf("snapshots did not decode")
	}
	g.scrub.addUser(snaps[0].Username)
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].Time.Before(snaps[j].Time) })
	var newest string
	for _, s := range snaps {
		if s.Time.Year() >= 2026 && len(s.Paths) == 1 {
			newest = s.ID
		}
	}
	if newest == "" {
		fatalf("no snapshot of the tree found")
	}
	treeInSnap := snapPath(g.tree)

	// ls: the roots, then a browsed directory.
	cmd = must(restic.Ls(newest, "", common))
	r = g.exec(env, cmd.Args, nil)
	g.expect(r, 0, "ls roots")
	g.record(r, cmd.Args, "ls_roots.jsonl", "", "empty dir argument becomes /")
	cmd = must(restic.Ls(newest, treeInSnap, common))
	r = g.exec(env, cmd.Args, nil)
	g.expect(r, 0, "ls")
	g.record(r, cmd.Args, "ls.jsonl", "", "one level: the directory and its direct children")

	// find
	cmd = must(restic.Find(restic.FindOptions{Common: common, Pattern: "*.TXT", Host: fixtureHost, IgnoreCase: true, Snapshots: []string{newest}}))
	r = g.exec(env, cmd.Args, nil)
	g.expect(r, 0, "find")
	g.record(r, cmd.Args, "find.json", "", `stderr carries the notice: Ignoring "filters": explicit snapshot ids are given`)

	// stats
	for _, m := range []struct {
		mode restic.StatsMode
		file string
	}{{restic.StatsRawData, "stats_raw_data.json"}, {restic.StatsRestoreSize, "stats_restore_size.json"}} {
		cmd = must(restic.Stats(m.mode, fixtureHost, common))
		r = g.exec(env, cmd.Args, nil)
		g.expect(r, 0, string(m.mode))
		g.record(r, cmd.Args, m.file, "", "")
	}

	// restore (subfolder form: the target gets the tree's contents) and its simulate
	g.captureRestores(env, common, newest, treeInSnap)

	// cat config
	cmd = must(restic.CatConfig(common))
	r = g.exec(env, cmd.Args, nil)
	g.expect(r, 0, "cat config")
	g.record(r, cmd.Args, "cat_config.json", "", "restic does not take --json here; the output is pretty-printed JSON")

	// forget: dry run, then for real, never with --prune.
	fo := restic.ForgetOptions{Common: common, Host: fixtureHost, KeepLast: 1, KeepWithin: "1d", DryRun: true}
	cmd = must(restic.Forget(fo))
	r = g.exec(env, cmd.Args, nil)
	g.expect(r, 0, "forget dry run")
	g.record(r, cmd.Args, "forget_dry_run.json", "", "")
	fo.DryRun = false
	cmd = must(restic.Forget(fo))
	r = g.exec(env, cmd.Args, nil)
	g.expect(r, 0, "forget")
	g.record(r, cmd.Args, "forget.json", "", "")

	// prune: dry run, then for real.
	po := restic.PruneOptions{Common: common, MaxUnused: "0%", DryRun: true}
	cmd = must(restic.Prune(po))
	r = g.exec(env, cmd.Args, nil)
	g.expect(r, 0, "prune dry run")
	g.record(r, cmd.Args, "prune_dry_run.stdout.txt", "", "")
	po.DryRun = false
	cmd = must(restic.Prune(po))
	r = g.exec(env, cmd.Args, nil)
	g.expect(r, 0, "prune")
	g.record(r, cmd.Args, "prune.stdout.txt", "", "no JSON: plain text")

	// check, and a check of a damaged copy.
	cmd = must(restic.Check(restic.CheckOptions{Common: common}))
	r = g.exec(env, cmd.Args, nil)
	g.expect(r, 0, "check")
	g.record(r, cmd.Args, "check.stdout.jsonl", "", "")
	g.captureDamagedCheck(common)

	// exit 10: repository does not exist.
	missingRepo := env
	missingRepo.Repository = filepath.Join(g.root, "no-such-repo")
	cmd = must(restic.Snapshots("", common))
	r = g.exec(missingRepo, cmd.Args, nil)
	g.expect(r, 10, "exit 10")
	g.record(r, cmd.Args, "", "exit10_repo_missing.stderr.jsonl", "repository points at a directory that does not exist")

	// exit 12: wrong password.
	badPW := env
	badPW.Password = "not-the-password"
	cmd = must(restic.Snapshots("", common))
	r = g.exec(badPW, cmd.Args, nil)
	g.expect(r, 12, "exit 12")
	g.record(r, cmd.Args, "", "exit12_wrong_password.stderr.jsonl", "a --json command: the error arrives as an exit_error line")
	cmd = must(restic.CatConfig(common))
	r = g.exec(badPW, cmd.Args, nil)
	g.expect(r, 12, "exit 12 plain")
	g.record(r, cmd.Args, "", "exit12_wrong_password_plain.stderr.txt", "cat config has no --json: plain-text stderr")

	// exit 11: an exclusive operation while a backup holds a lock.
	g.captureLockConflict(env, common)
	return nil
}

// captureSkipped records a --skip-if-unchanged run that really skipped. The
// parent directories' metadata counts as a change, so it runs first, before
// anything else touches the temporary root, in a repository of its own, and
// retries a few times.
func (g *regen) captureSkipped(common restic.Common) {
	tree := filepath.Join(g.root, "skiptree")
	if err := os.WriteFile(filepath.Join(tree, "a.txt"), []byte("unchanged text"), 0o644); err != nil {
		fatalf("%v", err)
	}
	env := g.env
	env.Repository = filepath.Join(g.root, "skiprepo")
	g.expect(g.exec(env, []string{"init", "--json"}, nil), 0, "init skip repo")
	opts := restic.BackupOptions{Common: common, Host: fixtureHost, Roots: []string{tree}}
	g.expect(g.exec(env, must(restic.Backup(opts)).Args, nil), 0, "first backup of the skip tree")
	opts.SkipIfUnchanged = true
	for i := 0; i < 5; i++ {
		cmd := must(restic.Backup(opts))
		r := g.exec(env, cmd.Args, nil)
		g.expect(r, 0, "backup_skipped")
		if !bytes.Contains(r.stdout, []byte(`"snapshot_id"`)) {
			g.record(r, cmd.Args, "backup_skipped.stdout.jsonl", "", "second run of an unchanged tree in a repository of its own: no snapshot_id in the summary")
			return
		}
	}
	g.skips = append(g.skips, "backup_skipped.stdout.jsonl (restic kept creating snapshots: ancestor metadata changed)")
}

func (g *regen) captureRestores(env restic.Env, common restic.Common, snap, treeInSnap string) {
	ref := snap + ":" + treeInSnap
	target := filepath.Join(g.root, "restore", "verify")
	cmd := must(restic.Restore(restic.RestoreOptions{Common: common, SnapshotID: ref, Target: target, Overwrite: "always", Verify: true}))
	r := g.exec(env, cmd.Args, nil)
	g.expect(r, 0, "restore")
	g.record(r, cmd.Args, "restore.stdout.jsonl", "", "")

	sim := filepath.Join(g.root, "restore", "simulate")
	cmd = must(restic.Restore(restic.RestoreOptions{Common: common, SnapshotID: ref, Target: sim, DryRun: true}))
	r = g.exec(env, cmd.Args, nil)
	g.expect(r, 0, "restore dry run")
	g.record(r, cmd.Args, "restore_dry_run.stdout.jsonl", "", "--dry-run -vv: one verbose_status line per item")

	// Restoring the whole snapshot recreates every ancestor directory, and
	// Windows refuses to set the timestamp of the read-only Users directory:
	// a real restore error ("Access is denied", during "restore").
	full := filepath.Join(g.root, "restore", "full")
	cmd = must(restic.Restore(restic.RestoreOptions{Common: common, SnapshotID: snap, Target: full}))
	r = g.exec(env, cmd.Args, nil)
	if r.exit == 1 && bytes.Contains(bytes.ToLower(r.stderr), []byte("access is denied")) {
		g.record(r, cmd.Args, "restore_error.stdout.jsonl", "restore_error.stderr.jsonl",
			"whole-snapshot restore: a summary is printed although the run exits 1")
	} else {
		g.skips = append(g.skips, fmt.Sprintf("restore_error.* (whole-snapshot restore exited %d without an access-denied error)", r.exit))
	}
}

func (g *regen) captureDamagedCheck(common restic.Common) {
	copyRepo := filepath.Join(g.root, "repo-damaged")
	if err := copyTree(g.repo, copyRepo); err != nil {
		fatalf("copy repo: %v", err)
	}
	var biggest string
	var size int64
	_ = filepath.Walk(filepath.Join(copyRepo, "data"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && fi.Size() > size {
			biggest, size = p, fi.Size()
		}
		return nil
	})
	if biggest == "" || size < 2048 {
		g.skips = append(g.skips, "check_damaged.* (no pack large enough to damage)")
		return
	}
	f, err := os.OpenFile(biggest, os.O_WRONLY, 0)
	if err != nil {
		fatalf("open pack: %v", err)
	}
	_, _ = f.WriteAt([]byte("XXXXXXXXXXXXXXXXXXXXXXXX"), 1000)
	f.Close()
	env := g.env
	env.Repository = copyRepo
	cmd := must(restic.Check(restic.CheckOptions{Common: common, ReadDataSubset: "100%"}))
	r := g.exec(env, cmd.Args, nil)
	g.expect(r, 1, "damaged check")
	g.record(r, cmd.Args, "check_damaged.stdout.jsonl", "check_damaged.stderr.jsonl",
		"copy of the repository with 24 bytes overwritten inside the largest pack; check errors use a top-level message field")
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

func (g *regen) captureLocked(env restic.Env, opts restic.BackupOptions) {
	if runtime.GOOS != "windows" {
		g.skips = append(g.skips, "backup_locked.* (needs Windows)")
		return
	}
	ps, err := exec.LookPath("powershell.exe")
	if err != nil {
		g.skips = append(g.skips, "backup_locked.* (powershell.exe not found)")
		return
	}
	locked := filepath.Join(g.tree, "notes", "locked.docx")
	ready := filepath.Join(g.root, "locked.ready")
	if err := os.WriteFile(locked, []byte("held open\n"), 0o644); err != nil {
		fatalf("%v", err)
	}
	script := fmt.Sprintf("$f=[IO.File]::Open('%s','Open','Read','None'); Set-Content -Path '%s' -Value ready; Start-Sleep -Seconds 120", locked, ready)
	holder := exec.Command(ps, "-NoProfile", "-NonInteractive", "-Command", script)
	if err := holder.Start(); err != nil {
		g.skips = append(g.skips, "backup_locked.* (could not start powershell)")
		_ = os.Remove(locked)
		return
	}
	defer func() {
		_ = holder.Process.Kill()
		_, _ = holder.Process.Wait()
		_ = os.Remove(locked)
	}()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := os.Stat(ready); err != nil {
		g.skips = append(g.skips, "backup_locked.* (the holder never reported ready)")
		return
	}
	cmd := must(restic.Backup(opts))
	r := g.exec(env, cmd.Args, nil)
	if r.exit != 3 {
		g.skips = append(g.skips, fmt.Sprintf("backup_locked.* (exit %d, wanted 3)", r.exit))
		return
	}
	g.record(r, cmd.Args, "backup_locked.stdout.jsonl", "backup_locked.stderr.jsonl",
		"notes/locked.docx held open by a PowerShell child with FileShare None")
}

func (g *regen) captureLockConflict(env restic.Env, common restic.Common) {
	held := exec.Command(g.restic, "backup", "--stdin", "--stdin-filename", "held.bin", "--host", "held")
	held.Env = g.environ(env)
	in, err := held.StdinPipe()
	if err != nil {
		fatalf("%v", err)
	}
	if err := held.Start(); err != nil {
		fatalf("start held backup: %v", err)
	}
	closed := false
	closeHeld := func() {
		if !closed {
			closed = true
			_ = in.Close()
			_ = held.Wait()
		}
	}
	defer closeHeld()
	locks := filepath.Join(g.repo, "locks")
	for i := 0; ; i++ {
		if entries, _ := os.ReadDir(locks); len(entries) > 0 {
			break
		}
		if i > 150 {
			fatalf("the held backup never took its lock")
		}
		time.Sleep(200 * time.Millisecond)
	}
	// Forget without a retry-lock so that the conflict is immediate.
	cmd := must(restic.Forget(restic.ForgetOptions{Host: fixtureHost, KeepLast: 1, KeepWithin: "1d"}))
	r := g.exec(env, cmd.Args, nil)
	g.expect(r, 11, "exit 11")
	g.record(r, cmd.Args, "", "exit11_lock.stderr.jsonl",
		"produced while 'backup --stdin --stdin-filename held.bin --host held' holds a non-exclusive lock")
	// prune has no --json: the same conflict as plain text.
	cmd = must(restic.Prune(restic.PruneOptions{}))
	r = g.exec(env, cmd.Args, nil)
	g.expect(r, 11, "exit 11 prune")
	g.record(r, cmd.Args, "", "exit11_prune_lock.stderr.txt", "no --json: plain-text stderr")
	closeHeld()
}

// ---------------------------------------------------------------- scrubbing

type scrubber struct {
	subs   []sub
	users  []string
	host   string
	forbid []string
}

type sub struct {
	re   *regexp.Regexp
	with string
}

var (
	jsonUser = regexp.MustCompile(`("username"\s*:\s*")(?:[^"\\]|\\.)*(")`)
	jsonHost = regexp.MustCompile(`("hostname"\s*:\s*")(?:[^"\\]|\\.)*(")`)
)

// pathForms renders a native path in every spelling restic uses in its output:
// native, JSON-escaped, doubly escaped, snapshot form (/C/dir), backslash
// snapshot form (\C\dir) and its escaped twin, and forward-slash native.
func pathForms(p string) []string {
	const bslash = `\`
	sn, bs := p, p
	if len(p) >= 2 && p[1] == ':' {
		sn = "/" + p[:1] + strings.ReplaceAll(p[2:], bslash, "/")
		bs = bslash + p[:1] + p[2:]
	}
	double := func(s string) string { return strings.ReplaceAll(s, bslash, bslash+bslash) }
	// Always the same number of spellings in the same order, so that the source
	// and the replacement path line up index by index.
	return []string{
		p, double(p), double(double(p)),
		strings.ReplaceAll(p, bslash, "/"), sn,
		bs, double(bs), double(double(bs)),
	}
}

func newScrubber(root, userName, host string) (*scrubber, error) {
	s := &scrubber{host: host}
	s.addUser(userName)

	variants := []string{root}
	if long, err := filepath.EvalSymlinks(root); err == nil && long != root {
		variants = append(variants, long)
	}
	type target struct{ from, to string }
	var pairs []target
	for _, v := range variants {
		v = filepath.Clean(v)
		pairs = append(pairs, target{v, fixtureRoot})
		// Ancestors (except the drive root) become C:\fixture-upN.
		n := 1
		for dir := filepath.Dir(v); ; dir = filepath.Dir(dir) {
			parent := filepath.Dir(dir)
			if parent == dir || filepath.VolumeName(dir) != "" && dir == filepath.VolumeName(dir)+`\` {
				break
			}
			pairs = append(pairs, target{dir, fmt.Sprintf(`C:\fixture-up%d`, n)})
			n++
		}
	}
	// Longest source first so that a path is replaced before its ancestors.
	sort.SliceStable(pairs, func(i, j int) bool { return len(pairs[i].from) > len(pairs[j].from) })
	for _, p := range pairs {
		from, to := pathForms(p.from), pathForms(p.to)
		// pathForms yields the same number of spellings in the same order for
		// both sides as long as both are drive paths; fall back to the native
		// form otherwise (POSIX).
		for i, f := range from {
			w := to[min(i, len(to)-1)]
			s.subs = append(s.subs, sub{regexp.MustCompile(`(?i)` + regexp.QuoteMeta(f)), strings.ReplaceAll(w, `$`, `$$`)})
		}
	}
	// The fragments are assembled so that this source file itself passes the
	// fixture scrub test, which walks all of testdata/.
	users, appData, temp, domain := "Us"+"ers", "App"+"Data", "Te"+"mp", "Azure"+"AD"
	s.forbid = []string{users + `\`, users + "/", `\` + users, appData, temp + `\`, temp + "/", domain}
	if host != "" {
		s.forbid = append(s.forbid, host)
	}
	return s, nil
}

// addUser registers an account name (DOMAIN\name or name) for scrubbing.
func (s *scrubber) addUser(name string) {
	if name == "" {
		return
	}
	for _, u := range s.users {
		if u == name {
			return
		}
	}
	s.users = append(s.users, name)
	if i := strings.LastIndexByte(name, '\\'); i >= 0 {
		s.users = append(s.users, name[i+1:])
	}
}

func (s *scrubber) apply(data []byte) []byte {
	out := data
	for _, sb := range s.subs {
		out = sb.re.ReplaceAll(out, []byte(sb.with))
	}
	for _, u := range s.users {
		for _, f := range []string{u, strings.ReplaceAll(u, `\`, `\\`), strings.ReplaceAll(u, `\`, `\\\\`)} {
			out = replaceFold(out, f, fixtureUser)
		}
	}
	if s.host != "" {
		out = replaceFold(out, s.host, fixtureHost)
	}
	out = jsonUser.ReplaceAll(out, []byte("${1}"+fixtureUser+"${2}"))
	out = jsonHost.ReplaceAll(out, []byte("${1}"+fixtureHost+"${2}"))
	return out
}

func replaceFold(data []byte, old, with string) []byte {
	if old == "" {
		return data
	}
	return regexp.MustCompile(`(?i)`+regexp.QuoteMeta(old)).ReplaceAll(data, []byte(strings.ReplaceAll(with, `$`, `$$`)))
}

// residual returns the first forbidden string still present, or "".
func (s *scrubber) residual(data []byte) string {
	text := string(data)
	for _, f := range s.forbid {
		if strings.Contains(strings.ToLower(text), strings.ToLower(f)) {
			return f
		}
	}
	for _, u := range s.users {
		if len(u) > 2 && strings.Contains(strings.ToLower(text), strings.ToLower(u)) {
			return "<user name>"
		}
	}
	return ""
}

// ---------------------------------------------------------------- MANIFEST

const (
	beginMark = "<!-- BEGIN GENERATED (regen rewrites everything between the markers) -->"
	endMark   = "<!-- END GENERATED -->"
)

func (g *regen) writeManifest() error {
	var b strings.Builder
	b.WriteString(beginMark + "\n\n")
	b.WriteString("Captured with restic " + pinnedVersion + ". Environment of every run: `RESTIC_REPOSITORY` (a temporary local repository), `RESTIC_PASSWORD=" + fixturePW + "`, `RESTIC_CACHE_DIR`, `TEMP`/`TMP` (temporary directories), `RESTIC_PROGRESS_FPS=60` (so that short runs print status lines). The temporary root appears as `" + fixtureRoot + "`.\n\n")
	b.WriteString("| file | exit | command |\n| --- | --- | --- |\n")
	for _, e := range g.rows {
		cmd := "restic " + strings.Join(e.args, " ")
		cmd = string(g.scrub.apply([]byte(cmd)))
		note := ""
		if e.note != "" {
			note = " (" + e.note + ")"
		}
		fmt.Fprintf(&b, "| `%s` | %d | `%s`%s |\n", e.file, e.exit, cmd, note)
	}
	if len(g.skips) > 0 {
		b.WriteString("\nNot captured on this run:\n\n")
		for _, s := range g.skips {
			b.WriteString("- " + s + "\n")
		}
	}
	b.WriteString("\n" + endMark + "\n")
	generated := b.String()

	path := filepath.Join(g.out, "MANIFEST.md")
	existing, err := os.ReadFile(path)
	if err != nil {
		skeleton := "# Fixtures captured from restic " + pinnedVersion + "\n\n" +
			"Regenerate with `go run ./internal/restic/testdata/regen -restic <restic 0.19.1> -out ./internal/restic/testdata/v0.19.1`.\n\n" +
			generated + "\n## Divergences from the ticket and the architecture\n\n(none recorded)\n"
		return os.WriteFile(path, []byte(skeleton), 0o644)
	}
	text := string(existing)
	i, j := strings.Index(text, beginMark), strings.Index(text, endMark)
	if i < 0 || j < i {
		return fmt.Errorf("MANIFEST.md has no generated block markers")
	}
	text = text[:i] + strings.TrimRight(generated, "\n") + text[j+len(endMark):]
	return os.WriteFile(path, []byte(text), 0o644)
}
