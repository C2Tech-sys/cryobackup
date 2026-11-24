package cli

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// P21-T7: the console behind a reverse proxy on the same host. Documentation (RFC 5737) and
// private (RFC 1918) addresses only: the proxy is 198.51.100.7, clients are 192.0.2.x and a forger
// is 203.0.113.50.

const (
	t7Proxy     = "198.51.100.7"
	t7ProxyPeer = t7Proxy + ":40000"
	t7Client    = "192.0.2.10"
	t7Forger    = "203.0.113.50"
	t7Password  = "fixtures-pass-1"
)

// t7Fixtures builds a fixtures-mode App with the given trusted_proxies.
func t7Fixtures(t *testing.T, trusted ...string) *App {
	t.Helper()
	cfg := config.Default()
	cfg.Resolve()
	cfg.Fixtures = true
	cfg.AdminListen = "127.0.0.1:0"
	cfg.TrustedProxies = trusted
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	app, err := Build(context.Background(), cfg, BuildOptions{FixturesPassword: t7Password})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app
}

// t7Login posts the API login from peer with the given X-Forwarded-For (none when empty) and
// returns the status.
func t7Login(t *testing.T, h http.Handler, peer, xff, user, pw string) int {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "http://console.test/api/v1/auth/login",
		strings.NewReader(fmt.Sprintf(`{"username":%q,"password":%q}`, user, pw)))
	r.Header.Set("Content-Type", api.ContentTypeJSON)
	r.RemoteAddr = peer
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

// t7Addresses reads the address of the newest successful login in the audit trail and of the newest
// session row. (last_login_ip is written by the MFA step, through the same httpx.ClientIP; the
// fixture admin has no authenticator, so it is not reached here.)
func t7Addresses(t *testing.T, st *store.Store) (audit, session string) {
	t.Helper()
	var a, s sql.NullString
	if err := st.Read(context.Background(), func(tx *store.Tx) error {
		if err := tx.QueryRow(`SELECT source_ip FROM audit_events WHERE action = 'auth.login' AND result = 'ok' ORDER BY rowid DESC LIMIT 1`).Scan(&a); err != nil {
			return err
		}
		return tx.QueryRow(`SELECT source_ip FROM sessions ORDER BY rowid DESC LIMIT 1`).Scan(&s)
	}); err != nil {
		t.Fatal(err)
	}
	return a.String, s.String
}

// The golden of the audit row and the session row: with trusted_proxies unset they
// hold the direct peer (today's behaviour, a forged header changes nothing); with it set and the
// peer listed, the client the proxy named; with it set and the peer not listed, the peer again.
func TestLoginAddressesFollowTrustedProxies(t *testing.T) {
	cases := []struct {
		name    string
		trusted []string
		peer    string
		xff     string
		want    string
	}{
		{"unset: the proxy's address, header ignored", nil, t7ProxyPeer, t7Client, t7Proxy},
		{"unset: a forged header from anyone changes nothing", nil, t7Forger + ":5555", t7Client, t7Forger},
		{"set: the client the trusted proxy names", []string{t7Proxy}, t7ProxyPeer, t7Client, t7Client},
		{"set via a prefix: the client", []string{"198.51.100.0/24"}, t7ProxyPeer, "192.0.2.99, " + t7Client, t7Client},
		{"set: a forged header from an unlisted peer changes nothing", []string{t7Proxy}, t7Forger + ":5555", t7Client, t7Forger},
		{"set: a malformed chain falls back to the proxy", []string{t7Proxy}, t7ProxyPeer, "unknown", t7Proxy},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			app := t7Fixtures(t, c.trusted...)
			if code := t7Login(t, app.Server.AdminHandler(), c.peer, c.xff, store.FixtureAdminUsername, t7Password); code != http.StatusOK {
				t.Fatalf("login: %d", code)
			}
			audit, session := t7Addresses(t, app.Store)
			if audit != c.want || session != c.want {
				t.Fatalf("audit %q, session %q; want %q in both", audit, session, c.want)
			}
		})
	}
}

// The per-IP login limit (10 a minute): behind a trusted proxy each client has its own bucket;
// with trusted_proxies unset everyone behind the proxy shares one, as before.
func TestLoginLimitIsPerClientBehindATrustedProxy(t *testing.T) {
	for _, c := range []struct {
		name    string
		trusted []string
		other   int // what a second client gets once the first used its ten attempts
	}{
		{"set", []string{t7Proxy}, http.StatusUnauthorized},
		{"unset", nil, http.StatusTooManyRequests},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := t7Fixtures(t, c.trusted...)
			h := app.Server.AdminHandler()
			for i := 0; i < 10; i++ {
				if code := t7Login(t, h, t7ProxyPeer, t7Client, fmt.Sprintf("nobody-%d", i), "wrong-password"); code != http.StatusUnauthorized {
					t.Fatalf("attempt %d: %d", i+1, code)
				}
			}
			if code := t7Login(t, h, t7ProxyPeer, t7Client, "nobody-x", "wrong-password"); code != http.StatusTooManyRequests {
				t.Fatalf("eleventh attempt of the same client: %d, want 429", code)
			}
			if code := t7Login(t, h, t7ProxyPeer, "192.0.2.20", "nobody-y", "wrong-password"); code != c.other {
				t.Fatalf("another client behind the proxy: %d, want %d", code, c.other)
			}
		})
	}
}

