package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
	"github.com/c2tech-sys/cryobackup/internal/agent/logs"
	"github.com/c2tech-sys/cryobackup/internal/agent/service"
)

// ErrStateUntrusted is returned when a layout directory's owner or DACL is
// not config.SDDLStateDir (architecture §6.5), or when an earlier elevated
// start recorded the untrusted-repair-required verdict (config.Verdict:
// HKLM\SOFTWARE\CryoBackup\Agent on Windows): agent.json, the journal and the
// cache in it may have been written by someone other than SYSTEM or an
// administrator, so the agent does not use them.
var ErrStateUntrusted = errors.New("agent: the state directory's owner or ACL is not SYSTEM + Administrators; its contents are not trusted")

// Event Log ids of the state-directory checks.
const (
	eventStateRefused     = 1004 // error: refused to start on an unprotected or untrusted state directory
	eventStateInsecure    = 1005 // warning: running with --insecure-unprotected-state
	eventStateTampered    = 1006 // error: a layout directory's descriptor changed while the agent ran
	eventStateQuarantined = 1007 // warning: --repair-acl moved an untrusted state directory aside and created a fresh one
	// 1008 is retired (ticket P1-FIX10): it reported a planted file marker; the
	// verdict now lives where an unprivileged account cannot write it. Not reused.
	lineInsecureFlag = "INSECURE: --insecure-unprotected-state is set: the state directory is not verified as SYSTEM + Administrators only; secrets protected by machine-scope DPAPI are readable by any local process that can read the files. Lab use only."
	// lineRefusedUnprotected ends a refusal of a process that cannot restrict.
	lineRefusedUnprotected = "refusing the state directory (start elevated: run \"cryo-agent install\" or \"enroll\" from an elevated prompt; in a lab only, pass --insecure-unprotected-state)"
	// lineRefusedUntrusted ends a refusal of a state directory whose contents
	// are not trusted.
	lineRefusedUntrusted = "refusing the state directory: its contents are not trusted and are never adopted; from an elevated prompt, --repair-acl (install, enroll or run) moves it aside whole to <name>.quarantine-<UTC stamp>-<8 random hex> next to it, creates a fresh one and clears the untrusted verdict, after which the device must be enrolled again"
)

// Hooks of PrepareState and the running agent's re-check, replaced by this
// package's tests only.
var (
	// stateElevated: can this process restrict and verify (config.CanRestrict)?
	stateElevated = config.CanRestrict
	// verifyStateDir reads one directory's owner and DACL back.
	verifyStateDir = config.VerifyDir
	// restrictStateDir sets the owner and DACL on one directory (Ensure's hook).
	restrictStateDir = config.RestrictDir
	// quarantineState moves an untrusted root aside.
	quarantineState = config.QuarantineRoot
	// readVerdict, writeVerdict and clearVerdict keep the
	// untrusted-repair-required verdict (config.ReadVerdict: HKLM on Windows,
	// which only an elevated process can write).
	readVerdict  = config.ReadVerdict
	writeVerdict = config.WriteVerdict
	clearVerdict = config.ClearVerdict
	// stateNow stamps quarantines and verdicts.
	stateNow = time.Now
)

// StateOptions configures PrepareState.
type StateOptions struct {
	// Strict refuses every unprotected or unverified outcome, whatever
	// AllowUnprotected says: the service (ServiceMode) and "install".
	Strict bool
	// AllowUnprotected is the lab flag --insecure-unprotected-state: outside
	// Strict mode, a process that cannot restrict (unelevated, or no ACLs on
	// the platform) uses an unverified state directory with a loud warning
	// instead of refusing. It waives that inability only: it never adopts a
	// directory another account owns, never overrides the untrusted verdict and
	// never a verdict an elevated process could reach.
	AllowUnprotected bool
	// Repair is --repair-acl (elevated only): when the existing state
	// directory is untrusted, move it aside whole (config.QuarantineRoot) and
	// create a fresh one; audited in the fresh logs\agent.log and Event Log
	// 1007. The device is then not enrolled.
	Repair bool
	// EventLog receives the refusal and warning lines (service mode).
	EventLog *service.EventLog

	// sink is an Event Log writer that reports failures ("install" and
	// "enroll", FIX20); a failed write lands in StateReport.EventLogErr.
	// EventLog is used the same way when sink is nil.
	sink eventSink

	// events replaces EventLog and sink (tests).
	events func(level string, id uint32, msg string)
}

