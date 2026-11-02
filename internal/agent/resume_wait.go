package agent

import (
	"context"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/scheduler"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/policy"
)

// Network wait after resume (architecture §6.6, the resume row; M1-FIX3).
//
// A laptop that wakes finds its Wi-Fi or VPN a few seconds behind. A wake run
// started at once would find the console and the gateway unreachable, fail
// open on the start lease and defer on the gateway: one noisy failed attempt
// and a retry. So a scheduled start that a resume's plan decided
// (scheduler.Output.Resume) first waits, inside the run's goroutine, until the
// console answers a heartbeat or resumeNetWait passes, whichever comes first:
//
//   - the check is the heartbeat itself (no new endpoint): the wait kicks the
//     heartbeat loop at once and every resumeProbeEvery after that (the loop's
//     minimum gap still applies), and returns at the first heartbeat the
//     console answers after the wait began;
//   - a service stop or a cancel command ends the wait at once: the run is
//     journaled interrupted or cancelled, as a run stopped while it waits for
//     its start lease, and restic never starts;
//   - when the time is up the run starts anyway, and the normal paths apply
//     (the start lease fails open, a gateway that cannot be reached defers);
//   - after the wait, and before anything else, the run re-checks pause,
//     maintenance hold and backup window (resumeGate) against the current
//     policy response, pulled once more when the console answered (M1-FIX4):
//     an operator's pause set while the machine slept wins, and the run ends
//     without starting or being journaled.
//
// The run is active while it waits (the next plan sees Running), and the
// deadline is read from the agent's clock, so a test can move it.

// resumeNetWait is how long a resume's run waits for the console.
const resumeNetWait = 2 * time.Minute

// resumeProbeEvery is how often the wait asks for another heartbeat.
const resumeProbeEvery = 10 * time.Second

// noteConsoleReached records a heartbeat the console answered and wakes a
// waiting resume run.
func (a *Agent) noteConsoleReached() {
	a.mu.Lock()
	a.hbOKAt = a.clock()
	a.mu.Unlock()
	kick(a.consoleReached)
}

// waitForConsole blocks until the console answers a heartbeat sent after the
// call began (true), the wait's limit passes on the agent's clock (false), or
// ctx ends (false; the caller looks at ctx).
func (a *Agent) waitForConsole(ctx context.Context) bool {
	limit, every := a.o.resumeWait, a.o.resumeProbe
	if limit <= 0 {
		limit = resumeNetWait
	}
	if every <= 0 {
		every = resumeProbeEvery
	}
	began := a.clock()
	select { // an answer from before the wait does not count
	case <-a.consoleReached:
	default:
	}
	kick(a.kickHeartbeat)
	for {
		a.mu.Lock()
		reached := !a.hbOKAt.Before(began)
		a.mu.Unlock()
		if reached {
			return true
		}
		left := limit - a.clock().Sub(began)
		if left <= 0 {
			return false
		}
		tick, stop := a.waitTimer(min(every, left))
		select {
		case <-ctx.Done():
			stop()
			return false
		case <-a.consoleReached:
			stop()
		case <-tick:
			kick(a.kickHeartbeat)
		}
	}
}

// waitTimer starts the wait's probe timer: a real one, or the test's
// Options.resumeTimer, which lets a test move the wait with its own clock and
// no sleeping. stop releases the timer.
func (a *Agent) waitTimer(d time.Duration) (<-chan time.Time, func()) {
	if a.o.resumeTimer != nil {
		return a.o.resumeTimer(d)
	}
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// waitAfterResume runs the wait for a run a resume's plan started (runner.go). It
// returns false when ctx ended during the wait: the caller journals the run as
// stopped and returns.
func (a *Agent) waitAfterResume(ctx context.Context, id string) bool {
	limit := a.o.resumeWait
	if limit <= 0 {
		limit = resumeNetWait
	}
	a.log.Info("resume: waiting for the console before the backup starts", "run", id, "limit_s", int(limit/time.Second))
	began := a.clock()
	reached := a.waitForConsole(ctx)
	if ctx.Err() != nil {
		return false
	}
	waited := int(a.clock().Sub(began) / time.Second)
	if !reached {
		a.log.Warn("resume: the console did not answer in time; starting the backup anyway", "run", id, "waited_s", waited)
		return true
	}
	a.log.Info("resume: the console answered", "run", id, "waited_s", waited)
	// The console is back: fetch what it says now (a pause or a hold set while
	// the machine slept) before the run looks at the policy (resumeGate). A
	// failed pull keeps the policy held, as the policy loop would.
	pctx, cancel := context.WithTimeout(ctx, resumePolicyPull)
	err := a.pullPolicy(pctx)
	cancel()
	if ctx.Err() != nil {
		return false
	}
	if err != nil {
		a.log.Warn("resume: the policy could not be pulled after the wait; deciding on the policy held", "run", id, "err", err.Error())
	}
	return true
}

// resumePolicyPull bounds the policy pull after the wait.
const resumePolicyPull = 30 * time.Second

// resumeGate re-checks, after the wait, what a plan holds back on (the
// scheduler's pause, hold and window rules) against the current policy
// response: it returns the scheduler's reason when the run must not start, or
// "". A run a resume started waited up to resumeNetWait with a plan made
// before the console was reachable, so an operator's pause set while the
// laptop slept must win here.
func resumeGate(pol *api.PolicyResponse, now time.Time) string {
	switch {
	case pol == nil:
		return ""
	case pausedNow(pol, now):
		return scheduler.ReasonPaused
	case pol.HoldUntil != nil && pol.HoldUntil.After(now):
		return scheduler.ReasonHold
	}
	if w := pol.Policy.Schedule.Window; w != "" {
		// An unparsable window never blocks (as in the scheduler); Validate
		// rejects it upstream.
		if win, err := policy.ParseWindow(w); err == nil && !win.Contains(now.In(time.Local)) {
			return scheduler.ReasonWindow
		}
	}
	return ""
}
