package httpx

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// P21-T7: trusted_proxies on the admin listener. All addresses are documentation (RFC 5737, RFC 3849)
// or private (RFC 1918) ones.

var t7Trusted = []netip.Prefix{netip.MustParsePrefix("198.51.100.7/32"), netip.MustParsePrefix("10.0.0.0/8")}

// seen records what a handler behind ForwardedFor reads.
type seen struct {
	ip                string
	set, peerTrusted  bool
	calls             int
	lastRemoteAddress string
}

func (s *seen) ServeHTTP(_ http.ResponseWriter, r *http.Request) {
	s.calls++
	s.ip = ClientIP(r)
	s.set, s.peerTrusted = ProxyTrust(r)
	s.lastRemoteAddress = r.RemoteAddr
}

func t7Request(peer string, xff ...string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "http://console.test/api/v1/auth/login", nil)
	r.RemoteAddr = peer
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

// Without the middleware (the agent listener, and the admin listener with trusted_proxies unset)
// ClientIP is the peer, whatever X-Forwarded-For says: today's behaviour.
func TestClientIPIgnoresXForwardedForWithoutTheMiddleware(t *testing.T) {
	for _, peer := range []string{"198.51.100.7:5000", "10.0.0.5:5000", "127.0.0.1:5000", "203.0.113.50:5000"} {
		r := t7Request(peer, "192.0.2.1")
		if got, want := ClientIP(r), peer[:len(peer)-5]; got != want {
			t.Errorf("ClientIP from %s with a forged header = %q, want the peer %q", peer, got, want)
		}
		if set, trusted := ProxyTrust(r); set || trusted {
			t.Errorf("ProxyTrust without the middleware = %v, %v; want false, false", set, trusted)
		}
	}
	r := t7Request("pipe", "192.0.2.1") // no port: the raw value, as before
	if got := ClientIP(r); got != "pipe" {
		t.Errorf("ClientIP of a portless peer = %q, want it unchanged", got)
	}
}

// An empty list changes nothing: the very handler and registrar given are returned.
func TestEmptyTrustedProxiesReturnsTheSameHandlerAndRegistrar(t *testing.T) {
	h := &seen{}
	if got := ForwardedFor(nil, nil, h); got != http.Handler(h) {
		t.Fatalf("ForwardedFor(nil) wrapped the handler")
	}
	if got := ForwardedFor([]netip.Prefix{}, nil, h); got != http.Handler(h) {
		t.Fatalf("ForwardedFor(empty) wrapped the handler")
	}
	reg := &recordingRegistrar{}
	if got := ForwardedForRegistrar(reg, nil, nil); got != Registrar(reg) {
		t.Fatalf("ForwardedForRegistrar(nil) wrapped the registrar")
	}
}

