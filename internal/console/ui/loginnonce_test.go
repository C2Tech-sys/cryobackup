package ui

import (
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
)

var nonceField = regexp.MustCompile(`<input type="hidden" name="login_nonce" value="([^"]+)">`)

// loginPostWith posts the sign-in form with exactly the cookies and headers
// given (the env's session cookies are not added), so a test can model a browser
// that strips Origin, Referer and Sec-Fetch-Site.
func (e *env) loginPostWith(form url.Values, cookies []*http.Cookie, hdr ...string) (*http.Response, string) {
	e.t.Helper()
	r, _ := http.NewRequest("POST", e.srv.URL+"/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := e.cli.Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func cookieNamed(res *http.Response, name string) *http.Cookie {
	for _, c := range res.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// getNonce loads /login and returns the cookie and the hidden field value.
func getNonce(t *testing.T, e *env) (*http.Cookie, string) {
	t.Helper()
	res, body := e.do("GET", "/login", nil)
	c := cookieNamed(res, auth.LoginNonceCookie)
	m := nonceField.FindStringSubmatch(body)
	if c == nil || m == nil {
		t.Fatalf("GET /login: cookie %v, field %v", c, m)
	}
	return c, m[1]
}

func TestLoginPageIssuesTheNonce(t *testing.T) {
	e := newEnv(t)
	c, field := getNonce(t, e)
	if c.Value == field || len(c.Value) != 43 || len(field) != 43 {
		t.Fatalf("cookie %q field %q: the field must be a server MAC of the cookie, not the cookie", c.Value, field)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/login" || c.MaxAge != 600 || c.Secure {
		t.Fatalf("cookie attributes: %+v", c)
	}
	_, body := e.do("GET", "/login", nil)
	mustContain(t, "login form", body, `<form method="post" action="/login"`, `name="username"`, `name="password"`, `for="username"`, `for="password"`)
	if c2, _ := getNonce(t, e); c2.Value == c.Value {
		t.Fatal("two page loads shared a nonce")
	}
}

func TestLoginWithMatchingNonceNeedsNoOriginHeaders(t *testing.T) {
	e := newEnv(t)
	c, field := getNonce(t, e)
	// A hardened browser: Origin: null, no Referer, no Sec-Fetch-Site.
	res, body := e.loginPostWith(url.Values{"username": {"admin"}, "password": {"wrong"}, auth.LoginNonceField: {field}}, []*http.Cookie{c}, "Origin", "null")
	if res.StatusCode != 401 || !strings.Contains(body, "not recognised") {
		t.Fatalf("bad credentials with a good nonce: %d", res.StatusCode)
	}
	nc := cookieNamed(res, auth.LoginNonceCookie)
	if nc == nil || nc.Value == c.Value || nc.Value == "" {
		t.Fatalf("a re-rendered login page must issue a fresh nonce: %+v", nc)
	}
	m := nonceField.FindStringSubmatch(body)
	if m == nil || m[1] == field {
		t.Fatal("re-rendered page: hidden field missing or not fresh")
	}
	// The fresh pair works on its own.
	if res2, _ := e.loginPostWith(url.Values{"username": {"admin"}, "password": {"wrong"}, auth.LoginNonceField: {m[1]}}, []*http.Cookie{nc}, "Origin", "null"); res2.StatusCode != 401 {
		t.Fatalf("fresh pair from the re-rendered page: %d", res2.StatusCode)
	}
	res, body = e.loginPostWith(url.Values{"username": {"admin"}, "password": {testPassword}, auth.LoginNonceField: {field}}, []*http.Cookie{c}, "Origin", "null")
	if res.StatusCode != 303 || res.Header.Get("Location") != "/" {
		t.Fatalf("good credentials with a good nonce: %d %s", res.StatusCode, body)
	}
	if cookieNamed(res, auth.SessionCookie) == nil {
		t.Fatal("no session cookie after login")
	}
	if cl := cookieNamed(res, auth.LoginNonceCookie); cl == nil || cl.MaxAge >= 0 {
		t.Fatalf("the login nonce cookie must be cleared after a successful login: %+v", cl)
	}
}

func TestLoginNonceMissingCookieOrMismatchIsRefused(t *testing.T) {
	e := newEnv(t)
	c, field := getNonce(t, e)
	good := url.Values{"username": {"admin"}, "password": {testPassword}, auth.LoginNonceField: {field}}
	res, body := e.loginPostWith(good, nil, "Origin", "null")
	if res.StatusCode != 403 || !strings.Contains(body, "another site") || cookieNamed(res, auth.SessionCookie) != nil {
		t.Fatalf("field without cookie: %d", res.StatusCode)
	}
	other := &http.Cookie{Name: auth.LoginNonceCookie, Value: c.Value + "x"}
	res, _ = e.loginPostWith(good, []*http.Cookie{other}, "Origin", "null")
	if res.StatusCode != 403 || cookieNamed(res, auth.SessionCookie) != nil {
		t.Fatalf("mismatching nonce: %d", res.StatusCode)
	}
	// A cross-site Origin does not become acceptable by carrying a stale nonce.
	res, _ = e.loginPostWith(good, []*http.Cookie{other}, "Origin", "https://evil.example")
	if res.StatusCode != 403 {
		t.Fatalf("mismatching nonce, foreign origin: %d", res.StatusCode)
	}
}

// The nonce makes up for missing header evidence only (FIX23b): headers that
// point at another site refuse the post even with a server-issued pair.
func TestLoginNonceDoesNotOverrideContraryHeaders(t *testing.T) {
	e := newEnv(t)
	c, field := getNonce(t, e)
	form := url.Values{"username": {"admin"}, "password": {testPassword}, auth.LoginNonceField: {field}}
	for name, hdr := range map[string][]string{
		"foreign Origin":              {"Origin", "https://evil.example"},
		"Sec-Fetch-Site cross-site":   {"Origin", "null", "Sec-Fetch-Site", "cross-site"},
		"Sec-Fetch-Site same-site":    {"Origin", "null", "Sec-Fetch-Site", "same-site"},
		"foreign Referer":             {"Origin", "null", "Referer", "https://evil.example/login"},
		"matching Origin, cross-site": {"Origin", e.srv.URL, "Sec-Fetch-Site", "cross-site"},
	} {
		res, _ := e.loginPostWith(form, []*http.Cookie{c}, hdr...)
		if res.StatusCode != 403 || cookieNamed(res, auth.SessionCookie) != nil {
			t.Errorf("%s: %d", name, res.StatusCode)
		}
	}
	// Missing evidence is made up for by the nonce.
	for name, hdr := range map[string][]string{
		"Origin null":                 {"Origin", "null"},
		"no headers":                  {},
		"Origin null, same-origin":    {"Origin", "null", "Sec-Fetch-Site", "same-origin"},
		"matching Origin and Referer": {"Origin", e.srv.URL, "Referer", e.srv.URL + "/login"},
	} {
		res, body := e.loginPostWith(form, []*http.Cookie{c}, hdr...)
		if res.StatusCode != 303 {
			t.Errorf("%s: %d %s", name, res.StatusCode, body)
		}
	}
}

// An injected cookie with an invented field (no server key) is refused.
func TestLoginForgedNonceIsRefused(t *testing.T) {
	e := newEnv(t)
	forged := &http.Cookie{Name: auth.LoginNonceCookie, Value: "client-chosen"}
	for _, field := range []string{"client-chosen", "anything"} {
		form := url.Values{"username": {"admin"}, "password": {testPassword}, auth.LoginNonceField: {field}}
		res, _ := e.loginPostWith(form, []*http.Cookie{forged}, "Origin", "null")
		if res.StatusCode != 403 || cookieNamed(res, auth.SessionCookie) != nil {
			t.Fatalf("forged pair %q: %d", field, res.StatusCode)
		}
	}
}

func TestLoginWithoutNonceFallsBackToTheOriginRule(t *testing.T) {
	e := newEnv(t)
	form := url.Values{"username": {"admin"}, "password": {testPassword}}
	if res, _ := e.loginPostWith(form, nil, "Origin", "null"); res.StatusCode != 403 {
		t.Fatalf("no nonce, Origin null: %d", res.StatusCode)
	}
	if res, _ := e.loginPostWith(form, nil, "Origin", "https://evil.example"); res.StatusCode != 403 {
		t.Fatalf("no nonce, foreign Origin: %d", res.StatusCode)
	}
	res, body := e.loginPostWith(form, nil, "Origin", e.srv.URL)
	if res.StatusCode != 303 || res.Header.Get("Location") != "/" {
		t.Fatalf("no nonce, matching Origin: %d %s", res.StatusCode, body)
	}
}
