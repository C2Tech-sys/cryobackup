package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// P22-K: the drill key (ADR 0029, ruling C8).

func TestDrillDefaultsOffAndLoadsFromFileAndEnv(t *testing.T) {
	if Default().Drill {
		t.Fatal("drill must default to false")
	}
	c, err := Load("config.example.json", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if c.Drill || c.AgentListenerOff() {
		t.Fatalf("the example is a production console: drill=%v agent listener off=%v", c.Drill, c.AgentListenerOff())
	}

	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(`{"drill": true, "tls": {"cert": "", "key": ""}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = Load(p, noEnv)
	if err != nil {
		t.Fatalf("a drill config without an agent certificate must load: %v", err)
	}
	if !c.Drill || !c.AgentListenerOff() {
		t.Fatalf("drill from the file not applied: drill=%v off=%v", c.Drill, c.AgentListenerOff())
	}

	env := map[string]string{"CRYO_DRILL": "true", "CRYO_TLS_CERT": "c", "CRYO_TLS_KEY": "k"}
	c, err = Load("", func(k string) string { return env[k] })
	if err != nil || !c.Drill {
		t.Fatalf("CRYO_DRILL=true: drill=%v err=%v", c.Drill, err)
	}
	if _, err := Load("", func(k string) string {
		if k == "CRYO_DRILL" {
			return "maybe"
		}
		return ""
	}); err == nil || !strings.Contains(err.Error(), "CRYO_DRILL must be true or false") {
		t.Fatalf("a malformed CRYO_DRILL must be refused by name: %v", err)
	}
}

func TestDrillValidation(t *testing.T) {
	base := func() Config {
		c := Default()
		c.Resolve()
		return c
	}
	cases := []struct {
		name string
		mut  func(*Config)
		ok   bool
	}{
		{"production without an agent certificate is refused", func(c *Config) {}, false},
		{"drill needs no agent certificate", func(c *Config) { c.Drill = true }, true},
		{"drill with the production certificate still loads", func(c *Config) { c.Drill = true; c.TLS = TLS{Cert: "c", Key: "k"} }, true},
		{"drill ignores the agent certificate: no agent listener uses it", func(c *Config) { c.Drill = true; c.TLS = TLS{Cert: "c"} }, true},
		{"drill and fixtures together are refused", func(c *Config) { c.Drill = true; c.Fixtures = true }, false},
		{"drill keeps the admin exposure rule", func(c *Config) { c.Drill = true; c.AdminListen = "0.0.0.0:8080" }, false},
		{"drill keeps the rest of validation", func(c *Config) { c.Drill = true; c.RefreshIntervalS = 1 }, false},
	}
	for _, tc := range cases {
		c := base()
		tc.mut(&c)
		err := c.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("%s: ok=%v err=%v", tc.name, tc.ok, err)
		}
	}
	c := base()
	c.Drill, c.Fixtures = true, true
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "drill and fixtures cannot both be set") {
		t.Fatalf("the refusal must say why: %v", err)
	}
}