func TestForwardedForTakesTheRightmostUntrustedEntry(t *testing.T) {
	const proxy = "198.51.100.7:40000"
	cases := []struct {
		name, peer  string
		xff         []string
		want        string
		peerTrusted bool
	}{
		{"untrusted peer: a spoofed header changes nothing", "203.0.113.50:1234", []string{"192.0.2.1"}, "203.0.113.50", false},
		{"untrusted private peer: ignored too", "192.168.1.20:1234", []string{"192.0.2.1"}, "192.168.1.20", false},
		{"untrusted peer, trusted-looking chain", "203.0.113.50:1234", []string{"10.0.0.1, 198.51.100.7"}, "203.0.113.50", false},
		{"trusted peer, one entry", proxy, []string{"192.0.2.1"}, "192.0.2.1", true},
		{"trusted peer, the rightmost entry wins", proxy, []string{"192.0.2.66, 192.0.2.1"}, "192.0.2.1", true},
		{"trusted entries on the right are skipped", proxy, []string{"192.0.2.1, 10.1.2.3, 198.51.100.7"}, "192.0.2.1", true},
		{"several header lines are one chain", proxy, []string{"192.0.2.66", "192.0.2.1"}, "192.0.2.1", true},
		{"a forged left part is never reached", proxy, []string{"not-an-ip, 192.0.2.1"}, "192.0.2.1", true},
		{"no header: the peer", proxy, nil, "198.51.100.7", true},
		{"empty header: the peer", proxy, []string{""}, "198.51.100.7", true},
		{"blank header: the peer", proxy, []string{"  ,  "}, "198.51.100.7", true},
		{"malformed rightmost: the peer", proxy, []string{"192.0.2.1, garbage"}, "198.51.100.7", true},
		{"empty rightmost: the peer", proxy, []string{"192.0.2.1,,"}, "198.51.100.7", true},
		{"an entry with a port is malformed", proxy, []string{"192.0.2.1:443"}, "198.51.100.7", true},
		{"a bracketed entry is malformed", proxy, []string{"[2001:db8::1]"}, "198.51.100.7", true},
		{"a zone is malformed", proxy, []string{"fe80::1%eth0"}, "198.51.100.7", true},
		{"all trusted: the peer", proxy, []string{"10.0.0.1, 10.0.0.2"}, "198.51.100.7", true},
		{"IPv4-mapped entry is unmapped", proxy, []string{"::ffff:192.0.2.1"}, "192.0.2.1", true},
		{"IPv6 client", proxy, []string{"2001:db8::1"}, "2001:db8::1", true},
		{"IPv4-mapped trusted peer", "[::ffff:198.51.100.7]:40000", []string{"192.0.2.1"}, "192.0.2.1", true},
		{"peer in a trusted prefix", "10.20.30.40:5", []string{"192.0.2.1"}, "192.0.2.1", true},
		{"unparseable peer: not trusted", "pipe", []string{"192.0.2.1"}, "pipe", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &seen{}
			ForwardedFor(t7Trusted, nil, h).ServeHTTP(httptest.NewRecorder(), t7Request(c.peer, c.xff...))
			if h.calls != 1 {
				t.Fatalf("handler called %d times", h.calls)
			}
			if h.ip != c.want {
				t.Errorf("ClientIP = %q, want %q", h.ip, c.want)
			}
			if !h.set || h.peerTrusted != c.peerTrusted {
				t.Errorf("ProxyTrust = %v, %v; want true, %v", h.set, h.peerTrusted, c.peerTrusted)
			}
			if h.lastRemoteAddress != c.peer {
				t.Errorf("RemoteAddr changed to %q", h.lastRemoteAddress)
			}
		})
	}
}

// The decision is made once: a page's in-process call to the admin API runs on the page request's
// context with the peer's RemoteAddr and no X-Forwarded-For, and must keep the client the outer
// request was given, not fall back to the proxy.
func TestForwardedForDecidesOncePerRequest(t *testing.T) {
	inner := &seen{}
	innerChain := ForwardedFor(t7Trusted, nil, inner)
	outer := ForwardedFor(t7Trusted, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/devices", nil).WithContext(r.Context())
		req.RemoteAddr = r.RemoteAddr // what the page layer copies; the header is not copied
		innerChain.ServeHTTP(w, req)
	}))
	outer.ServeHTTP(httptest.NewRecorder(), t7Request("198.51.100.7:40000", "192.0.2.1"))
	if inner.ip != "192.0.2.1" {
		t.Fatalf("in-process call saw ClientIP %q, want the outer client 192.0.2.1", inner.ip)
	}
}

type recordingRegistrar struct{ handlers map[string]http.Handler }

func (r *recordingRegistrar) Handle(pattern string, h http.Handler) {
	if r.handlers == nil {
		r.handlers = map[string]http.Handler{}
	}
	r.handlers[pattern] = h
}

// The registrar wraps every handler registered on it, under the same pattern.
func TestForwardedForRegistrarWrapsEveryHandler(t *testing.T) {
	reg := &recordingRegistrar{}
	h := &seen{}
	w := ForwardedForRegistrar(reg, t7Trusted, nil)
	w.Handle("POST /api/v1/auth/login", h)
	HandleFunc(w, "GET /x", h.ServeHTTP)
	if len(reg.handlers) != 2 {
		t.Fatalf("registered %d patterns, want 2", len(reg.handlers))
	}
	for pattern, got := range reg.handlers {
		got.ServeHTTP(httptest.NewRecorder(), t7Request("198.51.100.7:40000", "192.0.2.1"))
		if h.ip != "192.0.2.1" {
			t.Errorf("%s: ClientIP %q, want the forwarded client", pattern, h.ip)
		}
	}
}

