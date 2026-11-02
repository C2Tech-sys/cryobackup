package ui

import (
	"bytes"
	"context"
	"encoding/base32"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// uiMFA is the test side of the MFA step: a TOTP clock that moves one step per
// code and the secrets and recovery codes of the accounts it enrolled.
type uiMFA struct {
	mu      sync.Mutex
	at      time.Time
	secrets map[string][]byte
	codes   map[string][]string
}

func newUIMFA(am *auth.Manager, mk *escrow.MasterKey) *uiMFA {
	m := &uiMFA{at: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), secrets: map[string][]byte{}, codes: map[string][]string{}}
	am.Master = mk
	am.TOTPNow = func() time.Time { m.mu.Lock(); defer m.mu.Unlock(); return m.at }
	return m
}

func (m *uiMFA) code(user string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.at = m.at.Add(auth.TOTPPeriod)
	return auth.TOTPCode(m.secrets[user], m.at)
}

var (
	secretRE = regexp.MustCompile(`<pre class="code secret"[^>]*>([A-Z2-7 ]+)</pre>`)
	codesRE  = regexp.MustCompile(`<li><code>([a-z2-9]{5}-[a-z2-9]{5})</code></li>`)
)

// adopt takes the session and CSRF cookies a response set.
func (e *env) adopt(res *http.Response) {
	for _, c := range res.Cookies() {
		kept := e.sess[:0]
		for _, o := range e.sess {
			if o.Name != c.Name {
				kept = append(kept, o)
			}
		}
		e.sess = append(kept, c)
		if c.Name == auth.CSRFCookie {
			e.csrf = c.Value
		}
	}
}

// completeMFA finishes the login e.sess began: enrollment on the account's
// first login (the QR page's key), the TOTP step afterwards.
func (e *env) completeMFA(user string) {
	e.t.Helper()
	res, body := e.do("GET", "/login/mfa", nil)
	if res.StatusCode != 200 {
		e.t.Fatalf("mfa page: %d", res.StatusCode)
	}
	if strings.Contains(body, `action="/login/mfa/enroll"`) {
		m := secretRE.FindStringSubmatch(body)
		if m == nil {
			e.t.Fatal("the enrollment page shows no key")
		}
		secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ReplaceAll(m[1], " ", ""))
		if err != nil {
			e.t.Fatal(err)
		}
		e.mfa.mu.Lock()
		e.mfa.secrets[user] = secret
		e.mfa.mu.Unlock()
		res, body = e.post("/login/mfa/enroll", url.Values{"code": {e.mfa.code(user)}}, false)
		if res.StatusCode != 200 || !strings.Contains(body, "Save your recovery codes") {
			e.t.Fatalf("enroll: %d", res.StatusCode)
		}
		var codes []string
		for _, c := range codesRE.FindAllStringSubmatch(body, -1) {
			codes = append(codes, c[1])
		}
		if len(codes) != auth.RecoveryCodeCount {
			e.t.Fatalf("%d recovery codes shown", len(codes))
		}
		e.mfa.mu.Lock()
		e.mfa.codes[user] = codes
		e.mfa.mu.Unlock()
		e.adopt(res)
		return
	}
	res, _ = e.post("/login/mfa", url.Values{"code": {e.mfa.code(user)}}, false)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		e.t.Fatalf("totp step: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	e.adopt(res)
}

// loginAs signs in as another account (password, then MFA) and becomes it.
func (e *env) loginAs(user, pw string) {
	e.t.Helper()
	e.sess, e.csrf = nil, ""
	res, body := e.do("POST", "/login", url.Values{"username": {user}, "password": {pw}})
	if res.StatusCode != http.StatusSeeOther {
		e.t.Fatalf("login %s: %d %s", user, res.StatusCode, body)
	}
	e.adopt(res)
	e.completeMFA(user)
}

