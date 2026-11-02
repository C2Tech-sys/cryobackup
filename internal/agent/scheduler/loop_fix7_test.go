package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"
)

// L-STATUS-1: the loop hands every plan to Planned (the agent stores the
// planned start for "status"), and a zero Output when it cannot schedule.
func TestLoopReportsEveryPlan(t *testing.T) {
	var (
		mu    sync.Mutex
		ok    = true
		plans []Output
	)
	l := &Loop{
		Tick: time.Hour,
		Input: func(ctx context.Context, now time.Time) (Input, bool) {
			mu.Lock()
			defer mu.Unlock()
			in := input()
			in.Now, in.BootAt = now, now
			in.Policy.Schedule.StartupDelayS = 600
			return in, ok
		},
		Start: func(ctx context.Context, out Output) { t.Error("nothing may start during the startup delay") },
		Planned: func(ctx context.Context, out Output) {
			mu.Lock()
			plans = append(plans, out)
			mu.Unlock()
		},
	}
	l.Init()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	count := func() int { mu.Lock(); defer mu.Unlock(); return len(plans) }
	waitFor(t, "the first plan", func() bool { return count() == 1 })
	mu.Lock()
	first := plans[0]
	ok = false
	mu.Unlock()
	if first.Action != ActionNone || first.Reason != ReasonStartupDelay || first.NextCheck.IsZero() {
		t.Fatalf("first plan = %+v, want a wait for the startup delay with its end", first)
	}
	l.Notify(TriggerPolicyChange)
	waitFor(t, "the plan of a loop that cannot schedule", func() bool { return count() == 2 })
	mu.Lock()
	defer mu.Unlock()
	if p := plans[1]; p.Action != ActionNone || !p.NextCheck.IsZero() || p.Reason != "" {
		t.Fatalf("plan without input = %+v, want the zero Output", p)
	}
}
