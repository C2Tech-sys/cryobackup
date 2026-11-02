package maintain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// This file holds the test doubles: a fake restic (the test binary itself,
// copied under the name "fakerestic" and replaying internal/restic fixtures), a
// fake KeyUnwrapper and a frozen clock.

// fakeName is the base name under which TestMain recognises its own copy.
const fakeName = "fakerestic"

var (
	fakeOnce sync.Once
	fakeDir  string
	fakePath string
	fakeSum  string
)

func TestMain(m *testing.M) {
	if strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") == fakeName {
		os.Exit(runFakeRestic())
	}
	code := m.Run()
	if fakeDir != "" {
		_ = os.RemoveAll(fakeDir)
	}
	os.Exit(code)
}

// fakeResticPath returns the path of the fake restic executable and its SHA-256,
// creating it on first use.
func fakeResticPath(t testing.TB) (string, string) {
	t.Helper()
	fakeOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			t.Fatalf("os.Executable: %v", err)
		}
		dir, err := os.MkdirTemp("", "cryo-fake-")
		if err != nil {
			t.Fatal(err)
		}
		name := fakeName
		if filepath.Ext(exe) == ".exe" {
			name += ".exe"
		}
		dst := filepath.Join(dir, name)
		data, err := os.ReadFile(exe)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		fakeDir, fakePath, fakeSum = dir, dst, hex.EncodeToString(sum[:])
	})
	if fakePath == "" {
		t.Fatal("fake restic was not created")
	}
	return fakePath, fakeSum
}

// fakeRule is one scripted answer of the fake restic.
type fakeRule struct {
	// Cmd is the first argument ("version", "init", "cat", "ls", "dump", ...).
	Cmd string `json:"cmd"`
	// Contains, when set, must occur in the space-joined arguments.
	Contains string `json:"contains,omitempty"`
	Exit     int    `json:"exit"`
	// Stdout and Stderr name a fixture file, or hold "inline:<text>".
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	// SleepMS delays the answer (for the timeout test).
	SleepMS int `json:"sleep_ms,omitempty"`
	// WriteTarget is written to the file named by --target.
	WriteTarget string `json:"write_target,omitempty"`
	// StdoutFile names a file (absolute path) whose bytes go to stdout after
	// Stdout: a binary dump stream.
	StdoutFile string `json:"stdout_file,omitempty"`
	// SleepAfterMS delays the exit after the output was written (a long dump).
	SleepAfterMS int `json:"sleep_after_ms,omitempty"`
}

type fakeScenario struct {
	Fixtures string     `json:"fixtures"`
	Rules    []fakeRule `json:"rules"`
}

// fakeCall is one recorded invocation.
type fakeCall struct {
	Args     []string `json:"args"`
	EnvNames []string `json:"env_names"`
	Repo     string   `json:"repo"`
	Password string   `json:"password"`
	RClone   string   `json:"rclone_config"`
	Cache    string   `json:"cache"`
	// CacheSet is whether RESTIC_CACHE_DIR was in the environment at all.
	CacheSet bool   `json:"cache_set"`
	TmpDir   string `json:"tmpdir"`
	RTemp    string `json:"rclone_temp_dir"`
	RCache   string `json:"rclone_cache_dir"`
}

