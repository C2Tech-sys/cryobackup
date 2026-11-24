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
			return " 198.51.100.7 ,2001:db8::/32 "
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
	// What is validated and what goes into mails does not change (V4): AdminLink keeps the value as
	// written, leading zeros and case included.
	if got := (Config{AdminURL: "https://Console.Example.NET:0443"}).AdminLink(); got != "https://Console.Example.NET:0443/" {
		t.Fatalf("AdminLink changed: %q", got)
	}
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
		// V4 (fix batch 1): the form a browser writes in Origin (WHATWG URL serialisation).
		"https://Console.Example.NET":         "console.example.net",
		"HTTPS://console.example.net":         "", // not valid: the scheme is lower case (unchanged validation)
		"https://CONSOLE.example.net:8443":    "console.example.net:8443",
		"https://[2001:DB8::1]":               "[2001:db8::1]",
		"https://[2001:DB8:0:0:0:0:0:1]:8443": "[2001:db8::1]:8443",
		"https://[2001:0db8:0000::0001]":      "[2001:db8::1]",
		"https://[::FFFF:10.0.0.5]":           "[::ffff:a00:5]",
		"https://[::ffff:a00:5]:8080":         "[::ffff:a00:5]:8080",
		"https://console.example.net:0443":    "console.example.net",
		"https://console.example.net:00443":   "console.example.net",
		"http://console.example.net:080":      "console.example.net",
		"https://console.example.net:08443":   "console.example.net:8443",
		"https://console.example.net:0080":    "console.example.net:80",
		"https://[2001:db8::1]:0443":          "[2001:db8::1]",
		"http://10.0.0.5:0080/":               "10.0.0.5",
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

// V5 (fix batch 1): a list whose entries together trust every IPv4 or every IPv6 address is the
// world in pieces and is refused like /0; a wide list that leaves a gap is the operator's choice.
func TestTrustedProxiesRefuseAWholeFamilyInPieces(t *testing.T) {
	refused := [][]string{
		{"0.0.0.0/1", "128.0.0.0/1"},
		{"128.0.0.0/1", "0.0.0.0/1"}, // order does not matter
		{"0.0.0.0/2", "64.0.0.0/2", "128.0.0.0/1"},
		{"0.0.0.0/1", "64.0.0.0/2", "128.0.0.0/2", "192.0.0.0/2"}, // overlapping
		{"10.0.0.5", "0.0.0.0/1", "2001:db8::/32", "128.0.0.0/1"},
		{"::ffff:0.0.0.0/97", "::ffff:128.0.0.0/97"}, // IPv4-mapped, unmapped to the two IPv4 halves
		{"::/1", "8000::/1"},
		{"8000::/1", "::/2", "4000::/2"},
	}
	for _, list := range refused {
		_, err := ParseTrustedProxies(list)
		if err == nil || !strings.Contains(err.Error(), "together trust every IPv4 or every IPv6 address") {
			t.Errorf("ParseTrustedProxies(%q) = %v, want the whole-family refusal", list, err)
		}
		c := Default()
		c.Fixtures = true
		c.Resolve()
		c.TrustedProxies = list
		if c.Validate() == nil {
			t.Errorf("Validate accepted %q", list)
		}
	}
	accepted := [][]string{
		{"0.0.0.0/1"},
		{"0.0.0.0/1", "128.0.0.0/2", "192.0.0.0/3", "224.0.0.0/4"},    // 240.0.0.0/4 left out
		{"0.0.0.0/1", "128.0.0.1/32", "128.0.0.2/31", "128.0.0.4/30"}, // 128.0.0.0 itself left out
		{"0.0.0.0/2", "128.0.0.0/1"},                                  // 64.0.0.0/2 left out
		{"::/1", "8000::/2"},
		{"0.0.0.0/1", "8000::/1"}, // one half of each family
		{"::/1", "128.0.0.0/1"},   // an IPv6 half and an IPv4 half do not add up
		{"255.255.255.255", "0.0.0.0"},
	}
	for _, list := range accepted {
		if _, err := ParseTrustedProxies(list); err != nil {
			t.Errorf("ParseTrustedProxies(%q) refused: %v", list, err)
		}
	}
}

// V5 (fix batch 1): an empty entry in CRYO_TRUSTED_PROXIES is malformed and refused, never dropped:
// "," alone, a doubled comma, a trailing comma, white space only.
func TestTrustedProxiesEnvironmentRefusesEmptyEntries(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	if err := os.WriteFile(p, []byte(`{"fixtures": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{",", " , ", "198.51.100.7,,10.0.0.5", "198.51.100.7,", ",198.51.100.7", " "} {
		_, err := Load(p, func(k string) string {
			if k == "CRYO_TRUSTED_PROXIES" {
				return v
			}
			return ""
		})
		if err == nil || !strings.Contains(err.Error(), "CRYO_TRUSTED_PROXIES has an empty entry") {
			t.Errorf("CRYO_TRUSTED_PROXIES=%q: %v, want the empty-entry refusal", v, err)
		}
	}
	// Unset (getenv ""): no override, the file's (empty) list stands.
	c, err := Load(p, noEnv)
	if err != nil || c.TrustedProxies != nil {
		t.Fatalf("unset: %v %q", err, c.TrustedProxies)
	}
}