func (e *env) addAccount(user, role string) {
	e.t.Helper()
	h, _ := auth.HashPassword(testPassword)
	if _, err := e.st.CreateAdmin(context.Background(), store.Admin{Username: user, Email: user + "@example.invalid", Role: role, PasswordHash: h}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) audits(action string) int {
	e.t.Helper()
	l, err := e.st.ListAudit(context.Background(), store.ScopeAll(), store.AuditFilter{Action: action}, store.Page{Limit: 1000})
	if err != nil {
		e.t.Fatal(err)
	}
	return len(l.Items)
}

// The MFA step page: enrollment with a server-rendered QR (inline SVG, no
// script, no remote asset) on the first login, the code form afterwards.
func TestMFAPagesRender(t *testing.T) {
	e := newEnv(t)
	res, _ := e.do("POST", "/login", url.Values{"username": {"admin"}, "password": {testPassword}})
	e.adopt(res)
	res, body := e.do("GET", "/login/mfa", nil)
	if res.StatusCode != 200 || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("enroll page: %d %q", res.StatusCode, res.Header.Get("Cache-Control"))
	}
	mustContain(t, "enroll", body, `<svg class="qr" role="img"`, `class="qr-ink"`, `autocomplete="one-time-code"`, `inputmode="numeric"`,
		"Set up your authenticator", `action="/login/mfa/enroll"`, `action="/login/mfa/cancel"`, "Fixtures mode")
	if strings.Contains(body, "<script>") || strings.Contains(body, "http://") || strings.Contains(body, "https://") {
		t.Fatal("the MFA page must have no inline script and no absolute URL")
	}
	// The same pending session sees the same key on reload (one enrollment per session).
	_, again := e.do("GET", "/login/mfa", nil)
	if secretRE.FindString(again) != secretRE.FindString(body) {
		t.Fatal("a reload must show the same key")
	}
	e.completeMFA("admin")
	// Second login: the code form and the recovery-code form.
	e.loginAsPasswordOnly("admin")
	res, body = e.do("GET", "/login/mfa", nil)
	mustContain(t, "verify", body, `action="/login/mfa"`, `action="/login/mfa/recovery"`, "Lost your authenticator?")
	if strings.Contains(body, `class="qr"`) {
		t.Fatal("an enrolled account must not see a QR code at sign-in")
	}
	res, body = e.post("/login/mfa", url.Values{"code": {"000000"}}, false)
	if res.StatusCode != 401 || !strings.Contains(body, "Code incorrect or expired.") || !strings.Contains(body, `role="alert"`) {
		t.Fatalf("wrong code: %d", res.StatusCode)
	}
	// A recovery code signs in and lands on /account with the warning.
	res, _ = e.post("/login/mfa/recovery", url.Values{"code": {e.mfa.codes["admin"][0]}}, false)
	if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/account?done=recovery_login") {
		t.Fatalf("recovery: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	e.adopt(res)
	_, body = e.do("GET", res.Header.Get("Location"), nil)
	mustContain(t, "after recovery", body, "You signed in with a recovery code.", "9 recovery codes are left.")
	// A full session is sent away from the MFA page.
	if res, _ := e.do("GET", "/login/mfa", nil); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		t.Fatalf("mfa page with a full session: %d", res.StatusCode)
	}
}

func (e *env) loginAsPasswordOnly(user string) {
	e.t.Helper()
	e.sess, e.csrf = nil, ""
	res, _ := e.do("POST", "/login", url.Values{"username": {user}, "password": {testPassword}})
	e.adopt(res)
}

