package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
)

// r24: a resume from sleep re-plans at once, gated on run_on_wake, and a run it
// makes due is journaled "wake" (api.TriggerWake). TriggerResume is only the
// loop's name for the reason.

func TestPlanResumeLabelsADueRunWake(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Input)
		action  Action
		trigger api.Trigger
	}{
		{"due and resumed: wake", func(in *Input) { in.Resume = true }, ActionStart, api.TriggerWake},
		{"run_on_wake off: the flag changes nothing", func(in *Input) {
			in.Resume = true
			in.Policy.Schedule.RunOnWake = false
		}, ActionStart, api.TriggerSchedule},
		{"wake wins over catch-up (a week asleep is one wake run)", func(in *Input) {
			in.Resume = true
			in.LastSuccess = t0.Add(-100 * time.Hour)
			in.LastAttemptStart = in.LastSuccess
		}, ActionStart, api.TriggerWake},
		{"not due: a resume starts nothing", func(in *Input) {
			in.Resume = true
			in.LastSuccess = t0.Add(-time.Hour)
			in.LastAttemptStart = in.LastSuccess
		}, ActionNone, ""},
		{"the first run of the process stays boot", func(in *Input) { in.Resume, in.BootPending = true, true }, ActionStart, api.TriggerBoot},
		{"an owed retry stays retry", func(in *Input) {
			in.Resume = true
			in.Retry = RetryState{Pending: true, NotBefore: t0.Add(-time.Minute)}
		}, ActionStart, api.TriggerRetry},
		{"no resume: schedule, as before", func(in *Input) {}, ActionStart, api.TriggerSchedule},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := input()
			in.Policy.Schedule.RunOnWake = true
			tc.mutate(&in)
			out := Plan(in)
			if out.Action != tc.action || out.Trigger != tc.trigger {
				t.Fatalf("Plan = %s %q (%s), want %s %q", out.Action, out.Trigger, out.Reason, tc.action, tc.trigger)
			}
		})
	}
}

// resumeRig is a Loop whose input the test steers and whose clock it owns.
type resumeRig struct {
	l       *Loop
	now     atomic.Int64 // unix nanos of the fake clock
	due     atomic.Bool  // false: last success is recent; true: one interval overdue
	wake    atomic.Bool  // the policy's run_on_wake
	jitter  atomic.Int32 // policy jitter minutes
	inputs  atomic.Int32
	mu      sync.Mutex
	started []Output
}

func newResumeRig(t *testing.T) *resumeRig {
	t.Helper()
	r := &resumeRig{}
	r.now.Store(t0.UnixNano())
	r.wake.Store(true)
	r.l = &Loop{
		Clock: func() time.Time { return time.Unix(0, r.now.Load()).UTC() },
		Tick:  time.Hour, // only a trigger wakes the loop after the first plan
		Input: func(ctx context.Context, now time.Time) (Input, bool) {
			r.inputs.Add(1)
			in := input()
			in.Now = now
			in.Policy.Schedule.RunOnWake = r.wake.Load()
			in.Policy.Schedule.JitterMinutes = int(r.jitter.Load())
			// Absolute, so the slot stays the same while the fake clock moves.
			last := t0.Add(-time.Hour)
			if r.due.Load() {
				last = t0.Add(-25 * time.Hour)
			}
			in.LastSuccess, in.LastAttemptStart = last, last
			return in, true
		},
		Start: func(ctx context.Context, out Output) {
			r.mu.Lock()
			r.started = append(r.started, out)
			r.mu.Unlock()
			r.due.Store(false) // the run completed
		},
	}
	r.l.Init()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.l.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, "the first plan", func() bool { return r.inputs.Load() >= 1 })
	return r
}

func (r *resumeRig) advance(d time.Duration) { r.now.Add(int64(d)) }

func (r *resumeRig) starts() []Output {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Output(nil), r.started...)
}

func TestLoopReplansOnResumeAndLabelsTheRunWake(t *testing.T) {
	r := newResumeRig(t)
	if n := len(r.starts()); n != 0 {
		t.Fatalf("%d runs before anything was due", n)
	}
	r.due.Store(true) // the machine slept past its slot
	if !r.l.NotifyResume() {
		t.Fatal("the first resume was not accepted")
	}
	waitFor(t, "the resume to start a run", func() bool { return len(r.starts()) == 1 })
	if got := r.starts()[0]; got.Trigger != api.TriggerWake || got.Reason != ReasonDue {
		t.Errorf("run = trigger %q reason %q, want wake/due", got.Trigger, got.Reason)
	}
}

