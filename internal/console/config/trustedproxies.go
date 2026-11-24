package config

import (
	"fmt"
	"net/netip"
	"strings"
)

// ParseTrustedProxies parses trusted_proxies: each entry an IP address (a single
// host) or a CIDR prefix, IPv4 or IPv6, white space around it ignored. An empty
// list is valid and means no proxy is trusted (the admin listener then believes no
// forwarded header but X-Forwarded-Proto from a loopback or private peer, as
// before the key existed). A prefix of length 0 (0.0.0.0/0, ::/0) is refused: it
// would let anyone who can reach the admin listener name any client address. A
// zone (fe80::1%eth0) is refused. IPv4-mapped IPv6 entries are unmapped, the way
// the peer and the X-Forwarded-For entries are compared.
func ParseTrustedProxies(list []string) ([]netip.Prefix, error) {
	if len(list) == 0 {
		return nil, nil
	}
	out := make([]netip.Prefix, 0, len(list))
	for i, raw := range list {
		s := strings.TrimSpace(raw)
		var p netip.Prefix
		if strings.Contains(s, "/") {
			q, err := netip.ParsePrefix(s)
			if err != nil {
				return nil, fmt.Errorf("trusted_proxies entry %d is not an IP address or CIDR prefix", i+1)
			}
			if q.Bits() == 0 {
				return nil, fmt.Errorf("trusted_proxies entry %d trusts every address (a /0 prefix): name the proxy's own address instead", i+1)
			}
			a := q.Addr()
			bits := q.Bits()
			if a.Is4In6() {
				if bits < 96 {
					return nil, fmt.Errorf("trusted_proxies entry %d is not an IP address or CIDR prefix", i+1)
				}
				a, bits = a.Unmap(), bits-96
				if bits == 0 {
					return nil, fmt.Errorf("trusted_proxies entry %d trusts every address (a /0 prefix): name the proxy's own address instead", i+1)
				}
			}
			p = netip.PrefixFrom(a, bits).Masked()
		} else {
			a, err := netip.ParseAddr(s)
			if err != nil || a.Zone() != "" {
				return nil, fmt.Errorf("trusted_proxies entry %d is not an IP address or CIDR prefix", i+1)
			}
			a = a.Unmap()
			p = netip.PrefixFrom(a, a.BitLen())
		}
		out = append(out, p)
	}
	return out, nil
}

// TrustedProxyList is trusted_proxies as doctor names it: the parsed prefixes,
// comma-separated, a single address without its /32 or /128. "" when the list is
// empty or not valid (Validate refuses the latter).
func (c Config) TrustedProxyList() string {
	ps, err := ParseTrustedProxies(c.TrustedProxies)
	if err != nil || len(ps) == 0 {
		return ""
	}
	names := make([]string, len(ps))
	for i, p := range ps {
		if p.IsSingleIP() {
			names[i] = p.Addr().String()
		} else {
			names[i] = p.String()
		}
	}
	return strings.Join(names, ", ")
}
