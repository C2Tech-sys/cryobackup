package maintain

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

func dev(id string) adminpath.DeviceRequest { return adminpath.DeviceRequest{DeviceID: id} }

// wantCode asserts err is an *adminpath.Error with the given code (and class
// when class is non-empty).
func wantCode(t *testing.T, err error, code string, class restic.ErrorClass) {
	t.Helper()
	var ae *adminpath.Error
	if !errors.As(err, &ae) {
		t.Fatalf("error = %v, want *adminpath.Error %s", err, code)
	}
	if ae.Code != code {
		t.Fatalf("code = %s, want %s", ae.Code, code)
	}
	if class != "" && ae.Class != class {
		t.Fatalf("class = %s, want %s", ae.Class, class)
	}
}

func TestNewChecksTheBinary(t *testing.T) {
	good := newTenv(t, nil, nil)
	exe, sum := fakeResticPath(t)

	t.Run("hash mismatch", func(t *testing.T) {
		cfg := good.cfg
		cfg.ResticSHA256 = strings.Repeat("0", 64)
		if _, err := New(cfg, good.st, good.keys, nil); !errors.Is(err, ErrResticBinary) {
			t.Fatalf("err = %v, want ErrResticBinary", err)
		}
	})
	t.Run("missing binary", func(t *testing.T) {
		cfg := good.cfg
		cfg.ResticProgram = filepath.Join(good.state, "no-such-restic")
		if _, err := New(cfg, good.st, good.keys, nil); !errors.Is(err, ErrResticBinary) {
			t.Fatalf("err = %v, want ErrResticBinary", err)
		}
	})
	t.Run("hash match", func(t *testing.T) {
		cfg := good.cfg
		cfg.ResticProgram, cfg.ResticSHA256 = exe, strings.ToUpper(sum)
		if _, err := New(cfg, good.st, good.keys, nil); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("wrong version", func(t *testing.T) {
		good.scenario([]fakeRule{{Cmd: "version", Stdout: `inline:{"message_type":"version","version":"0.18.0"}`}})
		if _, err := New(good.cfg, good.st, good.keys, nil); !errors.Is(err, ErrResticVersion) {
			t.Fatalf("err = %v, want ErrResticVersion", err)
		}
	})
	t.Run("version exits nonzero", func(t *testing.T) {
		good.scenario([]fakeRule{{Cmd: "version", Exit: 1, Stdout: "version.json"}})
		if _, err := New(good.cfg, good.st, good.keys, nil); !errors.Is(err, ErrResticVersion) {
			t.Fatalf("err = %v, want ErrResticVersion", err)
		}
	})
}

func TestConfigValidation(t *testing.T) {
	e := newTenv(t, nil, nil)
	for name, mut := range map[string]func(*Config){
		"bypass governance": func(c *Config) { c.BypassGovernance = true },
		"no bucket":         func(c *Config) { c.Bucket = "" },
		"relative scratch":  func(c *Config) { c.ScratchDir = "relative/scratch" },
		"remote with colon": func(c *Config) { c.Remote = "a:b" },
		"bad sha":           func(c *Config) { c.ResticSHA256 = "xyz" },
		"rclone with space": func(c *Config) { c.RcloneProgram = "/opt/my rclone/rclone" },
		"rclone with quote": func(c *Config) { c.RcloneProgram = "/opt/rclone'x" },
	} {
		cfg := e.cfg
		mut(&cfg)
		if _, err := New(cfg, e.st, e.keys, nil); !errors.Is(err, errConfig) {
			t.Errorf("%s: err = %v, want errConfig", name, err)
		}
	}
	// restic splits rclone.program like a shell on every OS, so a backslash is
	// never passed through. Windows normalises it to a slash; elsewhere a path
	// holding a backslash is invalid (legal in a POSIX file name, but restic
	// would read it as an escape).
	cfg := e.cfg
	if runtime.GOOS == "windows" {
		cfg.RcloneProgram = `C:\tools\rclone.exe`
		got, err := cfg.withDefaults()
		if err != nil || got.RcloneProgram != "C:/tools/rclone.exe" {
			t.Errorf("windows rclone program = %q, %v", got.RcloneProgram, err)
		}
	} else {
		cfg.RcloneProgram = "/usr/local/bin/rclone"
		got, err := cfg.withDefaults()
		if err != nil || got.RcloneProgram != "/usr/local/bin/rclone" {
			t.Errorf("posix rclone program = %q, %v", got.RcloneProgram, err)
		}
		cfg.RcloneProgram = `C:\tools\rclone.exe`
		if _, err := cfg.withDefaults(); !errors.Is(err, errConfig) {
			t.Errorf("backslash rclone program: err = %v, want errConfig", err)
		}
	}
	if _, err := New(e.cfg, nil, e.keys, nil); err == nil {
		t.Error("nil store accepted")
	}
	if _, err := New(e.cfg, e.st, nil, nil); err == nil {
		t.Error("nil key unwrapper accepted")
	}
}

func TestPingReportsState(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "cat", Stdout: "cat_config.json"}}, func(c *Config) { c.Version = "9.9.9" })
	e.clk.Advance(90 * time.Second)
	p, err := e.w.Ping(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != "9.9.9" || p.Uptime != 90*time.Second || !p.MasterKeyLoaded || p.StorageReachable != nil {
		t.Fatalf("ping = %+v", p)
	}
	if _, err := e.w.CatConfig(e.ctx, dev(e.esc)); err != nil {
		t.Fatal(err)
	}
	p, _ = e.w.Ping(e.ctx)
	if p.StorageReachable == nil || !*p.StorageReachable {
		t.Fatalf("storage reachable = %v after a successful call", p.StorageReachable)
	}
}

