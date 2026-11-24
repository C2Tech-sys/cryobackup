package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// P21-T7: trusted_proxies, the admin host of the origin check, and bracketed hosts in public_url
// and admin_url. Documentation (RFC 5737, RFC 3849) and private (RFC 1918) addresses only.

func TestParseTrustedProxies(t *testing.T) {
	ok := []struct {
		in   []string
		want []string
	}{
		{nil, nil},
		{[]string{}, nil},
		{[]string{"10.0.0.5"}, []string{"10.0.0.5/32"}},
		{[]string{" 10.0.0.5 "}, []string{"10.0.0.5/32"}},
		{[]string{"10.0.0.0/24"}, []string{"10.0.0.0/24"}},
		{[]string{"10.0.0.9/24"}, []string{"10.0.0.0/24"}}, // masked
		{[]string{"2001:db8::1"}, []string{"2001:db8::1/128"}},
		{[]string{"2001:db8::/32"}, []string{"2001:db8::/32"}},
		{[]string{"::ffff:192.0.2.10"}, []string{"192.0.2.10/32"}},   // unmapped
		{[]string{"::ffff:192.0.2.0/120"}, []string{"192.0.2.0/24"}}, // unmapped
		{[]string{"127.0.0.1", "192.0.2.0/28"}, []string{"127.0.0.1/32", "192.0.2.0/28"}},
		{[]string{"0.0.0.0/1"}, []string{"0.0.0.0/1"}}, // wide, but not the world: the operator's choice
	}
	for _, c := range ok {
		got, err := ParseTrustedProxies(c.in)
		if err != nil {
			t.Errorf("ParseTrustedProxies(%q): %v", c.in, err)
			continue
		}
		var names []string
		for _, p := range got {
			names = append(names, p.String())
		}
		if strings.Join(names, ",") != strings.Join(c.want, ",") {
			t.Errorf("ParseTrustedProxies(%q) = %v, want %v", c.in, names, c.want)
		}
	}
	bad := map[string]string{
		"0.0.0.0/0":         "trusts every address",
		"::/0":              "trusts every address",
		"192.0.2.1/0":       "trusts every address",
		"::ffff:0.0.0.0/96": "trusts every address",
		"::ffff:0:0/95":     "not an IP address",
		"10.0.0.0/33":       "not an IP address",
		"proxy.example.net": "not an IP address",
		"":                  "not an IP address",
		"   ":               "not an IP address",
		"fe80::1%eth0":      "not an IP address",
		"10.0.0.5:8080":     "not an IP address",
		"[2001:db8::1]":     "not an IP address",
		"10.0.0.0/8,":       "not an IP address",
	}
	for in, why := range bad {
		_, err := ParseTrustedProxies([]string{"10.0.0.5", in})
		if err == nil {
			t.Errorf("ParseTrustedProxies accepted %q", in)
			continue
		}
		if !strings.Contains(err.Error(), why) || !strings.Contains(err.Error(), "entry 2") {
			t.Errorf("ParseTrustedProxies(%q) error %q, want it to name entry 2 and say %q", in, err, why)
		}
	}
}

// Validate refuses what ParseTrustedProxies refuses, with the plain message, and accepts the default.
func TestValidateTrustedProxies(t *testing.T) {
	base := func() Config {
		c := Default()
		c.Fixtures = true
		c.Resolve()
		return c
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("default: %v", err)
	}
	if base().TrustedProxies != nil {
		t.Fatal("trusted_proxies must default to empty")
	}
	for _, world := range []string{"0.0.0.0/0", "::/0"} {
		c := base()
		c.TrustedProxies = []string{world}
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "trusted_proxies entry 1 trusts every address (a /0 prefix): name the proxy's own address instead") {
			t.Errorf("trusted_proxies [%s]: %v", world, err)
		}
	}
	c := base()
	c.TrustedProxies = []string{"192.0.2.10", "10.0.0.0/8"}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid list refused: %v", err)
	}
	if got := c.TrustedProxyList(); got != "192.0.2.10, 10.0.0.0/8" {
		t.Errorf("TrustedProxyList = %q", got)
	}
	c.TrustedProxies = []string{"nonsense"}
	if c.TrustedProxyList() != "" {
		t.Error("TrustedProxyList of an invalid list must be empty")
	}
}

