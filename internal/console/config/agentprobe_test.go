package config

import (
	"strings"
	"testing"
)

// ADR 0013: agent_probe has sane defaults, environment overrides and bounds.
func TestAgentProbeSettings(t *testing.T) {
	c := Default()
	c.Resolve()
	c.TLS = TLS{Cert: "a.crt", Key: "a.key"}
	if c.AgentProbe.IntervalS != 180 || c.AgentProbe.CAFile != "" || c.AgentProbe.ConnectAddress != "" {
		t.Fatalf("defaults: %+v", c.AgentProbe)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	env := map[string]string{"CRYO_AGENT_PROBE_INTERVAL_S": "60", "CRYO_AGENT_PROBE_CA_FILE": "/etc/ca.pem", "CRYO_AGENT_PROBE_CONNECT_ADDRESS": "192.0.2.10:8443"}
	if err := ApplyEnv(&c, func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	if c.AgentProbe.IntervalS != 60 || c.AgentProbe.CAFile != "/etc/ca.pem" || c.AgentProbe.ConnectAddress != "192.0.2.10:8443" {
		t.Fatalf("env: %+v", c.AgentProbe)
	}
	for _, bad := range []struct {
		iv   int
		addr string
		want string
	}{{29, "", "agent_probe.interval_s"}, {3601, "", "agent_probe.interval_s"}, {180, "no-port", "agent_probe.connect_address"},
		{180, ":8443", "agent_probe.connect_address"}, {180, "h:0", "agent_probe.connect_address"}, {180, "h:99999", "agent_probe.connect_address"}} {
		d := c
		d.AgentProbe.IntervalS, d.AgentProbe.ConnectAddress = bad.iv, bad.addr
		if err := d.Validate(); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}
