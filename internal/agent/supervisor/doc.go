// Package supervisor runs the pinned restic.exe for the agent (architecture
// §6.1 "restic supervisor", §6.3, §6.4, engine sheet §5.4).
//
// Run takes a Spec (binary path, expected SHA-256, an internal/restic Command
// and Env, a run-log path, limits) and does the following:
//
//  1. Verify, once per process and again if the file changes: the binary's
//     SHA-256 against the pinned value (ErrHashMismatch; ErrNotPinned when
//     there is none and unpinned mode is off) and "restic version --json"
//     against PinnedVersion (ErrVersionMismatch). A refused binary is never run.
//  2. Build the child's environment with restic.Env.Environ, the only way a
//     secret reaches it (DH3).
//  3. Start the child (CREATE_NEW_PROCESS_GROUP; CREATE_NO_WINDOW in service
//     mode) and, on Windows, assign it to a job object with
//     JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE and an optional process memory cap, so
//     a crashed agent cannot leave restic running. With a cap, the job's
//     completion port reports JOB_OBJECT_MSG_PROCESS_MEMORY_LIMIT; a child
//     that hit the cap gets memoryKillGrace (5 s) to exit and is then
//     terminated (a Go runtime that cannot commit memory prints "fatal error:
//     out of memory" and may hang instead of exiting), and the run maps to
//     failed/memory (Result.MemoryLimitHit). A cancel during that grace ends
//     the child at once; the outcome stays failed/memory and the cancel is
//     recorded (Result.Cancel, CancelMode terminated, a run log note). The
//     runtime's traceback is not counted: a memory death has one memory row
//     in Result.Counts. The port reader is stopped before the port is closed.
//     PhysicalMemoryMiB serves the agent's auto cap.
//  4. Read stdout and stderr concurrently. Every line is teed verbatim (plus a
//     newline) to the run log, which is path-bearing and local-only (DH15), and
//     decoded through restic.NewDecoder(...).ForKind(kind): the path-free parse
//     boundary. Only counts, classes and the summary leave this package.
//     Plain-text lines that classify (a missing root) are counted like errors.
//     restic's per-volume VSS fallback line on stderr ("failed to create
//     snapshot for [c:\]") is kept out of the decoder: it counts as one vss
//     error, not a failed file, and its drive letter goes to
//     Result.VSSFallbackVolumes (restic.VSSFallbackVolume).
//  5. Sample BackupStatus into a Progress struct every 5 s (Supervisor.Progress,
//     Spec.OnProgress), with a stalled flag when no byte moved for StallAfter.
//  6. Enforce MaxRuntime and honour context cancellation through one path:
//     interrupt, wait CancelGrace (10 s), then terminate. The context cause
//     decides the classification: ErrServiceStop gives "interrupted", anything
//     else "cancelled"; a max-runtime cancel sets Result.LongRun.
//  7. Map the exit with restic.Map and the evidence (summary seen, cancel
//     cause, stderr class). With a cancel pending, the exit becomes 130 only
//     when the supervisor stopped the child: it terminated it (job object or
//     Kill), the child exited 130 after honouring the interrupt, or the
//     interrupt itself killed it (an NTSTATUS above 255, or a signal). A child
//     that exits with a real code (1 and 2 included) on its own while a cancel
//     is pending keeps restic.Map's result for that code; the pending cancel is
//     written to the run log as a "cryo_supervisor_note" line and kept in
//     Result.Cancel.
//
// Document commands (snapshots, version, cat config, stats, find) have their
// stdout captured into Result.Stdout (64 MiB cap) instead of decoded as a
// stream.
//
// # Cancellation (L-CANCEL-1, still an open lab test)
//
// The intended graceful stop is CTRL_BREAK_EVENT to the child's process group
// so restic exits 130 and removes its lock (§6.6). It reaches the child only
// when agent and child share a console, which is the case in console mode
// ("cryo-agent run") and not in service mode, where the child is started with
// CREATE_NO_WINDOW and the call fails. A failed interrupt goes straight to
// TerminateJobObject; an interrupt that is ignored goes there after the grace
// period. The leftover lock goes stale after 30 minutes and is harmless
// (engine §2.1). Result.CancelMode records which way it went ("graceful" or
// "terminated") so the lab test can read it from the logs. Off Windows the
// interrupt is SIGINT and the hard stop is Kill.
package supervisor
