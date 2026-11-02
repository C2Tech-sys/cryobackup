package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

func run(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Main(args, IO{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb})
	return code, out.String(), errb.String()
}

func initArgs(dir string, extra ...string) []string {
	return append([]string{"init", "--config", filepath.Join(dir, "config.json"), "--data-dir", filepath.Join(dir, "data"), "--non-interactive",
		"--endpoint", "s3.example.invalid", "--bucket", "b", "--gateway-url", "https://gw.example.invalid:9443", "--owner", "owner"}, extra...)
}

func TestVersionUsageAndUnknown(t *testing.T) {
	if code, out, _ := run(t, "", "version"); code != 0 || !strings.HasPrefix(out, "cryo-console ") {
		t.Fatalf("version: %d %q", code, out)
	}
	if code, _, errb := run(t, ""); code != ExitUsage || !strings.Contains(errb, "usage: cryo-console") {
		t.Fatalf("no args: %d", code)
	}
	if code, _, errb := run(t, "", "frobnicate"); code != ExitUsage || !strings.Contains(errb, "unknown command") {
		t.Fatalf("unknown: %d", code)
	}
	// maintain is wired: without --loop or --once it is a usage error from the worker itself.
	if code, _, errb := run(t, "", "maintain"); code != ExitUsage || !strings.Contains(errb, "exactly one of --loop and --once") {
		t.Fatalf("maintain: %d %s", code, errb)
	}
}

