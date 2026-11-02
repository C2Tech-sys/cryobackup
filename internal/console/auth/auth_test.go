package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

func TestArgonKnownVectorFromTheReferenceImplementation(t *testing.T) {
	// password "password", salt "somesalt", argon2id, t=1, m=64 KiB, p=1, 24-byte
	// output: the vector in golang.org/x/crypto/argon2 (from the C reference).
	salt := b64.EncodeToString([]byte("somesalt"))
	key := "655ad15eac652dc59f7170a7332bf49b8469be1fdb9c28bb"
	raw := make([]byte, 24)
	for i := range raw {
		raw[i] = hexNibble(key[2*i])<<4 | hexNibble(key[2*i+1])
	}
	phc := "$argon2id$v=19$m=64,t=1,p=1$" + salt + "$" + b64.EncodeToString(raw)
	ok, rehash := VerifyPassword(phc, "password")
	if !ok || !rehash {
		t.Fatalf("reference vector: ok=%v rehash=%v", ok, rehash)
	}
	if ok, _ := VerifyPassword(phc, "Password"); ok {
		t.Fatal("wrong password must fail")
	}
}

func hexNibble(c byte) byte {
	if c >= 'a' {
		return c - 'a' + 10
	}
	return c - '0'
}

func TestHashFormatAndVerify(t *testing.T) {
	h, err := HashPassword("s3cret pass")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("PHC prefix / parameters: %s", h)
	}
	parts := strings.Split(h, "$")
	if salt, _ := b64.DecodeString(parts[4]); len(salt) != 16 {
		t.Fatalf("salt must be 16 bytes: %d", len(salt))
	}
	if key, _ := b64.DecodeString(parts[5]); len(key) != 32 {
		t.Fatalf("key must be 32 bytes: %d", len(key))
	}
	if ok, rehash := VerifyPassword(h, "s3cret pass"); !ok || rehash {
		t.Fatalf("verify: ok=%v rehash=%v", ok, rehash)
	}
	h2, _ := HashPassword("s3cret pass")
	if h == h2 {
		t.Fatal("salts must differ")
	}
	if ok, _ := VerifyPassword(h, "s3cret pasS"); ok {
		t.Fatal("wrong password")
	}
}

