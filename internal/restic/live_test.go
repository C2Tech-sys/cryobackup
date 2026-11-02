package restic

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Live tests run the builders against a real restic binary. They are skipped
// unless CRYO_RESTIC names the pinned binary:
//
//	CRYO_RESTIC=<path to restic 0.19.1> go test ./internal/restic -run Live -v
//
// Every repository lives in a test temp directory; nothing outside it is touched.

const livePassword = "live-test-password"

type live struct {
	t    *testing.T
	bin  string
	dir  string
	env  Env
	tree string
}

func newLive(t *testing.T) *live {
	t.Helper()
	bin := os.Getenv("CRYO_RESTIC")
	if bin == "" {
		t.Skip("CRYO_RESTIC is not set; live tests need the pinned restic binary")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("CRYO_RESTIC does not name a file: %v", err)
	}
	dir := t.TempDir()
	l := &live{t: t, bin: bin, dir: dir, tree: filepath.Join(dir, "tree")}
	l.env = Env{
		Repository: filepath.Join(dir, "repo"),
		Password:   livePassword,
		CacheDir:   filepath.Join(dir, "cache"),
		TempDir:    filepath.Join(dir, "tmp"),
	}
	for _, d := range []string{l.tree, l.env.CacheDir, l.env.TempDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, content string) {
		p := filepath.Join(l.tree, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join("invoices", "2026-01.pdf"), "invoice\n")
	write(filepath.Join("notes", "readme.txt"), "readme\n")
	write(filepath.Join("notes", "todo.docx"), "todo\n")
	return l
}

type ran struct {
	stdout, stderr []byte
	exit           int
}

func (l *live) run(env Env, c Command) ran {
	l.t.Helper()
	return l.runArgs(env, c.Args, nil)
}

func (l *live) runArgs(env Env, args []string, stdin *bytes.Reader) ran {
	l.t.Helper()
	environ, err := env.Environ(os.Environ())
	if err != nil {
		l.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, l.bin, args...)
	cmd.Env = environ
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	r := ran{}
	var ee *exec.ExitError
	switch err := cmd.Run(); {
	case err == nil:
	case errors.As(err, &ee):
		r.exit = ee.ExitCode()
	default:
		l.t.Fatalf("cannot run restic: %v", err)
	}
	r.stdout, r.stderr = so.Bytes(), se.Bytes()
	return r
}

func (l *live) expect(r ran, want int, what string) {
	l.t.Helper()
	if r.exit != want {
		l.t.Fatalf("%s: exit %d, want %d (stderr class of exit_error: %s)", what, r.exit, want, exitClass(r.stderr))
	}
}

func exitClass(stderr []byte) ErrorClass {
	for _, m := range decodeAll(nil2t{}, stderr, "") {
		if x, ok := m.(ExitError); ok {
			return x.Class
		}
	}
	return ClassUnknown
}

// nil2t lets exitClass reuse decodeAll outside a test: decodeAll only calls
// Helper and Fatalf.
type nil2t struct{ testing.TB }

func (nil2t) Helper()               {}
func (nil2t) Fatalf(string, ...any) { panic("decode failed") }

func liveSnapPath(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	if len(p) >= 2 && p[1] == ':' {
		return "/" + p[:1] + p[2:]
	}
	return p
}

func mustCmd(t *testing.T) func(Command, error) Command {
	return func(c Command, err error) Command {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
}

func TestLiveLifecycle(t *testing.T) {
	l := newLive(t)
	mk := mustCmd(t)
	common := Common{RetryLock: 30 * time.Second}

	// version
	r := l.run(l.env, Version())
	l.expect(r, 0, "version")
	v, err := DecodeVersion(bytes.NewReader(r.stdout))
	if err != nil || v.Version != "0.19.1" {
		t.Fatalf("restic %+v (%v): the pinned version is 0.19.1", v, err)
	}

	// init (no builder: setup is not part of the run vocabulary)
	r = l.runArgs(l.env, []string{"init", "--json"}, nil)
	l.expect(r, 0, "init")
	ini, err := DecodeInit(bytes.NewReader(r.stdout))
	if err != nil || len(ini.ID) != 64 {
		t.Fatalf("init = %+v, %v", ini, err)
	}

	// cat config
	r = l.run(l.env, mk(CatConfig(common)))
	l.expect(r, 0, "cat config")
	cfg, err := DecodeConfig(bytes.NewReader(r.stdout))
	if err != nil || cfg.Version < 1 || cfg.ID != ini.ID {
		t.Fatalf("config = %+v, %v", cfg, err)
	}

	// backup (no VSS, no cloud-files flag): exit 0 and a summary
	bo := BackupOptions{Common: common, Host: "live-host", Tags: StandardTags("cust", "pol", 1, "run-1"), Roots: []string{l.tree}}
	r = l.run(l.env, mk(Backup(bo)))
	l.expect(r, 0, "backup")
	msgs := decodeAll(t, r.stdout, KindBackup)
	sums := only[BackupSummary](msgs)
	if len(sums) != 1 || sums[0].SnapshotID == "" || sums[0].FilesNew != 3 {
		t.Fatalf("summary = %+v", sums)
	}
	if got := Map(KindBackup, r.exit, Evidence{SummarySeen: true}); got.Outcome != OutcomeSuccess {
		t.Fatalf("Map = %+v", got)
	}
	if got := Map(KindBackup, r.exit, Evidence{}); got.Outcome != OutcomeFailed || got.ErrorClass != ClassParse {
		t.Fatalf("Map without summary = %+v", got)
	}

	// a root that does not exist next to a real one: exit 3, partial. restic
	// 0.19.1 reports it as a plain-text stderr line, which the decoder counts as
	// Unparsed with class missing_source (there is no JSON error line).
	partial := bo
	partial.Roots = []string{l.tree, filepath.Join(l.dir, "does-not-exist")}
	r = l.run(l.env, mk(Backup(partial)))
	l.expect(r, 3, "partial backup")
	if s := only[BackupSummary](decodeAll(t, r.stdout, KindBackup)); len(s) != 1 || s[0].SnapshotID == "" {
		t.Fatalf("partial summary = %+v", s)
	}
	errs := decodeAll(t, r.stderr, KindBackup)
	var sawMissing bool
	counts := make(ErrorCounts)
	for _, m := range errs {
		switch v := m.(type) {
		case Unparsed:
			if v.Class == ClassMissingSource {
				sawMissing = true
				counts.Add(ErrorEvent{During: "scan", Class: v.Class})
			}
		case ErrorEvent:
			counts.Add(v)
		}
	}
	if !sawMissing {
		t.Fatalf("no missing_source line in stderr: %#v", errs)
	}
	ev := Evidence{SummarySeen: true, Stderr: counts.Dominant()}
	if got := Map(KindBackup, 3, ev); got.Outcome != OutcomePartial {
		t.Fatalf("Map = %+v", got)
	}

	// snapshots
	r = l.run(l.env, mk(Snapshots("live-host", common)))
	l.expect(r, 0, "snapshots")
	snaps, err := DecodeSnapshots(bytes.NewReader(r.stdout))
	if err != nil || len(snaps) != 2 {
		t.Fatalf("snapshots = %d, %v", len(snaps), err)
	}
	newest := snaps[len(snaps)-1].ID
	if snaps[0].Hostname != "live-host" || len(snaps[0].Tags) != 3 {
		t.Errorf("snapshot = %+v", snaps[0])
	}

	// ls: the roots, then a directory of the tree
	r = l.run(l.env, mk(Ls(newest, "", common)))
	l.expect(r, 0, "ls roots")
	if len(only[LsSnapshot](decodeAll(t, r.stdout, ""))) != 1 || len(only[Node](decodeAll(t, r.stdout, ""))) == 0 {
		t.Fatalf("ls roots = %s", "(see decode)")
	}
	treeInSnap := liveSnapPath(l.tree)
	r = l.run(l.env, mk(Ls(newest, treeInSnap+"/notes", common)))
	l.expect(r, 0, "ls dir")
	var names []string
	for _, n := range only[Node](decodeAll(t, r.stdout, "")) {
		names = append(names, n.Name)
	}
	if strings.Join(names, ",") != "notes,readme.txt,todo.docx" {
		t.Fatalf("ls dir nodes = %v", names)
	}

	// find
	r = l.run(l.env, mk(Find(FindOptions{Common: common, Pattern: "*.DOCX", Host: "live-host", IgnoreCase: true, Snapshots: []string{newest}})))
	l.expect(r, 0, "find")
	found, err := DecodeFind(bytes.NewReader(r.stdout))
	if err != nil || len(found) != 1 || found[0].Hits != 1 || !strings.HasSuffix(found[0].Matches[0].Path, "/todo.docx") {
		t.Fatalf("find = %+v, %v", found, err)
	}

	// stats
	r = l.run(l.env, mk(Stats(StatsRawData, "live-host", common)))
	l.expect(r, 0, "stats")
	st, err := DecodeStats(bytes.NewReader(r.stdout))
	if err != nil || st.TotalSize == 0 || st.SnapshotsCount != 2 {
		t.Fatalf("stats = %+v, %v", st, err)
	}

	// restore with --verify, then the simulate
	ref := newest + ":" + treeInSnap
	target := filepath.Join(l.dir, "restored")
	r = l.run(l.env, mk(Restore(RestoreOptions{Common: common, SnapshotID: ref, Target: target, Overwrite: "always", Verify: true})))
	l.expect(r, 0, "restore")
	if s := only[RestoreSummary](decodeAll(t, r.stdout, KindRestore)); len(s) != 1 || s[0].FilesRestored < 3 {
		t.Fatalf("restore summary = %+v", s)
	}
	if b, err := os.ReadFile(filepath.Join(target, "notes", "readme.txt")); err != nil || string(b) != "readme\n" {
		t.Fatalf("restored file: %q %v", b, err)
	}
	r = l.run(l.env, mk(Restore(RestoreOptions{Common: common, SnapshotID: ref, Target: filepath.Join(l.dir, "simulated"), DryRun: true})))
	l.expect(r, 0, "restore dry run")
	if len(only[VerboseStatus](decodeAll(t, r.stdout, KindRestore))) == 0 {
		t.Error("simulate printed no verbose_status lines")
	}
	if _, err := os.Stat(filepath.Join(l.dir, "simulated")); err == nil {
		t.Error("the simulated restore wrote files")
	}

	// dump as zip
	zipPath := filepath.Join(l.dir, "dump.zip")
	r = l.run(l.env, mk(Dump(ref, "/", zipPath, common)))
	l.expect(r, 0, "dump")
	if fi, err := os.Stat(zipPath); err != nil || fi.Size() == 0 {
		t.Fatalf("dump produced no archive: %v", err)
	}

	// an old snapshot so that forget has something to do, then forget (dry run), check, prune
	old := bo
	old.Extra = []string{"--time", "2020-01-01 00:00:00"}
	l.expect(l.run(l.env, mk(Backup(old))), 0, "old backup")
	fo := ForgetOptions{Common: common, Host: "live-host", KeepLast: 1, KeepWithin: "1d", DryRun: true}
	r = l.run(l.env, mk(Forget(fo)))
	l.expect(r, 0, "forget dry run")
	groups, err := DecodeForget(bytes.NewReader(r.stdout))
	if err != nil || len(groups) != 1 || len(groups[0].Remove) != 1 || len(groups[0].Keep) != 2 {
		t.Fatalf("forget dry run = %+v, %v", groups, err)
	}
	if snaps, _ := DecodeSnapshots(bytes.NewReader(l.run(l.env, mk(Snapshots("live-host", common))).stdout)); len(snaps) != 3 {
		t.Fatalf("a dry run removed snapshots: %d left", len(snaps))
	}
	fo.DryRun = false
	l.expect(l.run(l.env, mk(Forget(fo))), 0, "forget")

	r = l.run(l.env, mk(Check(CheckOptions{Common: common})))
	l.expect(r, 0, "check")
	if got := only[CheckSummary](decodeAll(t, r.stdout, KindCheck)); len(got) != 1 || got[0].NumErrors != 0 || got[0].BrokenPacks != 0 {
		t.Fatalf("check summary = %+v", got)
	}

	r = l.run(l.env, mk(Prune(PruneOptions{Common: common, MaxUnused: "0%"})))
	l.expect(r, 0, "prune")
	ps := ParsePruneText(bytes.NewReader(r.stdout))
	if ps.RemainingBlobs == 0 || ps.RemainingBytes == 0 {
		t.Fatalf("prune stats = %+v", ps)
	}

	// unlock with nothing to unlock succeeds
	l.expect(l.run(l.env, mk(Unlock(false, common))), 0, "unlock")
}

func TestLiveErrorExits(t *testing.T) {
	l := newLive(t)
	mk := mustCmd(t)
	common := Common{RetryLock: time.Second}
	l.expect(l.runArgs(l.env, []string{"init", "--json"}, nil), 0, "init")

	// exit 12: wrong password
	bad := l.env
	bad.Password = "not-the-password"
	// cat config has no --json: its stderr is plain text, still classified.
	r := l.run(bad, mk(CatConfig(common)))
	l.expect(r, 12, "wrong password (cat config)")
	if got := decodeAll(t, r.stderr, ""); len(got) != 1 || got[0] != Message(Unparsed{Class: ClassWrongPassword}) {
		t.Fatalf("plain-text stderr = %#v", got)
	}
	// a --json command reports the same thing as an exit_error line
	r = l.run(bad, mk(Snapshots("", common)))
	l.expect(r, 12, "wrong password")
	x := only[ExitError](decodeAll(t, r.stderr, ""))
	if len(x) != 1 || x[0] != (ExitError{Code: 12, Class: ClassWrongPassword}) {
		t.Fatalf("exit_error = %+v", x)
	}
	if got := Map(KindCheck, r.exit, Evidence{Stderr: x[0].Class}); got.Outcome != OutcomeFailed || got.ErrorClass != ClassWrongPassword {
		t.Fatalf("Map = %+v", got)
	}

	// exit 10: repository does not exist
	missing := l.env
	missing.Repository = filepath.Join(l.dir, "no-such-repo")
	r = l.run(missing, mk(Snapshots("", common)))
	l.expect(r, 10, "missing repository")
	x = only[ExitError](decodeAll(t, r.stderr, ""))
	if len(x) != 1 || x[0] != (ExitError{Code: 10, Class: ClassRepoMissing}) {
		t.Fatalf("exit_error = %+v", x)
	}
	if got := Map(KindBackup, r.exit, Evidence{}); got.Outcome != OutcomeFailed || got.ErrorClass != ClassRepoMissing || !got.ErrorClass.Hard() {
		t.Fatalf("Map = %+v", got)
	}
}

func TestLiveLockConflict(t *testing.T) {
	l := newLive(t)
	mk := mustCmd(t)
	l.expect(l.runArgs(l.env, []string{"init", "--json"}, nil), 0, "init")

	// A backup that waits on stdin holds a non-exclusive lock; forget wants an
	// exclusive one and must exit 11.
	environ, err := l.env.Environ(os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	held := exec.Command(l.bin, "backup", "--stdin", "--stdin-filename", "held.bin", "--host", "held")
	held.Env = environ
	stdin, err := held.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := held.Start(); err != nil {
		t.Fatal(err)
	}
	closed := false
	release := func() {
		if !closed {
			closed = true
			_ = stdin.Close()
			_ = held.Wait()
		}
	}
	defer release()
	locks := filepath.Join(l.dir, "repo", "locks")
	deadline := time.Now().Add(30 * time.Second)
	for {
		if entries, _ := os.ReadDir(locks); len(entries) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the held backup never took its lock")
		}
		time.Sleep(100 * time.Millisecond)
	}

	r := l.run(l.env, mk(Forget(ForgetOptions{Host: "held", KeepLast: 1, KeepWithin: "1d"})))
	l.expect(r, 11, "forget under a lock")
	x := only[ExitError](decodeAll(t, r.stderr, ""))
	if len(x) != 1 || x[0] != (ExitError{Code: 11, Class: ClassLock}) {
		t.Fatalf("exit_error = %+v", x)
	}
	if got := Map(KindForget, r.exit, Evidence{}); got.Outcome != OutcomeDeferred || got.DeferredReason != DeferredLock {
		t.Fatalf("Map = %+v", got)
	}

	// prune has no --json: its stderr is plain text, which still classifies.
	r = l.run(l.env, mk(Prune(PruneOptions{})))
	l.expect(r, 11, "prune under a lock")
	var classes []ErrorClass
	for _, m := range decodeAll(t, r.stderr, "") {
		if u, ok := m.(Unparsed); ok {
			classes = append(classes, u.Class)
		}
	}
	var sawLock bool
	for _, c := range classes {
		if c == ClassLock {
			sawLock = true
		}
	}
	if !sawLock {
		t.Fatalf("plain-text prune stderr gave classes %v, want one lock", classes)
	}
	release()
}