func runFakeRestic() int {
	// The harness directory holds rclone.conf (Config.RcloneConfig), the
	// scenario and the call log; it is not one of the worker's directories.
	state := filepath.Dir(os.Getenv("RCLONE_CONFIG"))
	args := os.Args[1:]
	call := fakeCall{
		Args:     args,
		Repo:     os.Getenv("RESTIC_REPOSITORY"),
		Password: os.Getenv("RESTIC_PASSWORD"),
		RClone:   os.Getenv("RCLONE_CONFIG"),
		Cache:    os.Getenv("RESTIC_CACHE_DIR"),
		TmpDir:   os.Getenv("TMPDIR"),
		RTemp:    os.Getenv("RCLONE_TEMP_DIR"),
		RCache:   os.Getenv("RCLONE_CACHE_DIR"),
	}
	_, call.CacheSet = os.LookupEnv("RESTIC_CACHE_DIR")
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			call.EnvNames = append(call.EnvNames, strings.ToUpper(kv[:i]))
		}
	}
	if f, err := os.OpenFile(filepath.Join(state, "fake.calls"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		b, _ := json.Marshal(call)
		_, _ = f.Write(append(b, '\n'))
		_ = f.Close()
	}
	raw, err := os.ReadFile(filepath.Join(state, "fake.json"))
	if err != nil {
		os.Stderr.WriteString("fake: no scenario\n")
		return 99
	}
	var sc fakeScenario
	if err := json.Unmarshal(raw, &sc); err != nil {
		os.Stderr.WriteString("fake: bad scenario\n")
		return 99
	}
	joined := strings.Join(args, " ")
	for _, r := range sc.Rules {
		if len(args) == 0 || r.Cmd != args[0] || (r.Contains != "" && !strings.Contains(joined, r.Contains)) {
			continue
		}
		if r.SleepMS > 0 {
			time.Sleep(time.Duration(r.SleepMS) * time.Millisecond)
		}
		emit := func(dst *os.File, spec string) {
			switch {
			case spec == "":
			case strings.HasPrefix(spec, "inline:"):
				_, _ = io.WriteString(dst, strings.TrimPrefix(spec, "inline:"))
			default:
				if b, err := os.ReadFile(filepath.Join(sc.Fixtures, spec)); err == nil {
					_, _ = dst.Write(b)
				}
			}
		}
		emit(os.Stdout, r.Stdout)
		if r.StdoutFile != "" {
			if b, err := os.ReadFile(r.StdoutFile); err == nil {
				_, _ = os.Stdout.Write(b)
			}
		}
		emit(os.Stderr, r.Stderr)
		if r.SleepAfterMS > 0 {
			time.Sleep(time.Duration(r.SleepAfterMS) * time.Millisecond)
		}
		if r.WriteTarget != "" {
			for i, a := range args {
				if a == "--target" && i+1 < len(args) {
					_ = os.WriteFile(args[i+1], []byte(r.WriteTarget), 0o600)
				}
			}
		}
		return r.Exit
	}
	os.Stderr.WriteString("fake: no rule for " + args[0] + "\n")
	return 99
}

// fakeKeys is a KeyUnwrapper that returns a fixed password.
type fakeKeys struct {
	mu    sync.Mutex
	pw    string
	err   error
	calls []string
}

func (k *fakeKeys) Unwrap(_ context.Context, deviceID string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls = append(k.calls, deviceID)
	if k.err != nil {
		return "", k.err
	}
	return k.pw, nil
}

func (k *fakeKeys) called() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.calls...)
}

// fakeClock is a frozen, advanceable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var t0 = time.Date(2026, 10, 22, 13, 42, 10, 0, time.UTC)

// tenv is one test world: a fixtures-seeded store, the fake restic scripted by
// a scenario, and a Worker over them.
type tenv struct {
	t        *testing.T
	ctx      context.Context
	st       *store.Store
	clk      *fakeClock
	w        *Worker
	cfg      Config
	keys     *fakeKeys
	state    string // the test harness: rclone.conf, fake.json, fake.calls
	scratch  string // Config.ScratchDir
	fixtures string
	logs     *bytes.Buffer
	esc      string // an escrowed device id
	held     string // a customer-held device id
}

const testPassword = "fake-repo-password-0123456789abcdef"

