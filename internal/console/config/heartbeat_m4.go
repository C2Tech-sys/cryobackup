package config

import (
	"net"
	"net/url"
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
// URL carries the monitor's secret token and plain http would put it on the wire. The ping job
// asks the same question before it sends, so the file and the job cannot disagree.
func HeartbeatURLAllowed(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		return isLoopbackHost(u.Hostname())
	}
	return false
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
