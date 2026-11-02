package scheduler

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/journal"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// 2026-10-22 is a Thursday.
var t0 = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

const dev = "01JB0000000000000000000000"

// input is a quiet baseline: a desktop with the default policy, process started
// an hour ago, last success 25 hours ago (so one interval is overdue), no
// jitter unless a test asks for it.
func input() Input {
	p := policy.Default(policy.ClassDesktop)
	p.Schedule.JitterMinutes = 0
	return Input{
		Now: t0, DeviceID: dev, Policy: p,
		LastSuccess: t0.Add(-25 * time.Hour), LastAttemptStart: t0.Add(-25 * time.Hour),
		BootAt: t0.Add(-time.Hour),
	}
}

func TestPlanTable(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Input)
		action  Action
		trigger api.Trigger
		reason  string
		next    func(Input) time.Time // expected NextCheck, nil = don't care
	}{
		{"never succeeded starts at once, without jitter", func(in *Input) {
			in.LastSuccess, in.LastAttemptStart = time.Time{}, time.Time{}
			in.Policy.Schedule.JitterMinutes = 30
		}, ActionStart, api.TriggerSchedule, ReasonDue, nil},
		{"first run of the process is labelled boot", func(in *Input) { in.BootPending = true }, ActionStart, api.TriggerBoot, ReasonDue, nil},
		{"a long overdue slot is a catch-up run", func(in *Input) {
			in.LastSuccess = t0.Add(-100 * time.Hour)
			in.LastAttemptStart = in.LastSuccess
		}, ActionStart, api.TriggerCatchup, ReasonDue, nil},
		{"startup delay holds the first run", func(in *Input) { in.BootAt = t0.Add(-30 * time.Second) },
			ActionNone, "", ReasonStartupDelay, func(in Input) time.Time { return in.BootAt.Add(120 * time.Second) }},
		{"not yet due", func(in *Input) {
			in.LastSuccess = t0.Add(-time.Hour)
			in.LastAttemptStart = in.LastSuccess
		}, ActionNone, "", policy.ReasonNotYet, func(in Input) time.Time {
			return in.LastSuccess.Add(24*time.Hour + policy.StableOffset(dev, 1*time.Hour))
		}},
		{"a run in progress holds everything", func(in *Input) { in.Running = true }, ActionNone, "", ReasonRunning, nil},
		{"paused", func(in *Input) { in.Paused = true }, ActionNone, "", ReasonPaused, nil},
		{"hold until a later time", func(in *Input) { in.HoldUntil = t0.Add(2 * time.Hour) },
			ActionNone, "", ReasonHold, func(in Input) time.Time { return in.HoldUntil }},
		{"a hold in the past is ignored", func(in *Input) { in.HoldUntil = t0.Add(-time.Hour) }, ActionStart, api.TriggerSchedule, ReasonDue, nil},
		{"outside the window the run waits for the next opening", func(in *Input) { in.Policy.Schedule.Window = "Mon/8:00-9:00" },
			ActionNone, "", policy.ReasonWindow, func(Input) time.Time { return time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC) }},
		{"backup_now bypasses the window", func(in *Input) {
			in.Policy.Schedule.Window = "Mon/8:00-9:00"
			in.ForceNow = true
		}, ActionStart, api.TriggerManual, ReasonForced, nil},
		{"backup_now bypasses the startup delay and the backoff", func(in *Input) {
			in.BootAt = t0.Add(-time.Second)
			in.Retry = RetryState{Pending: true, NotBefore: t0.Add(time.Hour)}
			in.ForceNow = true
		}, ActionStart, api.TriggerManual, ReasonForced, nil},
		{"backup_now does not bypass a pause", func(in *Input) { in.ForceNow, in.Paused = true, true }, ActionNone, "", ReasonPaused, nil},
		{"backup_now while running is skipped (skip_if_running)", func(in *Input) { in.ForceNow, in.Running = true, true },
			ActionSkip, api.TriggerManual, ReasonRunning, nil},
		{"backup_now while running stays pending without skip_if_running", func(in *Input) {
			in.ForceNow, in.Running = true, true
			in.Policy.Schedule.SkipIfRunning = false
		}, ActionNone, "", ReasonRunning, nil},
		{"retry waits for its backoff gate", func(in *Input) { in.Retry = RetryState{Pending: true, NotBefore: t0.Add(5 * time.Minute)} },
			ActionNone, "", ReasonRetryWait, func(in Input) time.Time { return in.Retry.NotBefore }},
		{"retry starts once the gate has passed", func(in *Input) { in.Retry = RetryState{Pending: true, NotBefore: t0.Add(-time.Minute)} },
			ActionStart, api.TriggerRetry, ReasonRetry, nil},
		{"a retry still honours the window", func(in *Input) {
			in.Retry = RetryState{Pending: true, NotBefore: t0.Add(-time.Minute)}
			in.Policy.Schedule.Window = "Mon/8:00-9:00"
		}, ActionNone, "", ReasonWindow, func(Input) time.Time { return time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC) }},
		{"a retry runs even when the slot was already attempted (no catch-up)", func(in *Input) {
			in.Policy.Schedule.Catchup = false
			in.LastAttemptStart = t0.Add(-10 * time.Minute)
			in.Retry = RetryState{Pending: true, NotBefore: t0.Add(-time.Minute)}
		}, ActionStart, api.TriggerRetry, ReasonRetry, nil},
		{"without catch-up an attempted slot is not run again", func(in *Input) {
			in.Policy.Schedule.Catchup = false
			in.LastAttemptStart = t0.Add(-10 * time.Minute)
		}, ActionNone, "", policy.ReasonSlotAttempted, nil},
		{"a success in the future is a clock-skew run, without jitter", func(in *Input) {
			in.LastSuccess = t0.Add(48 * time.Hour)
			in.Policy.Schedule.JitterMinutes = 30
		}, ActionStart, api.TriggerSchedule, ReasonDue, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := input()
			tc.mutate(&in)
			out := Plan(in)
			if out.Action != tc.action || out.Trigger != tc.trigger || out.Reason != tc.reason {
				t.Fatalf("Plan = %s %q %q, want %s %q %q", out.Action, out.Trigger, out.Reason, tc.action, tc.trigger, tc.reason)
			}
			if tc.next != nil {
				if want := tc.next(in); !out.NextCheck.Equal(want) {
					t.Errorf("NextCheck = %v, want %v", out.NextCheck, want)
				}
			}
			if again := Plan(in); !reflect.DeepEqual(again, out) {
				t.Errorf("Plan is not pure: %+v then %+v", out, again)
			}
		})
	}
}