// StateReport is the outcome of PrepareState.
type StateReport struct {
	// Applied: owner SYSTEM and the protected DACL are on every layout directory.
	Applied bool
	// Verified: every layout directory reads back as config.SDDLStateDir and,
	// if the root existed before this start, already did so before anything
	// was applied. Only a verified state directory is re-checked while the
	// agent runs.
	Verified bool
	// Quarantined is where --repair-acl moved the untrusted state directory
	// (empty when nothing was moved). agent.json and the secrets went with it:
	// the device must be enrolled again.
	Quarantined string
	// Warning is non-empty when the state directory is used although it is not
	// verified (lab flag, outside Strict mode). It names no path.
	Warning string
	// EventLogErr is non-empty when an Event Log line could not be written, or
	// the Event Log could not be opened for a --repair-acl from "install" or
	// "enroll"; those commands print it to stderr. It names no path.
	EventLogErr string
}

// stateFacts are the observations decideState rules on.
type stateFacts struct {
	existed bool  // the root existed before this start (and was not quarantined)
	pre     error // verifyLayout before anything was applied (existed only)
	applied bool  // Ensure applied the owner and DACL everywhere
	post    error // verifyLayout of all ten directories after Ensure (applied only)
}

// decideState is the trust rule of the state directory (pure, unit-tested):
//
//   - nothing applied (not elevated, no ACLs on the platform), or applied but
//     not every layout directory reading back as SDDLStateDir: unprotected;
//   - a root that existed and did not already verify before this start: its
//     contents may have been written while it was open, so they are untrusted
//     (an elevated process refuses this before it applies anything, see
//     PrepareState; the rule here is the backstop);
//   - the lab flag, outside Strict mode, turns a problem into a warning only
//     when nothing could be applied: it waives the inability to restrict,
//     nothing else; every other problem refuses.
//
// The error wraps ErrACLRefused when nothing could be applied and
// ErrStateUntrusted otherwise.
func decideState(f stateFacts, strict, allowUnprotected bool) (verified bool, problem string, err error) {
	var problems []string
	switch {
	case !f.applied:
		problems = append(problems, "the SYSTEM + Administrators owner and DACL could not be applied (not elevated, or no ACLs on this platform)")
	case f.post != nil:
		problems = append(problems, "the owner and DACL were applied but do not read back as expected ("+f.post.Error()+")")
	}
	if f.existed && f.pre != nil {
		problems = append(problems, "the existing state directory was not restricted to SYSTEM + Administrators before this start, so its contents are not trusted")
	}
	if len(problems) == 0 {
		return true, "", nil
	}
	problem = strings.Join(problems, "; ")
	if !f.applied {
		if !strict && allowUnprotected {
			return false, problem, nil
		}
		return false, problem, fmt.Errorf("%w: %s; %s", ErrACLRefused, problem, lineRefusedUnprotected)
	}
	return false, problem, fmt.Errorf("%w: %s; %s", ErrStateUntrusted, problem, lineRefusedUntrusted)
}

