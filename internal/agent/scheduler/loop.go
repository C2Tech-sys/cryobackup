package scheduler

import (
	"context"
	"sync"
	"time"
)

// Trigger names the reason the loop re-plans before its tick (§6.1: resume,
// boot, command, policy change and connectivity are triggers; logon and
// unlock are not: the agent records a session change and ignores it, see
// Agent.HandleEvent).
type Trigger string

const (
	TriggerTick Trigger = "tick"
	// TriggerResume is the loop's internal reason for a re-plan after the
	// machine woke. It is never journaled: a run made due by it is recorded
	// with api.TriggerWake, the run vocabulary's value (architecture §6.2).
	TriggerResume       Trigger = "resume"
	TriggerBoot         Trigger = "boot"
	TriggerCommand      Trigger = "command"
	TriggerPolicyChange Trigger = "policy_change"
	TriggerConnectivity Trigger = "connectivity"
)

// DefaultTick is the scheduler's heartbeat (architecture §6.1: 60 s).
const DefaultTick = 60 * time.Second

// ResumeDebounce is the window in which further resume events add nothing: the
// OS sends PBT_APMRESUMEAUTOMATIC and PBT_APMRESUMESUSPEND for one wake, and
// Modern Standby can send bursts. The window starts at the first accepted one.
const ResumeDebounce = 30 * time.Second

// Loop runs Plan on a tick and on triggers. It keeps only the jitter memo; all
// facts come from Input, so due-ness is always recomputed from the journal and
// no next-run is stored (§6.1).
type Loop struct {
	// Clock is the device clock; nil means time.Now.
	Clock func() time.Time
	// Tick is the planning period; zero means DefaultTick.
	Tick time.Duration
	// Input gathers the facts for Now (device-local). ok=false means the agent
	// must not schedule yet (no policy, no repository key): the loop idles.
	Input func(ctx context.Context, now time.Time) (in Input, ok bool)
	// Start and Skip carry out the decision. They must return quickly (the
	// agent runs the backup on its own goroutine) and must mark the run active
	// before returning so the next plan sees Running.
	Start func(ctx context.Context, out Output)
	Skip  func(ctx context.Context, out Output)
	// Planned, if set, receives every plan's Output after Start or Skip, and
	// a zero Output when Input says the agent must not schedule. The agent
	// keeps the planned start for "cryo-agent status" (L-STATUS-1), which
	// cannot know the startup delay or the jitter memo. It must return
	// quickly.
	Planned func(ctx context.Context, out Output)

	triggers chan Trigger
	memo     JitterMemo

	// Resume state: the time of the last accepted resume (debounce) and
	// whether the next plan was asked for by one (it then labels a due run
	// "wake").
	resumeMu      sync.Mutex
	resumeAt      time.Time
	resumePending bool
}

func (l *Loop) now() time.Time {
	if l.Clock != nil {
		return l.Clock()
	}
	return time.Now()
}

// NotifyResume asks the loop to re-plan at once because the machine resumed
// from sleep. A resume within ResumeDebounce of the last accepted one is
// dropped (it reports false). An accepted one makes the next plan label a run
// it finds due as api.TriggerWake, provided the policy's run_on_wake is on
// (Plan decides: the loop does not read the policy here). It never blocks.
func (l *Loop) NotifyResume() bool {
	if l == nil {
		return false
	}
	now := l.now()
	l.resumeMu.Lock()
	if d := now.Sub(l.resumeAt); !l.resumeAt.IsZero() && d >= 0 && d < ResumeDebounce {
		l.resumeMu.Unlock()
		return false
	}
	l.resumeAt, l.resumePending = now, true
	l.resumeMu.Unlock()
	l.Notify(TriggerResume)
	return true
}

// takeResume reports and clears the pending resume.
func (l *Loop) takeResume() bool {
	l.resumeMu.Lock()
	defer l.resumeMu.Unlock()
	r := l.resumePending
	l.resumePending = false
	return r
}

// keepResume re-arms the pending resume.
func (l *Loop) keepResume() {
	l.resumeMu.Lock()
	l.resumePending = true
	l.resumeMu.Unlock()
}

// Notify asks the loop to re-plan soon. It never blocks.
func (l *Loop) Notify(t Trigger) {
	if l == nil || l.triggers == nil {
		return
	}
	select {
	case l.triggers <- t:
	default: // a trigger is already waiting; one re-plan covers them all
	}
}

// Init must be called before Notify or Run.
func (l *Loop) Init() {
	if l.triggers == nil {
		l.triggers = make(chan Trigger, 1)
	}
}

// Run plans until ctx ends.
func (l *Loop) Run(ctx context.Context) {
	l.Init()
	clock := l.Clock
	if clock == nil {
		clock = time.Now
	}
	tick := l.Tick
	if tick <= 0 {
		tick = DefaultTick
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-l.triggers:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		now := clock()
		wait := tick
		// Taken before the facts are gathered: a resume that arrives meanwhile
		// stays pending (with its own trigger queued) for the next plan.
		resume := l.takeResume()
		if in, ok := l.Input(ctx, now); ok {
			in.Jitter = l.memo
			in.Resume = resume
			out := Plan(in)
			l.memo = out.Jitter
			// A run that the resume made due but that is waiting out its
			// jitter still belongs to the resume.
			if resume && out.Reason == ReasonJitter {
				l.keepResume()
			}
			switch out.Action {
			case ActionStart:
				l.Start(ctx, out)
			case ActionSkip:
				if l.Skip != nil {
					l.Skip(ctx, out)
				}
			}
			if l.Planned != nil {
				l.Planned(ctx, out)
			}
			if !out.NextCheck.IsZero() {
				if d := out.NextCheck.Sub(now); d > 0 && d < wait {
					wait = d
				}
			}
		} else if l.Planned != nil {
			l.Planned(ctx, Output{})
		}
		if wait < time.Second {
			wait = time.Second
		}
		timer.Reset(wait)
	}
}