func TestClockSkewReasonIsCarried(t *testing.T) {
	in := input()
	in.LastSuccess = t0.Add(48 * time.Hour)
	if out := Plan(in); out.Decision.Reason != policy.ReasonClockSkew {
		t.Fatalf("decision reason = %q, want clock_skew", out.Decision.Reason)
	}
}

func TestJitterIsAppliedOncePerSlot(t *testing.T) {
	in := input()
	in.Policy.Schedule.JitterMinutes = 30
	in.LastSuccess, in.LastAttemptStart = t0.Add(-25*time.Hour), t0.Add(-25*time.Hour)

	first := Plan(in)
	slot := first.Decision.SlotStart
	delay := policy.Jitter(dev, slot, 30*time.Minute)
	if delay == 0 {
		t.Skip("this device's jitter for the slot is zero; nothing to wait for")
	}
	if first.Action != ActionNone || first.Reason != ReasonJitter || !first.NextCheck.Equal(t0.Add(delay)) {
		t.Fatalf("first plan = %s %q next %v, want a jitter wait until %v", first.Action, first.Reason, first.NextCheck, t0.Add(delay))
	}
	if !first.Jitter.Slot.Equal(slot) || !first.Jitter.FirstSeen.Equal(t0) {
		t.Fatalf("memo = %+v", first.Jitter)
	}

	// Later ticks keep the memo: the wait is counted from the first sighting,
	// not restarted at every tick.
	mid := in
	mid.Now, mid.Jitter = t0.Add(delay/2), first.Jitter
	if out := Plan(mid); out.Action != ActionNone || !out.NextCheck.Equal(t0.Add(delay)) {
		t.Fatalf("mid plan = %s next %v", out.Action, out.NextCheck)
	}
	late := in
	late.Now, late.Jitter = t0.Add(delay), first.Jitter
	if out := Plan(late); out.Action != ActionStart {
		t.Fatalf("plan at the end of the jitter = %s %q, want start", out.Action, out.Reason)
	}

	// A new slot starts a new wait.
	next := in
	next.LastSuccess, next.LastAttemptStart = in.LastSuccess.Add(24*time.Hour), in.LastSuccess.Add(24*time.Hour)
	next.Now, next.Jitter = t0.Add(26*time.Hour), first.Jitter
	if out := Plan(next); out.Jitter.Slot.Equal(slot) {
		t.Errorf("the memo of the old slot was reused for a new slot: %+v", out.Jitter)
	}
}