func TestDeviceResolution(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "cat", Stdout: "cat_config.json"}}, nil)

	_, err := e.w.CatConfig(e.ctx, dev("01NOSUCHDEVICE0000000000"))
	wantCode(t, err, adminpath.CodeNotFound, "")

	before := len(e.keys.called())
	_, err = e.w.CatConfig(e.ctx, dev(e.held))
	wantCode(t, err, adminpath.CodeCustodyCustomerHeld, "")
	if len(e.keys.called()) != before {
		t.Error("the key unwrapper was asked for a customer-held device")
	}

	e.keys.err = ErrCustodyCustomerHeld
	_, err = e.w.CatConfig(e.ctx, dev(e.esc))
	wantCode(t, err, adminpath.CodeCustodyCustomerHeld, "")
	e.keys.err = errors.New("tampered row C:\\secret\\path")
	_, err = e.w.CatConfig(e.ctx, dev(e.esc))
	wantCode(t, err, adminpath.CodeInternal, "")
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("error text leaks the underlying error: %v", err)
	}
	e.keys.err = nil

	if n := len(e.repoCalls()); n != 0 {
		t.Fatalf("restic ran %d times for refused calls", n)
	}
}

func TestCommandEnvironmentAndArgs(t *testing.T) {
	t.Setenv("CRYO_LEAK_SENTINEL", "leak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "leak")
	e := newTenv(t, []fakeRule{{Cmd: "cat", Stdout: "cat_config.json"}}, nil)
	cfg, err := e.w.CatConfig(e.ctx, dev(e.esc))
	if err != nil || cfg.ID == "" {
		t.Fatalf("CatConfig = %+v, %v", cfg, err)
	}
	calls := e.repoCalls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	c := calls[0]
	if c.Repo != "rclone:admin:test-bucket/pre/"+e.esc {
		t.Errorf("repository = %q", c.Repo)
	}
	if c.Password != testPassword {
		t.Errorf("password did not reach RESTIC_PASSWORD")
	}
	// ADR 0008: no cache on disk; temp files in the scratch tmpfs.
	if c.CacheSet {
		t.Errorf("RESTIC_CACHE_DIR reached the child (%q): the worker runs restic with --no-cache", c.Cache)
	}
	if c.TmpDir != filepath.Join(e.scratch, "tmp") || c.RTemp != c.TmpDir || c.RCache != filepath.Join(e.scratch, "rclone") {
		t.Errorf("TMPDIR %q, RCLONE_TEMP_DIR %q, RCLONE_CACHE_DIR %q: want the scratch directory", c.TmpDir, c.RTemp, c.RCache)
	}
	if c.RClone != e.cfg.RcloneConfig {
		t.Errorf("RCLONE_CONFIG = %q", c.RClone)
	}
	for _, n := range c.EnvNames {
		if n == "CRYO_LEAK_SENTINEL" || n == "AWS_SECRET_ACCESS_KEY" {
			t.Errorf("parent environment variable %s reached the child", n)
		}
	}
	args := strings.Join(c.Args, " ")
	for _, want := range []string{"cat config", "--no-cache", "rclone.program=rclone-test-program", "rclone.args=serve restic --stdio", "--retry-lock 30s"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q lack %q", args, want)
		}
	}
	for _, bad := range []string{testPassword, "bypass-governance", "--password", "-r "} {
		if strings.Contains(args, bad) {
			t.Errorf("args contain %q", bad)
		}
	}
}