func TestMalformedHashesNeverVerify(t *testing.T) {
	good, _ := HashPassword("pw-pw-pw-pw")
	bad := []string{"", "!", "plain", "$argon2i$v=19$m=65536,t=3,p=4$AAAAAAAAAAAAAAAAAAAAAA$" + strings.Repeat("A", 43),
		strings.Replace(good, "m=65536", "m=999999999", 1), strings.Replace(good, "t=3", "t=0", 1),
		strings.Replace(good, "v=19", "v=16", 1), good + "$extra", strings.Replace(good, "p=4", "p=300", 1)}
	for _, h := range bad {
		if ok, _ := VerifyPassword(h, "pw-pw-pw-pw"); ok {
			t.Errorf("%q must not verify", h)
		}
	}
	if ok, _ := VerifyPassword(good, ""); ok {
		t.Error("empty password must not verify")
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type env struct {
	st  *store.Store
	m   *Manager
	clk *fakeClock
	mfa *mfaHelper // mfa_test.go: completes the MFA step for test logins
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := &fakeClock{t: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	st, err := store.OpenMemory(context.Background(), clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	m := NewManager(st, 12*time.Hour, 7*24*time.Hour, 10, 3, 15*time.Minute, clk.Now)
	e := &env{st: st, m: m, clk: clk}
	e.mfa = newMFAHelper(t, m)
	return e
}

func (e *env) addAdmin(t *testing.T, name, role, pw string) store.Admin {
	t.Helper()
	h, err := HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.st.CreateAdmin(context.Background(), store.Admin{Username: name, Email: name + "@example.invalid", Role: role, PasswordHash: h})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestLoginSessionLifetimesAndLogout(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "ann", "operator", "right horse")
	res, err := e.m.Login(ctx, "ann", "right horse", "203.0.113.5", "UA")
	if err != nil {
		t.Fatal(err)
	}
	if !res.MFARequired || res.Token == "" || res.CSRF != CSRFToken(res.Token) || res.Session.MFAVerifiedAt != nil {
		t.Fatalf("the password step must yield a pending session: %+v", res)
	}
	if got := res.Session.AbsoluteExpiresAt.Sub(e.clk.Now()); got != PendingTTL {
		t.Fatalf("pending session lifetime: %v", got)
	}
	if _, err := e.m.Authenticate(ctx, res.Token); !errors.Is(err, ErrMFAPending) {
		t.Fatalf("a pending session must not authenticate: %v", err)
	}
	res = e.mfa.complete(t, res)
	if got := res.Session.ExpiresAt.Sub(e.clk.Now()); got != 12*time.Hour {
		t.Fatalf("idle expiry: %v", got)
	}
	if got := res.Session.AbsoluteExpiresAt.Sub(e.clk.Now()); got != 7*24*time.Hour {
		t.Fatalf("absolute expiry: %v", got)
	}
	p, err := e.m.Authenticate(ctx, res.Token)
	if err != nil || p.Role != RoleOperator || !p.Scope.All {
		t.Fatalf("authenticate: %v %+v", err, p)
	}
	// Activity slides the idle window: 11 h, touch, 11 h again is still alive.
	e.clk.Advance(11 * time.Hour)
	if _, err := e.m.Authenticate(ctx, res.Token); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(11 * time.Hour)
	if _, err := e.m.Authenticate(ctx, res.Token); err != nil {
		t.Fatalf("sliding idle expiry failed: %v", err)
	}
	// Idle: nothing for 13 h kills it.
	e.clk.Advance(13 * time.Hour)
	if _, err := e.m.Authenticate(ctx, res.Token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("idle expiry: %v", err)
	}
	// Absolute: a busy session still dies at 7 d.
	res = login(t, e, "ann", "right horse")
	dead := false
	for i := 0; i < 16; i++ {
		e.clk.Advance(11 * time.Hour)
		if _, err := e.m.Authenticate(ctx, res.Token); err != nil {
			if i < 14 {
				t.Fatalf("session died after only %d h of activity", 11*(i+1))
			}
			dead = true
			break
		}
	}
	if !dead {
		t.Fatal("a busy session must still die at the absolute limit")
	}
	// Logout.
	res = login(t, e, "ann", "right horse")
	p, _ = e.m.Authenticate(ctx, res.Token)
	if err := e.m.Logout(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Authenticate(ctx, res.Token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("after logout: %v", err)
	}
	if _, err := e.m.Authenticate(ctx, "garbage"); !errors.Is(err, ErrNoSession) {
		t.Fatal("garbage cookie")
	}
}

func TestLoginFailuresLockoutAndUnknownUser(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "bob", "admin", "right horse")
	if _, err := e.m.Login(ctx, "nobody", "x", "198.51.100.1", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown user: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := e.m.Login(ctx, "bob", "wrong", "198.51.100.1", ""); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, err := e.m.Login(ctx, "bob", "right horse", "198.51.100.1", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("locked account must refuse even the right password: %v", err)
	}
	e.clk.Advance(16 * time.Minute)
	if _, err := e.m.Login(ctx, "bob", "right horse", "198.51.100.1", ""); err != nil {
		t.Fatalf("lock must expire: %v", err)
	}
	// Failures are audited without the attempted username of an unknown account.
	ev, err := e.st.ListAudit(ctx, store.ScopeAll(), store.AuditFilter{Action: "auth.login"}, store.Page{Limit: 50})
	if err != nil || len(ev.Items) < 5 {
		t.Fatalf("audit rows: %d %v", len(ev.Items), err)
	}
	for _, x := range ev.Items {
		if strings.Contains(string(x.Details), "nobody") || x.ActorLabel == "nobody" {
			t.Fatal("unknown username leaked into audit")
		}
	}
}

func TestLoginRateLimitPerIP(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var rl *RateLimitedError
	for i := 0; i < 12; i++ {
		_, err := e.m.Login(ctx, "nobody", "x", "192.0.2.77", "")
		if errors.As(err, &rl) {
			break
		}
	}
	if rl == nil || rl.RetryAfter <= 0 {
		t.Fatal("expected a rate limit with a Retry-After")
	}
	var rl2 *RateLimitedError
	if _, err := e.m.Login(ctx, "nobody", "x", "192.0.2.78", ""); errors.As(err, &rl2) {
		t.Fatal("another IP is independent")
	}
}

func TestDisabledAccountCannotLoginOrUseSession(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.addAdmin(t, "cy", "owner", "right horse")
	res, err := e.m.Login(ctx, "cy", "right horse", "192.0.2.1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.Write(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE admin_users SET disabled_at = ? WHERE id = ?`, e.clk.Now().UnixMilli(), a.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Authenticate(ctx, res.Token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("live session of a disabled admin: %v", err)
	}
	if _, err := e.m.Login(ctx, "cy", "right horse", "192.0.2.1", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("disabled login: %v", err)
	}
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := FromContext(r.Context())
		if !ok {
			http.Error(w, "no principal", 500)
			return
		}
		_, _ = w.Write([]byte("hello " + p.Admin.Username))
	})
}

func login(t *testing.T, e *env, user, pw string) LoginResult {
	t.Helper()
	res, err := e.m.Login(context.Background(), user, pw, "192.0.2.1", "")
	if err != nil {
		t.Fatal(err)
	}
	return e.mfa.complete(t, res)
}

func req(method, target string, res *LoginResult) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	if res != nil {
		r.AddCookie(&http.Cookie{Name: SessionCookie, Value: res.Token})
		r.AddCookie(&http.Cookie{Name: CSRFCookie, Value: res.CSRF})
	}
	return r
}

func TestCSRFRules(t *testing.T) {
	e := newEnv(t)
	e.addAdmin(t, "dee", "operator", "right horse")
	res := login(t, e, "dee", "right horse")
	h := e.m.Require(RoleReadOnly, ModeAPI, okHandler())

	do := func(mut func(r *http.Request)) int {
		r := req("POST", "http://console.example/api/v1/x", &res)
		r.Host = "console.example"
		mut(r)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	good := func(r *http.Request) {
		r.Header.Set("Origin", "http://console.example")
		r.Header.Set(CSRFHeader, res.CSRF)
	}
	if c := do(good); c != 200 {
		t.Fatalf("good request: %d", c)
	}
	// Origin: null (what a browser sends under Referrer-Policy: no-referrer) is
	// allowed when the browser says the request is same-origin.
	if c := do(func(r *http.Request) {
		r.Header.Set("Origin", "null")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set(CSRFHeader, res.CSRF)
	}); c != 200 {
		t.Fatalf("null origin + Sec-Fetch-Site same-origin: %d", c)
	}
	// Sec-Fetch-Site same-origin does not replace the CSRF token.
	if c := do(func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-origin") }); c != 403 {
		t.Fatalf("same-origin without a token: %d", c)
	}
	// Evidence precedence (FIX23b): a token never overrides a browser that says the
	// request is cross-site or same-site, even beside a matching Origin.
	for _, site := range []string{"cross-site", "same-site"} {
		if c := do(func(r *http.Request) {
			good(r)
			r.Header.Set("Sec-Fetch-Site", site)
		}); c != 403 {
			t.Fatalf("matching origin, Sec-Fetch-Site %s: %d", site, c)
		}
	}
	if c := do(func(r *http.Request) {
		r.Header.Set("Referer", "https://evil.example/x")
		r.Header.Set(CSRFHeader, res.CSRF)
	}); c != 403 {
		t.Fatalf("token, foreign Referer, no Origin: %d", c)
	}
	// FIX23: a browser that strips Origin, Referer and Sec-Fetch-* is judged on the
	// session-bound token alone.
	if c := do(func(r *http.Request) { r.Header.Set(CSRFHeader, res.CSRF) }); c != 200 {
		t.Fatalf("token, no Origin, no Referer: %d", c)
	}
	if c := do(func(r *http.Request) { r.Header.Set("Origin", "null"); r.Header.Set(CSRFHeader, res.CSRF) }); c != 200 {
		t.Fatalf("token, Origin null, no Referer: %d", c)
	}
	if c := do(func(r *http.Request) {
		r.Header.Set("Origin", "null")
		r.Header.Set("Referer", "http://console.example/restore")
		r.Header.Set(CSRFHeader, res.CSRF)
	}); c != 200 {
		t.Fatalf("token, Origin null, Referer naming this host: %d", c)
	}
	cases := map[string]func(r *http.Request){
		"no token": func(r *http.Request) { r.Header.Set("Origin", "http://console.example") },
		"wrong token": func(r *http.Request) {
			r.Header.Set("Origin", "http://console.example")
			r.Header.Set(CSRFHeader, "nope")
		},
		"foreign origin": func(r *http.Request) {
			r.Header.Set("Origin", "https://evil.example")
			r.Header.Set(CSRFHeader, res.CSRF)
		},
		"no token, no origin":   func(r *http.Request) {},
		"no token, null origin": func(r *http.Request) { r.Header.Set("Origin", "null") },
		"null origin, foreign referer": func(r *http.Request) {
			r.Header.Set("Origin", "null")
			r.Header.Set("Referer", "https://evil.example/x")
			r.Header.Set(CSRFHeader, res.CSRF)
		},
		"no origin, cross-site fetch": func(r *http.Request) {
			r.Header.Set("Sec-Fetch-Site", "cross-site")
			r.Header.Set(CSRFHeader, res.CSRF)
		},
		"no origin, same-site fetch": func(r *http.Request) {
			r.Header.Set("Sec-Fetch-Site", "same-site")
			r.Header.Set(CSRFHeader, res.CSRF)
		},
		"null origin, cross-site fetch": func(r *http.Request) {
			r.Header.Set("Origin", "null")
			r.Header.Set("Sec-Fetch-Site", "cross-site")
			r.Header.Set(CSRFHeader, res.CSRF)
		},
		"foreign origin, cross-site fetch": func(r *http.Request) {
			r.Header.Set("Origin", "https://evil.example")
			r.Header.Set("Sec-Fetch-Site", "cross-site")
			r.Header.Set(CSRFHeader, res.CSRF)
		},
		"foreign origin, same-site fetch": func(r *http.Request) {
			r.Header.Set("Origin", "https://evil.example")
			r.Header.Set("Sec-Fetch-Site", "same-site")
			r.Header.Set(CSRFHeader, res.CSRF)
		},
		"cookie mismatch": func(r *http.Request) {
			good(r)
			r.Header.Del("Cookie")
			r.AddCookie(&http.Cookie{Name: SessionCookie, Value: res.Token})
			r.AddCookie(&http.Cookie{Name: CSRFCookie, Value: "other"})
		},
		"token of another session": func(r *http.Request) {
			r.Header.Set("Origin", "http://console.example")
			r.Header.Set(CSRFHeader, CSRFToken("some other session token"))
		},
	}
	for name, mut := range cases {
		if c := do(mut); c != 403 {
			t.Errorf("%s: want 403, got %d", name, c)
		}
	}
	// Form field instead of header, and a Referer instead of an Origin.
	r := req("POST", "http://console.example/x", &res)
	r.Host = "console.example"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Referer", "http://console.example/devices")
	r.Body = httptestBody(url.Values{CSRFField: {res.CSRF}}.Encode())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("form token + referer: %d", w.Code)
	}
	// Safe methods need no token.
	r = req("GET", "http://console.example/x", &res)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("GET: %d", w.Code)
	}
}

func httptestBody(s string) *bodyCloser { return &bodyCloser{strings.NewReader(s)} }

type bodyCloser struct{ *strings.Reader }

func (bodyCloser) Close() error { return nil }

func TestRoleGatesAndUnauthenticated(t *testing.T) {
	e := newEnv(t)
	e.addAdmin(t, "ro", "read_only", "right horse")
	e.addAdmin(t, "op", "operator", "right horse")
	e.addAdmin(t, "ow", "owner", "right horse")
	ro, op, ow := login(t, e, "ro", "right horse"), login(t, e, "op", "right horse"), login(t, e, "ow", "right horse")

	api401 := func(h http.Handler, res *LoginResult) (int, string) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req("GET", "/x", res))
		return w.Code, w.Header().Get("Content-Type")
	}
	needOP := e.m.Require(RoleOperator, ModeAPI, okHandler())
	if c, ct := api401(needOP, nil); c != 401 || ct != api.ContentTypeProblem {
		t.Fatalf("anonymous API: %d %s", c, ct)
	}
	if c, ct := api401(needOP, &ro); c != 403 || ct != api.ContentTypeProblem {
		t.Fatalf("read_only on an operator route: %d %s", c, ct)
	}
	if c, _ := api401(needOP, &op); c != 200 {
		t.Fatalf("operator: %d", c)
	}
	if c, _ := api401(e.m.Require(RoleOwner, ModeAPI, okHandler()), &op); c != 403 {
		t.Fatalf("operator on an owner route: %d", c)
	}
	if c, _ := api401(e.m.Require(RoleOwner, ModeAPI, okHandler()), &ow); c != 200 {
		t.Fatalf("owner: %d", c)
	}
	// UI mode redirects anonymous users to /login.
	w := httptest.NewRecorder()
	e.m.Require(RoleReadOnly, ModeUI, okHandler()).ServeHTTP(w, req("GET", "/", nil))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("UI anonymous: %d %s", w.Code, w.Header().Get("Location"))
	}
	if RoleOperator.Short() != "OP" || RoleOwner.Short() != "OW" || RoleReadOnly.Short() != "RO" || RoleAdmin.Short() != "AD" {
		t.Fatal("role abbreviations")
	}
}