// Fixtures mode renders /account and /settings/accounts with every card.
func TestAccountPagesRenderInFixturesMode(t *testing.T) {
	e := newEnv(t)
	e.login()
	res, body := e.do("GET", "/account", nil)
	if res.StatusCode != 200 {
		t.Fatalf("/account: %d", res.StatusCode)
	}
	mustContain(t, "/account", body, "My account", "Authenticator", `<span class="st sm ok"><i aria-hidden="true"></i>ON</span>`, "10 of 10 left",
		"Password", `autocomplete="new-password"`, "Signed-in sessions", "THIS SESSION", "API tokens", "No API tokens",
		`action="/ui/account/tokens"`, "Changing your password signs out your other sessions and revokes your API tokens.", `class="usermenu"`, `href="/settings/accounts"`, `href="/account" aria-current="page"`)
	res, body = e.do("GET", "/settings/accounts", nil)
	if res.StatusCode != 200 {
		t.Fatalf("/settings/accounts: %d", res.StatusCode)
	}
	mustContain(t, "/settings/accounts", body, "Accounts and roles", "<caption>Console accounts, by username</caption>", "YOU", "TOTP",
		"1 enabled owner", "+ Add an account", `action="/ui/accounts"`, "Limit to the customers")
	for _, b := range []string{body} {
		if strings.Contains(b, "https://") || strings.Contains(b, "<script>") {
			t.Fatal("remote asset or inline script")
		}
	}
}

// Every UI route of the ticket refuses each role below its minimum with 403,
// serves the roles at or above it, and refuses a Bearer token outright.
func TestAccountUIRoutesRoleMatrix(t *testing.T) {
	e := newEnv(t)
	e.addAccount("ro", "read_only")
	e.addAccount("op", "operator")
	e.addAccount("ad", "admin")
	victim, err := e.st.CreateAdmin(context.Background(), store.Admin{Username: "victim", Email: "v@example.invalid", Role: "read_only", PasswordHash: "!"})
	if err != nil {
		t.Fatal(err)
	}
	rank := map[string]int{"RO": 1, "OP": 2, "AD": 3, "OW": 4, "SELF": 1}
	users := map[string]string{"RO": "ro", "OP": "op", "AD": "ad", "OW": "admin"}
	for role, user := range users {
		e.loginAs(user, testPassword)
		for pattern, min := range AccountUIRoutes() {
			if min == "ANON" {
				continue
			}
			method, path, _ := strings.Cut(pattern, " ")
			path = strings.ReplaceAll(path, "{id}", victim.ID)
			var res *http.Response
			if method == "GET" {
				res, _ = e.do("GET", path, nil)
			} else {
				res, _ = e.post(path, url.Values{}, false)
			}
			switch {
			case min == "PENDING":
				if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
					t.Errorf("%s as %s (full session): %d %s", pattern, role, res.StatusCode, res.Header.Get("Location"))
				}
			case rank[role] < rank[min]:
				if res.StatusCode != 403 {
					t.Errorf("%s as %s: %d, want 403", pattern, role, res.StatusCode)
				}
			default:
				if res.StatusCode == 403 || (res.StatusCode == http.StatusSeeOther && strings.HasPrefix(res.Header.Get("Location"), "/login")) {
					t.Errorf("%s as %s: %d %s, want the route to serve it", pattern, role, res.StatusCode, res.Header.Get("Location"))
				}
			}
		}
	}
	// Bearer is refused on UI routes, even with a valid token.
	e.loginAs("admin", testPassword)
	p := &auth.Principal{}
	_ = p
	am := e.svc.Auth
	pr, err := am.Authenticate(context.Background(), cookieValue(e.sess, auth.SessionCookie))
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := am.CreateAPIToken(context.Background(), pr, "ui probe", auth.ScopeFull, 7, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	saved := e.sess
	e.sess = nil
	for _, path := range []string{"/", "/account", "/settings/accounts"} {
		res, _ := e.do("GET", path, nil, "Authorization", "Bearer "+tok)
		if res.StatusCode != 403 {
			t.Errorf("Bearer on %s: %d, want 403", path, res.StatusCode)
		}
	}
	// An Authorization header of another scheme (a proxy's Basic auth) is ignored, not refused.
	e.sess = saved
	if res, _ := e.do("GET", "/account", nil, "Authorization", "Basic dXNlcjpwYXNz"); res.StatusCode != 200 {
		t.Fatalf("Basic Authorization beside a session: %d", res.StatusCode)
	}
}

