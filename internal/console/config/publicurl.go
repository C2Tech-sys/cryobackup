package config

import (
	"errors"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// publicURLRE is the only shape public_url and admin_url may have: http(s), a host of
// letters, digits, dots and hyphens or a bracketed IPv6 literal, an optional
// numeric port, at most a trailing slash. The expression only bounds the bracket's
// characters; parseServerURL then requires the bracketed text to be an IPv6
// address (RFC 3986 IP-literal: an IPv4 address in brackets is not one, and
// net/url refuses it, so a link built from it would never render). No userinfo, path, query, fragment
// or percent-escape. public_url ends up inside generated PowerShell and msiexec
// text, and admin_url inside every e-mail the console sends, so anything else is
// refused here instead of being quoted there. One expression for both keys: a
// change to it changes both.
var publicURLRE = regexp.MustCompile(`^(https?)://((?:[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?)|(?:\[[0-9A-Fa-f:.]+\]))(?::([0-9]{1,5}))?/?$`)

// ParsePublicURL validates a public_url and returns it reduced to
// scheme://host[:port]. The error never echoes the value.
func ParsePublicURL(raw string) (string, error) {
	return parseServerURL("public_url", raw)
}

// ParseAdminURL validates an admin_url, the address an operator types to open the
// console (not the agent API address: that is public_url), and returns it reduced
// to scheme://host[:port]. It accepts exactly what ParsePublicURL accepts. The
// error never echoes the value.
func ParseAdminURL(raw string) (string, error) {
	return parseServerURL("admin_url", raw)
}

// AdminLink is the console link of an e-mail: admin_url as scheme://host[:port]/, or
// "" when admin_url is unset or not valid (Validate refuses the latter at load). It
// never falls back to public_url: that is the agent API, which serves no console page.
func (c Config) AdminLink() string {
	u, err := ParseAdminURL(c.AdminURL)
	if err != nil {
		return ""
	}
	return u + "/"
}

// AdminHost is the host[:port] of admin_url as a browser names it in an Origin or
// Referer header (the WHATWG URL serialisation): the host in lower case, an IPv6
// literal in its canonical compressed form (an IPv4-mapped one in hex, as browsers
// write it), the port without leading zeros and left out when it is the scheme's
// default (443 for https, 80 for http). "" when admin_url is unset or not valid.
// The admin listener accepts it beside its own Host and the public host in the
// origin check of an unsafe request (package auth). It changes nothing about what
// is validated or what AdminLink puts into e-mails.
func (c Config) AdminHost() string {
	u, err := ParseAdminURL(c.AdminURL)
	if err != nil {
		return ""
	}
	scheme, rest, _ := strings.Cut(u, "://")
	host, port := rest, ""
	if i := strings.LastIndexByte(rest, ':'); i >= 0 && !strings.HasSuffix(rest, "]") {
		host, port = rest[:i], rest[i+1:]
	}
	if strings.HasPrefix(host, "[") {
		a, _ := netip.ParseAddr(host[1 : len(host)-1]) // parseServerURL made sure it is IPv6
		host = "[" + browserIPv6(a) + "]"
	} else {
		host = strings.ToLower(host)
	}
	if port != "" {
		n, _ := strconv.Atoi(port) // 1-65535, digits only: parseServerURL checked
		if (scheme == "https" && n == 443) || (scheme == "http" && n == 80) {
			return host
		}
		return host + ":" + strconv.Itoa(n)
	}
	return host
}

// browserIPv6 is an IPv6 address as a browser serialises it: RFC 5952 text, which
// netip writes, except that an IPv4-mapped address keeps hex groups
// (::ffff:a00:5, not ::ffff:10.0.0.5).
func browserIPv6(a netip.Addr) string {
	if !a.Is4In6() {
		return a.String()
	}
	b := a.As16()
	return "::ffff:" + strconv.FormatUint(uint64(b[12])<<8|uint64(b[13]), 16) + ":" + strconv.FormatUint(uint64(b[14])<<8|uint64(b[15]), 16)
}

func parseServerURL(key, raw string) (string, error) {
	m := publicURLRE.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return "", errors.New(key + " must be http(s)://host[:port]: a host name or IP of letters, digits, dots and hyphens (or a bracketed IPv6 literal), an optional numeric port, and no user, path, query or escapes")
	}
	if strings.HasPrefix(m[2], "[") {
		a, err := netip.ParseAddr(m[2][1 : len(m[2])-1])
		if err != nil || !a.Is6() {
			return "", errors.New(key + " has brackets around something that is not an IPv6 address: write an IPv4 address or a host name without brackets")
		}
	}
	if m[3] != "" {
		port := 0
		for _, c := range m[3] {
			port = port*10 + int(c-'0')
		}
		if port < 1 || port > 65535 {
			return "", errors.New(key + " has a port outside 1-65535")
		}
	}
	out := m[1] + "://" + m[2]
	if m[3] != "" {
		out += ":" + m[3]
	}
	return out, nil
}
