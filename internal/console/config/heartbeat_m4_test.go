package config

import (
	"os"
	"strings"
	"testing"
)

// ADR 0020 decision 12: the optional outbound heartbeat ping (healthcheck_ping). https only unless
// the host is loopback; the period 60 s to 24 h; the method GET, HEAD or POST; every key has its
// CRYO_* override; both shipped examples carry the block with an empty URL; and a refusal never
// echoes the URL, which carries the monitor's secret token.
func heartbeatBase() Config {
	c := Default()
	c.DBPath, c.MasterKeyPath, c.InsecureHTTPLab, c.AgentListen = "x.db", "mk", true, "127.0.0.1:8443"
	return c
}

func TestM4HeartbeatPingConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*HealthcheckPing)
		ok   bool
	}{
		{"off by default", func(h *HealthcheckPing) {}, true},
		{"https", func(h *HealthcheckPing) { h.URL = "https://monitor.example.test/ping/abc?k=v" }, true},
		{"https with a port", func(h *HealthcheckPing) { h.URL = "https://monitor.example.test:8443/ping/abc" }, true},
		{"http on a public name", func(h *HealthcheckPing) { h.URL = "http://monitor.example.test/ping/abc" }, false},
		{"http on a private address", func(h *HealthcheckPing) { h.URL = "http://10.0.0.5/p" }, false},
		{"http on loopback v4", func(h *HealthcheckPing) { h.URL = "http://127.0.0.1:9000/p" }, true},
		{"http on loopback v6", func(h *HealthcheckPing) { h.URL = "http://[::1]:9000/p" }, true},
		{"http on localhost", func(h *HealthcheckPing) { h.URL = "http://localhost:9000/p" }, true},
		{"http on a name that only starts with localhost", func(h *HealthcheckPing) { h.URL = "http://localhost.example.test/p" }, false},
		{"ftp", func(h *HealthcheckPing) { h.URL = "ftp://monitor.example.test/p" }, false},
		{"no scheme", func(h *HealthcheckPing) { h.URL = "monitor.example.test/p" }, false},
		{"no host", func(h *HealthcheckPing) { h.URL = "https:///p" }, false},
		{"method GET", func(h *HealthcheckPing) { h.Method = "GET" }, true},
		{"method HEAD", func(h *HealthcheckPing) { h.Method = "HEAD" }, true},
		{"method POST", func(h *HealthcheckPing) { h.Method = "POST" }, true},
		{"method PUT", func(h *HealthcheckPing) { h.Method = "PUT" }, false},
		{"method empty", func(h *HealthcheckPing) { h.Method = "" }, false},
		{"method lower case", func(h *HealthcheckPing) { h.Method = "get" }, false},
		{"interval 59", func(h *HealthcheckPing) { h.IntervalS = 59 }, false},
		{"interval 60", func(h *HealthcheckPing) { h.IntervalS = 60 }, true},
		{"interval 3601 (was refused before ADR 0020)", func(h *HealthcheckPing) { h.IntervalS = 3601 }, true},
		{"interval 86400", func(h *HealthcheckPing) { h.IntervalS = 86400 }, true},
		{"interval 86401", func(h *HealthcheckPing) { h.IntervalS = 86401 }, false},
		{"interval 0", func(h *HealthcheckPing) { h.IntervalS = 0 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := heartbeatBase()
			tc.mod(&c.HealthcheckPing)
			if err := c.Validate(); (err == nil) != tc.ok {
				t.Fatalf("valid = %v, want %v (%v)", err == nil, tc.ok, err)
			}
		})
	}
}

func TestM4HeartbeatPingRefusalNeverEchoesTheURL(t *testing.T) {
	for _, raw := range []string{
		"http://monitor.example.test/ping/s3cr3t-token?key=q9w8e7",
		"ftp://monitor.example.test/ping/s3cr3t-token?key=q9w8e7",
		"https:///ping/s3cr3t-token?key=q9w8e7",
		"https://monitor.example.test/%zz/s3cr3t-token?key=q9w8e7",
	} {
		c := heartbeatBase()
		c.HealthcheckPing.URL = raw
		err := c.Validate()
		if err == nil {
			t.Fatalf("%.20s... accepted", raw)
		}
		if strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), "q9w8e7") || strings.Contains(err.Error(), "monitor.example.test") {
			t.Fatalf("the refusal echoes the URL: %v", err)
		}
		if !strings.Contains(err.Error(), "healthcheck_ping.url") {
			t.Fatalf("the refusal does not name the key: %v", err)
		}
	}
}

func TestM4HeartbeatPingEnvOverrides(t *testing.T) {
	env := map[string]string{
		"CRYO_HEALTHCHECK_PING_URL":        "https://monitor.example.test/ping/abc",
		"CRYO_HEALTHCHECK_PING_METHOD":     "HEAD",
		"CRYO_HEALTHCHECK_PING_INTERVAL_S": "7200",
	}
	c := Default()
	if err := ApplyEnv(&c, func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	if h := c.HealthcheckPing; h.URL != env["CRYO_HEALTHCHECK_PING_URL"] || h.Method != "HEAD" || h.IntervalS != 7200 {
		t.Fatalf("%+v", h)
	}
	keys := EnvKeys()
	for _, k := range []string{"CRYO_HEALTHCHECK_PING_URL", "CRYO_HEALTHCHECK_PING_METHOD", "CRYO_HEALTHCHECK_PING_INTERVAL_S"} {
		if keys[k] == "" {
			t.Errorf("%s is not an override", k)
		}
	}
}

// Both shipped examples document the block, with an empty URL (the feature is off until the
// operator names one) and the defaults the code uses.
func TestM4HeartbeatPingExamplesCarryTheBlock(t *testing.T) {
	for _, path := range []string{"config.example.json", "../../../deploy/compose/config.example.json"} {
		t.Run(path, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{`"healthcheck_ping"`, `"url": ""`, `"method": "GET"`, `"interval_s": 300`} {
				if !strings.Contains(string(raw), want) {
					t.Fatalf("no %s in the example", want)
				}
			}
			c, err := Read(path, func(string) string { return "" }) // strict: an unknown key in the example fails here
			if err != nil {
				t.Fatal(err)
			}
			if h := c.HealthcheckPing; h.URL != "" || h.Method != "GET" || h.IntervalS != 300 {
				t.Fatalf("example block %+v", h)
			}
		})
	}
}
