package scheduler

import (
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/policy"
)

// Action is what Plan decides.
type Action int

const (
	// ActionNone: nothing to start; Output.NextCheck says when to look again.
	ActionNone Action = iota
	// ActionStart: start a backup now.
	ActionStart
	// ActionSkip: an explicit start was refused because a run is active and the
	// policy says skip_if_running; the caller journals a "skipped" run.
	ActionSkip
)

func (a Action) String() string {
	switch a {
	case ActionStart:
		return "start"
	case ActionSkip:
		return "skip"
	}
	return "none"
}

// Reasons of an Output (fixed vocabulary; also what "status" prints).
const (
	ReasonForced       = "forced"        // backup_now: bypasses window and conditions
	ReasonRunning      = "running"       // a run is active
	ReasonPaused       = "paused"        // the policy pull says paused
	ReasonHold         = "hold"          // hold_until is in the future
	ReasonStartupDelay = "startup_delay" // startup_delay_s since process start
	ReasonRetryWait    = "retry_wait"    // the backoff after a failed or deferred run
	ReasonRetry        = "retry"         // a retry is due
	ReasonWindow       = "window"        // outside the backup window
	ReasonJitter       = "jitter"        // waiting out the per-slot jitter
	ReasonDue          = "due"           // policy.Due says due
	ReasonLeaseWait    = "lease_wait"    // the console refused a start lease (fleet cap); asking again later
)

// JitterMemo remembers, per slot, when the slot was first seen due, so that
// policy.Jitter is applied once per slot (§13.3). It is carried between Plan
// calls by the caller; losing it (a restart) only restarts the jitter wait.
type JitterMemo struct {
	Slot      time.Time
	FirstSeen time.Time
}

// RetryState is the backoff after a failed, deferred or interrupted run.
type RetryState struct {
	// Pending: the last attempt did not complete the slot and a retry is owed.
	Pending bool
	// NotBefore: the earliest retry time (the backoff gate).
	NotBefore time.Time
	// AuthDeferredAt is the finish time of the newest run when it ended
	// deferred with class auth (the gateway turned the device away); zero
	// otherwise. Input.AuthDeferralsWait reads it.
	AuthDeferredAt time.Time
}

// Input is everything Plan reads. There is no clock and no I/O here.
type Input struct {
	// Now is the device-local time (Location set): windows are evaluated on it.
	Now      time.Time
	DeviceID string
	Policy   policy.Policy

	// Paused and HoldUntil come from the policy response (pause and hold).
	Paused    bool
	HoldUntil time.Time

	// Running: a restic job is active.
	Running bool
	// LastSuccess is the finish of the last success or tolerated partial run
	// (zero = never); LastAttemptStart the start of the last attempt.
	LastSuccess      time.Time
	LastAttemptStart time.Time
	Retry            RetryState

	// AuthDeferralsWait: a run that ended deferred with class auth (Retry.
	// AuthDeferredAt) is not retried on the backoff ladder but at the policy's
	// normal interval, counted from that deferral, and never before the ladder
	// gate. The agent sets it while the
	// console refuses its device token (ADR 0018, M3-L10): the gateway login of
	// a retired device is gone too, so the ladder would knock about hourly for
	// good. With it unset an auth deferral keeps the ladder.
	AuthDeferralsWait bool

	// BootAt is when this process started; BootPending is true until the first
	// run of this process starts (the run is then labelled "boot").
	BootAt      time.Time
	BootPending bool

	// ForceNow: a backup_now command is waiting.
	ForceNow bool

	// LeaseNotBefore: the console refused the last start lease (the fleet
	// cap, ADR 0005) or the request was refused outright; no scheduled start
	// before this time. backup_now is not held back by it.
	LeaseNotBefore time.Time

	// Resume: this plan was asked for by a resume from sleep (Loop sets it; the
	// agent's input function does not). A run it finds due is labelled "wake"
	// when the policy's run_on_wake is on; with run_on_wake off it changes
	// nothing.
	Resume bool

	Jitter JitterMemo
}

