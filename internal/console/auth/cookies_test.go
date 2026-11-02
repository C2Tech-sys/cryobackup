package auth

import (
	"crypto/tls"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// cookieByName returns the Set-Cookie with the given name from a recorder.
func cookieByName(t *testing.T, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %s cookie in response", name)
	return nil
}

// TestCookieSecureFlagFollowsTheConnection covers the plain-HTTP admin listener
// (FIX17): Secure only when the request came over TLS or via a TLS front, and
// the logout clear mirrors what login set. HttpOnly and SameSite never change.
func TestCookieSecureFlagFollowsTheConnection(t *testing.T) {
	e := newEnv(t)
	e.addAdmin(t, "ed", "admin", "right horse")
	res := login(t, e, "ed", "right horse")

	const private, public = "10.0.0.5:51000", "203.0.113.9:51000"
	plain := func() *http.Request {
		r := httptest.NewRequest("POST", "http://console.test/login", nil)
		r.RemoteAddr = private
		return r
	}
	withTLS := func() *http.Request {
		r := plain()
		r.RemoteAddr = public // TLS is the primary rule, whoever the peer is
		r.TLS = &tls.ConnectionState{}
		return r
	}
	forwarded := func(peer, v string) func() *http.Request {
		return func() *http.Request {
			r := plain()
			r.RemoteAddr = peer
			r.Header.Set("X-Forwarded-Proto", v)
			return r
		}
	}
	cases := []struct {
		name   string
		req    func() *http.Request
		secure bool
	}{
		{"plain http", plain, false},
		{"tls", withTLS, true},
		{"xfp https from private peer", forwarded(private, "https"), true},
		{"xfp https from loopback v4", forwarded("127.0.0.1:1234", "https"), true},
		{"xfp https from loopback v6", forwarded("[::1]:1234", "https"), true},
		{"xfp https from ULA peer", forwarded("[fd00::5]:1234", "https"), true},
		{"xfp HTTPS with space from loopback", forwarded("127.0.0.1:1234", " HTTPS"), true},
		{"xfp http, https from loopback (last wins)", forwarded("127.0.0.1:1234", "http, https"), true},
		{"xfp https, http from loopback (last wins)", forwarded("127.0.0.1:1234", "https, http"), false},
		{"xfp http from loopback", forwarded("127.0.0.1:1234", "http"), false},
		{"xfp garbage from loopback", forwarded("127.0.0.1:1234", "httpsx"), false},
		{"xfp https from CGNAT mesh peer (100.64.0.0)", forwarded("100.64.0.0:1234", "https"), true},
		{"xfp https from CGNAT mesh peer (100.127.255.255)", forwarded("100.127.255.255:1234", "https"), true},
		{"xfp https from just below CGNAT is ignored", forwarded("100.63.255.255:1234", "https"), false},
		{"xfp https from just above CGNAT is ignored", forwarded("100.128.0.0:1234", "https"), false},
		{"xfp https from public peer is ignored", forwarded(public, "https"), false},
		{"xfp http, https from public peer is ignored", forwarded(public, "http, https"), false},
		{"xfp https with unparsable peer is ignored", forwarded("not-an-addr", "https"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			e.m.SetCookies(w, tc.req(), res)
			sess, csrf := cookieByName(t, w, SessionCookie), cookieByName(t, w, CSRFCookie)
			if sess.Secure != tc.secure || csrf.Secure != tc.secure {
				t.Fatalf("Secure: session=%v csrf=%v, want %v", sess.Secure, csrf.Secure, tc.secure)
			}
			if !sess.HttpOnly || csrf.HttpOnly || sess.SameSite != http.SameSiteStrictMode || csrf.SameSite != http.SameSiteStrictMode {
				t.Fatalf("hardening flags changed: %+v %+v", sess, csrf)
			}

			w = httptest.NewRecorder()
			ClearCookies(w, tc.req())
			for _, n := range []string{SessionCookie, CSRFCookie} {
				c := cookieByName(t, w, n)
				if c.Secure != tc.secure || c.MaxAge >= 0 || c.Value != "" || c.SameSite != http.SameSiteStrictMode {
					t.Fatalf("clear %s: %+v, want Secure=%v and expired", n, c, tc.secure)
				}
				if c.HttpOnly != (n == SessionCookie) {
					t.Fatalf("clear %s HttpOnly=%v", n, c.HttpOnly)
				}
			}
		})
	}
}

// TestPlainHTTPSessionCookieIsReturnedByAJar is the lab regression: the cookie
// set over http:// must be one a client will send back (Secure cookies are not).
func TestPlainHTTPSessionCookieIsReturnedByAJar(t *testing.T) {
	e := newEnv(t)
	// The jar judges cookie expiry by the real clock, so the session must be
	// issued at real time, not at the fixed test date.
	e.clk.Advance(time.Since(e.clk.Now()))
	e.addAdmin(t, "ed", "admin", "right horse")
	res := login(t, e, "ed", "right horse")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.m.SetCookies(w, r, res)
	}))
	defer srv.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Jar: jar}).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	u, _ := url.Parse(srv.URL)
	if got := len(jar.Cookies(u)); got != 2 {
		t.Fatalf("jar would send %d cookies back over http, want 2", got)
	}
}
