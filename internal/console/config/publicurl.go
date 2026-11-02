package config

import (
	"errors"
	"regexp"
	"strings"
)

// publicURLRE is the only shape public_url may have: http(s), a host of
// letters, digits, dots and hyphens or a bracketed IPv6 literal, an optional
// numeric port, at most a trailing slash. No userinfo, path, query, fragment
// or percent-escape. public_url ends up inside generated PowerShell and msiexec
// text, so anything else is refused here instead of being quoted there.
var publicURLRE = regexp.MustCompile(`^(https?)://((?:[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?)|(?:\[[0-9A-Fa-f:.]+\]))(?::([0-9]{1,5}))?/?$`)

// ParsePublicURL validates a public_url and returns it reduced to
// scheme://host[:port]. The error never echoes the value.
func ParsePublicURL(raw string) (string, error) {
	m := publicURLRE.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return "", errors.New("public_url must be http(s)://host[:port]: a host name or IP of letters, digits, dots and hyphens (or a bracketed IPv6 literal), an optional numeric port, and no user, path, query or escapes")
	}
	if m[3] != "" {
		port := 0
		for _, c := range m[3] {
			port = port*10 + int(c-'0')
		}
		if port < 1 || port > 65535 {
			return "", errors.New("public_url has a port outside 1-65535")
		}
	}
	out := m[1] + "://" + m[2]
	if m[3] != "" {
		out += ":" + m[3]
	}
	return out, nil
}
