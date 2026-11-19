package config

import (
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Bounds of healthcheck_ping.interval_s: a minute to a day (ADR 0020 decision 12).
const (
	HeartbeatMinIntervalS = 60
	HeartbeatMaxIntervalS = 86400
)

// HeartbeatURLAllowed reports whether rawURL is one the outbound heartbeat ping will send to:
// https with a host, or http when the host is loopback (the name localhost or a loopback address),
// which exists for tests and for a relay on the same machine. Anything else is refused, because the
// URL carries the monitor's secret token and plain http would put it on the wire. So is a URL with
// a user name or password in it (the ping carries no credentials of its own), with white space
// around it, or with a port outside 1 to 65535. The ping job asks the same question before it
// sends, so the file and the job cannot disagree.
func HeartbeatURLAllowed(rawURL string) bool { return heartbeatURLProblem(rawURL) == "" }

// heartbeatURLProblem is why rawURL is refused, as a fixed text that names the key and never the
// URL (it may carry a token), or "" when it is allowed.
func heartbeatURLProblem(rawURL string) string {
	if strings.TrimSpace(rawURL) != rawURL {
		return "healthcheck_ping.url must not begin or end with white space"
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return heartbeatSchemeProblem
	}
	if u.User != nil {
		return "healthcheck_ping.url must not carry a user name or password: put the monitor's token in the path or query"
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return "healthcheck_ping.url port must be between 1 and 65535"
		}
	}
	switch u.Scheme {
	case "https":
		return ""
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return ""
		}
	}
	return heartbeatSchemeProblem
}

// heartbeatSchemeProblem is the refusal of a URL that is not https (or loopback http) with a host.
const heartbeatSchemeProblem = "healthcheck_ping.url must be an https URL (http only for a loopback host)"

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