func TestLoopResumeWithRunOnWakeOffStartsNothingExtra(t *testing.T) {
	r := newResumeRig(t)
	r.wake.Store(false)
	r.due.Store(true)
	n := r.inputs.Load()
	r.l.NotifyResume()
	// The loop re-plans (it cannot read the policy before it plans), but the
	// run is an ordinary scheduled one, not a wake run.
	waitFor(t, "the re-plan", func() bool { return r.inputs.Load() > n })
	waitFor(t, "the run", func() bool { return len(r.starts()) == 1 })
	if got := r.starts()[0].Trigger; got == api.TriggerWake {
		t.Errorf("trigger = %q with run_on_wake off", got)
	}
}

func TestResumeDebounce(t *testing.T) {
	r := newResumeRig(t)
	if !r.l.NotifyResume() {
		t.Fatal("first resume rejected")
	}
	waitFor(t, "the first resume's re-plan", func() bool { return r.inputs.Load() >= 2 })
	n := r.inputs.Load()
	r.advance(10 * time.Second)
	if r.l.NotifyResume() {
		t.Error("a second resume 10 s later was accepted")
	}
	r.advance(19 * time.Second) // 29 s after the first
	if r.l.NotifyResume() {
		t.Error("a third resume 29 s after the first was accepted")
	}
	time.Sleep(150 * time.Millisecond)
	if got := r.inputs.Load(); got != n {
		t.Errorf("%d re-plans for the dropped resumes, want none", got-n)
	}
	r.advance(2 * time.Second) // 31 s after the first
	if !r.l.NotifyResume() {
		t.Error("a resume after the window was dropped")
	}
	waitFor(t, "the later resume's re-plan", func() bool { return r.inputs.Load() > n })
}

func TestResumeBurstIsOneReplan(t *testing.T) {
	r := newResumeRig(t)
	n := r.inputs.Load()
	accepted := 0
	for i := 0; i < 5; i++ { // PBT_APMRESUMEAUTOMATIC, PBT_APMRESUMESUSPEND, then a Modern Standby burst
		if r.l.NotifyResume() {
			accepted++
		}
	}
	if accepted != 1 {
		t.Errorf("%d of 5 resumes accepted, want 1", accepted)
	}
	waitFor(t, "the re-plan", func() bool { return r.inputs.Load() > n })
	time.Sleep(150 * time.Millisecond)
	if got := r.inputs.Load() - n; got != 1 {
		t.Errorf("%d re-plans for a burst, want 1", got)
	}
}

// A run that the resume made due but that waits out its jitter is still the
// resume's run.
func TestResumeSurvivesTheJitterWait(t *testing.T) {
	r := newResumeRig(t)
	r.jitter.Store(30)
	r.due.Store(true)
	r.l.NotifyResume()
	waitFor(t, "the resume's re-plan", func() bool { return r.inputs.Load() >= 2 })
	time.Sleep(100 * time.Millisecond)
	if n := len(r.starts()); n != 0 {
		t.Fatalf("%d runs started during the jitter wait (this device id's jitter is zero: pick another)", n)
	}
	r.advance(31 * time.Minute) // past any jitter of 30 minutes
	r.l.Notify(TriggerTick)
	waitFor(t, "the run after the jitter", func() bool { return len(r.starts()) == 1 })
	if got := r.starts()[0].Trigger; got != api.TriggerWake {
		t.Errorf("trigger = %q, want wake", got)
	}
}

// A resume is used up by the plan that took it: after the wake run, the next
// run the schedule makes due (no new resume) is journaled "schedule", not
// "wake" (takeResume clears resumePending; M1-FIX1 D14).
func TestUsedResumeIsCleared(t *testing.T) {
	r := newResumeRig(t)
	r.due.Store(true)
	if !r.l.NotifyResume() {
		t.Fatal("the resume was not accepted")
	}
	waitFor(t, "the wake run", func() bool { return len(r.starts()) == 1 })
	if got := r.starts()[0].Trigger; got != api.TriggerWake {
		t.Fatalf("first run trigger = %q, want wake", got)
	}
	// Later the next slot comes due by the clock alone: an ordinary tick.
	r.advance(2 * time.Minute)
	r.due.Store(true)
	r.l.Notify(TriggerTick)
	waitFor(t, "the scheduled run", func() bool { return len(r.starts()) == 2 })
	if got := r.starts()[1].Trigger; got != api.TriggerSchedule {
		t.Errorf("second run trigger = %q, want schedule: the used resume was not cleared", got)
	}
}

func TestNotifyResumeOnANilLoop(t *testing.T) {
	var l *Loop
	if l.NotifyResume() {
		t.Error("a nil loop accepted a resume")
	}
}
