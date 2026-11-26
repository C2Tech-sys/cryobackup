package maintain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// P22-K: drill mode of the worker (ADR 0029, ruling C8; drill.go).

// drillWorld is a tenv whose worker runs DrillLoop on a socket, with a configured self-backup and a
// 1 ms scheduler period: if anything of the scheduler or the self-backup could start, it would.
type drillWorld struct {
	*tenv
	cl   *adminpath.Client
	stop func()
}

func newDrillWorld(t *testing.T, rules []fakeRule) *drillWorld {
	t.Helper()
	dir, err := os.MkdirTemp("", "cd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	stateDir := t.TempDir()
	e := newTenv(t, rules, func(c *Config) {
		c.SocketPath = sock
		c.TickEvery = time.Millisecond
		c.SelfPrefix = testSelfPrefix
		c.StateDir = stateDir
	})
	e.onlyDevice(e.esc)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.w.DrillLoop(ctx) }()
	cl := adminpath.NewClient(sock, adminpath.ClientOptions{})
	var pingErr error
	for i := 0; i < 100; i++ {
		if _, pingErr = cl.Ping(ctx); pingErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pingErr != nil {
		cancel()
		t.Fatalf("ping over the drill socket: %v", pingErr)
	}
	d := &drillWorld{tenv: e, cl: cl}
	d.stop = func() {
		cancel()
		cl.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("DrillLoop returned %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("DrillLoop did not stop")
		}
	}
	t.Cleanup(d.stop)
	return d
}

// noJobRows asserts the scheduler and the self-backup never wrote their job_state rows nor held
// their leases.
func noJobRows(t *testing.T, e *tenv) {
	t.Helper()
	for _, job := range []string{SchedulerJob, SelfBackupJob, SelfBackupVerifyJob} {
		js, err := e.st.GetJobState(e.ctx, job)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			t.Fatalf("%s: %v", job, err)
		case js.RunCount != 0 || js.LastStatus != "" || js.LeaseHolder != "":
			t.Errorf("job %s ran in drill mode: %+v", job, js)
		}
	}
}

// TestDrillMethodTable pins which admin-path methods a drill worker serves: exactly the four read
// methods and ping. A method added to the admin path later is refused until someone adds it here.
func TestDrillMethodTable(t *testing.T) {
	want := map[string]bool{
		adminpath.MethodPing: true, adminpath.MethodCatConfig: true, adminpath.MethodLs: true,
		adminpath.MethodSnapshots: true, adminpath.MethodDump: true,
	}
	for _, m := range adminpath.Methods() {
		if got := DrillServes(m); got != want[m] {
			t.Errorf("DrillServes(%s) = %v, want %v", m, got, want[m])
		}
	}
	if DrillServes("") || DrillServes("purge") || DrillServes("PURGE_REPO") {
		t.Error("an unknown method name must not be served")
	}
}

