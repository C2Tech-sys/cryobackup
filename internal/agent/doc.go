// Package agent is the CryoBackup endpoint: it wires configuration, secrets,
// journal, uplink, scheduler, restic supervisor and command executor into the
// agent that runs on a Windows computer (architecture §6; ticket P1-T7, M0
// scope). The packages below it do the work; this one is the boot sequence, the
// loops and the backup run.
//
//	config      state directory layout, agent.json, the ACL hook
//	secrets     DPAPI store: device token, gateway password, repository password
//	journal     SQLite: runs, outbox, kv, command log
//	scheduler   Plan (pure), RetryDelay, Facts from the journal, the 60 s Loop
//	supervisor  restic.exe under a job object, JSON stream decoding, run log
//	uplink      HTTPS client for internal/api, backoff, Retry-After, clock offset
//	commands    backup_now, cancel, refresh_policy, check_in; the rest fail loudly
//	service     svc.Handler, install and uninstall, Event Log
//	diag        "cryo-agent status"
//	logs        rotating agent.log, run-log retention
//
// # What runs where
//
// cmd/cryo-agent is one binary. "run" is the Windows service when the service
// control manager started it and a foreground process otherwise (console mode,
// for lab work, needs no privileges). restic.exe is a child of the agent, never
// a service of its own, and the agent is its only caller. The agent opens every
// connection: to the console's agent listener (uplink) and, through restic, to
// the gateway. It exposes no listener. The endpoint holds no storage
// credential; it holds its device token, its gateway login and the repository
// password, protected by DPAPI.
//
// # Boot sequence (New, architecture §6.1)
//
//  1. PrepareState (see "State trust model" below): read the existing layout
//     back before anything is changed; refuse an untrusted one (or, with an
//     elevated --repair-acl, quarantine it and create a fresh one); create
//     what is missing with config.SDDLStateDir already set; read all ten
//     layout directories back. In service mode anything short of "verified"
//     is a refusal written to the Event Log (id 1004): agent.json, the
//     journal and the cache are not trusted. In console mode the lab flag
//     --insecure-unprotected-state turns the inability of an unelevated
//     process to restrict into a warning logged on every start; without the
//     flag it is a refusal.
//  2. Open the device-local log (logs\agent.log).
//  3. Load agent.json; refuse when the device is not enrolled.
//  4. Load the secrets into memory (device token, gateway password required;
//     a missing repository key is reported, see below).
//  5. Open the journal, close runs a previous process left open ("interrupted"
//     if it recorded a stop, otherwise "abandoned"), empty tmp, prune run logs
//     older than 30 days.
//  6. Verify the restic binary: SHA-256 against the build's pin
//     (PinnedResticSHA256, or --restic-sha256, or --insecure-unpinned-restic in
//     the lab) and "restic version --json" against the pinned 0.19.1. Failure
//     does not stop the agent: it keeps its uplink alive, reports
//     last_error_class = guard_refused and does not schedule.
//  7. Load the last-known-good policy from the journal; with none, the agent
//     does not schedule until the first pull succeeds.
//  8. Build the uplink; write an Event Log line (Windows). If the console is
//     unreachable at boot the agent runs from the last-known-good policy.
//
// Run then starts five loops: heartbeat (60 s plus or minus jitter, never
// closer than 20 s), policy pull (ETag, on the heartbeat's etag hint), run
// upload (the outbox), command long poll (one at a time, 30 s) and the
// scheduler (60 s tick and triggers).
//
// # The run state machine as implemented (architecture §6.2)
//
// The scheduler calls policy.Due with device-local time and applies
// policy.Jitter once per slot. A due run is assembled from the effective policy
// (roots and exclude file from policy.Roots and policy.ExcludeFile with the
// discovered environment; flags from the policy), the run row is written to the
// journal and queued as an open record BEFORE restic starts, then the child runs
// under the supervisor. When it ends, restic.Map turns the exit into the wire
// outcome and the closed record replaces the open one in the outbox.
//
//	GATED -> STARTING     implemented (window, startup delay, jitter, backoff gate)
//	start lease (M1)      a scheduled run asks POST start-lease first (lease.go,
//	                      ADR 0005): granted starts; refused journals
//	                      deferred(fleet_cap), no attempt, waits retry_after_s
//	                      plus up to 20 %; unreachable, timeout (10 s), 5xx,
//	                      429 or no such route starts anyway (stats.lease
//	                      bypassed, warn); other 4xx does not start; backup_now
//	                      does not ask (not_required)
//	STARTING -> RUNNING   implemented (row written first; open record queued)
//	RUNNING -> success, partial, deferred(lock), deferred(gateway), failed,
//	           cancelled, interrupted      implemented through restic.Map
//	max_runtime exceeded  cancelled with stats.long_run
//	gateway watchdog      no progress and no gateway (TCP probe) for
//	                      throttle.gateway_down_grace_minutes (15): restic is
//	                      stopped and the run journaled deferred(gateway),
//	                      class network; heartbeat stalled meanwhile
//	                      (gateway_watchdog.go, M1-FIX4)
//	journal open on start abandoned or interrupted
//	DEFERRED -> GATED     the backoff is recomputed from the journal on every plan
//	                      (5 min x 2^n capped at 60 min, plus or minus 20 %; lock
//	                      deferrals 2 to 15 min); a cancelled, interrupted or
//	                      abandoned run is retried the same way
//	GATED by conditions   NOT in M0 (battery, metered, idle, Wi-Fi, VPN: M8)
//	resume                re-plans at once (Agent.HandleEvent); a run it finds
//	                      due is journaled "wake"; gated on the policy's
//	                      run_on_wake; resumes within 30 s give one re-plan;
//	                      the run waits for a heartbeat the console answers,
//	                      up to 2 min, before it starts, then re-checks pause,
//	                      hold and window on the policy pulled anew
//	                      (resume_wait.go)
//	suspend, logon,
//	unlock                recorded at debug level and ignored
//
// There is no stored next-run: due-ness is recomputed from the journal's last
// success (a success, or a partial within the policy's tolerance). A skipped
// start (backup_now while a run is active) is journaled as a "skipped" run.
//
// # On-disk layout and ACLs (architecture §6.5)
//
// See package config. Everything under the state directory (default
// C:\ProgramData\CryoBackup, override --state-dir) is local to the device.
// Every layout directory carries owner SYSTEM and a protected DACL granting
// full control to SYSTEM and Administrators only
// (O:SYG:SYD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)), applied by an elevated
// process or SYSTEM with SeRestorePrivilege enabled for the call.
//
// # State trust model (P1-FIX5, P1-FIX9, P1-FIX10)
//
// ProgramData lets any user create a subdirectory first, and a tree that was
// ever writable by someone else may hold an agent.json pointing at their
// console or a planted cache. The rules:
//
//   - Fresh root with the descriptor: a missing root, and every missing
//     layout directory, is created with SDDLStateDir already set
//     (CreateDirectory with SECURITY_ATTRIBUTES); it is never open, not even
//     for a moment.
//   - Verify everywhere: "verified" means all ten layout directories (the
//     root, config, secrets, policy, journal, cache, cache\restic, tmp, logs,
//     logs\runs) read back as SDDLStateDir through no-follow handles. They are
//     read at start before anything is changed, again after Ensure, and
//     before every heartbeat while the agent runs; one failing directory is
//     the same as a failing root: refusal 1004 at a service start, a refusal
//     in console mode, and last_error_class = permission (Event Log 1006,
//     scheduling stopped until a restart) while running. An owner (or a
//     descriptor) that cannot be read, anywhere in the layout, is untrusted
//     when the check is an elevated one, never trusted.
//   - Fail closed, no takeover: when an elevated process finds an existing
//     root untrusted (foreign owner, unreadable owner, open DACL, never
//     restricted, any layout directory failing) it refuses BEFORE any
//     descriptor is modified, and records the untrusted verdict: on Windows
//     the REG_SZ value UntrustedRepairRequired-<id> of
//     HKLM\SOFTWARE\CryoBackup\Agent ("<UTC stamp>|<reason>", path-free;
//     <id> is config.VerdictID, 16 hex digits of the SHA-256 of the
//     lower-cased, cleaned absolute state directory path), which only an
//     elevated process can write or clear and any process can read; nothing
//     is written into ProgramData, so no unprivileged account can plant a
//     verdict (to force a refusal and a re-enrolment) or remove one. There is
//     one verdict per state directory: a refused lab --state-dir never blocks
//     the service's default one. While a directory's verdict exists every
//     start on it refuses
//     (ErrStateUntrusted), elevated or not, lab flag or not, even if the
//     descriptors later read back as expected: a refused start never becomes
//     a verified one. A verdict that cannot be read refuses as well. Off
//     Windows (CI and lab only) the stand-in is the file
//     <name>.untrusted-verdict next to the root, mode 0600, honoured only
//     when root owns it (config.ReadVerdict).
//   - Quarantine instead of repair: an elevated --repair-acl (install, enroll,
//     console-mode run; never the service) moves the whole tree, unread, to
//     <name>.quarantine-<UTC stamp>-<8 random hex> next to it
//     (config.QuarantineRoot: a rename through a no-follow handle on the same
//     volume that never replaces a target, refused if anything in the tree is
//     a reparse point; a name someone already took is retried with a fresh
//     random suffix, up to 5 renames, before ErrQuarantineExists, so the name
//     cannot be squatted ahead of a repair), gives the quarantine the owner
//     Administrators and an admin-only DACL (best effort, logged), creates a
//     fresh root and verifies it, and only then clears the verdict and writes
//     an audit line naming the quarantine to the fresh logs\agent.log and
//     Event Log 1007. Nothing in the old tree is re-ACLed, followed or
//     trusted. The operator reviews and deletes the quarantine.
//   - Re-enrol after quarantine: agent.json and the secrets went with the
//     quarantine, so run, check-in and the service refuse with
//     ErrNotEnrolled until an elevated "cryo-agent enroll" writes them again
//     (a new enrollment, with a new enrollment token).
//   - The lab flag waives only the inability to restrict (an unelevated run):
//     an existing root or layout directory whose owner can be read must still
//     be owned by SYSTEM, Administrators or the user running the agent, the
//     verdict still refuses, and nothing an elevated process could judge is
//     waived. Only on this unprotected lab path is a missing child created
//     with os.Mkdir after os.Lstat (a window that does not exist when the
//     directories are created with their descriptor).
//
// Event Log ids: 1004 refused, 1005 lab flag in use, 1006 tampered while
// running, 1007 quarantined; 1008 once reported a planted file marker (P1-FIX9,
// retired) and now reports a refused device token (see "Refused token").
// "cryo-agent status" shows the verdict
// ("state trust: UNTRUSTED (HKLM verdict since <stamp>: <reason>)") and the
// quarantines next to the state directory (count and newest
// "<stamp>-<suffix>", no path).
//
// # Trust boundary of the secrets
//
// The secrets are protected with machine-scope DPAPI: any process on this
// machine that can read the files can decrypt them. Machine-scope DPAPI is
// therefore only as strong as the directory ACL, which is why a secret is
// never written into a directory that does not read back as SYSTEM +
// Administrators only (secrets.ErrUnprotectedDir), except under the lab flag.
// It keeps the architecture's model (an administrator enrolls, the SYSTEM
// service reads). The stronger design, SYSTEM user-scope DPAPI with the
// service performing the enrollment, is an M1 design note recorded in the
// ledger, not built here.
//
// # What is path-free and what is not (DH15, §8.4)
//
// Path-bearing, local-only, never uploaded and never part of "status": the
// verbatim restic output in logs\runs\<run_id>.jsonl, logs\agent.log (which
// carries ids, classes and counts, not restic's text, but may name local
// files in an error), policy\excludes.txt and roots.txt and the policy mirror.
// Path-free by construction, and checked with api.AssertPathFree before every
// send: the run records (outcome, class, counts, the projection
// {during, class, ext, count} of every error, stats), heartbeats, command
// results. The two documented exemptions (§13.3) are the snapshot list's Paths
// (the configured roots, never file names; the account name is blanked) and the
// restore selection (not part of M0). The journal holds no path and no secret.
// Secrets live in memory, in the DPAPI store and in the restic child's
// environment (restic.Env): never argv, never a log line, never the journal.
// TestNoPathLeavesTheDevice proves the first claim end to end.
//
// # Lab loop
//
//	cryo-agent enroll --server https://console.example:8443 --token-file token.txt --state-dir D:\lab\state --insecure-unprotected-state
//	cryo-agent run --state-dir D:\lab\state --restic C:\tools\restic.exe --restic-sha256 <hex> --insecure-unprotected-state
//	cryo-agent status --state-dir D:\lab\state
//
// "--insecure-tls-for-lab" on enroll allows an http console and skips TLS
// verification (it is stored in agent.json and shown by "status");
// "--insecure-unpinned-restic" runs restic without a pinned hash (the version is
// still checked); "--insecure-unprotected-state" lets an unprivileged run use a
// state directory it cannot restrict (warned about on every start, recorded
// for "status", ignored by the service). None exists in a production install.
// "--repair-acl" (install, enroll, console-mode run; elevated) quarantines an
// untrusted state directory, creates a fresh one and clears the HKLM verdict;
// the device is then enrolled again. The test
// TestLiveAgentLoop (CRYO_RESTIC set to restic 0.19.1) runs the same loop
// against a fake console and a real repository on disk.
//
// # Credentials rotation and enrollment
//
// A pending gateway password in a policy pull replaces gateway.cred at once and
// is acknowledged by the next heartbeat (gateway_secret_acked, DH13). Enrollment
// persists its nonce before the first request, so a retry after a lost response
// reuses it and the console issues a fresh device token (DH17); the response
// that arrives last is the one stored. Enrolling a device that already has a
// device id is refused.
//
// # Refused token (ADR 0018 decision 1)
//
// Three heartbeats in a row answered 401 mean the console refuses the device
// token (a 403, a 429, a 5xx or a transport failure restarts the count). The
// agent then says so once (one ERROR line, Event Log 1008, the journal key
// token_refused_since, a block in "cryo-agent status"), the policy, run upload
// and command loops stop calling the console, and the heartbeat asks once an
// hour (jittered). Backups go on with the last policy; the agent neither stops
// nor re-enrolls, and a run the gateway turns away (deferred, class auth) waits
// for the policy's next slot instead of the run ladder (scheduler.Input.
// AuthDeferralsWait, M3-L10), so a retired device writes one deferred run per
// interval, not one an hour. The next heartbeat the console accepts ends the
// state, deletes the key, logs one INFO line and wakes every loop; the ladder is
// back at the re-plan that follows.
//
// # M0 limits
//
// No power or session events (the handler accepts and ignores them), no
// conditions evaluator, no tray, no self-update, no restore executor, no
// pause/resume/hold/restart_service/update_agent/restore/uninstall (journaled as
// failed, reason not_implemented_m0), no throttle-window restarts (the limits
// in force at start apply for the run), no selection.includes and path_flags (a
// missing root is restic's exit 3), no VSS fallback volume list (restic names no
// volume in its JSON), no resource caps (the policy document has none), no
// journal overflow summary (§6.7) and no SOCKS proxy.
//
// # Snapshot tags
//
// Every backup is tagged cb.customer=<customer_id>, cb.policy=<policy_id>.<version>
// and cb.run=<run_id> (architecture §6.3). The customer id arrives once, in
// the enrollment response, and is stored in agent.json (an enrollment without
// one is refused); the policy id arrives with every policy response and is
// stored with the applied policy. A run without either id is refused with
// guard_refused rather than tagged with a placeholder.
//
// # Open lab test L-CANCEL-1
//
// The graceful stop of restic is CTRL_BREAK_EVENT to the child's process group.
// It reaches the child only when agent and child share a console (console mode,
// where this repository's tests see it work); a service has none, the call
// fails and the supervisor terminates the job object at once, leaving a lock
// that goes stale after 30 minutes. Whether restic removes its lock on the
// graceful path against the real gateway, and what a service sees, is for
// P1-T8's VM. See package supervisor.
//
// This package imports the standard library, the internal packages, and
// golang.org/x/sys/windows (service, event log, DPAPI, job object); the
// journal is modernc.org/sqlite. The shared path-free internal/logx boundary is
// for output that leaves a device and is not needed on the endpoint: the
// device-local log is path-bearing by design.
package agent
