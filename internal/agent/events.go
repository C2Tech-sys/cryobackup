package agent

import (
	"fmt"

	"github.com/c2tech-sys/cryobackup/internal/agent/service"
)

// eventSink writes one Event Log line and reports the failure. The running
// agent drops a failed write (it has its own log); "install" and "enroll" must
// not: a quarantine that cannot be audited is said out loud on stderr (FIX20).
type eventSink func(level string, id uint32, msg string) error

// openEventSink ensures the Event Log source exists (registering it when this
// is the first command to need it) and opens it. Replaced by tests.
var openEventSink = func() (sink eventSink, closeFn func(), err error) {
	if err := service.EnsureEventSource(); err != nil {
		return nil, nil, err
	}
	// The source may have been registered just now: OpenEventLogFresh retries
	// a failed open briefly (FIX6) and returns the last error, which
	// repairSink hands to stderr.
	el, err := service.OpenEventLogFresh()
	if err != nil {
		return nil, nil, err
	}
	return el.Write, el.Close, nil
}

// repairSink prepares the Event Log for a --repair-acl run of "install" or
// "enroll" (FIX20): the same 1007 line "run" writes. It returns a nil sink when
// there is no repair to audit or when this process cannot repair at all (the
// refusal for that comes from PrepareState). If the Event Log cannot be
// opened the repair still proceeds: warn then holds the failure for stderr and
// the sink is nil. closeFn is never nil.
func repairSink(repair bool) (sink eventSink, closeFn func(), warn string) {
	closeFn = func() {}
	if !repair || !stateElevated() {
		return nil, closeFn, ""
	}
	s, c, err := openEventSink()
	if err != nil {
		return nil, closeFn, "event log: the CryoBackup source could not be registered or opened, so the quarantine will not be recorded there (" + err.Error() + ")"
	}
	return s, c, ""
}

// eventLogFailure is the stderr line for an event that could not be written.
func eventLogFailure(id uint32, err error) string {
	return fmt.Sprintf("event log: event %d could not be written (%v)", id, err)
}

// PrepareInstallState is PrepareState as "cryo-agent install" runs it: Strict,
// and with --repair-acl the Event Log source is registered first (it may not
// exist before the first install) so that the quarantine writes Event Log 1007
// exactly as "run" does. A failure to reach the Event Log never stops the
// repair; it comes back in StateReport.EventLogErr for the caller to print.
func PrepareInstallState(stateDir string, repair bool) (StateReport, error) {
	sink, closeFn, warn := repairSink(repair)
	defer closeFn()
	st, err := PrepareState(stateDir, StateOptions{Strict: true, Repair: repair, sink: sink})
	if st.EventLogErr == "" {
		st.EventLogErr = warn
	}
	return st, err
}
