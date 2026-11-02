package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func nonceReq(field string, cookie *http.Cookie) *http.Request {
	form := url.Values{}
	if field != "" {
		form.Set(LoginNonceField, field)
	}
	r := httptest.NewRequest("POST", "http://console.example/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	return r
}

func TestIssueLoginNonceCookieAttributes(t *testing.T) {
	m := &Manager{}
	for _, tc := range []struct {
		name   string
		target string
		secure bool
	}{{"plain http", "http://console.example/login", false}, {"tls", "https://console.example/login", true}} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", tc.target, nil)
			w := httptest.NewRecorder()
			field := m.IssueLoginNonce(w, r)
			c := cookieByName(t, w, LoginNonceCookie)
			if len(c.Value) != 43 || len(field) != 43 || field == c.Value {
				t.Fatalf("cookie %q field %q: the field must be a MAC of the cookie, not the cookie", c.Value, field)
			}
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/login" || c.MaxAge != 600 || c.Secure != tc.secure {
				t.Fatalf("attributes: %+v", c)
			}
			if w2 := httptest.NewRecorder(); m.IssueLoginNonce(w2, r) == field {
				t.Fatal("two nonces were equal")
			}
		})
	}
}

func TestLoginNonceOK(t *testing.T) {
	m, other := &Manager{}, &Manager{}
	w := httptest.NewRecorder()
	field := m.IssueLoginNonce(w, httptest.NewRequest("GET", "http://console.example/login", nil))
	c := cookieByName(t, w, LoginNonceCookie)
	// A client that injects a cookie and invents a field without the server key.
	forged := &http.Cookie{Name: LoginNonceCookie, Value: "client-chosen"}
	for _, tc := range []struct {
		name  string
		mgr   *Manager
		field string
		ck    *http.Cookie
		want  bool
	}{
		{"server-issued pair", m, field, c, true},
		{"field is the cookie value", m, c.Value, c, false},
		{"no cookie", m, field, nil, false},
		{"no field", m, "", c, false},
		{"empty cookie", m, field, &http.Cookie{Name: LoginNonceCookie, Value: ""}, false},
		{"invented cookie and field", m, "invented", forged, false},
		{"invented cookie, field equals cookie", m, forged.Value, forged, false},
		{"pair from another server key", other, field, c, false},
	} {
		if got := tc.mgr.LoginNonceOK(nonceReq(tc.field, tc.ck)); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestHeadersContradict(t *testing.T) {
	m := &Manager{PublicHost: "public.example"}
	for _, tc := range []struct {
		name string
		hdr  [][2]string
		want bool
	}{
		{"nothing", nil, false},
		{"origin null", [][2]string{{"Origin", "null"}}, false},
		{"origin this host", [][2]string{{"Origin", "http://console.example"}}, false},
		{"origin public host", [][2]string{{"Origin", "https://public.example"}}, false},
		{"origin foreign", [][2]string{{"Origin", "https://evil.example"}}, true},
		{"referer this host", [][2]string{{"Referer", "http://console.example/login"}}, false},
		{"referer foreign", [][2]string{{"Referer", "https://evil.example/"}}, true},
		{"origin null, referer foreign", [][2]string{{"Origin", "null"}, {"Referer", "https://evil.example/"}}, true},
		{"same-origin", [][2]string{{"Sec-Fetch-Site", "same-origin"}}, false},
		{"none", [][2]string{{"Sec-Fetch-Site", "none"}}, false},
		{"cross-site", [][2]string{{"Sec-Fetch-Site", "cross-site"}}, true},
		{"same-site", [][2]string{{"Sec-Fetch-Site", "same-site"}}, true},
		{"cross-site beside matching origin", [][2]string{{"Origin", "http://console.example"}, {"Sec-Fetch-Site", "cross-site"}}, true},
	} {
		r := httptest.NewRequest("POST", "http://console.example/login", nil)
		for _, h := range tc.hdr {
			r.Header.Set(h[0], h[1])
		}
		if got := m.HeadersContradict(r); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestClearLoginNonceMirrorsTheIssue(t *testing.T) {
	r := httptest.NewRequest("POST", "https://console.example/login", nil)
	w := httptest.NewRecorder()
	ClearLoginNonce(w, r)
	c := cookieByName(t, w, LoginNonceCookie)
	if c.MaxAge >= 0 || c.Path != "/login" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("clear: %+v", c)
	}
}