func TestRetryDelay(t *testing.T) {
	std := policy.Retries{Max: 3, BaseS: 300, CapS: 3600}
	within := func(t *testing.T, got, base time.Duration) {
		t.Helper()
		if lo, hi := time.Duration(float64(base)*0.8), time.Duration(float64(base)*1.2); got < lo || got > hi {
			t.Errorf("delay %v not within +-20%% of %v", got, base)
		}
	}
	for i, wantMin := range []int{5, 10, 20, 40, 60, 60, 60} {
		within(t, RetryDelay(std, i+1, false, 0.5), time.Duration(wantMin)*time.Minute)
	}
	for i, wantMin := range []int{2, 4, 8, 15, 15, 15} {
		within(t, RetryDelay(std, i+1, true, 0.5), time.Duration(wantMin)*time.Minute)
	}
	if got := RetryDelay(std, 1, false, 0); got != 4*time.Minute {
		t.Errorf("r=0 gives %v, want 0.8 x 5 min", got)
	}
	if got := RetryDelay(std, 1, false, 0.9999999); got < 5*time.Minute+59*time.Second || got > 6*time.Minute {
		t.Errorf("r~1 gives %v, want 1.2 x 5 min", got)
	}
	// An unset policy section falls back to the defaults of §6.2; a custom one wins.
	within(t, RetryDelay(policy.Retries{}, 2, false, 0.5), 10*time.Minute)
	within(t, RetryDelay(policy.Retries{BaseS: 60, CapS: 120}, 5, false, 0.5), 2*time.Minute)
	// Many attempts never overflow or shrink.
	within(t, RetryDelay(std, 500, false, 0.5), time.Hour)
	within(t, RetryDelay(std, 0, false, 0.5), 5*time.Minute)
}

// ---- facts ----------------------------------------------------------------

func row(id string, start time.Time, outcome restic.Outcome, mutate func(*journal.RunRow)) journal.RunRow {
	r := journal.RunRow{RunRecord: api.RunRecord{ID: id, Kind: restic.KindBackup, StartedAt: start, Attempt: 1}}
	if outcome != "" {
		fin := start.Add(10 * time.Minute)
		r.FinishedAt, r.Outcome = &fin, outcome
	}
	if mutate != nil {
		mutate(&r)
	}
	return r
}

