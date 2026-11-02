package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func noEnv(string) string { return "" }

func TestExampleLoadsAndEveryKeyHasAnEnvOverride(t *testing.T) {
	c, err := Load("config.example.json", noEnv)
	if err != nil {
		t.Fatalf("example must load: %v", err)
	}
	if c.AdminListen != "127.0.0.1:8080" || c.DisplayTimezone != "America/Chicago" {
		t.Fatalf("unexpected example values: %+v", c)
	}
	raw, _ := os.ReadFile("config.example.json")
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	keys := EnvKeys()
	have := map[string]bool{}
	for _, jsonPath := range keys {
		have[jsonPath] = true
	}
	var flat func(prefix string, m map[string]any)
	flat = func(prefix string, m map[string]any) {
		for k, v := range m {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			if sub, ok := v.(map[string]any); ok {
				flat(p, sub)
				continue
			}
			if !have[p] {
				t.Errorf("example key %s has no CRYO_* override", p)
			}
			delete(have, p)
		}
	}
	flat("", doc)
	for p := range have {
		t.Errorf("Config key %s is missing from config.example.json", p)
	}
	if keys["CRYO_TLS_CERT"] != "tls.cert" || keys["CRYO_RATE_LIMITS_RUNS_PER_MIN"] != "rate_limits.runs_per_min" {
		t.Fatalf("env naming changed: %v", keys)
	}
}

func TestEnvOverridesEveryType(t *testing.T) {
	env := map[string]string{
		"CRYO_DISPLAY_TIMEZONE":         "UTC",
		"CRYO_REFRESH_INTERVAL_S":       "45",
		"CRYO_ADMIN_ALLOW_PUBLIC":       "true",
		"CRYO_TLS_CERT":                 "/x/c.pem",
		"CRYO_TLS_KEY":                  "/x/k.pem",
		"CRYO_RATE_LIMITS_RUNS_PER_MIN": "7",
		"CRYO_ADMIN_LISTEN":             ":9000",
	}
	c, err := Load("", func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.DisplayTimezone != "UTC" || c.RefreshIntervalS != 45 || !c.AdminAllowPublic || c.RateLimits.RunsPerMin != 7 || c.AdminListen != ":9000" {
		t.Fatalf("overrides not applied: %+v", c)
	}
	if c.DBPath != filepath.Join("./data", "console.db") {
		t.Fatalf("path default not resolved: %q", c.DBPath)
	}
	if _, err := Load("", func(k string) string {
		if k == "CRYO_REFRESH_INTERVAL_S" {
			return "abc"
		}
		return ""
	}); err == nil {
		t.Fatal("a non-numeric override must fail")
	}
}

func TestUnknownKeyAndMissingFileAreErrors(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	_ = os.WriteFile(p, []byte(`{"data_dirr": "x"}`), 0o600)
	if _, err := Load(p, noEnv); err == nil {
		t.Fatal("unknown key must fail")
	}
	_, err := Load(filepath.Join(dir, "missing.json"), noEnv)
	if err == nil || strings.Contains(err.Error(), dir) {
		t.Fatalf("missing file must fail without printing the path: %v", err)
	}
}

func TestListenerRules(t *testing.T) {
	base := func() Config {
		c := Default()
		c.Resolve()
		c.TLS = TLS{Cert: "c", Key: "k"}
		return c
	}
	cases := []struct {
		name string
		mut  func(*Config)
		ok   bool
	}{
		{"defaults with tls", func(c *Config) {}, true},
		{"admin on public needs the flag", func(c *Config) { c.AdminListen = "0.0.0.0:8080" }, false},
		{"admin on empty host is public", func(c *Config) { c.AdminListen = ":8080" }, false},
		{"admin public with flag", func(c *Config) { c.AdminListen = "0.0.0.0:8080"; c.AdminAllowPublic = true }, true},
		{"admin on mesh ip", func(c *Config) { c.AdminListen = "10.8.0.5:8080" }, true},
		{"admin on 192.168", func(c *Config) { c.AdminListen = "192.168.1.5:8080" }, true},
		{"admin on public ip", func(c *Config) { c.AdminListen = "203.0.113.9:8080" }, false},
		{"admin on ipv6 loopback", func(c *Config) { c.AdminListen = "[::1]:8080" }, true},
		{"admin on name", func(c *Config) { c.AdminListen = "console.example.com:8080" }, false},
		{"port zero is fine", func(c *Config) { c.AdminListen = "127.0.0.1:0" }, true},
		{"no tls without lab flag", func(c *Config) { c.TLS = TLS{} }, false},
		{"lab flag on public agent bind", func(c *Config) { c.TLS = TLS{}; c.InsecureHTTPLab = true }, false},
		{"lab flag on loopback", func(c *Config) { c.TLS = TLS{}; c.InsecureHTTPLab = true; c.AgentListen = "127.0.0.1:8443" }, true},
		{"lab flag on private", func(c *Config) { c.TLS = TLS{}; c.InsecureHTTPLab = true; c.AgentListen = "10.0.0.2:8443" }, true},
		{"lab flag with tls still needs private bind", func(c *Config) { c.InsecureHTTPLab = true }, false},
		{"cert without key", func(c *Config) { c.TLS.Key = "" }, false},
		{"bad zone", func(c *Config) { c.DisplayTimezone = "Mars/Base" }, false},
		{"bad refresh", func(c *Config) { c.RefreshIntervalS = 1 }, false},
		{"absolute below idle", func(c *Config) { c.Session.AbsoluteH = 1 }, false},
		{"long poll over 30", func(c *Config) { c.RateLimits.CommandsLongPollMaxS = 31 }, false},
		{"smtp absent is fine", func(c *Config) { c.SMTP = SMTP{} }, true},
		{"smtp present and valid", func(c *Config) {
			c.SMTP = SMTP{Host: "mail.example.com", Port: 587, Security: "starttls", From: "a@example.com"}
		}, true},
		{"smtp present and bad", func(c *Config) { c.SMTP = SMTP{Host: "mail.example.com", Port: 0, Security: "x", From: "nobody"} }, false},
		{"bad public url", func(c *Config) { c.PublicURL = "ftp://x" }, false},
		{"gateway url must be https", func(c *Config) { c.GatewayURL = "http://gw" }, false},
	}
	for _, tc := range cases {
		c := base()
		tc.mut(&c)
		err := c.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("%s: ok=%v err=%v", tc.name, tc.ok, err)
		}
	}
}