// newTenv builds the world with the given rules (a version rule is added when
// missing) and applies mut to the Config before New.
func newTenv(t *testing.T, rules []fakeRule, mut func(*Config)) *tenv {
	t.Helper()
	exe, sum := fakeResticPath(t)
	fixtures, err := filepath.Abs(filepath.Join("..", "..", "restic", "testdata", "v0.19.1"))
	if err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: t0}
	ctx := context.Background()
	st, err := store.OpenMemory(ctx, clk.Now)
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.LoadFixtures(ctx, st, t0); err != nil {
		t.Fatalf("LoadFixtures: %v", err)
	}
	e := &tenv{
		t: t, ctx: ctx, st: st, clk: clk, fixtures: fixtures,
		keys:    &fakeKeys{pw: testPassword},
		state:   filepath.Join(t.TempDir(), "harness"),
		scratch: filepath.Join(t.TempDir(), "scratch"),
		logs:    &bytes.Buffer{},
	}
	if err := os.MkdirAll(e.state, 0o700); err != nil {
		t.Fatal(err)
	}
	e.esc = e.deviceID("FRONTDESK-01")
	e.held = e.heldDeviceID()
	e.scenario(rules)
	e.cfg = Config{
		SocketPath:    "",
		ResticProgram: exe,
		ResticSHA256:  sum,
		RcloneProgram: "rclone-test-program",
		RcloneConfig:  filepath.Join(e.state, "rclone.conf"),
		Remote:        "admin",
		Bucket:        "test-bucket",
		Prefix:        "pre",
		ScratchDir:    e.scratch,
		Logger:        logx.New(e.logs, logx.LevelDebug),
	}
	if mut != nil {
		mut(&e.cfg)
	}
	w, err := New(e.cfg, st, e.keys, clk.Now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e.w = w
	return e
}

// scenario (re)writes the fake restic's script.
func (e *tenv) scenario(rules []fakeRule) {
	e.t.Helper()
	has := false
	for _, r := range rules {
		if r.Cmd == "version" {
			has = true
		}
	}
	if !has {
		rules = append([]fakeRule{{Cmd: "version", Stdout: "version.json"}}, rules...)
	}
	b, err := json.Marshal(fakeScenario{Fixtures: e.fixtures, Rules: rules})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.state, "fake.json"), b, 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// calls returns the recorded fake invocations, oldest first.
func (e *tenv) calls() []fakeCall {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.state, "fake.calls"))
	if err != nil {
		return nil
	}
	var out []fakeCall
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var c fakeCall
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			e.t.Fatalf("bad call record: %v", err)
		}
		out = append(out, c)
	}
	return out
}

// repoCalls are the calls after the start-up version check.
func (e *tenv) repoCalls() []fakeCall {
	var out []fakeCall
	for _, c := range e.calls() {
		if len(c.Args) > 0 && c.Args[0] != "version" {
			out = append(out, c)
		}
	}
	return out
}

func (e *tenv) deviceID(host string) string {
	e.t.Helper()
	l, err := e.st.ListDevices(e.ctx, store.ScopeAll(), store.ListDevicesOptions{Search: host})
	if err != nil || len(l.Items) == 0 {
		e.t.Fatalf("device %s: %v", host, err)
	}
	for _, d := range l.Items {
		if d.Hostname == host {
			return d.ID
		}
	}
	e.t.Fatalf("device %s not found", host)
	return ""
}

func (e *tenv) heldDeviceID() string {
	e.t.Helper()
	cs, err := e.st.ListCustomers(e.ctx, store.ScopeAll())
	if err != nil {
		e.t.Fatal(err)
	}
	for _, c := range cs {
		if c.KeyCustody != "customer_held" {
			continue
		}
		l, err := e.st.ListDevices(e.ctx, store.ScopeAll(), store.ListDevicesOptions{CustomerID: c.ID})
		if err != nil || len(l.Items) == 0 {
			e.t.Fatalf("customer-held devices: %v", err)
		}
		return l.Items[0].ID
	}
	e.t.Fatal("no customer-held customer in the fixtures")
	return ""
}

// consoleRuns lists the origin=console runs of a device that this test started
// (the fixtures already carry older console runs).
func (e *tenv) consoleRuns(deviceID string) []store.Run {
	e.t.Helper()
	l, err := e.st.ListRuns(e.ctx, store.ScopeAll(), deviceID, store.Page{Limit: 1000})
	if err != nil {
		e.t.Fatal(err)
	}
	var out []store.Run
	for _, r := range l.Items {
		if r.Origin == "console" && !r.StartedAt.Before(t0) {
			out = append(out, r)
		}
	}
	return out
}

// onlyDevice disables maintenance for every device except id.
func (e *tenv) onlyDevice(id string) {
	e.t.Helper()
	err := e.st.Write(e.ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE maintenance_schedule SET enabled = 0 WHERE device_id <> ?`, id)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE maintenance_schedule SET enabled = 1 WHERE device_id = ?`, id)
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
}