// V3 (P21-T7 fix batch 1): with trusted_proxies set, a loopback or private peer outside the list that
// sends X-Forwarded-For or X-Forwarded-Proto is most likely the proxy under the wrong address. One
// warning per peer address per middleware, bounded, naming the peer and the key and no header value.
func TestUnlistedPrivatePeerWithForwardedHeadersWarnsOncePerPeer(t *testing.T) {
	// The message is free text the log handler keeps only in plain words (logx redact.go); the key
	// name rides in its own field.
	const msg = "forwarded headers ignored"
	var logs bytes.Buffer
	reg := &recordingRegistrar{}
	fw := ForwardedForRegistrar(reg, t7Trusted, logx.New(&logs, logx.LevelDebug))
	h := &seen{}
	fw.Handle("POST /login", h)
	fw.Handle("GET /x", h)
	hit := func(pattern, peer string, hdr ...string) {
		t.Helper()
		r := t7Request(peer)
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Add(hdr[i], hdr[i+1])
		}
		reg.handlers[pattern].ServeHTTP(httptest.NewRecorder(), r)
	}
	count := func() int { return strings.Count(logs.String(), msg) }

	hit("POST /login", "192.168.1.20:1000", "X-Forwarded-For", "192.0.2.77")
	if count() != 1 {
		t.Fatalf("unlisted private peer with X-Forwarded-For: %d warnings, want 1\n%s", count(), logs.String())
	}
	line := logs.String()
	if !strings.Contains(line, "192.168.1.20") || !strings.Contains(line, "trusted_proxies") {
		t.Fatalf("the warning must name the peer and the key: %s", line)
	}
	if strings.Contains(line, "192.0.2.77") || strings.Contains(line, "X-Forwarded-For:") {
		t.Fatalf("the warning carries a header value: %s", line)
	}
	if h.ip != "192.168.1.20" {
		t.Fatalf("the header must still be ignored: ClientIP %q", h.ip)
	}
	// The same peer again, on another route of the same registrar, with either header: no second line.
	hit("GET /x", "192.168.1.20:2000", "X-Forwarded-Proto", "https")
	hit("POST /login", "192.168.1.20:3000", "X-Forwarded-For", "192.0.2.78")
	if count() != 1 {
		t.Fatalf("same peer again: %d warnings, want 1", count())
	}
	// No warning: a public unlisted peer (the open internet forging is not a misconfiguration), a
	// listed peer, an unlisted private peer without forwarded headers.
	hit("POST /login", "203.0.113.50:1000", "X-Forwarded-For", "192.0.2.1")
	hit("POST /login", "198.51.100.7:1000", "X-Forwarded-For", "192.0.2.1")
	hit("POST /login", "10.1.2.3:1000", "X-Forwarded-For", "192.0.2.1") // inside 10.0.0.0/8: listed
	hit("POST /login", "172.16.0.9:1000")
	if count() != 1 {
		t.Fatalf("public, listed or header-less peers warned: %d warnings, want 1\n%s", count(), logs.String())
	}
	// Loopback with X-Forwarded-Proto only: its own warning.
	hit("GET /x", "127.0.0.1:5000", "X-Forwarded-Proto", "https")
	if count() != 2 || !strings.Contains(logs.String(), "127.0.0.1") {
		t.Fatalf("loopback peer with X-Forwarded-Proto: %d warnings, want 2\n%s", count(), logs.String())
	}
	// Bounded: distinct peers past maxUnlistedWarned are not reported.
	for i := 0; i < 2*maxUnlistedWarned; i++ {
		hit("POST /login", "172.16.1."+strconv.Itoa(i)+":1", "X-Forwarded-For", "192.0.2.1")
	}
	if count() != maxUnlistedWarned {
		t.Fatalf("%d warnings after many distinct peers, want the bound %d", count(), maxUnlistedWarned)
	}
}

// With trusted_proxies unset nothing is wrapped, so nothing warns either: today's behaviour.
func TestUnsetTrustedProxiesNeverWarns(t *testing.T) {
	var logs bytes.Buffer
	h := &seen{}
	got := ForwardedFor(nil, logx.New(&logs, logx.LevelDebug), h)
	r := t7Request("192.168.1.20:1000", "192.0.2.1")
	r.Header.Set("X-Forwarded-Proto", "https")
	got.ServeHTTP(httptest.NewRecorder(), r)
	if logs.Len() != 0 {
		t.Fatalf("unset list logged: %s", logs.String())
	}
}
