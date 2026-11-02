package server_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/cli"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
	"github.com/c2tech-sys/cryobackup/internal/console/server"
	"github.com/c2tech-sys/cryobackup/internal/console/ui"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// fixturesApp builds the console in fixtures mode: both route sets exist, so the
// routers can be inspected without a network.
func fixturesApp(t *testing.T, log *logx.Logger) *cli.App {
	t.Helper()
	cfg := config.Default()
	cfg.Resolve()
	cfg.Fixtures = true
	cfg.AdminListen = "127.0.0.1:0"
	a, err := cli.Build(context.Background(), cfg, cli.BuildOptions{Log: log, FixturesPassword: "fixtures-pass-1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

// The M0 rows of architecture §5.1, §5.2 and §5.3.
var (
	agentM0 = []string{"POST /api/v1/agent/enroll", "GET /api/v1/agent/policy", "POST /api/v1/agent/heartbeat", "POST /api/v1/agent/runs",
		"POST /api/v1/agent/snapshots", "GET /api/v1/agent/commands", "POST /api/v1/agent/commands/{id}/result"}
	adminM0 = []string{
		// §5.2
		"POST /api/v1/auth/login", "POST /api/v1/auth/logout", "GET /api/v1/devices", "GET /api/v1/devices/{id}", "POST /api/v1/devices/{id}/restores",
		"GET /api/v1/restores/{id}", "GET /api/v1/restores/{id}/download", "GET /api/v1/version",
		// §5.3
		"GET /login", "POST /login", "GET /{$}", "GET /devices/{id}", "GET /devices/{id}/restore", "POST /ui/devices/{id}/restore",
		"GET /restores/{id}", "GET /ui/events", "GET /static/{path...}", "GET /healthz",
		// §13.1 fallback partials and /design (fixtures mode)
		"GET /ui/partials/board", "GET /ui/partials/run-progress/{run_id}", "GET /design", "GET /design/{component}",
	}
)

func set(ps []string) map[string]bool {
	m := map[string]bool{}
	for _, p := range ps {
		m[p] = true
	}
	return m
}

func TestTheTwoMuxesAreDisjointAndCoverTheM0Routes(t *testing.T) {
	a := fixturesApp(t, nil)
	agent, admin := a.Server.AgentPatterns(), a.Server.AdminPatterns()
	agentSet, adminSet := set(agent), set(admin)

	// Disjoint on the path (a method difference does not make two routes different).
	agentPaths, adminPaths := map[string]bool{}, map[string]bool{}
	for _, p := range agent {
		if p != server.CatchAll {
			agentPaths[server.PathOf(p)] = true
		}
	}
	for _, p := range admin {
		if p != server.CatchAll {
			adminPaths[server.PathOf(p)] = true
		}
	}
	for p := range agentPaths {
		if adminPaths[p] {
			t.Errorf("path %s is registered on both listeners", p)
		}
	}
	// The catch-all is the only pattern both have, and it is the not-found handler.
	if !agentSet[server.CatchAll] || !adminSet[server.CatchAll] {
		t.Fatal("each listener needs its own not-found handler")
	}

	// The agent listener serves only api.Pattern* routes.
	known := set(api.Patterns())
	for _, p := range agent {
		if p != server.CatchAll && !known[p] {
			t.Errorf("agent mux serves %q, which is not an api.Pattern", p)
		}
	}
	// No admin route lives under the agent prefix, and no agent route is outside it.
	for _, p := range admin {
		if strings.HasPrefix(server.PathOf(p), api.PathPrefix) {
			t.Errorf("admin mux serves agent route %q", p)
		}
	}
	for _, p := range agent {
		if p != server.CatchAll && !strings.HasPrefix(server.PathOf(p), api.PathPrefix) {
			t.Errorf("agent mux serves a route outside %s: %q", api.PathPrefix, p)
		}
	}
	// Route coverage: every M0 pattern is registered on the right listener.
	for _, p := range agentM0 {
		if !agentSet[p] {
			t.Errorf("agent M0 route %q is not registered", p)
		}
		if adminSet[p] {
			t.Errorf("agent route %q leaked onto the admin listener", p)
		}
	}
	for _, p := range adminM0 {
		if !adminSet[p] {
			t.Errorf("admin M0 route %q is not registered", p)
		}
		if agentSet[p] {
			t.Errorf("admin route %q leaked onto the agent listener", p)
		}
	}
	for _, p := range adminapi.Patterns() {
		if !adminSet[p] {
			t.Errorf("adminapi pattern %q is not on the admin mux", p)
		}
	}
	for _, p := range ui.Patterns(true) {
		if !adminSet[p] {
			t.Errorf("ui pattern %q is not on the admin mux", p)
		}
	}

	// Behaviour, not just names: an admin path on the agent handler is a problem+json 404, an agent path on the admin handler a plain 404.
	for _, tc := range []struct{ method, path string }{{"GET", "/login"}, {"GET", "/"}, {"GET", "/api/v1/devices"}, {"POST", "/api/v1/auth/login"}, {"GET", "/healthz"}, {"GET", "/static/app.css"}, {"GET", "/design"}} {
		w := httptest.NewRecorder()
		a.Server.AgentHandler().ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != 404 || w.Header().Get("Content-Type") != api.ContentTypeProblem {
			t.Errorf("admin path %s %s reachable on the agent listener: %d %s", tc.method, tc.path, w.Code, w.Header().Get("Content-Type"))
		}
	}
	for _, tc := range []struct{ method, path string }{{"POST", "/api/v1/agent/enroll"}, {"GET", "/api/v1/agent/policy"}, {"POST", "/api/v1/agent/heartbeat"}, {"POST", "/api/v1/agent/runs"}} {
		w := httptest.NewRecorder()
		a.Server.AdminHandler().ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != 404 {
			t.Errorf("agent path %s %s reachable on the admin listener: %d", tc.method, tc.path, w.Code)
		}
	}

	// The admin listener's catch-all: an unmatched /api/ path is RFC 9457
	// problem+json (code not_found) like the agent listener's; an unmatched UI
	// path may stay a plain 404 but is never JSON.
	for _, tc := range []struct{ method, path string }{{"GET", "/api/v1/nope"}, {"POST", "/api/v1/nope"}, {"GET", "/api/nope"}, {"GET", "/api/v1/devices/x/nope"}} {
		w := httptest.NewRecorder()
		a.Server.AdminHandler().ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != 404 || w.Header().Get("Content-Type") != api.ContentTypeProblem || !strings.Contains(w.Body.String(), `"code":"not_found"`) {
			t.Errorf("unmatched admin API path %s %s: %d %q %s", tc.method, tc.path, w.Code, w.Header().Get("Content-Type"), w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	a.Server.AdminHandler().ServeHTTP(w, httptest.NewRequest("GET", "/nope", nil))
	if w.Code != 404 || w.Header().Get("Content-Type") == api.ContentTypeProblem {
		t.Errorf("unmatched admin UI path: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
}

func TestAgentResponsesCarryServerTimeAndRequestID(t *testing.T) {
	a := fixturesApp(t, nil)
	for _, req := range []*http.Request{
		httptest.NewRequest("GET", "/api/v1/agent/policy", nil), // 401
		httptest.NewRequest("GET", "/nothing", nil),             // 404
		httptest.NewRequest("POST", "/api/v1/agent/enroll", nil),
	} {
		w := httptest.NewRecorder()
		a.Server.AgentHandler().ServeHTTP(w, req)
		ts := w.Header().Get(api.HeaderServerTime)
		got, err := api.ParseServerTime(ts)
		if err != nil || got.IsZero() {
			t.Errorf("%s: X-CryoBackup-Server-Time %q", req.URL.Path, ts)
		}
		if len(w.Header().Get(api.HeaderRequestID)) != 16 {
			t.Errorf("%s: request id %q", req.URL.Path, w.Header().Get(api.HeaderRequestID))
		}
	}
	// An inbound X-Request-Id is not trusted.
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set(api.HeaderRequestID, "attacker-chosen")
	w := httptest.NewRecorder()
	a.Server.AgentHandler().ServeHTTP(w, r)
	if w.Header().Get(api.HeaderRequestID) == "attacker-chosen" {
		t.Fatal("request ids are generated here")
	}
}

func TestAdminSecurityHeaders(t *testing.T) {
	a := fixturesApp(t, nil)
	w := httptest.NewRecorder()
	a.Server.AdminHandler().ServeHTTP(w, httptest.NewRequest("GET", "/login", nil))
	h := w.Header()
	csp := h.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'", "base-uri 'none'", "form-action 'self'", "object-src 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "script-src 'self' 'unsafe-inline'") || regexp.MustCompile(`https?://`).MatchString(csp) {
		t.Errorf("CSP too loose: %s", csp)
	}
	for k, v := range map[string]string{"Referrer-Policy": "same-origin", "X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Cache-Control": "no-store"} {
		if h.Get(k) != v {
			t.Errorf("%s = %q", k, h.Get(k))
		}
	}
	// The agent listener does not need (and does not get) the page headers.
	w = httptest.NewRecorder()
	a.Server.AgentHandler().ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if w.Header().Get("Content-Security-Policy") != "" {
		t.Error("CSP belongs on the admin listener")
	}
	// The page the CSP protects uses no inline script and no remote asset.
	w = httptest.NewRecorder()
	a.Server.AdminHandler().ServeHTTP(w, httptest.NewRequest("GET", "/login", nil))
	body := w.Body.String()
	for _, tag := range regexp.MustCompile(`(?i)<script[^>]*>`).FindAllString(body, -1) {
		if !strings.Contains(tag, "src=") {
			t.Errorf("inline script on the login page: %s", tag)
		}
	}
	if regexp.MustCompile(`(?i)(src|href)="https?://`).MatchString(body) {
		t.Error("remote asset on the login page")
	}
}

func TestHealthz(t *testing.T) {
	a := fixturesApp(t, nil)
	w := httptest.NewRecorder()
	a.Server.AdminHandler().ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"ok"`) || !strings.Contains(w.Body.String(), `"master_key":"loaded"`) || !strings.Contains(w.Body.String(), `"migrations":"current"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	// Without a master key the answer is 503 with the reason.
	cfg := config.Default()
	cfg.Resolve()
	cfg.Fixtures = true
	cfg.AdminListen = "127.0.0.1:0"
	s, err := server.New(cfg, server.Options{Health: func(context.Context) server.Health {
		return server.Health{Status: "degraded", Database: "ok", Migrations: "current", MasterKey: "unavailable"}
	}})
	if err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "unavailable") {
		t.Fatalf("degraded healthz: %d %s", w.Code, w.Body.String())
	}
}

func TestPanicIsRecoveredAndLogsNothingItCarried(t *testing.T) {
	var buf bytes.Buffer
	log := logx.New(&buf, logx.LevelDebug)
	cfg := config.Default()
	cfg.Resolve()
	cfg.Fixtures = true
	cfg.AdminListen = "127.0.0.1:0"
	s, err := server.New(cfg, server.Options{Log: log, RegisterAdmin: func(r httpx.Registrar) {
		r.Handle("GET /boom", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(`secret C:\Users\jdoe\patients.db`) }))
	}})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(w, httptest.NewRequest("GET", "/boom?token=abc123", nil))
	if w.Code != 500 || w.Header().Get("Content-Type") != api.ContentTypeProblem {
		t.Fatalf("%d %s", w.Code, w.Header().Get("Content-Type"))
	}
	out := buf.String()
	if strings.Contains(out, "jdoe") || strings.Contains(out, "patients") || strings.Contains(w.Body.String(), "jdoe") {
		t.Fatalf("the panic value leaked: %s", out)
	}
	if !strings.Contains(out, "handler panic") || !strings.Contains(out, `"status":500`) {
		t.Fatalf("panic and request must be logged: %s", out)
	}
	if strings.Contains(out, "abc123") || strings.Contains(out, "token=") {
		t.Fatalf("the query string must never be logged: %s", out)
	}
}

func TestAccessLogHasThePatternAndNeverTheQuery(t *testing.T) {
	var buf bytes.Buffer
	a := fixturesApp(t, logx.New(&buf, logx.LevelDebug))
	for _, p := range []string{"/login?token=SECRETTOKEN&next=/Users/x", "/devices/01J9ZZ0000000000000000ABCD?token=SECRETTOKEN", "/api/v1/restores/01J9ZZ0000000000000000ABCD/download?token=SECRETTOKEN"} {
		a.Server.AdminHandler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", p, nil))
	}
	a.Server.AgentHandler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/v1/agent/policy?token=SECRETTOKEN", nil))
	out := buf.String()
	if strings.Contains(out, "SECRETTOKEN") || strings.Contains(out, "token=") || strings.Contains(out, "01J9ZZ0000000000000000ABCD") {
		t.Fatalf("access log leaked the query or a path value:\n%s", out)
	}
	for _, want := range []string{`"route":"GET__login"`, `"route":"GET__devices__id_"`, `"route":"GET__api_v1_agent_policy"`, `"status":`, `"took":`, `"request_id":`} {
		if !strings.Contains(out, want) {
			t.Errorf("access log lacks %s:\n%s", want, out)
		}
	}
}

func TestAdminBindRulesAndLimitedListener(t *testing.T) {
	cfg := config.Default()
	cfg.Resolve()
	cfg.Fixtures = true
	for addr, ok := range map[string]bool{"127.0.0.1:0": true, "10.1.2.3:8080": true, "[::1]:0": true, ":8080": false, "0.0.0.0:8080": false, "203.0.113.5:8080": false} {
		c := cfg
		c.AdminListen = addr
		_, err := server.New(c, server.Options{})
		if (err == nil) != ok {
			t.Errorf("admin %s: ok=%v err=%v", addr, ok, err)
		}
	}
	c := cfg
	c.AdminListen, c.AdminAllowPublic = "0.0.0.0:8080", true
	if _, err := server.New(c, server.Options{}); err != nil {
		t.Errorf("--admin-allow-public must permit it: %v", err)
	}
	// Plain HTTP on the agent listener: only on loopback or private, and only when asked for.
	c = cfg
	c.Fixtures = false
	c.AdminListen = "127.0.0.1:0"
	if _, err := server.New(c, server.Options{}); err == nil {
		t.Error("no TLS and no lab flag must refuse to start")
	}
	c.InsecureHTTPLab, c.AgentListen = true, "0.0.0.0:8443"
	if _, err := server.New(c, server.Options{}); err == nil {
		t.Error("the lab flag on a public bind must refuse to start")
	}
	c.AgentListen = "127.0.0.1:0"
	if _, err := server.New(c, server.Options{}); err != nil {
		t.Errorf("the lab flag on loopback: %v", err)
	}
}

func TestListenersServeAndShutDown(t *testing.T) {
	cfg := config.Default()
	cfg.Resolve()
	cfg.AdminListen, cfg.AgentListen, cfg.InsecureHTTPLab = "127.0.0.1:0", "127.0.0.1:0", true
	cfg.RateLimits.MaxConns = 2
	s, err := server.New(cfg, server.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	if s.AdminAddr() == "" || s.AgentAddr() == "" || s.TLSEnabled() {
		t.Fatal("addresses")
	}
	res, err := http.Get("http://" + s.AdminAddr() + "/healthz")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("admin: %v", err)
	}
	res.Body.Close()
	res, err = http.Get("http://" + s.AgentAddr() + "/api/v1/agent/policy")
	if err != nil || res.StatusCode != 404 {
		t.Fatalf("agent catch-all: %v", err)
	}
	res.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("graceful shutdown did not finish")
	}
}

func selfSigned(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	certPath, keyPath = filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	_ = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	return
}

func TestAgentListenerRequiresTLS12OrNewer(t *testing.T) {
	dir := t.TempDir()
	cert, key := selfSigned(t, dir)
	cfg := config.Default()
	cfg.Resolve()
	cfg.AdminListen, cfg.AgentListen = "127.0.0.1:0", "127.0.0.1:0"
	cfg.TLS = config.TLS{Cert: cert, Key: key}
	s, err := server.New(cfg, server.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()
	if !s.TLSEnabled() {
		t.Fatal("TLS must be on")
	}
	dial := func(min, max uint16) error {
		c, err := tls.Dial("tcp", s.AgentAddr(), &tls.Config{InsecureSkipVerify: true, MinVersion: min, MaxVersion: max})
		if err == nil {
			_ = c.Close()
		}
		return err
	}
	if err := dial(tls.VersionTLS12, tls.VersionTLS13); err != nil {
		t.Fatalf("TLS 1.2/1.3 must work: %v", err)
	}
	if err := dial(tls.VersionTLS10, tls.VersionTLS11); err == nil {
		t.Fatal("TLS 1.1 and older must be refused")
	}
	// A plain HTTP request to the TLS port gets no application answer.
	if res, err := http.Get("http://" + s.AgentAddr() + "/x"); err == nil && res.StatusCode == 200 {
		t.Fatal("plain HTTP on the TLS listener")
	}
	// A broken key fails at Listen with a path-free error.
	bad := cfg
	bad.TLS = config.TLS{Cert: cert, Key: filepath.Join(dir, "missing.pem")}
	s2, _ := server.New(bad, server.Options{})
	if err := s2.Listen(); err == nil || strings.Contains(err.Error(), dir) {
		t.Fatalf("broken key: %v", err)
	}
	_ = io.Discard
}

func TestHygieneNoDirectSlogAndNoErrorStringLogging(t *testing.T) {
	root := filepath.Join("..", "..", "..") // repository root
	direct := regexp.MustCompile(`\bslog\.`)
	importSlog := regexp.MustCompile(`"log/slog"`)
	sprintfErr := regexp.MustCompile(`fmt\.Sprintf\("%[sv]", *err\)|err\.Error\(\)\)?\s*\)\s*//\s*log`)
	var bad []string
	// Every console package that logs; adminpath, store and gateway do not log (or are not covered by this rule yet).
	var dirs []string
	for _, d := range []string{"adminapi", "agentapi", "auth", "cli", "config", "enroll", "escrow", "fixtures", "health", "httpx", "jobs", "maintain", "ratelimit", "restores", "server", "ui"} {
		dirs = append(dirs, "internal/console/"+d)
	}
	dirs = append(dirs, "cmd/cryo-console")
	for _, dir := range dirs {
		_ = filepath.Walk(filepath.Join(root, filepath.FromSlash(dir)), func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() || !strings.HasSuffix(p, ".go") {
				return nil
			}
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			if rel == "internal/console/server/server_test.go" {
				return nil // holds the offending patterns as literals
			}
			if direct.Match(b) || importSlog.Match(b) {
				bad = append(bad, rel+": uses log/slog directly (use internal/logx)")
			}
			if sprintfErr.Match(b) {
				bad = append(bad, rel+": formats an error with %s for logging")
			}
			for _, banned := range []string{"log.Printf(", "log.Println(", "fmt.Fprintln(os.Stderr, err)", "fmt.Fprintf(os.Stderr, \"%v\", err"} {
				if strings.Contains(string(b), banned) && !strings.HasSuffix(rel, "_test.go") {
					bad = append(bad, rel+": "+banned)
				}
			}
			return nil
		})
	}
	if len(bad) > 0 {
		t.Fatalf("path-free logging rule (architecture §8.4 items 2 and 3):\n%s", strings.Join(bad, "\n"))
	}
	// And the same test can fail: a synthetic offender is caught by the patterns.
	if !direct.MatchString(`slog.Info("x")`) || !importSlog.MatchString(`import "log/slog"`) || !sprintfErr.MatchString(`msg := fmt.Sprintf("%s", err)`) {
		t.Fatal("hygiene patterns are blind")
	}
}
