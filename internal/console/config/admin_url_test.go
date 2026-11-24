package config

import (
	"strings"
	"testing"
)

// admin_url is the address an operator types to open the console, and it goes into every e-mail
// the console sends. It takes exactly the shape public_url takes (one parser, one expression), it
// is optional, and it never falls back to public_url (the agent API serves no console page).

func TestAdminURLValidationTable(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"unset is fine", "", true},
		{"https host", "https://console.example.net", true},
		{"https host trailing slash", "https://console.example.net/", true},
		{"https host and port", "https://console.example.net:8443", true},
		{"http lab address", "http://10.0.0.5:8080", true},
		{"ipv6 literal", "https://[2001:db8::1]:8080", true},
		{"padded with white space is trimmed", " https://console.example.net ", true},
		{"wrong scheme", "ftp://console.example.net", false},
		{"no scheme", "console.example.net", false},
		{"a path", "https://console.example.net/login", false},
		{"a query", "https://console.example.net?x=1", false},
		{"a fragment", "https://console.example.net#x", false},
		{"user info", "https://user:pw@console.example.net", false},
		{"percent escape", "https://%25windir%25", false},
		{"port zero", "https://console.example.net:0", false},
		{"port too large", "https://console.example.net:65536", false},
		{"a quote", "https://con'sole.example.net", false},
		{"white space inside", "https://con sole.example.net", false},
		{"upper-case scheme", "HTTPS://console.example.net", false},
		{"scheme only", "https://", false},
	}
	for _, tc := range cases {
		c := Default()
		c.Resolve()
		c.TLS = TLS{Cert: "c", Key: "k"}
		c.AdminURL = tc.in
		err := c.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("%s: admin_url %q: ok=%v err=%v", tc.name, tc.in, tc.ok, err)
			continue
		}
		if err != nil && !strings.Contains(err.Error(), "admin_url") {
			t.Errorf("%s: the refusal does not name admin_url: %v", tc.name, err)
		}
	}
}

// Same parser, same verdicts: whatever public_url refuses, admin_url refuses, and the other way round.
func TestAdminURLTakesTheShapeOfPublicURL(t *testing.T) {
	for _, in := range []string{
		"https://backup.example.net:8443", "https://backup.example.net:8443/", "http://10.0.0.5:8443", "https://[2001:db8::1]:8443", " https://a-b.c.example:1 ",
		"https://x$(calc)y:8443", "https://%25windir%25:8443", "https://host/path", "https://host?x=1", "https://host#f", "https://user:pw@host", "ftp://host",
		"HTTPS://host", "https://", "host:8443", "https://host:0", "https://host:65536", "https://-host", "https://host-", "https://ho'st", "https://höst", "https://[::1", "",
	} {
		pu, perr := ParsePublicURL(in)
		au, aerr := ParseAdminURL(in)
		if (perr == nil) != (aerr == nil) || pu != au {
			t.Errorf("%q: public_url gives %q, %v; admin_url gives %q, %v", in, pu, perr, au, aerr)
		}
		if aerr != nil && (strings.Contains(aerr.Error(), "calc") || strings.Contains(aerr.Error(), "windir") || !strings.HasPrefix(aerr.Error(), "admin_url ")) {
			t.Errorf("%q: the error echoes the value or names the wrong key: %v", in, aerr)
		}
	}
}

// public_url keeps its own words: the refusal names public_url, as before.
func TestPublicURLRefusalStillNamesPublicURL(t *testing.T) {
	if _, err := ParsePublicURL("ftp://x"); err == nil || !strings.HasPrefix(err.Error(), "public_url must be http(s)://host[:port]: ") {
		t.Fatalf("public_url error changed: %v", err)
	}
	if _, err := ParsePublicURL("https://x:0"); err == nil || err.Error() != "public_url has a port outside 1-65535" {
		t.Fatalf("public_url port error changed: %v", err)
	}
}

func TestAdminLink(t *testing.T) {
	for _, tc := range []struct{ admin, public, want string }{
		{"", "https://agents.example.test:8443", ""},                                                        // unset: no link, never public_url
		{"https://console.example.net", "https://agents.example.test:8443", "https://console.example.net/"}, // reduced and slashed
		{"https://console.example.net:8443/", "", "https://console.example.net:8443/"},
		{"console.example.net", "https://agents.example.test:8443", ""}, // not valid: no link, never public_url
	} {
		c := Config{AdminURL: tc.admin, PublicURL: tc.public}
		if got := c.AdminLink(); got != tc.want {
			t.Errorf("admin_url %q public_url %q: AdminLink = %q, want %q", tc.admin, tc.public, got, tc.want)
		}
	}
}

// The key loads from the file and from CRYO_ADMIN_URL, like every other key, and an unset key stays empty.
func TestAdminURLLoadsFromFileAndEnvironment(t *testing.T) {
	c, err := Load("config.example.json", noEnv)
	if err != nil || c.AdminURL != "https://console.example.net" {
		t.Fatalf("example: %q, %v", c.AdminURL, err)
	}
	if Default().AdminURL != "" {
		t.Fatalf("the default must be empty (no link), got %q", Default().AdminURL)
	}
	c, err = Load("config.example.json", func(k string) string {
		if k == "CRYO_ADMIN_URL" {
			return "http://10.0.0.5:8080"
		}
		return ""
	})
	if err != nil || c.AdminURL != "http://10.0.0.5:8080" {
		t.Fatalf("env override: %q, %v", c.AdminURL, err)
	}
	if _, err = Load("config.example.json", func(k string) string {
		if k == "CRYO_ADMIN_URL" {
			return "https://console.example.net/login"
		}
		return ""
	}); err == nil || !strings.Contains(err.Error(), "admin_url") || strings.Contains(err.Error(), "/login") {
		t.Fatalf("a bad env value must refuse the start naming the key and not the value: %v", err)
	}
}
