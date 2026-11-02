package config

import (
	"strings"
	"testing"
)

// public_url ends up in generated PowerShell and msiexec text, so a start with a
// hostile or sloppy value fails at config load, loudly, without echoing it.
func TestPublicURLShapeIsEnforcedAtLoad(t *testing.T) {
	good := map[string]string{
		"https://backup.example.net:8443":  "https://backup.example.net:8443",
		"https://backup.example.net:8443/": "https://backup.example.net:8443",
		"https://backup.example.net":       "https://backup.example.net",
		"http://10.0.0.5:8443":             "http://10.0.0.5:8443",
		"https://[2001:db8::1]:8443":       "https://[2001:db8::1]:8443",
		" https://a-b.c.example:1 ":        "https://a-b.c.example:1",
	}
	for in, want := range good {
		if got, err := ParsePublicURL(in); err != nil || got != want {
			t.Errorf("ParsePublicURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"https://x$(calc)y:8443", "https://%25windir%25:8443", "%25windir%25", "https://host/path", "https://host?x=1", "https://host#f",
		"https://user:pw@host", "ftp://host", "HTTPS://host", "https://", "host:8443", "https://host:0", "https://host:65536", "https://-host", "https://host-",
		"https://ho'st", "https://ho\"st", "https://host`x`", "https://ho st", "https://höst", "https://[::1", "https://[zz]"} {
		_, err := ParsePublicURL(bad)
		if err == nil {
			t.Errorf("ParsePublicURL(%q) accepted", bad)
			continue
		}
		if strings.Contains(err.Error(), "calc") || strings.Contains(err.Error(), "windir") {
			t.Errorf("the error echoes the value: %v", err)
		}
		c := Default()
		c.Resolve()
		c.TLS = TLS{Cert: "c", Key: "k"}
		c.PublicURL = bad
		if verr := c.Validate(); verr == nil || !strings.Contains(verr.Error(), "public_url") {
			t.Errorf("Validate accepted public_url %q: %v", bad, verr)
		}
	}
}
