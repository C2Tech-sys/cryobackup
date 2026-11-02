package scheduler

import (
	"testing"
	"time"
)

// M1-FIX3 item 10: Output.Resume marks a scheduled start that a resume's plan
// decided, so the agent waits for the console before it starts it; backup_now
// and plans without a resume are not marked.
func TestPlanMarksResumeStarts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Input)
		want   bool
	}{
		{"due after a resume", func(in *Input) { in.Resume = true }, true},
		{"retry after a resume", func(in *Input) {
			in.Resume = true
			in.Retry = RetryState{Pending: true, NotBefore: t0.Add(-time.Minute)}
		}, true},
		{"boot run found by a resume's plan", func(in *Input) { in.Resume, in.BootPending = true, true }, true},
		{"run_on_wake off (the agent drops such a resume first)", func(in *Input) {
			in.Resume = true
			in.Policy.Schedule.RunOnWake = false
		}, true},
		{"backup_now during a resume's plan", func(in *Input) { in.Resume, in.ForceNow = true, true }, false},
		{"no resume", func(in *Input) {}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := input()
			in.Policy.Schedule.RunOnWake = true
			tc.mutate(&in)
			out := Plan(in)
			if out.Action != ActionStart {
				t.Fatalf("Plan = %s (%s), want a start", out.Action, out.Reason)
			}
			if out.Resume != tc.want {
				t.Fatalf("Output.Resume = %v, want %v", out.Resume, tc.want)
			}
		})
	}
}
