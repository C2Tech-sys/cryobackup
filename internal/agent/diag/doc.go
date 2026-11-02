// Package diag renders the device state for "cryo-agent status" (architecture
// §6.1 "diagnostics").
//
// Status reads agent.json and the journal of the state directory read-only, so
// it works whether or not the service is running and never disturbs it, and
// returns a Report: device id, console, key custody, service state, last
// heartbeat and clock offset, policy version and etag, the newest backup run
// (outcome, error class, deferred reason, attempt), the last success, the
// earliest next start by the schedule (ignoring startup delay and jitter, which
// only the running agent knows), and the depth of the upload queue.
//
// The report is path-free by construction: it holds ids, classes, counts and
// times, never a path, a file name, a user-profile name or a secret, and it
// does not include the state directory. The path-bearing local logs
// (logs\agent.log, logs\runs\) are deliberately not part of any diagnostic
// output (DH15); a technician reads them on the machine.
package diag