// Output is the decision.
type Output struct {
	Action  Action
	Trigger api.Trigger // for ActionStart and ActionSkip
	Reason  string
	// NextCheck is the earliest time a re-plan could change the answer; zero
	// means "at the next tick".
	NextCheck time.Time
	Decision  policy.Decision
	Jitter    JitterMemo
	// Resume: a scheduled start (retry or due, never backup_now) decided by a
	// plan that a resume from sleep asked for. The agent then waits for the
	// console before it starts the run (architecture §6.6, Agent.waitForConsole).
	Resume bool
}

// Plan decides what the scheduler should do now. It is pure: the same Input
// gives the same Output. Rules (architecture §6.1 "scheduler", §6.2):
//
//   - backup_now (ForceNow) bypasses the window, conditions, startup delay and
//     backoff, but not pause or hold; with a run active it is skipped
//     (skip_if_running) or left pending.
//   - Otherwise a run in progress, pause, hold and the startup delay hold
//     everything back, and so does a refused start lease (LeaseNotBefore,
//     ADR 0005) until its time.
//   - A pending retry starts when its backoff gate has passed and the window is
//     open, with trigger "retry", whatever policy.Due says about the slot.
//   - Else policy.Due decides. A due slot waits for policy.Jitter(deviceID,
//     slot, jitter_minutes) counted from the first time the slot was seen due;
//     the very first backup ("never") and a clock-skew slot start without
//     jitter. The window gates starts only (policy.Due applies it).
//
// Conditions (battery, metered, idle, SSID, VPN) are absent in M0: a due run
// is never delayed by them and run_anyway_after_minutes has nothing to bound
// (milestone M8).
func Plan(in Input) Output {
	out := Output{Jitter: in.Jitter}
	sch := in.Policy.Schedule
	now := in.Now

	hold := !in.HoldUntil.IsZero() && in.HoldUntil.After(now)

	if in.ForceNow {
		switch {
		case in.Running && sch.SkipIfRunning:
			return with(out, ActionSkip, api.TriggerManual, ReasonRunning)
		case in.Running:
			return with(out, ActionNone, "", ReasonRunning)
		case in.Paused:
			return with(out, ActionNone, "", ReasonPaused)
		case hold:
			out = with(out, ActionNone, "", ReasonHold)
			out.NextCheck = in.HoldUntil
			return out
		}
		return with(out, ActionStart, api.TriggerManual, ReasonForced)
	}

	switch {
	case in.Running:
		return with(out, ActionNone, "", ReasonRunning)
	case in.Paused:
		return with(out, ActionNone, "", ReasonPaused)
	case hold:
		out = with(out, ActionNone, "", ReasonHold)
		out.NextCheck = in.HoldUntil
		return out
	}
	if delay := time.Duration(sch.StartupDelayS) * time.Second; delay > 0 {
		if until := in.BootAt.Add(delay); now.Before(until) {
			out = with(out, ActionNone, "", ReasonStartupDelay)
			out.NextCheck = until
			return out
		}
	}

	if !in.LeaseNotBefore.IsZero() && now.Before(in.LeaseNotBefore) {
		out = with(out, ActionNone, "", ReasonLeaseWait)
		out.NextCheck = in.LeaseNotBefore
		return out
	}

	// An auth deferral that waits for the next slot (AuthDeferralsWait) does
	// not start at the ladder gate: past it, the deferral stands in for a
	// success, so policy.Due counts the interval from it (a fixed-time schedule
	// needs no stand-in: its slot is attempted already) and catch-up cannot
	// fire at once. The gate itself still holds, so a policy with an interval
	// shorter than the ladder never tries more often than the ladder would.
	authWait := in.Retry.Pending && in.AuthDeferralsWait && !in.Retry.AuthDeferredAt.IsZero()
	lastSuccess := in.LastSuccess
	if authWait {
		lastSuccess = in.Retry.AuthDeferredAt
	}

	if in.Retry.Pending {
		if now.Before(in.Retry.NotBefore) {
			out = with(out, ActionNone, "", ReasonRetryWait)
			out.NextCheck = in.Retry.NotBefore
			return out
		}
		if !authWait {
			if open, next := windowOpen(sch.Window, now); !open {
				out = with(out, ActionNone, "", ReasonWindow)
				out.NextCheck = next
				return out
			}
			out.Resume = in.Resume
			return with(out, ActionStart, api.TriggerRetry, ReasonRetry)
		}
	}

	dec := policy.Due(policy.DueInput{
		Now: now, LastSuccess: lastSuccess, LastAttemptStart: in.LastAttemptStart,
		Schedule: sch, DeviceID: in.DeviceID,
	})
	out.Decision = dec
	if !dec.Due {
		out = with(out, ActionNone, "", dec.Reason)
		out.NextCheck = dec.NotBefore
		return out
	}

	trigger := api.TriggerSchedule
	switch {
	case in.BootPending:
		trigger = api.TriggerBoot
	case in.Resume && sch.RunOnWake:
		// Ahead of catch-up: a laptop that slept for a week and woke gets one
		// run either way (§6.1), and it is a wake run.
		trigger = api.TriggerWake
	case dec.Reason == policy.ReasonCatchup:
		trigger = api.TriggerCatchup
	}

	if dec.Reason != policy.ReasonNever && dec.Reason != policy.ReasonClockSkew && sch.JitterMinutes > 0 {
		memo := in.Jitter
		if !memo.Slot.Equal(dec.SlotStart) || memo.FirstSeen.IsZero() {
			memo = JitterMemo{Slot: dec.SlotStart, FirstSeen: now}
		}
		out.Jitter = memo
		delay := policy.Jitter(in.DeviceID, dec.SlotStart, time.Duration(sch.JitterMinutes)*time.Minute)
		if startAt := memo.FirstSeen.Add(delay); now.Before(startAt) {
			out = with(out, ActionNone, "", ReasonJitter)
			out.NextCheck = startAt
			return out
		}
	}
	out.Resume = in.Resume
	return with(out, ActionStart, trigger, ReasonDue)
}