// TestDrillRefusesEveryOtherMethodOverTheSocket calls every refused method directly on the drill
// worker's socket (no console in front) and checks the code, that the worker started no process
// for any of them, and that each refusal is logged by method name.
func TestDrillRefusesEveryOtherMethodOverTheSocket(t *testing.T) {
	d := newDrillWorld(t, []fakeRule{{Cmd: "cat", Stdout: "cat_config.json"}})
	ctx, id := d.ctx, d.esc
	before := len(d.calls())
	refused := map[string]func() error{
		adminpath.MethodInit: func() error { _, err := d.cl.Init(ctx, adminpath.InitRequest{DeviceRequest: dev(id)}); return err },
		adminpath.MethodFind: func() error {
			_, err := d.cl.Find(ctx, adminpath.FindRequest{DeviceRequest: dev(id), Pattern: "*.docx"})
			return err
		},
		adminpath.MethodStats: func() error {
			_, err := d.cl.Stats(ctx, adminpath.StatsRequest{DeviceRequest: dev(id), Mode: "raw-data"})
			return err
		},
		adminpath.MethodUnlock: func() error { _, err := d.cl.Unlock(ctx, adminpath.UnlockRequest{DeviceRequest: dev(id)}); return err },
		adminpath.MethodForget: func() error {
			_, err := d.cl.Forget(ctx, adminpath.ForgetRequest{DeviceRequest: dev(id), KeepWithin: "30d", Rules: adminpath.KeepRules{KeepLast: 3}})
			return err
		},
		adminpath.MethodPrune: func() error { _, err := d.cl.Prune(ctx, adminpath.PruneRequest{DeviceRequest: dev(id)}); return err },
		adminpath.MethodCheck: func() error { _, err := d.cl.Check(ctx, adminpath.CheckRequest{DeviceRequest: dev(id)}); return err },
		adminpath.MethodPurgeRepo: func() error {
			_, err := d.cl.PurgeRepo(ctx, adminpath.PurgeRepoRequest{DeviceRequest: adminpath.DeviceRequest{DeviceID: id, RequestID: "drill-test"}})
			return err
		},
	}
	for _, m := range adminpath.Methods() {
		call, ok := refused[m]
		if DrillServes(m) {
			if ok {
				t.Fatalf("%s is both served and in the refused table", m)
			}
			continue
		}
		if !ok {
			t.Fatalf("method %s is refused in drill mode but this test does not call it: add it", m)
		}
		wantCode(t, call(), adminpath.CodeUnavailable, "")
		if !strings.Contains(d.logs.String(), `"method":"`+m+`"`) {
			t.Errorf("the refusal of %s was not logged by name:\n%s", m, d.logs.String())
		}
	}
	// The storage probe's cat config is refused: the probe may create its repository.
	_, err := d.cl.CatConfig(ctx, adminpath.DeviceRequest{DeviceID: adminpath.ProbeRepoID})
	wantCode(t, err, adminpath.CodeUnavailable, "")
	if n := len(d.calls()); n != before {
		t.Fatalf("a refused method started restic or rclone: %d calls after, %d before: %+v", n, before, d.calls()[before:])
	}
	if strings.Contains(d.logs.String(), "repository purge started") {
		t.Fatal("purge_repo reached the purge")
	}
}

// TestDrillServesTheReadMethods proves the four read methods reach restic through the drill socket
// and answer as in production.
func TestDrillServesTheReadMethods(t *testing.T) {
	d := newDrillWorld(t, []fakeRule{
		{Cmd: "cat", Stdout: "cat_config.json"},
		{Cmd: "snapshots", Stdout: "snapshots.json"},
		{Cmd: "ls", Stdout: "inline:" + lsFileJSON},
		{Cmd: "dump", Stdout: "inline:" + docxBody},
	})
	ctx, id := d.ctx, d.esc
	if cfg, err := d.cl.CatConfig(ctx, dev(id)); err != nil || cfg.ID == "" {
		t.Fatalf("cat_config = %+v, %v", cfg, err)
	}
	if snaps, err := d.cl.Snapshots(ctx, dev(id)); err != nil || len(snaps) == 0 {
		t.Fatalf("snapshots = %d, %v", len(snaps), err)
	}
	if res, err := d.cl.Ls(ctx, adminpath.LsRequest{DeviceRequest: dev(id), SnapshotID: "latest", Dir: dumpTree}); err != nil {
		t.Fatalf("ls = %+v, %v", res, err)
	}
	var out bytes.Buffer
	if _, err := d.cl.Dump(ctx, adminpath.DumpRequest{DeviceRequest: dev(id), SnapshotID: "latest", Path: dumpTree + "/report.docx"}, &out); err != nil {
		t.Fatalf("dump: %v", err)
	}
	names, bodies := zipEntries(t, out.Bytes())
	if len(names) != 1 || bodies[names[0]] != docxBody {
		t.Fatalf("dump zip = %v", names)
	}
	// A customer-held device stays refused by custody, as in production.
	_, err := d.cl.CatConfig(ctx, dev(d.held))
	wantCode(t, err, adminpath.CodeCustodyCustomerHeld, "")
	noJobRows(t, d.tenv)
}