func TestScopedAdminPrincipalCarriesScope(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	h, _ := HashPassword("right horse")
	if _, err := e.st.CreateAdmin(ctx, store.Admin{Username: "scoped", Email: "s@example.invalid", Role: "operator", PasswordHash: h, CustomerScope: []string{"01J9ZZ0000000000000000AAAA"}}); err != nil {
		t.Fatal(err)
	}
	res := login(t, e, "scoped", "right horse")
	p, err := e.m.Authenticate(ctx, res.Token)
	if err != nil || p.Scope.All || !p.Scope.Allows("01J9ZZ0000000000000000AAAA") || p.Scope.Allows("01J9ZZ0000000000000000BBBB") {
		t.Fatalf("scope: %v %+v", err, p)
	}
}

func TestCookiesAreHardened(t *testing.T) {
	e := newEnv(t)
	e.addAdmin(t, "ed", "admin", "right horse")
	res := login(t, e, "ed", "right horse")
	w := httptest.NewRecorder()
	e.m.SetCookies(w, httptest.NewRequest("GET", "https://console.test/", nil), res)
	var sess, csrf *http.Cookie
	for _, c := range w.Result().Cookies() {
		switch c.Name {
		case SessionCookie:
			sess = c
		case CSRFCookie:
			csrf = c
		}
	}
	if sess == nil || !sess.HttpOnly || !sess.Secure || sess.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie flags: %+v", sess)
	}
	if csrf == nil || csrf.HttpOnly || !csrf.Secure || csrf.SameSite != http.SameSiteStrictMode {
		t.Fatalf("csrf cookie flags: %+v", csrf)
	}
	if raw, err := base64.RawURLEncoding.DecodeString(sess.Value); err != nil || len(raw) != 32 {
		t.Fatal("session token must be 32 random bytes, base64url")
	}
	if strings.Contains(sess.Value, "=") {
		t.Fatal("unpadded base64url")
	}
}