// verifyLayout reads every layout directory back with verify: the root, then
// the nine children. One failing directory fails the whole state directory.
// allowMissing skips a child that does not exist yet (the check before Ensure
// creates it); the root must exist. The error names the directory relative to
// the root, never the state directory's path.
func verifyLayout(l config.Layout, verify func(string) error, allowMissing bool) error {
	for i, d := range l.All() {
		err := verify(d)
		if err == nil || (allowMissing && i > 0 && errors.Is(err, fs.ErrNotExist)) {
			continue
		}
		name := "the state root"
		if i > 0 {
			name = l.Rel(d)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// PrepareState creates and secures the state directory and decides whether its
// contents can be trusted (architecture §6.5, tickets P1-FIX5, P1-FIX9 and
// P1-FIX10). New, Enroll and "cryo-agent install" call it first; a non-nil
// error is a refusal (written to the Event Log when one is given). In order:
//
//  1. Read only: a root that is a reparse point is refused; an existing root
//     and its existing children are read back (verifyLayout: an owner that
//     cannot be read is untrusted, never trusted); this state directory's
//     untrusted verdict is read (config.ReadVerdict: HKLM\SOFTWARE\CryoBackup\Agent
//     value UntrustedRepairRequired-<config.VerdictID> on Windows, one per
//     state directory, readable without privilege and writable only by an
//     elevated process, so it cannot be planted). A verdict that cannot be
//     read refuses.
//  2. Elevated, and the root untrusted (any layout directory fails, or the
//     verdict exists): the verdict is recorded (the first one stands) and,
//     without Repair, the start is refused BEFORE any descriptor is touched,
//     so a later start cannot turn this refusal into a verified start. With
//     Repair the whole tree is moved aside (config.QuarantineRoot) and a
//     fresh one is created below; nothing in the old tree is read or
//     re-ACLed.
//  3. Not elevated: the verdict refuses (lab flag or not); otherwise
//     decideState rules (refuse, or warn under the lab flag) before anything
//     is created.
//  4. Ensure creates what is missing (with the descriptor already set when
//     elevated) and applies the owner and DACL; all ten layout directories
//     are read back and decideState rules again.
//  5. Only after a successful quarantine and a verified fresh root is the
//     verdict cleared; an audit line naming the quarantine goes to the fresh
//     logs\agent.log and to Event Log id 1007. agent.json and the secrets are
//     in the quarantine: the device is not enrolled until an elevated
//     "cryo-agent enroll" writes them again.
func PrepareState(stateDir string, o StateOptions) (StateReport, error) {
	var rep StateReport
	if stateDir == "" {
		stateDir = config.DefaultStateDir()
	}
	layout := config.Layout{Root: stateDir}
	event := o.events
	if event == nil {
		write := o.sink
		if write == nil && o.EventLog != nil {
			write = o.EventLog.Write
		}
		if write != nil {
			event = func(level string, id uint32, msg string) {
				if err := write(level, id, msg); err != nil && rep.EventLogErr == "" {
					rep.EventLogErr = eventLogFailure(id, err)
				}
			}
		}
	}
	if event == nil {
		event = func(string, uint32, string) {}
	}
	refuse := func(err error) (StateReport, error) {
		event("error", eventStateRefused, "state directory refused: "+err.Error())
		return rep, err
	}
	secure := stateElevated()

	// 1. Read only.
	var f stateFacts
	switch _, err := os.Lstat(layout.Root); {
	case err == nil:
		f.existed = true
		f.pre = verifyLayout(layout, verifyStateDir, true)
		if errors.Is(f.pre, config.ErrReparsePoint) {
			return refuse(fmt.Errorf("%w: %w", ErrACLRefused, f.pre))
		}
	case !errors.Is(err, fs.ErrNotExist):
		return refuse(fmt.Errorf("%w: the state directory cannot be inspected", ErrACLRefused))
	}
	// The verdict: only an elevated process can have written it, so it is
	// never ignored; one that cannot be read is not a "no".
	verdict, marked, verr := readVerdict(layout.Root)
	if verr != nil {
		return refuse(fmt.Errorf("%w: the untrusted-repair-required verdict (%s) cannot be read, so the state directory is not trusted: %v", ErrStateUntrusted, config.VerdictSource, verr))
	}
	earlier := ""
	if marked {
		earlier = "an earlier elevated start found this state directory untrusted (" + verdict.Describe() + ")"
	}

	if o.Repair && !secure {
		return refuse(fmt.Errorf("%w: --repair-acl needs an elevated prompt (it moves an untrusted state directory aside and creates a fresh one with the SYSTEM + Administrators descriptor)", ErrACLRefused))
	}
	if marked && !secure {
		return refuse(fmt.Errorf("%w: %s; %s", ErrStateUntrusted, earlier, lineRefusedUntrusted))
	}

	// 2. Elevated and untrusted: record the verdict, then refuse before
	// touching anything, or quarantine.
	var quarantine config.Quarantine
	failedPre := f.existed && f.pre != nil
	repairing := secure && (marked || failedPre)
	reason := earlier // what a new verdict and the audit line say
	if failedPre {
		reason = f.pre.Error()
	}
	if repairing {
		var noteErr error
		if !marked {
			noteErr = writeVerdict(layout.Root, config.Verdict{Since: stateNow().UTC(), Reason: reason})
		}
		var untrusted error
		if failedPre {
			untrusted = fmt.Errorf("%w: %w", ErrStateUntrusted, f.pre)
			if marked {
				untrusted = fmt.Errorf("%w; %s", untrusted, earlier)
			}
		} else {
			untrusted = fmt.Errorf("%w: %s", ErrStateUntrusted, earlier)
		}
		if noteErr != nil {
			untrusted = fmt.Errorf("%w (and the untrusted verdict could not be recorded: %v)", untrusted, noteErr)
		}
		if !o.Repair {
			return refuse(fmt.Errorf("%w; %s", untrusted, lineRefusedUntrusted))
		}
		if f.existed {
			q, err := quarantineState(layout.Root, stateNow())
			if err != nil {
				return refuse(fmt.Errorf("%w: --repair-acl could not quarantine the untrusted state directory, so nothing in it was used or changed: %w (%v)", ErrStateUntrusted, err, untrusted))
			}
			quarantine, rep.Quarantined = q, q.Path
			f = stateFacts{} // the old root is gone; a fresh one is created below
		}
	}

	// 3. Not elevated: decide before anything is created.
	if !secure {
		if _, _, err := decideState(f, o.Strict, o.AllowUnprotected); err != nil {
			return refuse(err)
		}
	}

	// 4. Create what is missing, apply, read every layout directory back.
	res, err := layout.Ensure(config.EnsureOptions{Restrict: restrictStateDir})
	if err != nil {
		return refuse(fmt.Errorf("%w: %w", ErrACLRefused, err))
	}
	rep.Applied, f.applied = res.Applied, res.Applied
	if res.Applied {
		f.post = verifyLayout(layout, verifyStateDir, false)
	}
	verified, problem, err := decideState(f, o.Strict, o.AllowUnprotected)
	if err != nil {
		return refuse(err)
	}
	rep.Warning = problem

	// 5. Only a repair that produced a verified fresh state directory clears
	// the verdict (and the start is verified only once it is cleared).
	if repairing {
		if err := clearVerdict(layout.Root); err != nil {
			return refuse(fmt.Errorf("%w: the fresh state directory verifies, but the untrusted verdict (%s) could not be cleared (%v); every start refuses until an elevated --repair-acl clears it", ErrStateUntrusted, config.VerdictSource, err))
		}
		lines := []string{"repair-acl: the untrusted verdict was cleared; there was no state directory to quarantine; a fresh state directory was created with owner SYSTEM and the protected SYSTEM + Administrators DACL"}
		if rep.Quarantined != "" {
			lines = []string{"repair-acl: the untrusted state directory (" + reason + ") was moved aside, whole and unread, to " + rep.Quarantined +
				"; a fresh state directory was created with owner SYSTEM and the protected SYSTEM + Administrators DACL on every layout directory; this device is not enrolled until an elevated \"cryo-agent enroll\" writes agent.json and the secrets again"}
			if quarantine.SecureErr != nil {
				lines = append(lines, "repair-acl: the quarantine's owner (Administrators) and admin-only DACL could not be set ("+quarantine.SecureErr.Error()+"); restrict or delete it by hand")
			}
			event("warning", eventStateQuarantined, "the untrusted state directory was quarantined to "+rep.Quarantined+" (--repair-acl) and a fresh one was created; enrol this device again from an elevated prompt")
		} else {
			event("warning", eventStateQuarantined, "the untrusted verdict was cleared (--repair-acl) and a fresh state directory was created; enrol this device from an elevated prompt")
		}
		auditLocal(layout, lines)
	}
	rep.Verified = verified
	if !verified {
		event("warning", eventStateInsecure, "running on an unverified state directory (--insecure-unprotected-state): "+problem)
	}
	return rep, nil
}

// auditLocal appends lines to logs\agent.log (device-local). Quarantines
// happen before the agent's own logger exists, and from "install" and
// "enroll", which have none.
func auditLocal(layout config.Layout, lines []string) {
	rot, err := logs.OpenRotating(layout.AgentLog(), logs.DefaultMaxBytes, logs.DefaultKeep)
	if err != nil {
		return
	}
	defer rot.Close()
	l := logs.NewLogger(rot, slog.LevelInfo)
	for _, s := range lines {
		l.Warn(s)
	}
}

// verifyState reads all ten layout directories back (Options.verifyState in
// tests).
func (a *Agent) verifyState() error {
	verify := verifyStateDir
	if a.o.verifyState != nil {
		verify = a.o.verifyState
	}
	return verifyLayout(a.layout, verify, false)
}

// recheckState re-verifies every layout directory of a state directory that
// was verified at start; a change to any of them while the agent runs makes it
// stop scheduling and report last_error_class = permission (the uplink keeps
// running from the in-memory configuration loaded while the directory was
// trusted). The verdict holds until a restart, which verifies again and, in
// service mode, refuses.
func (a *Agent) recheckState() {
	a.mu.Lock()
	check := a.stateVerified && a.stateErr == nil
	a.mu.Unlock()
	if !check {
		return
	}
	if err := a.verifyState(); err != nil {
		a.mu.Lock()
		a.stateErr = err
		a.mu.Unlock()
		a.log.Error("a state directory's owner or DACL changed while the agent ran; backups stop until a restart verifies it again", "err", err.Error())
		a.eventLog("error", eventStateTampered, "a state directory's owner or DACL changed ("+err.Error()+"); backups are stopped")
	}
}