// TestDrillLoopNeverRunsTheScheduler runs the drill loop across many scheduler periods (1 ms each,
// a fake clock advanced by hours each time) with a configured self-backup and a device whose usage
// sample is due: no pass, no self-backup, no escrow verify, no usage sample, no restic call.
func TestDrillLoopNeverRunsTheScheduler(t *testing.T) {
	d := newDrillWorld(t, []fakeRule{
		{Cmd: "cat", Stdout: "cat_config.json"},
		{Cmd: "stats", Stdout: "stats_raw_data.json"},
		{Cmd: "snapshots", Stdout: "snapshots.json"},
	})
	before := len(d.calls())
	for i := 0; i < 200; i++ {
		d.clk.Advance(time.Hour)
		time.Sleep(2 * time.Millisecond) // two periods of the 1 ms TickEvery each time
	}
	if n := len(d.calls()); n != before {
		t.Fatalf("restic ran in drill mode with no request: %+v", d.calls()[before:])
	}
	if n, _ := usageRows(t, d.tenv, d.esc); n != 0 {
		t.Fatalf("a usage sample was written in drill mode: %d rows", n)
	}
	noJobRows(t, d.tenv)
	for _, line := range []string{"maintenance cycle failed", "self backup step failed", "scheduler lease"} {
		if strings.Contains(d.logs.String(), line) {
			t.Errorf("the log mentions %q in drill mode:\n%s", line, d.logs.String())
		}
	}
	// For contrast, the same world's production Loop starts a pass at once (TestLoopServesTheAdminPath):
	// the worker itself is unchanged; only DrillLoop runs nothing.
}

// TestDrillPurgeRepoRefusedInProcess: a worker in drill mode refuses purge_repo even when the
// Worker method is called directly, without the socket and without drillBackend.
func TestDrillPurgeRepoRefusedInProcess(t *testing.T) {
	e := newTenv(t, nil, nil)
	other := newTenv(t, nil, nil)
	e.w.enterDrill()
	t.Cleanup(func() { drillMu.Lock(); delete(drillWorkers, e.w); drillMu.Unlock() })
	before := len(e.calls())
	_, err := e.w.PurgeRepo(e.ctx, adminpath.PurgeRepoRequest{DeviceRequest: adminpath.DeviceRequest{DeviceID: e.esc, RequestID: "drill-test"}})
	wantCode(t, err, adminpath.CodeUnavailable, "")
	var ae *adminpath.Error
	if errors.As(err, &ae) && ae.Message != errDrill {
		t.Fatalf("in-process message = %q, want the drill sentence", ae.Message)
	}
	if strings.Contains(e.logs.String(), "repository purge started") || len(e.calls()) != before {
		t.Fatalf("the purge ran: %s %+v", e.logs.String(), e.calls())
	}
	if other.w.inDrill() {
		t.Fatal("drill mode leaked to another worker")
	}
	// The other worker is not in drill mode: its purge goes on to its own checks (the device is not
	// retired, so purge_eligible refuses it with a reason of its own, never the drill sentence).
	_, err = other.w.PurgeRepo(other.ctx, adminpath.PurgeRepoRequest{DeviceRequest: adminpath.DeviceRequest{DeviceID: other.esc, RequestID: "drill-test"}})
	if errors.As(err, &ae) && ae.Message == errDrill {
		t.Fatal("a production worker refused purge_repo as a drill copy")
	}
}

func TestDrillLoopNeedsASocket(t *testing.T) {
	e := newTenv(t, nil, nil)
	if err := e.w.DrillLoop(e.ctx); err == nil {
		t.Fatal("DrillLoop without SocketPath succeeded")
	}
}

