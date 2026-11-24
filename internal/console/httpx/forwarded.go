package httpx

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
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
// The decision is made once per request: a request that already carries one (a
// page's in-process call to the admin API runs on the page request's context)
// keeps it.
func ForwardedFor(trusted []netip.Prefix, next http.Handler) http.Handler {
	if len(trusted) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, done := r.Context().Value(forwardedKey{}).(forwarded); done {
			next.ServeHTTP(w, r)
			return
		}
		f := forwarded{}
		if peer, ok := peerAddr(r.RemoteAddr); ok && inPrefixes(trusted, peer) {
			f.peerTrusted = true
			f.client = clientFromChain(trusted, r.Header.Values("X-Forwarded-For"))
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), forwardedKey{}, f)))
	})
}

// ForwardedForRegistrar returns a Registrar that wraps every handler registered on
// it with ForwardedFor. With an empty list it returns reg itself.
func ForwardedForRegistrar(reg Registrar, trusted []netip.Prefix) Registrar {
	if len(trusted) == 0 {
		return reg
	}
	return forwardedRegistrar{reg: reg, trusted: trusted}
}

type forwardedRegistrar struct {
	reg     Registrar
	trusted []netip.Prefix
}

func (f forwardedRegistrar) Handle(pattern string, h http.Handler) {
	f.reg.Handle(pattern, ForwardedFor(f.trusted, h))
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
