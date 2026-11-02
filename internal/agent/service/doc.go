// Package service is the Windows service host of the agent (architecture §6.1
// "service host", §6.6; engine sheet §5.1, §5.3, §5.5).
//
// The agent runs as LocalSystem under the service control manager through
// golang.org/x/sys/windows/svc directly (kardianos/service would drop power and
// session events). Execute accepts Stop, Shutdown, PreShutdown, PowerEvent and
// SessionChange. It never blocks the control
// handler: the agent runs on its own goroutine; on Stop, Shutdown or
// PreShutdown the handler cancels the agent's context with the service-stop
// cause (a running restic is recorded as "interrupted"), reports StopPending
// with a checkpoint every two seconds and waits for the agent for 25 s (stop),
// 15 s (shutdown) or 120 s (pre-shutdown), which is also the pre-shutdown
// timeout Install registers. Power and session notifications are handed to
// Host.OnEvent (the run path wires it to Agent.HandleEvent through an
// EventRelay): a resume re-plans the schedule (run-on-wake, M1); a suspend and
// every session change is recorded at debug level and ignored. OnEvent runs on
// the control goroutine and must not block, or Stop would wait behind it.
//
// Install registers the service (LocalSystem, delayed automatic start, restart
// on failure after 1, 5 and 15 minutes, description) and the Event Log source
// "CryoBackup"; Uninstall removes both. Both need an elevated process. A source
// that already exists is success (EnsureEventSource decides on the registry
// key), so a second install, or "install --repair-acl" on an installed
// machine, works; "install" and "enroll" call EnsureEventSource before a
// --repair-acl quarantine so that Event Log 1007 has a source. State
// reads the service state with query rights only, for "cryo-agent status".
// IsService selects service or console mode; console mode ("cryo-agent run")
// is what lab work uses and needs no privileges.
//
// EventLog mirrors Info, Warning and Error lines to the Event Log source so an
// RMM can watch it; the messages are fixed phrases and classes, never paths.
// The source's message file is cryo-agent.exe, whose message table
// (deploy/windows/eventlog) has the text "%1" for every id in EventIDs, so
// Event Viewer renders each line (L-EVT-1). OpenEventLogFresh retries the
// first open after a registration briefly (FIX6).
// Off Windows every function reports ErrUnsupported or does nothing, so Linux
// builds compile.
//
// Not exercised in the development session (not elevated): Install, Uninstall
// and Run under the SCM; they are P1-T8's VM checks. Execute is tested by
// driving it with channels.
package service