// TestRunInDrillModeRefusesOnce: --once is the scheduler pass and the self-backup step (and what
// `self-backup run` and `verify` call), so a drill config refuses it before opening anything.
func TestRunInDrillModeRefusesOnce(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	missing := filepath.Join(dir, "no-such.db") // never opened: the refusal comes first
	if err := os.WriteFile(cfgPath, []byte(`{"db_path": "`+filepath.ToSlash(missing)+`", "master_key_path": "x", "drill": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := Run([]string{"--config", cfgPath, "--once"}, &out, &errb, nil); code != ExitUsage {
		t.Fatalf("drill --once exit = %d, want %d (stderr %q)", code, ExitUsage, errb.String())
	}
	if !strings.Contains(errb.String(), "drill mode runs no scheduler pass") {
		t.Fatalf("stderr = %q", errb.String())
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("the database was created: the refusal came too late")
	}

	// CRYO_DRILL from the environment does the same, and a malformed value is refused, never false.
	if err := os.WriteFile(cfgPath, []byte(`{"db_path": "`+filepath.ToSlash(missing)+`", "master_key_path": "x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRYO_DRILL", "true")
	errb.Reset()
	if code := Run([]string{"--config", cfgPath, "--once"}, &out, &errb, nil); code != ExitUsage || !strings.Contains(errb.String(), "drill mode") {
		t.Fatalf("CRYO_DRILL=true --once: exit %d, stderr %q", code, errb.String())
	}
	t.Setenv("CRYO_DRILL", "yes please")
	errb.Reset()
	if code := Run([]string{"--config", cfgPath, "--once"}, &out, &errb, nil); code != ExitUsage || !strings.Contains(errb.String(), "CRYO_DRILL must be true or false") {
		t.Fatalf("malformed CRYO_DRILL: exit %d, stderr %q", code, errb.String())
	}
	fc, err := loadFileConfig(cfgPath)
	if !errors.Is(err, errDrillEnv) || fc.Drill {
		t.Fatalf("loadFileConfig with a malformed CRYO_DRILL = %+v, %v", fc, err)
	}
	t.Setenv("CRYO_DRILL", "false")
	if fc, err := loadFileConfig(cfgPath); err != nil || fc.Drill {
		t.Fatalf("CRYO_DRILL=false = %+v, %v", fc, err)
	}
}

// leaveDrill forgets a test worker's drill mark when the test ends.
func leaveDrill(t *testing.T, w *Worker) {
	t.Cleanup(func() { drillMu.Lock(); delete(drillWorkers, w); drillMu.Unlock() })
}

// hasArg reports whether a restic call's argv holds arg.
func hasArg(c fakeCall, arg string) bool {
	for _, a := range c.Args {
		if a == arg {
			return true
		}
	}
	return false
}

// TestDrillReadsRunWithNoLock (FIX BATCH 1, K2): a production worker's reads carry --retry-lock
// and take restic's lock; the same worker in drill mode runs cat config, snapshots, ls and dump
// with --no-lock and no --retry-lock, so a drill copy leaves no lock object in storage.
func TestDrillReadsRunWithNoLock(t *testing.T) {
	e := newTenv(t, []fakeRule{
		{Cmd: "cat", Stdout: "cat_config.json"},
		{Cmd: "snapshots", Stdout: "snapshots.json"},
		{Cmd: "ls", Stdout: "inline:" + lsFileJSON},
		{Cmd: "dump", Stdout: "inline:" + docxBody},
	}, nil)
	reads := func() {
		t.Helper()
		if _, err := e.w.CatConfig(e.ctx, dev(e.esc)); err != nil {
			t.Fatalf("cat config: %v", err)
		}
		if _, err := e.w.Snapshots(e.ctx, dev(e.esc)); err != nil {
			t.Fatalf("snapshots: %v", err)
		}
		if _, err := e.w.Ls(e.ctx, adminpath.LsRequest{DeviceRequest: dev(e.esc), SnapshotID: "latest", Dir: dumpTree}); err != nil {
			t.Fatalf("ls: %v", err)
		}
		if _, err := e.w.Dump(e.ctx, adminpath.DumpRequest{DeviceRequest: dev(e.esc), SnapshotID: "latest", Path: dumpTree + "/report.docx"}, io.Discard); err != nil {
			t.Fatalf("dump: %v", err)
		}
	}
	reads()
	prod := e.repoCalls()
	if len(prod) < 4 {
		t.Fatalf("production reads made %d restic calls, want at least 4", len(prod))
	}
	for _, c := range prod {
		if hasArg(c, "--no-lock") || !hasArg(c, "--retry-lock") {
			t.Fatalf("production %s: argv %v, want --retry-lock and no --no-lock", c.Args[0], c.Args)
		}
	}
	e.w.enterDrill()
	leaveDrill(t, e.w)
	reads()
	drill := e.repoCalls()[len(prod):]
	seen := map[string]bool{}
	for _, c := range drill {
		seen[c.Args[0]] = true
		if !hasArg(c, "--no-lock") || hasArg(c, "--retry-lock") {
			t.Fatalf("drill %s: argv %v, want --no-lock and no --retry-lock", c.Args[0], c.Args)
		}
	}
	for _, cmd := range []string{"cat", "snapshots", "ls", "dump"} {
		if !seen[cmd] {
			t.Errorf("drill reads made no %s call: %v", cmd, drill)
		}
	}
}

// TestDrillWatchdogStopSchedulesNoUnlock (FIX BATCH 1, K2): a stalled dump in drill mode is still
// stopped by the idle watchdog and fails as in production, but no stale-lock unlock runs: no
// restic unlock, no unlock run row. (In production TestDumpMultiStalledChildTripsTheWatchdog
// sees exactly one.)
func TestDrillWatchdogStopSchedulesNoUnlock(t *testing.T) {
	shortIdle(t, 2*time.Second)
	e := newTenv(t, stalledDumpRules(t, 0), nil)
	e.w.enterDrill()
	leaveDrill(t, e.w)
	var out bytes.Buffer
	_, err := e.w.Dump(e.ctx, multiReq(e, "latest", multiTree+"/docs"), &out)
	wantCode(t, err, adminpath.CodeTimeout, "")
	notAZip(t, out.Bytes())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e.w.Drain(ctx)          // waits for any background work: there must be none
	time.Sleep(time.Second) // and nothing late either
	for _, c := range e.repoCalls() {
		if c.Args[0] == "unlock" {
			t.Fatalf("a drill copy ran restic unlock after the watchdog: %v", c.Args)
		}
		if c.Args[0] == "dump" && !hasArg(c, "--no-lock") {
			t.Fatalf("the drill dump ran without --no-lock: %v", c.Args)
		}
	}
	for _, r := range e.consoleRuns(e.esc) {
		if r.Kind == "unlock" {
			t.Fatalf("a drill copy recorded an unlock run: %+v", r)
		}
	}
	if !strings.Contains(e.logs.String(), "dump_idle_unlock") {
		t.Errorf("the skipped cleanup was not logged:\n%s", e.logs.String())
	}
}

// TestRunLoopInDrillModeServesTheDrillLoop (FIX BATCH 2, K4): `maintain --loop` with a drill config
// goes through Run to DrillLoop, not to Loop. The worker has a self-backup prefix and its keys, so a
// production Loop would start a scheduler pass and a self-backup step the moment it is up (the
// first pass is not delayed to a tick); here neither job row exists once the loop has stopped, the
// worker says it is in drill mode, and a method the drill socket refuses is refused. (Run builds the
// worker itself, so this is the only test that reaches the `if fc.Drill` branch of Run.)
func TestRunLoopInDrillModeServesTheDrillLoop(t *testing.T) {
	dir := t.TempDir()
	exe, sum := fakeResticPath(t)
	harness := filepath.Join(dir, "harness")
	if err := os.MkdirAll(harness, 0o700); err != nil {
		t.Fatal(err)
	}
	fixtures, err := filepath.Abs(filepath.Join("..", "..", "restic", "testdata", "v0.19.1"))
	if err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rules := append([]fakeRule{{Cmd: "version", Stdout: "version.json"}}, selfRules(filepath.Join(dataDir, SelfBackupStagingDir))...)
	sc, _ := json.Marshal(fakeScenario{Fixtures: fixtures, Rules: rules})
	if err := os.WriteFile(filepath.Join(harness, "fake.json"), sc, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(harness, "rclone.conf"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sockDir, err := os.MkdirTemp("", "cd") // a short path: a unix socket's name is length limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "d.sock")
	dbPath := filepath.Join(dataDir, "c.db")
	cfg := filepath.Join(dir, "config.json")
	body := `{"db_path":` + jsonStr(dbPath) + `,"master_key_path":` + jsonStr(filepath.Join(dir, "master.key")) +
		`,"adminpath_socket":` + jsonStr(sock) + `,"drill":true}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRYO_MAINTAIN_RESTIC", exe)
	t.Setenv("CRYO_MAINTAIN_RESTIC_SHA256", sum)
	t.Setenv("RCLONE_CONFIG", filepath.Join(harness, "rclone.conf"))
	t.Setenv("CRYO_MAINTAIN_SCRATCH_DIR", filepath.Join(dir, "scratch"))
	t.Setenv("CRYO_MAINTAIN_BUCKET", "bucket")
	t.Setenv("CRYO_MAINTAIN_PREFIX", "cb")
	t.Setenv("CRYO_MAINTAIN_SELF_PREFIX", testSelfPrefix)
	keys := func(string, *store.Store) (KeyUnwrapper, error) {
		return selfAndUnwrap{&fakeKeys{pw: "x"}, &fakeSelfKeys{pw: testSelfPW, keyID: testSelfKeyID}}, nil
	}

	// Stand in for SIGTERM: Run's context is cancelled by the test.
	runCtx, stopRun := context.WithCancel(context.Background())
	prev := notifyContext
	notifyContext = func(context.Context, ...os.Signal) (context.Context, context.CancelFunc) {
		return context.WithCancel(runCtx)
	}
	t.Cleanup(func() { notifyContext = prev; stopRun() })

	var out, errb bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- Run([]string{"--config", cfg, "--loop"}, &out, &errb, keys) }()

	cl := adminpath.NewClient(sock, adminpath.ClientOptions{})
	defer cl.Close()
	var pingErr error
	for i := 0; i < 200; i++ {
		select {
		case code := <-done:
			t.Fatalf("maintain --loop returned %d before serving (stderr %q)", code, errb.String())
		default:
		}
		if _, pingErr = cl.Ping(runCtx); pingErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pingErr != nil {
		stopRun()
		t.Fatalf("ping over the --loop socket: %v", pingErr)
	}
	// A drill socket refuses a method a production worker serves, with the refusal's own code.
	_, err = cl.Unlock(runCtx, adminpath.UnlockRequest{DeviceRequest: adminpath.DeviceRequest{DeviceID: "no-such-device"}})
	wantCode(t, err, adminpath.CodeUnavailable, "")

	stopRun()
	select {
	case code := <-done:
		if code != ExitOK {
			t.Fatalf("maintain --loop exit %d, want %d (stderr %q)", code, ExitOK, errb.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("maintain --loop did not stop")
	}
	// The log carries classes, not sentences (log messages are redacted): DrillLoop's own class, and
	// the refusal of the one method called above.
	for _, want := range []string{`"class":"drill"`, `"class":"drill_refused"`} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("--loop with a drill config did not log %s:\n%s", want, errb.String())
		}
	}
	st, err := store.Open(context.Background(), store.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, job := range []string{SchedulerJob, SelfBackupJob, SelfBackupVerifyJob} {
		js, err := st.GetJobState(context.Background(), job)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			t.Fatalf("%s: %v", job, err)
		case js.RunCount != 0 || js.LastStatus != "" || js.LeaseHolder != "":
			t.Errorf("job %s ran under maintain --loop in drill mode: %+v", job, js)
		}
	}
}