// TestIsLoopbackOrPrivateIncludesTheSharedAddressSpace pins RFC 6598
// (100.64.0.0/10, the Tailscale/Headscale mesh range) with its edges, and that
// nothing public or malformed slipped in with it.
func TestIsLoopbackOrPrivateIncludesTheSharedAddressSpace(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"100.63.255.255:80", false},
		{"100.64.0.0:80", true},
		{"100.100.100.100:80", true},
		{"100.127.255.255:80", true},
		{"100.128.0.0:80", false},
		{"[::ffff:100.64.0.1]:80", true},
		{"[::ffff:100.128.0.1]:80", false},
		{"100.64.0.1", false}, // no port: not an address:port pair
		{"127.0.0.1:80", true},
		{"[::1]:80", true},
		{"localhost:80", true},
		{"10.0.0.1:80", true},
		{"172.16.0.1:80", true},
		{"172.32.0.1:80", false},
		{"192.168.0.1:80", true},
		{"169.254.1.1:80", true},
		{"[fd00::1]:80", true},
		{"[fe80::1]:80", true},
		{"203.0.113.9:80", false},
		{"8.8.8.8:80", false},
		{":80", false},
		{"0.0.0.0:80", false},
		{"example.com:80", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsLoopbackOrPrivate(tc.addr); got != tc.want {
			t.Errorf("IsLoopbackOrPrivate(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestAdminTLSConfig(t *testing.T) {
	base := func() Config {
		c := Default()
		c.Resolve()
		c.TLS = TLS{Cert: "c", Key: "k"}
		return c
	}
	cases := []struct {
		name string
		mut  func(*Config)
		ok   bool
	}{
		{"unset", func(c *Config) {}, true},
		{"both set", func(c *Config) { c.Admin.TLS = TLS{Cert: "a.crt", Key: "a.key"} }, true},
		{"cert only", func(c *Config) { c.Admin.TLS.Cert = "a.crt" }, false},
		{"key only", func(c *Config) { c.Admin.TLS.Key = "a.key" }, false},
		{"mesh bind with tls", func(c *Config) { c.AdminListen = "100.100.10.20:8080"; c.Admin.TLS = TLS{Cert: "a", Key: "b"} }, true},
		{"mesh bind without tls is allowed (doctor warns)", func(c *Config) { c.AdminListen = "100.100.10.20:8080" }, true},
		{"lab flag on a mesh agent bind", func(c *Config) { c.TLS = TLS{}; c.InsecureHTTPLab = true; c.AgentListen = "100.100.10.20:8443" }, true},
		{"public admin bind still needs the flag", func(c *Config) { c.AdminListen = "100.128.0.9:8080" }, false},
	}
	for _, tc := range cases {
		c := base()
		tc.mut(&c)
		if err := c.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: ok=%v err=%v", tc.name, tc.ok, err)
		}
	}
	// The key is configurable from the file and from the environment.
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	_ = os.WriteFile(p, []byte(`{"admin": {"tls": {"cert": "/x/a.crt", "key": "/x/a.key"}}}`), 0o600)
	c, err := Read(p, noEnv)
	if err != nil || c.Admin.TLS.Cert != "/x/a.crt" || c.Admin.TLS.Key != "/x/a.key" {
		t.Fatalf("file: %+v %v", c.Admin, err)
	}
	c, err = Read("", func(k string) string {
		return map[string]string{"CRYO_ADMIN_TLS_CERT": "/e/c.pem", "CRYO_ADMIN_TLS_KEY": "/e/k.pem"}[k]
	})
	if err != nil || c.Admin.TLS.Cert != "/e/c.pem" || c.Admin.TLS.Key != "/e/k.pem" {
		t.Fatalf("env: %+v %v", c.Admin, err)
	}
	if EnvKeys()["CRYO_ADMIN_TLS_CERT"] != "admin.tls.cert" {
		t.Fatalf("env name: %v", EnvKeys())
	}
}

func TestFixturesModeNeedsNoCertificate(t *testing.T) {
	c := Default()
	c.Resolve()
	c.Fixtures = true
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}