func with(o Output, a Action, t api.Trigger, reason string) Output {
	o.Action, o.Trigger, o.Reason = a, t, reason
	return o
}

// windowOpen reports whether the window (empty = always open) contains now,
// and when it next opens. An unparsable window never blocks (policy.Validate
// rejects it upstream).
func windowOpen(window string, now time.Time) (bool, time.Time) {
	if window == "" {
		return true, time.Time{}
	}
	w, err := policy.ParseWindow(window)
	if err != nil || w.Contains(now) {
		return true, time.Time{}
	}
	next, _ := w.NextOpen(now)
	return false, next
}

// Lock deferrals and the generic backoff (architecture §6.2).
const (
	lockBase = 2 * time.Minute
	lockCap  = 15 * time.Minute

	defaultBase = 5 * time.Minute
	defaultCap  = 60 * time.Minute
)

// RetryDelay is the backoff after the attempt-th consecutive failed or
// deferred run (attempt 1 = the first retry): base x 2^(attempt-1) up to the
// cap, then spread by plus or minus 20 % with r in [0, 1). A lock deferral uses
// 2 min to 15 min; everything else uses the policy's retries (default 5 min to
// 60 min, §6.2). The policy's Retries.Max is recorded but never ends the
// retrying: a device that cannot back up keeps trying at the cap and stays
// visibly failing.
func RetryDelay(rt policy.Retries, attempt int, lock bool, r float64) time.Duration {
	base, limit := defaultBase, defaultCap
	if rt.BaseS > 0 {
		base = time.Duration(rt.BaseS) * time.Second
	}
	if rt.CapS > 0 {
		limit = time.Duration(rt.CapS) * time.Second
	}
	if lock {
		base, limit = lockBase, lockCap
	}
	if limit < base {
		limit = base
	}
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt && d < limit; i++ {
		d *= 2
	}
	if d > limit {
		d = limit
	}
	if r < 0 {
		r = 0
	}
	if r >= 1 {
		r = 0.999999
	}
	return time.Duration(float64(d) * (0.8 + 0.4*r))
}
