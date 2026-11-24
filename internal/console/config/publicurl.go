package config

import (
	"errors"
	"regexp"
	"strings"
)

// publicURLRE is the only shape public_url and admin_url may have: http(s), a host of
// letters, digits, dots and hyphens or a bracketed IPv6 literal, an optional
// numeric port, at most a trailing slash. No userinfo, path, query, fragment
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

func parseServerURL(key, raw string) (string, error) {
	m := publicURLRE.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return "", errors.New(key + " must be http(s)://host[:port]: a host name or IP of letters, digits, dots and hyphens (or a bracketed IPv6 literal), an optional numeric port, and no user, path, query or escapes")
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
