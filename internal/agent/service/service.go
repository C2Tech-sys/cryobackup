package service

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Identity of the Windows service and its Event Log source.
const (
	Name        = "CryoBackup"
	DisplayName = "CryoBackup Agent"
	Description = "Backs up this computer with restic on the schedule set in the CryoBackup console."
	EventSource = "CryoBackup"
)

// EventIDs are the Event Log ids the agent writes (internal/agent; README
// "Event Log"): 1000 started, 1001 restic verification failed, 1002 policy
// rejected, 1003 stopped, 1004 state directory refused, 1005 lab flag in use,
// 1006 state directory tampered, 1007 quarantined, 1008 retired (kept so that
// old events still render), 1009 service configuration repaired, 1010 backup
// finished, 1011 backup failed, 1012 backup deferred. Each has the message
// text "%1" (the agent's own line) in the message table compiled into
// cryo-agent.exe (deploy/windows/eventlog/messages.mc), which the source names
// as its EventMessageFile. EventCreate.exe, the earlier message file, only has
// texts for ids 1 to 1000, so Event Viewer could not show 1001 and above
// (L-EVT-1).
var EventIDs = []uint32{1000, 1001, 1002, 1003, 1004, 1005, 1006, 1007, 1008, 1009, 1010, 1011, 1012}

// Opening a source this process has just registered can fail for a moment
// with "Access is denied" (FIX6: seen once on the elevated CI runner), so
// OpenEventLogFresh tries this many times, this far apart.
const (
	freshOpenAttempts = 5
	freshOpenWait     = 200 * time.Millisecond
)

// OpenEventLogFresh is OpenEventLog for a source this process may just have
// registered (EnsureEventSource, then open: the --repair-acl path of
// "install" and "enroll", FIX20). It retries a failed open up to five times,
// 200 ms apart, and returns the last error if every attempt fails, so the
// caller can still say so loudly.
func OpenEventLogFresh() (*EventLog, error) {
	return openWithRetry(OpenEventLog, freshOpenAttempts, freshOpenWait, time.Sleep)
}

// openWithRetry calls open up to attempts times, sleeping wait between
// failures, and returns the first success or the last error.
func openWithRetry(open func() (*EventLog, error), attempts int, wait time.Duration, sleep func(time.Duration)) (*EventLog, error) {
	if attempts < 1 {
		attempts = 1
	}
	var err error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			sleep(wait)
		}
		var el *EventLog
		if el, err = open(); err == nil {
			return el, nil
		}
	}
	return nil, err
}

// ErrUnsupported is returned off Windows (CI and development builds).
var ErrUnsupported = errors.New("service: the Windows service host is not available on this platform")

// Local copies of the notification codes the service receives (UNCERTAIN
// whether x/sys exports them, engine §5.1). The service hands them to
// Host.OnEvent; the agent acts on resume only (Agent.HandleEvent).
const (
	PBTAPMSuspend         = 0x4
	PBTAPMResumeSuspend   = 0x7
	PBTAPMResumeAutomatic = 0x12

	WTSSessionLogon  = 5
	WTSSessionLogoff = 6
	WTSSessionLock   = 7
	WTSSessionUnlock = 8
)

// Event is a power or session notification delivered to OnEvent.
type Event struct {
	Power bool   // true for a power event, false for a session change
	Code  uint32 // PBT_* or WTS_* value
}

// Stop budgets. The control handler returns at once and the work happens on
// another goroutine (the SCM allows 30 s); the figures are how long the
// supervisor gets to cancel restic before the service reports stopped anyway.
const (
	StopBudget        = 25 * time.Second
	ShutdownBudget    = 15 * time.Second // Windows leaves about 20 s at shutdown
	PreShutdownBudget = 120 * time.Second
)