func TestInitCreatesEverythingAndRefusesToFinishWithoutTheKeyCopyConfirmation(t *testing.T) {
	dir := t.TempDir()
	code, _, errb := run(t, "", initArgs(dir)...)
	if code != ExitFailed || !strings.Contains(errb, "--master-key-copy-confirmed") {
		t.Fatalf("unconfirmed non-interactive init: %d %s", code, errb)
	}
	if fileExists(filepath.Join(dir, "config.json")) {
		t.Fatal("init must not write config.json until the operator confirms a key copy")
	}
	code, out, errb := run(t, "", initArgs(dir, "--master-key-copy-confirmed")...)
	if code != 0 {
		t.Fatalf("init: %d\n%s\n%s", code, out, errb)
	}
	if !regexp.MustCompile(`Password \(shown once[^)]*\): \S{20}`).MatchString(out) {
		t.Fatalf("owner password not printed once:\n%s", out)
	}
	keyPath := filepath.Join(dir, "data", "master.key")
	if st, err := os.Stat(keyPath); err != nil || st.Size() < 40 {
		t.Fatalf("master key: %v", err)
	}
	if st, _ := os.Stat(filepath.Join(dir, "config.json")); st == nil {
		t.Fatal("config.json")
	} else if os.PathSeparator == '/' && st.Mode().Perm() != 0o600 {
		t.Fatalf("config.json mode %v", st.Mode())
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if strings.Contains(string(raw), "BEGIN") || strings.Contains(out, "AAAAAAAA") {
		t.Fatal("no secret in the config")
	}
	// Database content: default policy, a default storage target, an owner with an argon2id hash.
	ctx := context.Background()
	st, err := store.Open(ctx, store.Options{Path: filepath.Join(dir, "data", "console.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if p, err := st.DefaultPolicy(ctx); err != nil || p.Scope != "default" {
		t.Fatalf("default policy: %v", err)
	}
	if tgt, err := st.DefaultStorageTarget(ctx); err != nil || tgt.Bucket != "b" || tgt.CredentialRef == "" {
		t.Fatalf("storage target: %+v %v", tgt, err)
	}
	a, err := st.GetAdminByUsername(ctx, "owner")
	if err != nil || a.Role != "owner" || !strings.HasPrefix(a.PasswordHash, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("owner: %+v %v", a, err)
	}
	pw := regexp.MustCompile(`Password \(shown once[^)]*\): (\S+)`).FindStringSubmatch(out)[1]
	if ok, _ := auth.VerifyPassword(a.PasswordHash, pw); !ok {
		t.Fatal("the printed password must be the owner's")
	}
	// A second init refuses to overwrite, and never replaces the master key.
	before, _ := os.ReadFile(keyPath)
	if code, _, errb := run(t, "", initArgs(dir, "--master-key-copy-confirmed")...); code != ExitFailed || !strings.Contains(errb, "already exists") {
		t.Fatalf("second init: %d %s", code, errb)
	}
	if code, out, _ := run(t, "", initArgs(dir, "--master-key-copy-confirmed", "--force")...); code != 0 || !strings.Contains(out, "found") || !strings.Contains(out, "already exists; its password is unchanged") {
		t.Fatalf("forced re-init must keep the key and the owner: %d\n%s", code, out)
	}
	if after, _ := os.ReadFile(keyPath); !bytes.Equal(before, after) {
		t.Fatal("init must never overwrite the master key")
	}
}

func TestInitInteractiveConfirmation(t *testing.T) {
	dir := t.TempDir()
	args := []string{"init", "--config", filepath.Join(dir, "config.json"), "--data-dir", filepath.Join(dir, "data"), "--endpoint", "s3.example.invalid", "--bucket", "b"}
	if code, _, errb := run(t, strings.Repeat("\n", 9)+"no\n", args...); code != ExitFailed || !strings.Contains(errb, "not confirmed") {
		t.Fatalf("answering no: %d %s", code, errb)
	}
	if fileExists(filepath.Join(dir, "config.json")) {
		t.Fatal("no config without the confirmation")
	}
	if code, out, errb := run(t, strings.Repeat("\n", 9)+"yes\n", args...); code != 0 {
		t.Fatalf("answering yes: %d\n%s\n%s", code, out, errb)
	}
}

func TestTokenAndUserCommands(t *testing.T) {
	dir := t.TempDir()
	if code, _, e := run(t, "", initArgs(dir, "--master-key-copy-confirmed")...); code != 0 {
		t.Fatal(e)
	}
	cfg := filepath.Join(dir, "config.json")
	code, out, errb := run(t, "", "token", "--config", cfg, "--create-customer", "Acme Dental", "--uses", "3", "--expires", "36h", "--label", "q4")
	if code != 0 || !regexp.MustCompile(`never stored\):\n[A-Za-z0-9_-]{43}\n`).MatchString(out) || !strings.Contains(out, "acme-dental") || !strings.Contains(out, "Uses:      3") {
		t.Fatalf("token: %d\n%s\n%s", code, out, errb)
	}
	if code, out, _ := run(t, "", "token", "--config", cfg, "--customer", "acme-dental"); code != 0 || !strings.Contains(out, "unlimited") {
		t.Fatalf("token for an existing customer: %d %s", code, out)
	}
	for name, args := range map[string][]string{
		"no customer":        {"token", "--config", cfg},
		"both":               {"token", "--config", cfg, "--customer", "a", "--create-customer", "b"},
		"unknown customer":   {"token", "--config", cfg, "--customer", "nobody"},
		"bad expiry":         {"token", "--config", cfg, "--customer", "acme-dental", "--expires", "soon"},
		"unknown policy":     {"token", "--config", cfg, "--customer", "acme-dental", "--policy", "nope"},
		"missing config":     {"token", "--config", filepath.Join(dir, "none.json"), "--customer", "x"},
		"duplicate customer": {"token", "--config", cfg, "--create-customer", "Acme Dental"},
	} {
		if code, _, _ := run(t, "", args...); code == 0 {
			t.Errorf("%s must fail", name)
		}
	}
	// Users.
	code, out, _ = run(t, "", "user", "add", "--config", cfg, "--username", "tech1", "--role", "operator")
	if code != 0 || !strings.Contains(out, "Password (shown once)") {
		t.Fatalf("user add: %d %s", code, out)
	}
	if code, _, _ := run(t, "", "user", "add", "--config", cfg, "--username", "tech1"); code == 0 {
		t.Fatal("a duplicate username must fail")
	}
	if code, _, _ := run(t, "", "user", "add", "--config", cfg, "--username", "x", "--role", "emperor"); code != ExitUsage {
		t.Fatal("a bad role is a usage error")
	}
	if code, _, _ := run(t, "short\n", "user", "add", "--config", cfg, "--username", "tech2", "--password-stdin"); code == 0 {
		t.Fatal("a short password from stdin must be refused")
	}
	if code, out, _ := run(t, "a-long-enough-password\n", "user", "add", "--config", cfg, "--username", "tech3", "--password-stdin"); code != 0 || strings.Contains(out, "a-long-enough-password") {
		t.Fatalf("stdin password must not be echoed: %d %s", code, out)
	}
	code, out, _ = run(t, "", "user", "list", "--config", cfg)
	if code != 0 || !strings.Contains(out, "tech1") || !strings.Contains(out, "owner") || strings.Contains(out, "argon2") {
		t.Fatalf("list: %d %s", code, out)
	}
	code, out, _ = run(t, "", "user", "passwd", "--config", cfg, "--username", "tech1")
	pw := regexp.MustCompile(`New password \(shown once\): (\S+)`).FindStringSubmatch(out)
	if code != 0 || pw == nil {
		t.Fatalf("passwd: %d %s", code, out)
	}
	ctx := context.Background()
	c2, _ := loadConfig(cfg)
	st, _ := store.Open(ctx, store.Options{Path: c2.DBPath})
	a, _ := st.GetAdminByUsername(ctx, "tech1")
	if ok, _ := auth.VerifyPassword(a.PasswordHash, pw[1]); !ok || !a.MustChangePassword {
		t.Fatal("passwd must set the printed password and require a change")
	}
	st.Close()
	if code, _, _ := run(t, "", "user", "disable", "--config", cfg, "--username", "tech1"); code != 0 {
		t.Fatal("disable")
	}
	_, out, _ = run(t, "", "user", "list", "--config", cfg)
	if !regexp.MustCompile(`tech1\s+operator\s+disabled`).MatchString(out) {
		t.Fatalf("disabled state:\n%s", out)
	}
	if code, _, _ := run(t, "", "user", "disable", "--config", cfg, "--username", "ghost"); code == 0 {
		t.Fatal("unknown account")
	}
	if code, _, _ := run(t, "", "user"); code != ExitUsage {
		t.Fatal("usage")
	}
	// Audit rows exist for the CLI actions and hold no secret.
	st, _ = store.Open(ctx, store.Options{Path: c2.DBPath})
	defer st.Close()
	evs, _ := st.ListAudit(ctx, store.ScopeAll(), store.AuditFilter{}, store.Page{Limit: 100})
	seen := map[string]bool{}
	for _, e := range evs.Items {
		seen[e.Action] = true
		if e.ActorType != "cli" && e.ActorType != "system" {
			continue
		}
		if strings.Contains(string(e.Details), pw[1]) {
			t.Fatal("secret in audit")
		}
	}
	for _, want := range []string{"console.init", "customer.create", "enrollment_token.create", "admin.create", "admin.password_reset", "admin.disable"} {
		if !seen[want] {
			t.Errorf("audit row %s missing", want)
		}
	}
}

func TestDoctor(t *testing.T) {
	dir := t.TempDir()
	if code, _, e := run(t, "", initArgs(dir, "--master-key-copy-confirmed", "--admin-listen", "127.0.0.1:0", "--agent-listen", "127.0.0.1:0")...); code != 0 {
		t.Fatal(e)
	}
	cfg := filepath.Join(dir, "config.json")
	code, out, _ := run(t, "", "doctor", "--config", cfg)
	// The config has no TLS certificate yet, so config validation fails loudly.
	if code != ExitFailed || !strings.Contains(out, "FAIL  config") || !strings.Contains(out, "tls.cert") {
		t.Fatalf("doctor on a config without TLS: %d\n%s", code, out)
	}
	for _, want := range []string{"OK    database", "OK    master key", "WARN  maintenance worker", "OK    gateway auth dir"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, dir) {
		t.Fatalf("doctor must not print paths:\n%s", out)
	}
	if code, _, _ := run(t, "", "doctor", "--config", filepath.Join(dir, "none.json")); code != ExitFailed {
		t.Fatal("missing config")
	}
}

// stopOnBanner is a stdout that cancels serve's context once the last banner
// line (the design check page) has been written, so serve builds, listens,
// prints and shuts down without a signal.
type stopOnBanner struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	cancel context.CancelFunc
}

func (s *stopOnBanner) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := s.buf.Write(p)
	if strings.Contains(s.buf.String(), "design check page:") {
		s.cancel()
	}
	return n, err
}

func (s *stopOnBanner) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// serveFixtures runs `serve --fixtures` to completion and returns its exit
// code, stdout and stderr.
func serveFixtures(t *testing.T, suppliedPassword string) (int, string, string) {
	t.Helper()
	t.Setenv("CRYO_FIXTURES_PASSWORD", suppliedPassword)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := serveContext
	serveContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	t.Cleanup(func() { serveContext = old })
	out, errb := &stopOnBanner{cancel: cancel}, &bytes.Buffer{}
	code := Main([]string{"serve", "--fixtures", "--admin-addr", "127.0.0.1:0"}, IO{Stdin: strings.NewReader(""), Stdout: out, Stderr: errb})
	return code, out.String(), errb.String()
}

func TestServeFixturesNeverEchoesASuppliedPassword(t *testing.T) {
	const supplied = "operator-chosen-Sup3rSecret"
	code, out, errb := serveFixtures(t, supplied)
	if code != ExitOK {
		t.Fatalf("serve: %d stdout=%q stderr=%q", code, out, errb)
	}
	if strings.Contains(out, supplied) || strings.Contains(errb, supplied) {
		t.Fatalf("the supplied fixtures password was echoed:\n%s\n%s", out, errb)
	}
	if !strings.Contains(out, "Sign in as admin with the password from CRYO_FIXTURES_PASSWORD") {
		t.Fatalf("missing the sign-in hint:\n%s", out)
	}
	if strings.Contains(out, "with password") {
		t.Fatalf("a password line was printed for a supplied password:\n%s", out)
	}
}

func TestServeFixturesPrintsAGeneratedPasswordOnce(t *testing.T) {
	code, out, errb := serveFixtures(t, "")
	if code != ExitOK {
		t.Fatalf("serve: %d stdout=%q stderr=%q", code, out, errb)
	}
	m := regexp.MustCompile(`Sign in as "admin" with password "([A-Za-z0-9]{16})"`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no generated password printed:\n%s", out)
	}
	if strings.Count(out, m[1]) != 1 {
		t.Fatalf("the generated password must be printed exactly once:\n%s", out)
	}
	if strings.Contains(out, "from CRYO_FIXTURES_PASSWORD\n") {
		t.Fatalf("the supplied-password hint was printed for a generated password:\n%s", out)
	}
}