// Nothing on the agent listener: in a serve-mode App whose trusted_proxies names the peer, the
// enrollment limit (10 a minute per address) still counts the peer, whatever X-Forwarded-For says,
// and the denied row records the peer. The admin listener of the same App believes the header.
func TestAgentListenerIgnoresTrustedProxies(t *testing.T) {
	dir := t.TempDir()
	if code, out, errb := run(t, "", initArgs(dir, "--master-key-copy-confirmed")...); code != 0 {
		t.Fatalf("init: %d\n%s\n%s", code, out, errb)
	}
	cfg, err := config.Read(filepath.Join(dir, "config.json"), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	cfg.InsecureHTTPLab, cfg.AgentListen, cfg.AdminListen = true, "127.0.0.1:0", "127.0.0.1:0"
	cfg.AdminpathSocket = filepath.Join(dir, "a.sock")
	cfg.TrustedProxies = []string{t7Proxy}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	app, err := Build(context.Background(), cfg, BuildOptions{Backend: &fixtures.Backend{}, Log: logx.New(&logs, logx.LevelDebug)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	agent := app.Server.AgentHandler()
	enroll := func(xff string) int {
		r := httptest.NewRequest(http.MethodPost, "http://console.test"+api.RouteEnroll, strings.NewReader(`{}`))
		r.RemoteAddr = t7ProxyPeer
		r.Header.Set("X-Forwarded-For", xff)
		w := httptest.NewRecorder()
		agent.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < 10; i++ {
		if code := enroll(fmt.Sprintf("192.0.2.%d", 100+i)); code == http.StatusTooManyRequests {
			t.Fatalf("enroll %d limited early: %d", i+1, code)
		}
	}
	if code := enroll("192.0.2.200"); code != http.StatusTooManyRequests {
		t.Fatalf("eleventh enroll from the same peer with a new X-Forwarded-For: %d, want 429 (the agent listener must count the peer)", code)
	}
	var denied sql.NullString
	if err := app.Store.Read(context.Background(), func(tx *store.Tx) error {
		return tx.QueryRow(`SELECT source_ip FROM audit_events WHERE action = 'agent.enroll.denied' ORDER BY rowid DESC LIMIT 1`).Scan(&denied)
	}); err != nil {
		t.Fatal(err)
	}
	if denied.String != t7Proxy {
		t.Fatalf("agent.enroll.denied source_ip %q, want the peer %s", denied.String, t7Proxy)
	}
	// The same App's admin listener does believe the trusted proxy.
	if code := t7Login(t, app.Server.AdminHandler(), t7ProxyPeer, t7Client, "nobody", "wrong-password"); code != http.StatusUnauthorized {
		t.Fatalf("admin login: %d", code)
	}
	var adminIP sql.NullString
	if err := app.Store.Read(context.Background(), func(tx *store.Tx) error {
		return tx.QueryRow(`SELECT source_ip FROM audit_events WHERE action = 'auth.login' ORDER BY rowid DESC LIMIT 1`).Scan(&adminIP)
	}); err != nil {
		t.Fatal(err)
	}
	if adminIP.String != t7Client {
		t.Fatalf("admin auth.login source_ip %q, want the forwarded client %s", adminIP.String, t7Client)
	}
}

// Doctor prints one line naming the list when trusted_proxies is set, nothing when it is unset, and
// NOT CHECKED beside the config FAIL when the list is not valid.
func TestDoctorNamesTrustedProxies(t *testing.T) {
	var lines []string
	rep := func(level, name, detail string) { lines = append(lines, level+"|"+name+"|"+detail) }
	checkTrustedProxies(config.Config{}, rep)
	if len(lines) != 0 {
		t.Fatalf("unset: %q", lines)
	}
	checkTrustedProxies(config.Config{TrustedProxies: []string{"192.0.2.10", " 10.0.0.0/8"}}, rep)
	want := "OK|trusted proxies|client address from X-Forwarded-For, and https from X-Forwarded-Proto, believed only from 192.0.2.10, 10.0.0.0/8 (trusted_proxies)"
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("set: %q\nwant %q", lines, want)
	}
	lines = nil
	checkTrustedProxies(config.Config{TrustedProxies: []string{"0.0.0.0/0"}}, rep)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "NOT CHECKED|trusted proxies|") {
		t.Fatalf("invalid: %q", lines)
	}

	// End to end: doctor on a config file with the key prints the line once; without it, never.
	dir := t.TempDir()
	if code, _, e := run(t, "", initArgs(dir, "--master-key-copy-confirmed", "--admin-listen", "127.0.0.1:0", "--agent-listen", "127.0.0.1:0")...); code != 0 {
		t.Fatal(e)
	}
	cfgPath := filepath.Join(dir, "config.json")
	_, out, _ := run(t, "", "doctor", "--config", cfgPath)
	if strings.Contains(out, "trusted proxies") {
		t.Fatalf("doctor without trusted_proxies prints a line for it:\n%s", out)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	// init does not write the key while it is unset, so a config it made still loads in an older
	// binary (which refuses an unknown key) as far as this key goes.
	if strings.Contains(string(raw), "trusted_proxies") {
		t.Fatalf("init wrote trusted_proxies although it is unset:\n%s", raw)
	}
	edited := strings.Replace(string(raw), "{", `{"trusted_proxies": ["127.0.0.1"],`, 1)
	if err := os.WriteFile(cfgPath, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	_, out, _ = run(t, "", "doctor", "--config", cfgPath)
	if strings.Count(out, "trusted proxies") != 1 || !strings.Contains(out, doctorLine("OK", "trusted proxies")) || !strings.Contains(out, "believed only from 127.0.0.1 (trusted_proxies)") {
		t.Fatalf("doctor with trusted_proxies:\n%s", out)
	}
}
