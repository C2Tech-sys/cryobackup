package httpx

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"

	"github.com/c2tech-sys/cryobackup/internal/console/config"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// forwardedKey is its own key type: nothing that arrives over the wire can set a
// context value, so only ForwardedFor below can record the forwarded decision.
type forwardedKey struct{}

// forwarded is what ForwardedFor decided for one request on the admin listener.
type forwarded struct {
	// client is the address taken from X-Forwarded-For; "" when the peer is the
	// client (peer not trusted, or no usable chain).
	client string
	// peerTrusted is whether the direct peer is one of the trusted proxies.
	peerTrusted bool
}

// ForwardedFor is the one middleware that lets a reverse proxy name the client
// (config trusted_proxies, ADR draft "trusted proxies"). It belongs on the admin
// listener only, never on the agent listener. With an empty list it returns next
// itself: nothing changes.
//
// When the direct peer (r.RemoteAddr) is inside one of the trusted prefixes, the
// client address every later reader sees (ClientIP: the login limit, the audit and
// session rows) becomes the rightmost X-Forwarded-For entry that is not itself a
// trusted proxy. Every X-Forwarded-For line counts, in order, as one
// comma-separated chain. A chain that is empty, holds an entry that is not a plain
// IP address before such an entry is found, or holds only trusted proxies leaves
// the peer as the client. A peer outside the list: the header is ignored entirely.
// RFC 7239 Forwarded is never read.
//
// A loopback or private peer outside the list that sends X-Forwarded-For or
// X-Forwarded-Proto is most likely the proxy, listed under the wrong address: log
// receives one warning per such peer address (at most maxUnlistedWarned of them
// per middleware), naming the peer and the key, never a header value. nil means
// logx.Default().
//
// The decision is made once per request: a request that already carries one (a
// page's in-process call to the admin API runs on the page request's context)
// keeps it.
func ForwardedFor(trusted []netip.Prefix, log *logx.Logger, next http.Handler) http.Handler {
	if len(trusted) == 0 {
		return next
	}
	return newForwardedFor(trusted, log).wrap(next)
}

// ForwardedForRegistrar returns a Registrar that wraps every handler registered on
// it with ForwardedFor, all sharing one record of the peers already warned about.
// With an empty list it returns reg itself.
func ForwardedForRegistrar(reg Registrar, trusted []netip.Prefix, log *logx.Logger) Registrar {
	if len(trusted) == 0 {
		return reg
	}
	return forwardedRegistrar{reg: reg, f: newForwardedFor(trusted, log)}
}

type forwardedRegistrar struct {
	reg Registrar
	f   *forwardedFor
}

func (r forwardedRegistrar) Handle(pattern string, h http.Handler) {
	r.reg.Handle(pattern, r.f.wrap(h))
}

// maxUnlistedWarned bounds the peers an unlisted-peer warning is kept for, so a
// stream of distinct private peers cannot grow the map: past it, no more warnings.
const maxUnlistedWarned = 32

// forwardedFor is one ForwardedFor middleware: the list and the peers it warned about.
type forwardedFor struct {
	trusted []netip.Prefix
	log     *logx.Logger
	mu      sync.Mutex
	warned  map[netip.Addr]struct{}
}

func newForwardedFor(trusted []netip.Prefix, log *logx.Logger) *forwardedFor {
	if log == nil {
		log = logx.Default()
	}
	return &forwardedFor{trusted: trusted, log: log, warned: map[netip.Addr]struct{}{}}
}

func (f *forwardedFor) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, done := r.Context().Value(forwardedKey{}).(forwarded); done {
			next.ServeHTTP(w, r)
			return
		}
		d := forwarded{}
		peer, ok := peerAddr(r.RemoteAddr)
		switch {
		case ok && inPrefixes(f.trusted, peer):
			d.peerTrusted = true
			d.client = clientFromChain(f.trusted, r.Header.Values("X-Forwarded-For"))
		case ok && config.IsLoopbackOrPrivate(r.RemoteAddr) &&
			(len(r.Header.Values("X-Forwarded-For")) > 0 || len(r.Header.Values("X-Forwarded-Proto")) > 0):
			f.warnUnlisted(peer)
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), forwardedKey{}, d)))
	})
}

// warnUnlisted logs, once per peer address, that a loopback or private peer outside
// trusted_proxies sent forwarded headers and they were ignored. The line names the
// peer (an address parsed from the connection, not from a header) and the key only.
func (f *forwardedFor) warnUnlisted(peer netip.Addr) {
	f.mu.Lock()
	_, seen := f.warned[peer]
	full := len(f.warned) >= maxUnlistedWarned
	if !seen && !full {
		f.warned[peer] = struct{}{}
	}
	f.mu.Unlock()
	if seen || full {
		return
	}
	f.log.Warn("forwarded headers ignored: the peer is loopback or private but not a listed trusted proxy",
		logx.Host("peer", peer.String()), logx.Str("key", "trusted_proxies"))
}

// ProxyTrust reports whether the trusted-proxy list is in force for this request
// (set: ForwardedFor ran, that is trusted_proxies is not empty) and, if so,
// whether the direct peer is one of the trusted proxies. The forwarded-scheme rule
// of package auth reads it: with the list set only a trusted peer may assert https.
func ProxyTrust(r *http.Request) (set, peerTrusted bool) {
	f, ok := r.Context().Value(forwardedKey{}).(forwarded)
	return ok, ok && f.peerTrusted
}

// peerAddr parses the host of a host:port peer address. IPv4-mapped IPv6 is
// unmapped; a zone is refused.
func peerAddr(remote string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return netip.Addr{}, false
	}
	return plainAddr(host)
}

// plainAddr parses s as an IP address with no port, brackets or zone, unmapped.
func plainAddr(s string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(s)
	if err != nil || a.Zone() != "" {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

func inPrefixes(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// clientFromChain walks the X-Forwarded-For chain from the right and returns the
// first entry that is not a trusted proxy, or "" (the peer stays the client) when
// the chain is empty, an entry before that one is not a plain IP address, or every
// entry is trusted.
func clientFromChain(trusted []netip.Prefix, lines []string) string {
	chain := strings.Join(lines, ",")
	if strings.TrimSpace(chain) == "" {
		return ""
	}
	parts := strings.Split(chain, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		a, ok := plainAddr(strings.TrimSpace(parts[i]))
		if !ok {
			return ""
		}
		if !inPrefixes(trusted, a) {
			return a.String()
		}
	}
	return ""
}
