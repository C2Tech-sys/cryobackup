package auth

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/httpx"
)

// P21-T7 A: with trusted_proxies set, only a trusted peer may assert https with X-Forwarded-Proto;
// with it unset the private-peer rule of FIX17 stays (TestCookieSecureFlagFollowsTheConnection).
func TestForwardedProtoFollowsTrustedProxiesWhenSet(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.2/32"), netip.MustParsePrefix("192.0.2.0/28")}
	cases := []struct {
		name   string
		list   []netip.Prefix // nil: trusted_proxies unset, the middleware is not installed
		peer   string
		xfp    string
		tls    bool
		secure bool
	}{
		{"unset: private peer may assert https", nil, "10.0.0.5:1", "https", false, true},
		{"unset: loopback peer may assert https", nil, "127.0.0.1:1", "https", false, true},
		{"unset: public peer may not", nil, "203.0.113.9:1", "https", false, false},
		{"set: trusted loopback proxy may assert https", trusted, "127.0.0.2:1", "https", false, true},
		{"set: trusted public-range proxy may assert https", trusted, "192.0.2.5:1", "https", false, true},
		{"set: trusted proxy, last value wins", trusted, "127.0.0.2:1", "https, http", false, false},
		{"set: trusted proxy without the header", trusted, "127.0.0.2:1", "", false, false},
		{"set: loopback peer outside the list may not", trusted, "127.0.0.1:1", "https", false, false},
		{"set: private peer outside the list may not", trusted, "10.0.0.5:1", "https", false, false},
		{"set: public peer outside the list may not", trusted, "203.0.113.9:1", "https", false, false},
		{"set: TLS is secure whoever the peer is", trusted, "203.0.113.9:1", "", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://console.test/login", nil)
			r.RemoteAddr = c.peer
			if c.xfp != "" {
				r.Header.Set("X-Forwarded-Proto", c.xfp)
			}
			if c.tls {
				r.TLS = &tls.ConnectionState{}
			}
			var got bool
			httpx.ForwardedFor(c.list, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = secureConn(r) })).
				ServeHTTP(httptest.NewRecorder(), r)
			if got != c.secure {
				t.Fatalf("secureConn = %v, want %v", got, c.secure)
			}
		})
	}
}

// P21-T7 B: the host of admin_url joins the hosts an Origin or Referer may name, beside the
// request's own Host and the public host; unset, nothing changes; any other host is still refused.
func TestOriginCheckAcceptsTheAdminHost(t *testing.T) {
	const bind = "127.0.0.1:8080" // the Host a proxy that does not preserve it would send
	mk := func(origin, referer string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "http://"+bind+"/api/v1/auth/login", nil)
		r.Host = bind
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if referer != "" {
			r.Header.Set("Referer", referer)
		}
		return r
	}
	set := &Manager{PublicHost: "agents.example.test:8443", AdminHost: "console.example.net"}
	unset := &Manager{PublicHost: "agents.example.test:8443"}
	cases := []struct {
		name            string
		m               *Manager
		origin, referer string
		ok              bool
	}{
		{"set: Origin names the admin host", set, "https://console.example.net", "", true},
		{"set: Referer names the admin host", set, "", "https://console.example.net/devices", true},
		{"set: the own Host still works", set, "http://" + bind, "", true},
		{"set: the public host still works", set, "https://agents.example.test:8443", "", true},
		{"set: a wrong host is refused", set, "https://evil.example.org", "", false},
		{"set: the admin host with another port is refused", set, "https://console.example.net:8443", "", false},
		{"set: a wrong Referer is refused", set, "", "https://evil.example.org/", false},
		{"unset: the admin host is refused", unset, "https://console.example.net", "", false},
		{"unset: the own Host works", unset, "http://" + bind, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := mk(c.origin, c.referer)
			if got := c.m.originOK(r); got != c.ok {
				t.Errorf("originOK = %v, want %v", got, c.ok)
			}
			if got := c.m.LoginOriginOK(r); got != c.ok {
				t.Errorf("LoginOriginOK = %v, want %v", got, c.ok)
			}
			if got := c.m.HeadersContradict(r); got == c.ok {
				t.Errorf("HeadersContradict = %v, want %v", got, !c.ok)
			}
			if got := c.m.csrfOriginOK(r); got != c.ok {
				t.Errorf("csrfOriginOK = %v, want %v", got, c.ok)
			}
		})
	}
}