// The file key and the environment override (comma-separated) both work.
func TestTrustedProxiesFromFileAndEnvironment(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	if err := os.WriteFile(p, []byte(`{"fixtures": true, "trusted_proxies": ["192.0.2.10", "10.0.0.0/8"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.TrustedProxies, "|") != "192.0.2.10|10.0.0.0/8" {
		t.Fatalf("file: %q", c.TrustedProxies)
	}
	c, err = Load(p, func(k string) string {
		if k == "CRYO_TRUSTED_PROXIES" {
			return " 198.51.100.7 , ,2001:db8::/32 "
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.TrustedProxies, "|") != "198.51.100.7|2001:db8::/32" {
		t.Fatalf("env: %q", c.TrustedProxies)
	}
	if EnvKeys()["CRYO_TRUSTED_PROXIES"] != "trusted_proxies" {
		t.Fatal("CRYO_TRUSTED_PROXIES is not the override of trusted_proxies")
	}
	if _, err := Load(p, func(k string) string {
		if k == "CRYO_TRUSTED_PROXIES" {
			return "::/0"
		}
		return ""
	}); err == nil {
		t.Fatal("::/0 from the environment must be refused")
	}
}

func TestAdminHost(t *testing.T) {
	for in, want := range map[string]string{
		"":                                 "",
		"console.example.net":              "", // not valid
		"https://console.example.net":      "console.example.net",
		"https://console.example.net/":     "console.example.net",
		"https://console.example.net:443":  "console.example.net",
		"https://console.example.net:4430": "console.example.net:4430",
		"https://console.example.net:8080": "console.example.net:8080",
		"http://console.example.net:80":    "console.example.net",
		"http://console.example.net:443":   "console.example.net:443",
		"https://console.example.net:80":   "console.example.net:80",
		"http://192.0.2.10:8080":           "192.0.2.10:8080",
		"https://[2001:db8::1]:443":        "[2001:db8::1]",
		"https://[2001:db8::1]:8443":       "[2001:db8::1]:8443",
	} {
		if got := (Config{AdminURL: in}).AdminHost(); got != want {
			t.Errorf("AdminHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// F1 of the P21-T6 blind check: brackets around something that is not an IPv6 address passed
// config, and net/url then refused the value, so the e-mail carried no link. One parser for both keys.
func TestBracketedHostMustBeIPv6(t *testing.T) {
	refused := []string{
		"https://[10.0.0.5]:8080", "https://[10.0.0.5]", "http://[192.0.2.1]/", "https://[1.2]",
		"https://[:::]", "https://[...]", "https://[::1::2]", "https://[.]", "https://[:]",
	}
	accepted := []string{
		"https://[2001:db8::1]", "https://[2001:db8::1]:8443/", "https://[::1]", "https://[::]",
		"https://[::ffff:10.0.0.5]:8080", "https://[::192.0.2.1]", "https://[2001:DB8::A]",
	}
	for _, parse := range []struct {
		key string
		fn  func(string) (string, error)
	}{{"public_url", ParsePublicURL}, {"admin_url", ParseAdminURL}} {
		for _, v := range refused {
			_, err := parse.fn(v)
			if err == nil {
				t.Errorf("%s accepted %q", parse.key, v)
				continue
			}
			if strings.Contains(err.Error(), strings.Trim(v[strings.Index(v, "["):], "/")) {
				t.Errorf("%s error echoes the value: %v", parse.key, err)
			}
			if !strings.HasPrefix(err.Error(), parse.key+" ") {
				t.Errorf("%s error does not name the key: %v", parse.key, err)
			}
		}
		for _, v := range accepted {
			if _, err := parse.fn(v); err != nil {
				t.Errorf("%s refused %q: %v", parse.key, v, err)
			}
		}
	}
	c := Default()
	c.Fixtures = true
	c.Resolve()
	c.PublicURL, c.AdminURL = "https://[10.0.0.5]:8443", "https://[10.0.0.5]:8080"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "public_url has brackets") || !strings.Contains(err.Error(), "admin_url has brackets") {
		t.Fatalf("Validate: %v", err)
	}
}