func cookieValue(cs []*http.Cookie, name string) string {
	for _, c := range cs {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// The account screens end to end: create an account (link shown once), the
// new user sets a password through the link and enrolls; the owner changes its
// role, disables and deletes it; the user changes their password and makes and
// revokes a token. Every change writes one audit row and no secret reaches a log.
func TestAccountFlowsThroughTheUI(t *testing.T) {
	e := newEnv(t)
	var logs bytes.Buffer
	e.svc.Log = logx.New(&logs, logx.LevelDebug)
	e.login()
	step := func(name, action string, f func()) {
		t.Helper()
		before := e.audits(action)
		f()
		if n := e.audits(action) - before; n != 1 {
			t.Errorf("%s wrote %d %s audit rows, want 1", name, n, action)
		}
	}
	var link string
	step("create", "admin.create", func() {
		res, body := e.post("/ui/accounts", url.Values{"username": {"tech1"}, "email": {"tech1@example.invalid"}, "role": {"operator"}}, false)
		m := regexp.MustCompile(`(https?://[^<"\s]+/login/reset\?token=[A-Za-z0-9_-]+)`).FindStringSubmatch(body)
		if res.StatusCode != 200 || m == nil || !strings.Contains(body, "Account created.") {
			t.Fatalf("create: %d", res.StatusCode)
		}
		link = html.UnescapeString(m[1])
	})
	owner := e.sess
	ownerCSRF := e.csrf
	// The new user follows the link.
	e.sess, e.csrf = nil, ""
	u, _ := url.Parse(link)
	res, body := e.do("GET", u.RequestURI(), nil)
	if res.StatusCode != 200 || !strings.Contains(body, "Choose a password for <b>tech1</b>") {
		t.Fatalf("reset page: %d", res.StatusCode)
	}
	nonce := regexp.MustCompile(`name="login_nonce" value="([^"]+)"`).FindStringSubmatch(body)
	e.adopt(res)
	step("reset", "admin.password_reset.complete", func() {
		res, body = e.do("POST", "/login/reset", url.Values{"token": {u.Query().Get("token")}, "password": {"tech one password"}, "confirm": {"tech one password"},
			auth.LoginNonceField: {nonce[1]}}, "Origin", e.srv.URL)
		if res.StatusCode != 200 || !strings.Contains(body, "Password set for tech1.") {
			t.Fatalf("reset post: %d", res.StatusCode)
		}
	})
	if res, _ := e.do("GET", u.RequestURI(), nil); res.StatusCode != 404 {
		t.Fatalf("a used link: %d", res.StatusCode)
	}
	e.loginAs("tech1", "tech one password")
	if res, _ := e.do("GET", "/settings/accounts", nil); res.StatusCode != 403 {
		t.Fatalf("an operator on the accounts page: %d", res.StatusCode)
	}
	// The user's own account: password, token.
	step("password", "admin.password_change", func() {
		res, _ := e.post("/ui/account/password", url.Values{"current_password": {"tech one password"}, "new_password": {"tech one new password"}, "confirm_password": {"tech one new password"}}, false)
		if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/account?done=password" {
			t.Fatalf("password: %d", res.StatusCode)
		}
	})
	var secret string
	step("token", "admin.api_token.create", func() {
		res, body := e.post("/ui/account/tokens", url.Values{"name": {"rmm"}, "scope": {"read"}, "days": {"30"}}, false)
		m := regexp.MustCompile(`(cbk_[A-Za-z0-9_-]{43})`).FindStringSubmatch(body)
		if res.StatusCode != 200 || m == nil || !strings.Contains(body, "Copy this token now.") {
			t.Fatalf("token: %d", res.StatusCode)
		}
		secret = m[1]
	})
	_, body = e.do("GET", "/account", nil)
	if strings.Contains(body, secret) || !strings.Contains(body, secret[:8]+"…") {
		t.Fatal("the token list must show the prefix and never the secret")
	}
	tokID := regexp.MustCompile(`/ui/account/tokens/([0-9A-Z]{26})/revoke`).FindStringSubmatch(body)
	step("revoke token", "admin.api_token.revoke", func() {
		if res, _ := e.post("/ui/account/tokens/"+tokID[1]+"/revoke", url.Values{}, false); res.StatusCode != http.StatusSeeOther {
			t.Fatalf("revoke: %d", res.StatusCode)
		}
	})
	tech, _ := e.st.GetAdminByUsername(context.Background(), "tech1")
	// Back to the owner: role, disable, delete (typed confirmation).
	e.sess, e.csrf = owner, ownerCSRF
	step("role", "admin.update", func() {
		if res, _ := e.post("/ui/accounts/"+tech.ID+"/role", url.Values{"role": {"read_only"}}, false); res.StatusCode != http.StatusSeeOther {
			t.Fatalf("role: %d", res.StatusCode)
		}
	})
	step("disable", "admin.disable", func() {
		if res, _ := e.post("/ui/accounts/"+tech.ID+"/disable", url.Values{}, false); res.StatusCode != http.StatusSeeOther {
			t.Fatalf("disable: %d", res.StatusCode)
		}
	})
	if res, body := e.post("/ui/accounts/"+tech.ID+"/delete", url.Values{"confirm": {"wrong"}}, false); res.StatusCode != 400 || !strings.Contains(body, "Type the username tech1 to confirm.") {
		t.Fatalf("delete without confirmation: %d", res.StatusCode)
	}
	step("delete", "admin.delete", func() {
		if res, _ := e.post("/ui/accounts/"+tech.ID+"/delete", url.Values{"confirm": {"tech1"}}, false); res.StatusCode != http.StatusSeeOther {
			t.Fatalf("delete: %d", res.StatusCode)
		}
	})
	// The last owner cannot be removed from the UI either (self is refused first).
	me, _ := e.st.GetAdminByUsername(context.Background(), "admin")
	if res, body := e.post("/ui/accounts/"+me.ID+"/disable", url.Values{}, false); res.StatusCode != 409 || !strings.Contains(body, "Not on your own account.") {
		t.Fatalf("disable self: %d", res.StatusCode)
	}
	// Nothing secret in the logs: no token, no reset token, no code, no TOTP key.
	for _, s := range append([]string{secret, u.Query().Get("token"), auth.TOTPSecretText(e.mfa.secrets["admin"])}, e.mfa.codes["admin"]...) {
		if s != "" && strings.Contains(logs.String(), s) {
			t.Fatalf("a secret reached the log: %.12s...", s)
		}
	}
}

// Regenerating recovery codes and replacing the authenticator from /account
// need a fresh code, and show the new codes once.
func TestAccountMFAManagement(t *testing.T) {
	e := newEnv(t)
	e.login()
	res, body := e.post("/ui/account/recovery-codes", url.Values{"code": {"000000"}}, false)
	if res.StatusCode != 401 || !strings.Contains(body, "Code incorrect or expired.") {
		t.Fatalf("regenerate with a wrong code: %d", res.StatusCode)
	}
	res, body = e.post("/ui/account/recovery-codes", url.Values{"code": {e.mfa.code("admin")}}, false)
	if res.StatusCode != 200 || len(codesRE.FindAllString(body, -1)) != auth.RecoveryCodeCount || !strings.Contains(body, "New recovery codes.") {
		t.Fatalf("regenerate: %d", res.StatusCode)
	}
	res, body = e.post("/ui/account/totp", url.Values{}, false)
	m := secretRE.FindStringSubmatch(body)
	if res.StatusCode != 200 || m == nil || !strings.Contains(body, "Code from your current app, or a recovery code") {
		t.Fatalf("replace authenticator: %d", res.StatusCode)
	}
	newSecret, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ReplaceAll(m[1], " ", ""))
	e.mfa.mu.Lock()
	e.mfa.at = e.mfa.at.Add(auth.TOTPPeriod)
	newCode := auth.TOTPCode(newSecret, e.mfa.at)
	e.mfa.mu.Unlock()
	res, body = e.post("/ui/account/totp/confirm", url.Values{"code": {newCode}, "current_code": {e.mfa.code("admin")}}, false)
	if res.StatusCode != 200 || !strings.Contains(body, "Authenticator saved.") {
		t.Fatalf("confirm new authenticator: %d", res.StatusCode)
	}
}