func TestGeneratedPassword(t *testing.T) {
	a, _ := GeneratePassword(20)
	b, _ := GeneratePassword(20)
	if len(a) != 20 || a == b || strings.ContainsAny(a, "0OIl1") {
		t.Fatalf("generated password: %q %q", a, b)
	}
}

// The failure Login returns for an unknown user, a wrong password, a disabled
// account and a locked account is one and the same value: no lockout or
// account-existence oracle. The real reason is in the audit row only.
func TestLoginFailureReasonsAreIdenticalToTheCallerAndDistinctInTheAudit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.addAdmin(t, "bob", "admin", "right horse")
	off := e.addAdmin(t, "dee", "admin", "right horse")
	if err := e.st.Write(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE admin_users SET disabled_at = ? WHERE id = ?`, e.clk.Now().UnixMilli(), off.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ip := "198.51.100.9"
	_, unknown := e.m.Login(ctx, "nobody", "x", ip, "")
	_, wrong := e.m.Login(ctx, "bob", "wrong", ip, "")
	_, disabled := e.m.Login(ctx, "dee", "right horse", ip, "")
	for i := 0; i < 2; i++ { // with the failure above that is LockAfter (3)
		_, _ = e.m.Login(ctx, "bob", "wrong", ip, "")
	}
	_, lockedBad := e.m.Login(ctx, "bob", "wrong", ip, "")
	res, lockedRight := e.m.Login(ctx, "bob", "right horse", ip, "")
	for name, err := range map[string]error{"unknown": unknown, "wrong": wrong, "disabled": disabled, "locked+wrong": lockedBad, "locked+right": lockedRight} {
		if err != ErrInvalidCredentials { // identity, not errors.Is: nothing wrapped that could carry a reason
			t.Errorf("%s: got %v, want exactly ErrInvalidCredentials", name, err)
		}
	}
	if res.Token != "" {
		t.Fatal("a locked account must not receive a session, even with the right password")
	}
	reasons := map[string]int{}
	ev, err := e.st.ListAudit(ctx, store.ScopeAll(), store.AuditFilter{Action: "auth.login"}, store.Page{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ev.Items {
		var d struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(x.Details, &d)
		reasons[d.Reason]++
	}
	for _, want := range []string{"unknown_user", "bad_password", "disabled", "locked_now", "locked"} {
		if reasons[want] == 0 {
			t.Errorf("audit has no %q row: %v", want, reasons)
		}
	}
	if reasons["locked"] != 2 {
		t.Errorf("both refused attempts on the locked account must be audited as locked: %v", reasons)
	}
	// The lock still lifts after the window.
	e.clk.Advance(16 * time.Minute)
	if _, err := e.m.Login(ctx, "bob", "right horse", ip, ""); err != nil {
		t.Fatalf("lock must expire: %v", err)
	}
}

// dummyHash is a literal; it must stay valid and at the live parameters, or the
// unknown-user path would stop costing what a real verification costs.
func TestDummyHashMatchesTheLiveParameters(t *testing.T) {
	h, ok := parsePHC(dummyHash)
	if !ok {
		t.Fatal("dummyHash does not parse")
	}
	if h.mem != argonMemKiB || h.time != argonTime || h.threads != argonThreads || len(h.key) != keyLen || len(h.salt) != saltLen {
		t.Fatalf("dummyHash parameters drifted from HashPassword's: %+v", h)
	}
	if ok, _ := VerifyPassword(dummyHash, "cryobackup dummy password"); !ok {
		t.Fatal("dummyHash must be a valid verification target")
	}
}

func TestOriginRules(t *testing.T) {
	m := &Manager{PublicHost: "console.example.org"}
	type hdr = map[string]string
	cases := []struct {
		name string
		host string
		h    hdr
		ok   bool
	}{
		{"origin null + same-origin", "10.0.0.1:8080", hdr{"Origin": "null", "Sec-Fetch-Site": "same-origin"}, true},
		{"same-origin, no origin, no referer", "10.0.0.1:8080", hdr{"Sec-Fetch-Site": "same-origin"}, true},
		{"origin null alone", "10.0.0.1:8080", hdr{"Origin": "null"}, false},
		{"origin null, no-referrer style", "10.0.0.1:8080", hdr{"Origin": "null", "Sec-Fetch-Site": "none"}, false},
		{"cross-site, foreign origin", "10.0.0.1:8080", hdr{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}, false},
		{"same-site, foreign origin", "10.0.0.1:8080", hdr{"Origin": "https://evil.example", "Sec-Fetch-Site": "same-site"}, false},
		{"cross-site, origin null", "10.0.0.1:8080", hdr{"Origin": "null", "Sec-Fetch-Site": "cross-site"}, false},
		{"cross-site, matching origin", "10.0.0.1:8080", hdr{"Origin": "http://10.0.0.1:8080", "Sec-Fetch-Site": "cross-site"}, true},
		{"matching origin, no fetch metadata", "10.0.0.1:8080", hdr{"Origin": "http://10.0.0.1:8080"}, true},
		{"public host origin", "10.0.0.1:8080", hdr{"Origin": "https://console.example.org"}, true},
		{"foreign origin", "10.0.0.1:8080", hdr{"Origin": "https://evil.example"}, false},
		{"matching referer", "10.0.0.1:8080", hdr{"Referer": "http://10.0.0.1:8080/login"}, true},
		{"foreign referer", "10.0.0.1:8080", hdr{"Referer": "https://evil.example/x"}, false},
		{"nothing", "10.0.0.1:8080", hdr{}, false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "http://"+c.host+"/login", nil)
		r.Host = c.host
		for k, v := range c.h {
			r.Header.Set(k, v)
		}
		if got := m.originOK(r); got != c.ok {
			t.Errorf("originOK %s: got %v want %v", c.name, got, c.ok)
		}
	}
	login := []struct {
		name string
		h    hdr
		ok   bool
	}{
		{"script, no headers", hdr{}, true},
		{"origin null + same-origin", hdr{"Origin": "null", "Sec-Fetch-Site": "same-origin"}, true},
		{"origin null alone", hdr{"Origin": "null"}, false},
		{"cross-site, foreign origin", hdr{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}, false},
		{"matching origin", hdr{"Origin": "http://10.0.0.1:8080"}, true},
	}
	for _, c := range login {
		r := httptest.NewRequest("POST", "http://10.0.0.1:8080/login", nil)
		r.Host = "10.0.0.1:8080"
		for k, v := range c.h {
			r.Header.Set(k, v)
		}
		if got := m.LoginOriginOK(r); got != c.ok {
			t.Errorf("LoginOriginOK %s: got %v want %v", c.name, got, c.ok)
		}
	}
}
