package cli

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// V1 (P21-T7 fix batch 1): the BROWSER path behind a trusted proxy. The UI routes (POST /login, the
// pages) are registered beside the admin API on the admin listener, and both must see the client the
// proxy names: a wiring that wraps the API but not the UI passed every API-only test. Documentation
// and private addresses only (see proxy_p21t7_test.go).

// t7Form posts a browser form to the admin handler from peer, with X-Forwarded-For xff (none when
// empty), the cookies given, and an Origin naming the console as a browser would.
func t7Form(t *testing.T, h http.Handler, path, peer, xff string, form url.Values, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "http://console.test"+path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://console.test")
	r.RemoteAddr = peer
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// t7AuditIP is the source_ip of the newest audit row of action with result ok.
func t7AuditIP(t *testing.T, st *store.Store, action string) string {
	t.Helper()
	var ip sql.NullString
	if err := st.Read(context.Background(), func(tx *store.Tx) error {
		return tx.QueryRow(`SELECT source_ip FROM audit_events WHERE action = ? AND result = 'ok' ORDER BY rowid DESC LIMIT 1`, action).Scan(&ip)
	}); err != nil {
		t.Fatalf("audit row %s: %v", action, err)
	}
	return ip.String
}

// Sign in through the browser form, then sign out (a page request that writes an audit row): the
// session row and both audit rows hold the client the listed proxy names. Unset: the proxy, as
// before (the golden of today's behaviour on the browser path).
func TestBrowserLoginAndPageFollowTrustedProxies(t *testing.T) {
	for _, c := range []struct {
		name    string
		trusted []string
		want    string
	}{
		{"set", []string{t7Proxy}, t7Client},
		{"unset", nil, t7Proxy},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := t7Fixtures(t, c.trusted...)
			h := app.Server.AdminHandler()
			w := t7Form(t, h, "/login", t7ProxyPeer, t7Client, url.Values{"username": {store.FixtureAdminUsername}, "password": {t7Password}}, nil)
			if w.Code != http.StatusSeeOther {
				t.Fatalf("browser login: %d %.300s", w.Code, w.Body.String())
			}
			audit, session := t7Addresses(t, app.Store)
			if audit != c.want || session != c.want {
				t.Fatalf("after the browser login: audit %q, session %q; want %q in both", audit, session, c.want)
			}
			cookies := w.Result().Cookies()
			csrf := ""
			for _, ck := range cookies {
				if ck.Name == auth.CSRFCookie {
					csrf = ck.Value
				}
			}
			if csrf == "" {
				t.Fatal("no CSRF cookie after the browser login")
			}
			// The fixture admin has no authenticator, so its new session waits for the MFA step (every
			// page sends it to /login/mfa); such a session signs out with /login/mfa/cancel, which writes
			// auth.logout like /logout does.
			if loc := w.Header().Get("Location"); loc != "/" {
				t.Fatalf("browser login redirected to %q", loc)
			}
			out := "/login/mfa/cancel"
			w = t7Form(t, h, out, t7ProxyPeer, t7Client, url.Values{"csrf_token": {csrf}}, cookies)
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
				t.Fatalf("sign-out page %s: %d to %q %.300s", out, w.Code, w.Header().Get("Location"), w.Body.String())
			}
			if got := t7AuditIP(t, app.Store, "auth.logout"); got != c.want {
				t.Fatalf("auth.logout source_ip %q, want %q", got, c.want)
			}
		})
	}
}

// A forged X-Forwarded-For on the browser form from a peer that is not listed changes nothing.
func TestBrowserLoginIgnoresAForgedHeaderFromAnUnlistedPeer(t *testing.T) {
	app := t7Fixtures(t, t7Proxy)
	w := t7Form(t, app.Server.AdminHandler(), "/login", t7Forger+":5555", t7Client,
		url.Values{"username": {store.FixtureAdminUsername}, "password": {t7Password}}, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("browser login: %d", w.Code)
	}
	if audit, session := t7Addresses(t, app.Store); audit != t7Forger || session != t7Forger {
		t.Fatalf("audit %q, session %q; want the peer %q", audit, session, t7Forger)
	}
}

// The per-address login limit on the browser form: behind a listed proxy each client has its own
// bucket; unset, everyone behind the proxy shares one, as before.
func TestBrowserLoginLimitIsPerClientBehindATrustedProxy(t *testing.T) {
	for _, c := range []struct {
		name    string
		trusted []string
		other   int
	}{
		{"set", []string{t7Proxy}, http.StatusUnauthorized},
		{"unset", nil, http.StatusTooManyRequests},
	} {
		t.Run(c.name, func(t *testing.T) {
			app := t7Fixtures(t, c.trusted...)
			h := app.Server.AdminHandler()
			bad := func(xff string, i int) int {
				return t7Form(t, h, "/login", t7ProxyPeer, xff, url.Values{"username": {fmt.Sprintf("nobody-%d", i)}, "password": {"wrong-password"}}, nil).Code
			}
			for i := 0; i < 10; i++ {
				if code := bad(t7Client, i); code != http.StatusUnauthorized {
					t.Fatalf("attempt %d: %d", i+1, code)
				}
			}
			if code := bad(t7Client, 10); code != http.StatusTooManyRequests {
				t.Fatalf("eleventh attempt of the same client: %d, want 429", code)
			}
			if code := bad("192.0.2.20", 11); code != c.other {
				t.Fatalf("another client behind the proxy: %d, want %d", code, c.other)
			}
		})
	}
}
