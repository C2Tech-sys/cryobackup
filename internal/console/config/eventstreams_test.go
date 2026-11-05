package config

import (
	"strings"
	"testing"
)

// M3-INT item D: rate_limits.event_streams_max and event_streams_per_account
// default to 32 and 4 (Default and config.example.json), are set by their
// CRYO_* overrides, and anything below 1 is refused.
func TestEventStreamLimits(t *testing.T) { // M3-INT D
	d := Default()
	if d.RateLimits.EventStreamsMax != 32 || d.RateLimits.EventStreamsPerAccount != 4 {
		t.Fatalf("defaults = %d / %d, want 32 / 4", d.RateLimits.EventStreamsMax, d.RateLimits.EventStreamsPerAccount)
	}
	ex, err := Load("config.example.json", func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if ex.RateLimits.EventStreamsMax != 32 || ex.RateLimits.EventStreamsPerAccount != 4 {
		t.Fatalf("config.example.json = %d / %d, want 32 / 4", ex.RateLimits.EventStreamsMax, ex.RateLimits.EventStreamsPerAccount)
	}
	maxKey, perKey := EnvName("rate_limits", "event_streams_max"), EnvName("rate_limits", "event_streams_per_account")
	if maxKey != "CRYO_RATE_LIMITS_EVENT_STREAMS_MAX" || perKey != "CRYO_RATE_LIMITS_EVENT_STREAMS_PER_ACCOUNT" {
		t.Fatalf("override names %s, %s", maxKey, perKey)
	}
	env := map[string]string{maxKey: "64", perKey: "2"}
	c, err := Load("config.example.json", func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.RateLimits.EventStreamsMax != 64 || c.RateLimits.EventStreamsPerAccount != 2 {
		t.Fatalf("overrides = %d / %d, want 64 / 2", c.RateLimits.EventStreamsMax, c.RateLimits.EventStreamsPerAccount)
	}
	for _, bad := range []map[string]string{{maxKey: "0"}, {perKey: "0"}, {maxKey: "-1"}, {perKey: "-3"}} {
		_, err := Load("config.example.json", func(k string) string { return bad[k] })
		if err == nil || !strings.Contains(err.Error(), "rate_limits.event_streams_") {
			t.Errorf("%v: err = %v, want an event_streams problem", bad, err)
		}
	}
}
