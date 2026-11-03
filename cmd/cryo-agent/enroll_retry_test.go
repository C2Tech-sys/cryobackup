package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent"
)

// The knob: unset means the default budget (0 in EnrollOptions, the 5-minute
// context of agent.EnrollTimeout); 0 is a single attempt; 1 to 600 seconds is
// that budget; anything else is ignored with a warning and the default applies.
func TestEnrollRetrySettings(t *testing.T) {
	extra := agent.EnrollTimeout - agent.EnrollRetryBudget
	for _, tc := range []struct {
		name, env   string
		budget, ctx time.Duration
		warns       bool
	}{
		{"unset", "", 0, agent.EnrollTimeout, false},
		{"blank", "  ", 0, agent.EnrollTimeout, false},
		{"zero is a single attempt", "0", -1, extra, false},
		{"seconds", "45", 45 * time.Second, 45*time.Second + extra, false},
		{"the maximum", "600", 600 * time.Second, 600*time.Second + extra, false},
		{"above the maximum", "601", 0, agent.EnrollTimeout, true},
		{"negative", "-1", 0, agent.EnrollTimeout, true},
		{"not a number", "soon", 0, agent.EnrollTimeout, true},
		{"a fraction", "1.5", 0, agent.EnrollTimeout, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(enrollRetryEnvVar, tc.env)
			var warn bytes.Buffer
			budget, timeout := enrollRetrySettings(&warn)
			if budget != tc.budget || timeout != tc.ctx {
				t.Errorf("budget %v, context %v; want %v, %v", budget, timeout, tc.budget, tc.ctx)
			}
			if got := strings.Contains(warn.String(), "ignored"); got != tc.warns {
				t.Errorf("warning = %q, want a warning: %v", warn.String(), tc.warns)
			}
			if tc.env != "" && strings.Contains(warn.String(), tc.env) && tc.warns {
				t.Errorf("the warning echoes the value: %q", warn.String())
			}
		})
	}
}

// "cryo-agent enroll" hands the knob to agent.Enroll: with the variable unset
// the default budget applies (RetryBudget 0, the context deadline of
// agent.EnrollTimeout); with 0 it is a single attempt.
func TestEnrollCommandPassesTheRetryBudget(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		budget    time.Duration
		deadline  time.Duration
	}{
		{"unset: the default budget applies", "", 0, agent.EnrollTimeout},
		{"0: a single attempt", "0", -1, agent.EnrollTimeout - agent.EnrollRetryBudget},
		{"90: ninety seconds", "90", 90 * time.Second, 90*time.Second + agent.EnrollTimeout - agent.EnrollRetryBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(enrollRetryEnvVar, tc.env)
			var got agent.EnrollOptions
			var left time.Duration
			swap(t, &enrollDevice, func(ctx context.Context, o agent.EnrollOptions) (agent.EnrollResult, error) {
				got = o
				dl, ok := ctx.Deadline()
				if !ok {
					t.Error("the enrollment context has no deadline")
				}
				left = time.Until(dl)
				return agent.EnrollResult{DeviceID: "dev"}, nil
			})
			code, _, errs := do("enroll", "--state-dir", filepath.Join(t.TempDir(), "state"), "--server", "https://console.example.invalid", "--token", "t")
			if code != exitOK {
				t.Fatalf("enroll: %d %q", code, errs)
			}
			if got.RetryBudget != tc.budget {
				t.Errorf("RetryBudget = %v, want %v", got.RetryBudget, tc.budget)
			}
			if got.Log == nil {
				t.Error("the retry log is not wired to stderr")
			}
			if left > tc.deadline || left < tc.deadline-time.Minute {
				t.Errorf("context deadline in %v, want about %v", left, tc.deadline)
			}
		})
	}
}