func TestCatConfigErrorMapping(t *testing.T) {
	cases := []struct {
		name  string
		rule  fakeRule
		code  string
		class restic.ErrorClass
	}{
		{"repo missing", fakeRule{Cmd: "cat", Exit: 10, Stderr: "exit10_repo_missing.stderr.jsonl"}, adminpath.CodeRepoMissing, restic.ClassRepoMissing},
		{"wrong password", fakeRule{Cmd: "cat", Exit: 12, Stderr: "exit12_wrong_password_plain.stderr.txt"}, adminpath.CodeWrongPassword, restic.ClassWrongPassword},
		{"locked", fakeRule{Cmd: "cat", Exit: 11, Stderr: "exit11_lock.stderr.jsonl"}, adminpath.CodeLocked, restic.ClassLock},
		{"network", fakeRule{Cmd: "cat", Exit: 1, Stderr: "inline:dial tcp 10.0.0.1:443: connect: connection refused\n"}, adminpath.CodeStorageUnreachable, restic.ClassNetwork},
		{"tls", fakeRule{Cmd: "cat", Exit: 1, Stderr: "inline:x509: certificate signed by unknown authority\n"}, adminpath.CodeStorageUnreachable, restic.ClassTLS},
		{"storage 503", fakeRule{Cmd: "cat", Exit: 1, Stderr: "inline:server returned 503 service unavailable\n"}, adminpath.CodeStorageUnreachable, restic.ClassStorage},
		{"permission", fakeRule{Cmd: "cat", Exit: 1, Stderr: "inline:permission denied\n"}, adminpath.CodeResticFailed, restic.ClassPermission},
		{"unknown", fakeRule{Cmd: "cat", Exit: 1, Stderr: "inline:something odd\n"}, adminpath.CodeResticFailed, restic.ClassUnknown},
		{"unparseable stdout", fakeRule{Cmd: "cat", Exit: 0, Stdout: "inline:not json at all"}, adminpath.CodeResticFailed, restic.ClassParse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTenv(t, []fakeRule{tc.rule}, nil)
			_, err := e.w.CatConfig(e.ctx, dev(e.esc))
			wantCode(t, err, tc.code, tc.class)
		})
	}
}

func TestTimeoutKillsTheChild(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "cat", SleepMS: 20000, Stdout: "cat_config.json"}}, func(c *Config) {
		c.Timeouts.CatConfig = 400 * time.Millisecond
	})
	start := time.Now()
	_, err := e.w.CatConfig(e.ctx, dev(e.esc))
	wantCode(t, err, adminpath.CodeTimeout, "")
	if d := time.Since(start); d > 8*time.Second {
		t.Fatalf("timeout took %v", d)
	}
}

