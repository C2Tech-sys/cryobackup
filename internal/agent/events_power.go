package agent

import (
	"sync/atomic"

	"github.com/c2tech-sys/cryobackup/internal/agent/service"
)

// HandleEvent receives the power and session notifications the service host
// delivers (service.Host.OnEvent, ticket r24 and r207). It runs on the SCM
// control goroutine, so it must never block: that goroutine is also what
// receives Stop, Shutdown and PreShutdown. It takes no lock the agent holds
// across I/O (the policy's run_on_wake is mirrored in an atomic, the scheduler
// is reached through a non-blocking Notify) and does no I/O but a log line.
//
//   - Resume from sleep (PBT_APMRESUMEAUTOMATIC, PBT_APMRESUMESUSPEND): the
//     scheduler re-plans at once, and a run it finds due is journaled with
//     trigger "wake" (api.TriggerWake; the loop's own name for the reason is
//     scheduler.TriggerResume). Ignored, with a debug line, when the effective
//     policy has run_on_wake off. Several resumes within
//     scheduler.ResumeDebounce give one re-plan. The run then waits for the
//     console, up to 2 minutes, before it starts (resume_wait.go).
//   - Suspend and every other power event: logged at debug and ignored.
//     Cancelling a running restic on suspend is not part of M1-T1; the lab
//     result L-WAKE-1 shows whether it is needed.
//   - Session changes (logon, logoff, lock, unlock): recorded at debug (the
//     code only, never a user name) and ignored. The agent runs as
//     LocalSystem whatever the interactive session does, and the schedule
//     (with run_on_wake for a wake) alone decides when a backup starts; a
//     logon or an unlock is no reason to start one.
//
// Network changes are out of scope; the policy loop's connectivity trigger
// covers the console coming back.
func (a *Agent) HandleEvent(e service.Event) {
	if !e.Power {
		a.log.Debug("session change ignored", "code", e.Code)
		return
	}
	switch e.Code {
	case service.PBTAPMResumeAutomatic, service.PBTAPMResumeSuspend:
		a.onResume(e.Code)
	default:
		a.log.Debug("power event ignored", "code", e.Code)
	}
}

func (a *Agent) onResume(code uint32) {
	select {
	case <-a.schedUp:
	default:
		// Nothing is scheduled before there is a policy and the loops run; the
		// first plan after that looks at the schedule anyway.
		a.log.Debug("resume ignored: the scheduler is not running yet", "code", code)
		return
	}
	if a.noRunOnWake.Load() {
		a.log.Debug("resume ignored: run_on_wake is off in the policy", "code", code)
		return
	}
	if !a.sched.NotifyResume() {
		a.log.Debug("resume folded into the previous one", "code", code)
		return
	}
	a.log.Debug("resume: the scheduler re-plans", "code", code)
}

// markSchedulerUp is called by every Agent.facts call: the scheduler's planner
// input, a backup's start and the heartbeat, whichever comes first. It closes
// schedUp, which publishes a.sched to the goroutine that delivers service
// events, and it refuses to do that before a.sched exists, so schedUp closed
// always means a.sched is set. Run sets a.sched before it starts any loop, so
// every caller inside Run sees it (the go statement orders the write before
// the read); a facts call outside Run (before it) leaves schedUp open.
func (a *Agent) markSchedulerUp() {
	if a.sched == nil {
		return
	}
	a.schedUpOnce.Do(func() { close(a.schedUp) })
}

// EventRelay carries service events to an Agent that does not exist yet when
// the service Host is built (the agent is created inside Host.Run). Events
// before Set, or after Set(nil), are dropped.
type EventRelay struct{ p atomic.Pointer[Agent] }

// Set installs the agent (nil removes it).
func (r *EventRelay) Set(a *Agent) { r.p.Store(a) }

// Handle is service.Host.OnEvent.
func (r *EventRelay) Handle(e service.Event) {
	if a := r.p.Load(); a != nil {
		a.HandleEvent(e)
	}
}
