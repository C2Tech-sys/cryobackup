package config

import (
	"strings"
	"testing"
)

// health_prober and healthcheck_ping: off and generic by default, validated, and
// a bad ping URL is refused without echoing it (it may carry a token).
func TestHealthProberAndHealthcheckPingConfig(t *testing.T) {
	d := Default()
	if d.HealthProber.IntervalS != 60 || d.HealthcheckPing.URL != "" || d.HealthcheckPing.Method != "GET" || d.HealthcheckPing.IntervalS != 300 {
		t.Fatalf("defaults %+v %+v", d.HealthProber, d.HealthcheckPing)
	}
	base := func() Config {
		c := Default()
		c.DBPath, c.MasterKeyPath, c.InsecureHTTPLab, c.AgentListen = "x.db", "mk", true, "127.0.0.1:8443"
		return c
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("base: %v", err)
	}
	for _, c := range []struct {
		name string
		mod  func(*Config)
		ok   bool
	}{
		{"prober interval too short", func(c *Config) { c.HealthProber.IntervalS = 10 }, false},
		{"prober interval too long", func(c *Config) { c.HealthProber.IntervalS = 900 }, false},
		{"ping https", func(c *Config) { c.HealthcheckPing.URL = "https://monitor.example.com/ping/abc" }, true},
		{"ping post", func(c *Config) { c.HealthcheckPing.URL, c.HealthcheckPing.Method = "http://10.0.0.5/p", "POST" }, true},
		{"ping bad method", func(c *Config) { c.HealthcheckPing.Method = "PUT" }, false},
		{"ping bad interval", func(c *Config) { c.HealthcheckPing.IntervalS = 5 }, false},
	} {
		cfg := base()
		c.mod(&cfg)
		if err := cfg.Validate(); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	cfg := base()
	cfg.HealthcheckPing.URL = "ftp://monitor.example.com/secret-token"
	err := cfg.Validate()
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("bad ping url: %v", err)
	}
}
