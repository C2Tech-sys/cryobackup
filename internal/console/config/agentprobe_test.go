package config

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/ratelimit"
)

// ADR 0013: agent_probe has sane defaults, environment overrides and bounds.
func TestAgentProbeSettings(t *testing.T) {
	c := Default()
	c.Resolve()
	c.TLS = TLS{Cert: "a.crt", Key: "a.key"}
	if c.AgentProbe.IntervalS != 180 || c.AgentProbe.CAFile != "" || c.AgentProbe.ConnectAddress != "" || c.AgentProbe.SPKIPin != "" {
		t.Fatalf("defaults: %+v", c.AgentProbe)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	sum := sha256.Sum256([]byte("a public key"))
	pin := base64.StdEncoding.EncodeToString(sum[:])
	env := map[string]string{"CRYO_AGENT_PROBE_INTERVAL_S": "60", "CRYO_AGENT_PROBE_CA_FILE": "/etc/ca.pem",
		"CRYO_AGENT_PROBE_CONNECT_ADDRESS": "192.0.2.10:8443", "CRYO_AGENT_PROBE_SPKI_PIN": pin}
	if err := ApplyEnv(&c, func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	if c.AgentProbe.IntervalS != 60 || c.AgentProbe.CAFile != "/etc/ca.pem" || c.AgentProbe.ConnectAddress != "192.0.2.10:8443" || c.AgentProbe.SPKIPin != pin {
		t.Fatalf("env: %+v", c.AgentProbe)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("env values must validate: %v", err)
	}
	short := base64.StdEncoding.EncodeToString(sum[:16])
	for _, bad := range []struct {
		iv        int
		addr, pin string
		want      string
	}{{29, "", "", "agent_probe.interval_s"}, {3601, "", "", "agent_probe.interval_s"}, {180, "no-port", "", "agent_probe.connect_address"},
		{180, ":8443", "", "agent_probe.connect_address"}, {180, "h:0", "", "agent_probe.connect_address"}, {180, "h:99999", "", "agent_probe.connect_address"},
		{180, "", "not base64!", "agent_probe.spki_pin"}, {180, "", short, "agent_probe.spki_pin"}} {
		d := c
		d.AgentProbe.IntervalS, d.AgentProbe.ConnectAddress, d.AgentProbe.SPKIPin = bad.iv, bad.addr, bad.pin
		err := d.Validate()
		if err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%+v: %v", bad, err)
		}
		if err != nil && bad.pin != "" && strings.Contains(err.Error(), bad.pin) {
			t.Errorf("the error echoes the pin: %v", err)
		}
	}
}

// The probe's own unauthenticated requests can never get the console's
// address blocked: Validate refuses an interval whose runs per minute exceed
// half the failed-auth allowance, and every accepted combination, driven
// through the agent listener's real Blocker for a day of failing probes with
// the worst-case spacing, never blocks.
func TestAgentProbeCannotBlockTheConsolesAddress(t *testing.T) {
	c := Default()
	c.Resolve()
	c.TLS = TLS{Cert: "a.crt", Key: "a.key"}
	c.AgentProbe.IntervalS, c.RateLimits.FailedAuthPerMinIP = 30, 5
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "failed_auth_per_min_per_ip") {
		t.Fatalf("30 s probes against an allowance of 5 must be refused: %v", err)
	}
	accepted := 0
	for _, iv := range []int{30, 31, 45, 59, 60, 61, 90, 180, 3600} {
		for fa := 1; fa <= 40; fa++ {
			d := c
			d.AgentProbe.IntervalS, d.RateLimits.FailedAuthPerMinIP = iv, fa
			if d.Validate() != nil {
				continue
			}
			accepted++
			now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
			clock := now
			b := ratelimit.NewBlocker(fa, time.Minute, 15*time.Minute, func() time.Time { return clock })
			// Each run's request lands anywhere inside the run's own 30 s timeout:
			// alternate late and early to pack requests as tightly as possible.
			for i := 0; clock.Sub(now) < 24*time.Hour; i++ {
				tick := now.Add(time.Duration(i*iv) * time.Second)
				clock = tick
				if i%2 == 1 {
					clock = tick.Add(29 * time.Second)
				}
				if b.Fail("console") {
					t.Fatalf("interval %d s, allowance %d: the probe blocked the console's address after %v", iv, fa, clock.Sub(now))
				}
			}
		}
	}
	if accepted == 0 {
		t.Fatal("no combination accepted")
	}
	// The default configuration leaves almost all of the allowance to agents.
	if d := Default(); 2*ProbeRunsPerMinute(d.AgentProbe.IntervalS) > d.RateLimits.FailedAuthPerMinIP {
		t.Fatal("defaults violate the rule")
	}
}