func TestFactsFromRuns(t *testing.T) {
	pol := policy.Default(policy.ClassDesktop) // partial tolerance: 20 files or 0.5 %
	h := func(n int) time.Time { return t0.Add(time.Duration(-n) * time.Hour) }

	t.Run("no runs", func(t *testing.T) {
		f := FactsFromRuns(nil, pol)
		if !f.LastSuccess.IsZero() || f.Retry.Pending || f.Attempts != 0 {
			t.Fatalf("%+v", f)
		}
	})
	t.Run("failures since the last success owe a retry with backoff", func(t *testing.T) {
		rows := []journal.RunRow{
			row("C", h(1), restic.OutcomeFailed, nil),
			row("B", h(2), restic.OutcomeDeferred, func(r *journal.RunRow) { r.DeferredReason = restic.DeferredGateway }),
			row("A", h(30), restic.OutcomeSuccess, nil),
		}
		f := FactsFromRuns(rows, pol)
		if f.Attempts != 2 || !f.Retry.Pending || f.Last == nil || f.Last.ID != "C" {
			t.Fatalf("%+v", f)
		}
		if !f.LastSuccess.Equal(h(30).Add(10 * time.Minute)) {
			t.Errorf("last success = %v", f.LastSuccess)
		}
		want := h(1).Add(10 * time.Minute).Add(RetryDelay(pol.Schedule.Retries, 2, false, stableFraction("C")))
		if !f.Retry.NotBefore.Equal(want) {
			t.Errorf("retry not before %v, want %v", f.Retry.NotBefore, want)
		}
		if again := FactsFromRuns(rows, pol); !again.Retry.NotBefore.Equal(f.Retry.NotBefore) {
			t.Error("the backoff spread changes between plans; it must be stable per run")
		}
	})
	t.Run("a lock deferral uses the lock backoff", func(t *testing.T) {
		rows := []journal.RunRow{row("L", h(1), restic.OutcomeDeferred, func(r *journal.RunRow) { r.DeferredReason = restic.DeferredLock })}
		f := FactsFromRuns(rows, pol)
		d := f.Retry.NotBefore.Sub(h(1).Add(10 * time.Minute))
		if d < 90*time.Second || d > 150*time.Second {
			t.Errorf("lock backoff = %v, want about 2 min", d)
		}
	})
	t.Run("a tolerated partial completes the slot, an intolerable one does not", func(t *testing.T) {
		ok := row("P1", h(1), restic.OutcomePartial, func(r *journal.RunRow) {
			r.Stats.FailedFiles, r.Stats.TotalFilesProcessed, r.ErrorClass = 3, 1000, restic.ClassLocked
		})
		if f := FactsFromRuns([]journal.RunRow{ok}, pol); f.LastSuccess.IsZero() || f.Retry.Pending {
			t.Errorf("tolerated partial: %+v", f)
		}
		bad := row("P2", h(1), restic.OutcomePartial, func(r *journal.RunRow) { r.Stats.FailedFiles, r.Stats.TotalFilesProcessed = 500, 1000 })
		if f := FactsFromRuns([]journal.RunRow{bad}, pol); !f.LastSuccess.IsZero() || !f.Retry.Pending {
			t.Errorf("intolerable partial: %+v", f)
		}
	})
	t.Run("skipped runs are ignored, open runs are attempts without a result", func(t *testing.T) {
		rows := []journal.RunRow{
			row("O", h(0), "", nil), // open
			row("S", h(1), restic.OutcomeSkipped, nil),
			row("A", h(30), restic.OutcomeSuccess, nil),
		}
		f := FactsFromRuns(rows, pol)
		if !f.LastAttemptStart.Equal(h(0)) || f.Attempts != 0 || f.Retry.Pending || f.Last == nil || f.Last.ID != "A" {
			t.Fatalf("%+v", f)
		}
	})
	t.Run("a cancelled and an interrupted run are retried", func(t *testing.T) {
		for _, o := range []restic.Outcome{restic.OutcomeCancelled, restic.OutcomeInterrupted, restic.OutcomeAbandoned} {
			f := FactsFromRuns([]journal.RunRow{row("X", h(1), o, nil)}, pol)
			if !f.Retry.Pending {
				t.Errorf("%s: no retry owed", o)
			}
		}
	})
}

// ---- loop -----------------------------------------------------------------

func TestLoopPlansOnTickAndTrigger(t *testing.T) {
	var (
		mu      sync.Mutex
		running bool
		inputs  atomic.Int32
		starts  atomic.Int32
	)
	l := &Loop{
		Tick: time.Hour, // only the triggers can wake it after the first plan
		Input: func(ctx context.Context, now time.Time) (Input, bool) {
			inputs.Add(1)
			mu.Lock()
			defer mu.Unlock()
			in := input()
			in.Now, in.Running = now, running
			return in, true
		},
		Start: func(ctx context.Context, out Output) {
			starts.Add(1)
			mu.Lock()
			running = true
			mu.Unlock()
		},
	}
	l.Init()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	waitFor(t, "the first plan to start a run", func() bool { return starts.Load() == 1 })
	n := inputs.Load()
	l.Notify(TriggerPolicyChange)
	waitFor(t, "the trigger to cause a re-plan", func() bool { return inputs.Load() > n })
	if starts.Load() != 1 {
		t.Errorf("a run was started while one was active: %d starts", starts.Load())
	}
	(*Loop)(nil).Notify(TriggerTick) // a nil loop must not panic (one-shot commands have no scheduler)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
