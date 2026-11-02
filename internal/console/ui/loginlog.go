package ui

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// loginRefusalAttrs are the log fields of a refused sign-in post. They are
// typed or derived, never a header copied through: whether Origin and Referer
// were sent and whether each names this console (or its configured public
// host), the two Sec-Fetch values from their closed vocabularies, and a browser
// family. That is what diagnosing "reached through a name the console does not
// know" needs; the header values themselves (a Referer can carry a path, a
// User-Agent identifies a workstation) stay out of the log.
func loginRefusalAttrs(r *http.Request, publicHost string) []any {
	names := func(raw string) bool {
		u, err := url.Parse(raw)
		return err == nil && u.Host != "" && (u.Host == r.Host || (publicHost != "" && u.Host == publicHost))
	}
	origin, referer := r.Header.Get("Origin"), r.Header.Get("Referer")
	return []any{
		logx.Bool("origin_present", origin != ""),
		logx.Bool("origin_matches_host", origin != "" && names(origin)),
		logx.Bool("referer_present", referer != ""),
		logx.Bool("referer_matches_host", referer != "" && names(referer)),
		logx.Bool("public_host_configured", publicHost != ""),
		logx.Str("sec_fetch_site", oneOf(r.Header.Get("Sec-Fetch-Site"), "same-origin", "same-site", "cross-site", "none")),
		logx.Str("sec_fetch_mode", oneOf(r.Header.Get("Sec-Fetch-Mode"), "navigate", "same-origin", "cors", "no-cors", "websocket")),
		logx.Str("ua_family", uaFamily(r.UserAgent())),
	}
}

// oneOf returns v when it is one of the allowed words, "absent" when empty and
// "other" otherwise, so a header value never reaches the log verbatim.
func oneOf(v string, allowed ...string) string {
	if v == "" {
		return "absent"
	}
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return "other"
}

// uaFamily reduces a User-Agent to a browser family from a fixed set.
func uaFamily(ua string) string {
	switch {
	case ua == "":
		return "absent"
	case strings.Contains(ua, "Edg/"):
		return "edge"
	case strings.Contains(ua, "OPR/"):
		return "opera"
	case strings.Contains(ua, "Firefox/"):
		return "firefox"
	case strings.Contains(ua, "Chrome/") || strings.Contains(ua, "Chromium/"):
		return "chrome"
	case strings.Contains(ua, "Safari/"):
		return "safari"
	case strings.HasPrefix(ua, "curl/"):
		return "curl"
	}
	return "other"
}