func TestInit(t *testing.T) {
	t.Run("creates", func(t *testing.T) {
		e := newTenv(t, []fakeRule{{Cmd: "init", Stdout: "init.json"}}, nil)
		res, err := e.w.Init(e.ctx, adminpath.InitRequest{DeviceRequest: dev(e.esc)})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Created || res.RepositoryID == "" || res.Outcome != restic.OutcomeSuccess || res.ExitCode != 0 {
			t.Fatalf("result = %+v", res)
		}
		calls := e.repoCalls()
		if len(calls) != 1 || calls[0].Args[0] != "init" || calls[0].Args[1] != "--json" {
			t.Fatalf("calls = %+v", calls)
		}
		if n := len(e.consoleRuns(e.esc)); n != 0 {
			t.Errorf("init recorded %d console runs", n)
		}
	})
	t.Run("idempotent", func(t *testing.T) {
		e := newTenv(t, []fakeRule{
			{Cmd: "init", Exit: 1, Stderr: `inline:{"message_type":"exit_error","code":1,"message":"Fatal: create repository failed: config file already exists"}` + "\n"},
			{Cmd: "cat", Stdout: "cat_config.json"},
		}, nil)
		res, err := e.w.Init(e.ctx, adminpath.InitRequest{DeviceRequest: dev(e.esc)})
		if err != nil {
			t.Fatal(err)
		}
		if res.Created || res.RepositoryID == "" || res.Outcome != restic.OutcomeSuccess {
			t.Fatalf("result = %+v", res)
		}
		if got := len(e.repoCalls()); got != 2 {
			t.Fatalf("restic ran %d times, want init then cat config", got)
		}
	})
	t.Run("existing repository, wrong key", func(t *testing.T) {
		e := newTenv(t, []fakeRule{
			{Cmd: "init", Exit: 1, Stderr: "inline:Fatal: config file already exists\n"},
			{Cmd: "cat", Exit: 12, Stderr: "exit12_wrong_password_plain.stderr.txt"},
		}, nil)
		res, err := e.w.Init(e.ctx, adminpath.InitRequest{DeviceRequest: dev(e.esc)})
		if err != nil {
			t.Fatal(err)
		}
		if res.Created || res.Outcome != restic.OutcomeFailed || res.ErrorClass != restic.ClassWrongPassword || res.ExitCode != 12 {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("storage down", func(t *testing.T) {
		down := "inline:dial tcp 10.0.0.1:443: connect: connection refused\n"
		e := newTenv(t, []fakeRule{{Cmd: "init", Exit: 1, Stderr: down}, {Cmd: "cat", Exit: 1, Stderr: down}}, nil)
		res, err := e.w.Init(e.ctx, adminpath.InitRequest{DeviceRequest: dev(e.esc)})
		if err != nil {
			t.Fatal(err)
		}
		if res.Created || res.Outcome != restic.OutcomeDeferred || res.DeferredReason != restic.DeferredGateway || res.ErrorClass != restic.ClassNetwork {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("customer held", func(t *testing.T) {
		e := newTenv(t, []fakeRule{{Cmd: "init", Stdout: "init.json"}}, nil)
		_, err := e.w.Init(e.ctx, adminpath.InitRequest{DeviceRequest: dev(e.held)})
		wantCode(t, err, adminpath.CodeCustodyCustomerHeld, "")
	})
}

func TestLs(t *testing.T) {
	e := newTenv(t, []fakeRule{
		{Cmd: "ls", Contains: "/C/fixture/tree", Stdout: "ls.jsonl"},
		{Cmd: "ls", Stdout: "ls_roots.jsonl"},
	}, nil)
	const snap = "c4709fdaf86559b31287cd581af7076550d83b73b567458d851dcbdbb8d975cd"

	res, err := e.w.Ls(e.ctx, adminpath.LsRequest{DeviceRequest: dev(e.esc), SnapshotID: snap, Dir: "/C/fixture/tree"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, n := range res.Nodes {
		names = append(names, n.Name)
	}
	if strings.Join(names, ",") != "invoices,media,notes" {
		t.Fatalf("nodes = %v (the directory's own node must be dropped)", names)
	}
	if res.Truncated || res.Snapshot.ID == "" || res.Outcome != restic.OutcomeSuccess {
		t.Fatalf("result = %+v", res)
	}

	res, err = e.w.Ls(e.ctx, adminpath.LsRequest{DeviceRequest: dev(e.esc), SnapshotID: snap, Dir: "/C/fixture/tree", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Nodes) != 2 || !res.Truncated {
		t.Fatalf("limit 2: %d nodes, truncated %v", len(res.Nodes), res.Truncated)
	}

	res, err = e.w.Ls(e.ctx, adminpath.LsRequest{DeviceRequest: dev(e.esc), SnapshotID: snap, Dir: "/"})
	if err != nil || len(res.Nodes) != 1 || res.Nodes[0].Name != "C" {
		t.Fatalf("roots = %+v, %v", res.Nodes, err)
	}
	calls := e.repoCalls()
	if got := calls[2].Args; got[0] != "ls" || got[3] != "/" {
		t.Fatalf("roots args = %v, want the directory argument \"/\"", got)
	}
	if got := calls[0].Args; got[3] != "/C/fixture/tree" {
		t.Fatalf("args = %v", got)
	}

	e.scenario([]fakeRule{{Cmd: "ls", Exit: 1, Stderr: "inline:Fatal: no matching ID found for prefix\n"}})
	_, err = e.w.Ls(e.ctx, adminpath.LsRequest{DeviceRequest: dev(e.esc), SnapshotID: snap, Dir: "/"})
	wantCode(t, err, adminpath.CodeResticFailed, "")
}

func TestFindSnapshotsStats(t *testing.T) {
	e := newTenv(t, []fakeRule{
		{Cmd: "find", Stdout: "find.json"},
		{Cmd: "snapshots", Stdout: "snapshots.json"},
		{Cmd: "stats", Stdout: "stats_raw_data.json"},
	}, nil)

	found, err := e.w.Find(e.ctx, adminpath.FindRequest{DeviceRequest: dev(e.esc), Pattern: "*.docx", IgnoreCase: true, SnapshotIDs: []string{"latest"}})
	if err != nil || len(found) != 1 || len(found[0].Matches) != 1 {
		t.Fatalf("find = %+v, %v", found, err)
	}
	args := strings.Join(e.repoCalls()[0].Args, " ")
	for _, want := range []string{"find --json", "--ignore-case", "--snapshot latest", "*.docx"} {
		if !strings.Contains(args, want) {
			t.Errorf("find args %q lack %q", args, want)
		}
	}

	snaps, err := e.w.Snapshots(e.ctx, dev(e.esc))
	if err != nil || len(snaps) != 5 {
		t.Fatalf("snapshots = %d, %v", len(snaps), err)
	}

	st, err := e.w.Stats(e.ctx, adminpath.StatsRequest{DeviceRequest: dev(e.esc), Mode: restic.StatsRawData})
	if err != nil || st.TotalSize != 2111764 || st.SnapshotsCount != 5 {
		t.Fatalf("stats = %+v, %v", st, err)
	}
	if got := strings.Join(e.repoCalls()[2].Args, " "); !strings.Contains(got, "stats --json --mode raw-data") {
		t.Errorf("stats args = %q", got)
	}
	_, err = e.w.Stats(e.ctx, adminpath.StatsRequest{DeviceRequest: dev(e.esc), Mode: "debug"})
	wantCode(t, err, adminpath.CodeInvalidRequest, "")
}

func TestFindCapsMatches(t *testing.T) {
	many := `[{"hits":3,"snapshot":"aaaaaaaa","matches":[{"path":"/a"},{"path":"/b"},{"path":"/c"}]},{"hits":2,"snapshot":"bbbbbbbb","matches":[{"path":"/d"},{"path":"/e"}]}]`
	e := newTenv(t, []fakeRule{{Cmd: "find", Stdout: "inline:" + many}}, nil)
	got, err := e.w.Find(e.ctx, adminpath.FindRequest{DeviceRequest: dev(e.esc), Pattern: "x", Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, r := range got {
		total += len(r.Matches)
	}
	if total != 4 || len(got) != 2 || len(got[1].Matches) != 1 {
		t.Fatalf("capped find = %+v", got)
	}
}

func TestUnlock(t *testing.T) {
	t.Run("remove all needs quiesced", func(t *testing.T) {
		e := newTenv(t, []fakeRule{{Cmd: "unlock"}}, nil)
		_, err := e.w.Unlock(e.ctx, adminpath.UnlockRequest{DeviceRequest: dev(e.esc), RemoveAll: true})
		wantCode(t, err, adminpath.CodeInvalidRequest, "")
		if len(e.repoCalls()) != 0 || len(e.consoleRuns(e.esc)) != 0 {
			t.Fatal("a refused unlock ran restic or wrote a run")
		}
	})
	t.Run("stale only is recorded", func(t *testing.T) {
		e := newTenv(t, []fakeRule{{Cmd: "unlock"}}, nil)
		res, err := e.w.Unlock(e.ctx, adminpath.UnlockRequest{DeviceRequest: dev(e.esc)})
		if err != nil || res.Outcome != restic.OutcomeSuccess {
			t.Fatalf("result = %+v, %v", res, err)
		}
		if a := e.repoCalls()[0].Args; a[0] != "unlock" || strings.Contains(strings.Join(a, " "), "--remove-all") {
			t.Fatalf("args = %v", a)
		}
		runs := e.consoleRuns(e.esc)
		if len(runs) != 1 || runs[0].Kind != "unlock" || runs[0].Trigger != "manual" || runs[0].Outcome != "success" ||
			runs[0].ResticExitCode == nil || *runs[0].ResticExitCode != 0 || runs[0].FinishedAt == nil {
			t.Fatalf("runs = %+v", runs)
		}
	})
	t.Run("remove all quiesced", func(t *testing.T) {
		e := newTenv(t, []fakeRule{{Cmd: "unlock", Contains: "--remove-all"}}, nil)
		res, err := e.w.Unlock(e.ctx, adminpath.UnlockRequest{DeviceRequest: dev(e.esc), RemoveAll: true, Quiesced: true})
		if err != nil || res.Outcome != restic.OutcomeSuccess {
			t.Fatalf("result = %+v, %v", res, err)
		}
	})
	t.Run("lock is a deferral", func(t *testing.T) {
		e := newTenv(t, []fakeRule{{Cmd: "unlock", Exit: 11, Stderr: "exit11_lock.stderr.jsonl"}}, nil)
		res, err := e.w.Unlock(e.ctx, adminpath.UnlockRequest{DeviceRequest: dev(e.esc)})
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != restic.OutcomeDeferred || res.DeferredReason != restic.DeferredLock || res.ExitCode != 11 {
			t.Fatalf("result = %+v", res)
		}
		runs := e.consoleRuns(e.esc)
		if len(runs) != 1 || runs[0].Outcome != "deferred" || runs[0].DeferredReason != "lock" {
			t.Fatalf("runs = %+v", runs)
		}
	})
	t.Run("timeout closes the run", func(t *testing.T) {
		e := newTenv(t, []fakeRule{{Cmd: "unlock", SleepMS: 20000}}, func(c *Config) { c.Timeouts.Unlock = 300 * time.Millisecond })
		_, err := e.w.Unlock(e.ctx, adminpath.UnlockRequest{DeviceRequest: dev(e.esc)})
		wantCode(t, err, adminpath.CodeTimeout, "")
		runs := e.consoleRuns(e.esc)
		if len(runs) != 1 || runs[0].FinishedAt == nil || runs[0].Outcome != "interrupted" {
			t.Fatalf("runs = %+v", runs)
		}
	})
	t.Run("customer held", func(t *testing.T) {
		e := newTenv(t, []fakeRule{{Cmd: "unlock"}}, nil)
		_, err := e.w.Unlock(e.ctx, adminpath.UnlockRequest{DeviceRequest: dev(e.held)})
		wantCode(t, err, adminpath.CodeCustodyCustomerHeld, "")
		if len(e.consoleRuns(e.held)) != 0 {
			t.Fatal("a run was recorded for a refused device")
		}
	})
}

func TestM5OperationsAreUnavailable(t *testing.T) {
	e := newTenv(t, nil, nil)
	for name, err := range map[string]error{
		"forget": func() error { _, err := e.w.Forget(e.ctx, adminpath.ForgetRequest{}); return err }(),
		"prune":  func() error { _, err := e.w.Prune(e.ctx, adminpath.PruneRequest{}); return err }(),
		"check":  func() error { _, err := e.w.Check(e.ctx, adminpath.CheckRequest{}); return err }(),
	} {
		wantCode(t, err, adminpath.CodeUnavailable, "")
		if !strings.Contains(err.Error(), "M5") {
			t.Errorf("%s: message does not name M5: %v", name, err)
		}
	}
	if len(e.repoCalls()) != 0 {
		t.Fatal("an M5 stub ran restic")
	}
}

// TestNoRawOutputRetained feeds the sentinel path of architecture §8.4 through
// stderr and stdout and requires that no log line and no error carries it.
func TestNoRawOutputRetained(t *testing.T) {
	const sentinel = `C:\fixture\PHI-sentinel\patient-record.docx`
	leak := `{"message_type":"error","error":{"message":"open ` + `\\\\?\\C:\\fixture\\PHI-sentinel\\patient-record.docx: The process cannot access the file"},"during":"archival","item":"C:\\fixture\\PHI-sentinel\\patient-record.docx"}` + "\n" +
		"Fatal: cannot read C:\\fixture\\PHI-sentinel\\patient-record.docx: permission denied\n"
	e := newTenv(t, []fakeRule{
		{Cmd: "snapshots", Exit: 1, Stderr: "inline:" + leak},
		{Cmd: "ls", Stdout: "inline:" + `{"struct_type":"snapshot","id":"aaaaaaaa","paths":["` + `C:\\fixture\\PHI-sentinel` + `"]}` + "\n"},
	}, nil)
	_, err := e.w.Snapshots(e.ctx, dev(e.esc))
	if err == nil || strings.Contains(err.Error(), "PHI-sentinel") {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.w.Ls(e.ctx, adminpath.LsRequest{DeviceRequest: dev(e.esc), SnapshotID: "latest", Dir: "/"}); err != nil {
		t.Fatal(err)
	}
	if out := e.logs.String(); strings.Contains(out, "PHI-sentinel") || strings.Contains(out, sentinel) || strings.Contains(out, "patient-record") {
		t.Fatalf("log output carries sentinel text:\n%s", out)
	}
	if out := e.logs.String(); !strings.Contains(out, "restic run finished") {
		t.Fatalf("expected a run line in the log:\n%s", out)
	}
}