// Host describes the agent as the service host runs it.
type Host struct {
	// Run runs the agent until ctx is cancelled and returns after it has shut
	// down (supervisor cancelled, journal flushed).
	Run func(ctx context.Context) error
	// StopCause is the cancel cause passed to ctx on stop, shutdown and
	// pre-shutdown (supervisor.ErrServiceStop), so a running restic is recorded
	// as "interrupted".
	StopCause error
	// OnEvent receives power and session notifications; nil ignores them. It
	// must not block.
	OnEvent func(Event)
	// OnStop is called with "stop", "shutdown" or "preshutdown" when a stop
	// request arrives (the stop marker of the journal is written here).
	OnStop func(kind string)
}

// ensureSource makes the Event Log source exist: a source that is already
// registered is success (FIX19: a second "install" or "install --repair-acl"
// on an installed machine must not exit 4). exists reports whether the source's
// registry key is present; install registers it. The decision is on the key,
// never on error text: when install fails, the key is checked once more (a
// concurrent install may have created it) and only a still-absent key is an
// error. exists failing is treated as "not known to exist": install then runs
// and its error, if any, is the one reported.
func ensureSource(exists func() (bool, error), install func() error) error {
	if ok, err := exists(); err == nil && ok {
		return nil
	}
	err := install()
	if err == nil {
		return nil
	}
	if ok, qerr := exists(); qerr == nil && ok {
		return nil
	}
	return err
}

// Settings is the part of the service's SCM configuration the agent depends
// on beyond its existence: what Install applies and EnsureConfig checks.
type Settings struct {
	AutoStart        bool            // start type automatic
	DelayedAutoStart bool            // ... delayed
	Restarts         []time.Duration // restart-on-failure delays, in order (every action is a restart)
	ResetPeriod      time.Duration   // the failure counter resets after this
	PreShutdown      time.Duration   // pre-shutdown budget granted by the SCM
}

// Wanted is the configuration Install applies: automatic delayed start,
// restart after 1, 5 and 15 minutes with the counter reset after a day, and
// PreShutdownBudget.
func Wanted() Settings {
	return Settings{
		AutoStart: true, DelayedAutoStart: true,
		Restarts:    []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute},
		ResetPeriod: 24 * time.Hour,
		PreShutdown: PreShutdownBudget,
	}
}

// Drift names, as fixed phrases, every setting in cur that differs from
// Wanted (nothing when they match). EnsureConfig re-applies those.
func Drift(cur Settings) []string {
	w := Wanted()
	var d []string
	if cur.AutoStart != w.AutoStart || cur.DelayedAutoStart != w.DelayedAutoStart {
		d = append(d, "delayed automatic start")
	}
	same := len(cur.Restarts) == len(w.Restarts)
	for i := 0; same && i < len(w.Restarts); i++ {
		same = cur.Restarts[i] == w.Restarts[i]
	}
	if !same || cur.ResetPeriod != w.ResetPeriod {
		d = append(d, "restart on failure after 1, 5 and 15 minutes")
	}
	if cur.PreShutdown != w.PreShutdown {
		d = append(d, "the 120 s pre-shutdown budget")
	}
	return d
}

// DriftReport turns the outcome of EnsureConfig into the warning the
// service writes to agent.log and the Event Log: empty when there was nothing
// to repair. The text is fixed phrases, no path.
func DriftReport(repaired []string, err error) string {
	switch {
	case err != nil && len(repaired) > 0:
		return "the service's configuration had lost " + strings.Join(repaired, ", ") + " and could not be repaired: " + err.Error() + " (run \"cryo-agent install\" from an elevated prompt)"
	case err != nil:
		return "the service's configuration could not be checked: " + err.Error()
	case len(repaired) > 0:
		return "the service's configuration had lost " + strings.Join(repaired, ", ") + " (an MSI rollback or a manual change); it was re-applied and takes effect from the next start"
	}
	return ""
}

// StopBudgetFor returns the budget for a stop kind.
func StopBudgetFor(kind string) time.Duration {
	switch kind {
	case "shutdown":
		return ShutdownBudget
	case "preshutdown":
		return PreShutdownBudget
	}
	return StopBudget
}
