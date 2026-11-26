# ADR 0032 - A running device restore whose device stops reporting is closed as interrupted, and a late report is kept beside it

**Date:** 2026-11-26 - **Status:** Accepted (foreman, 2026-11-26; operator told) - **Decider:** the foreman, for the operator, on the findings of the blind review r1a of tickets P22-G and P22-I (MEDIUM-1); recorded by ticket P22-R2a

## Context

A device restore becomes RUNNING when the agent fetches its selection (ADR 0027), and only the agent's run record (run id =
restore id) closes it. ADR 0027 allows one active device restore per device, and a RUNNING row counts as active with no age
limit (`store.activeDeviceRestore`, the wizard's `activeDeviceRestore`).

The blind review r1a seeded the fixtures, moved the clock 72 hours on and ran the restore-purge job: the status page still showed
a pulsing RUNNING, "Files are being restored.", with no word that the device had gone silent, and the wizard refused every
device restore to that machine as "already queued or running". A machine that dies, or loses its agent, mid-restore would look
alive forever and block every later restore to it: the quiet failure this product exists to prevent.

What already existed: ticket P22-E (fix E3) taught the abandoned-run-sweeper (`store.SweepAbandonedRuns`, run by the jobs
monitor every five minutes) to close a RUNNING device restore as FAILED, class `interrupted`, audited `restore.finish`, in two
cases: 48 hours after the fetch unless the device reports the run active (`no_run_record`), and an agent restart after the fetch
with no run record of the restore 30 minutes after the boot (`agent_restarted`). The review's probe ran only the purge job, not
the sweeper. Two gaps remained:

- **The "active" exemption never applies to a restore.** The heartbeat leaves the run id and progress out while a restore holds
  the restic slot (`internal/agent/loops.go`, `buildHeartbeat`), and a restore takes no start lease. So the 48-hour rule is a
  ceiling counted from the fetch, heartbeats or not, and nothing reacted to silence sooner.
- **A late run record was dropped without a trace.** `closeDeviceRestore` left a closed row alone; the run row was stored, but
  the restore row and the audit trail said nothing of what the agent reported.

The agent's own bounds (`internal/agent`, numbers as built):

| Bound | Value | Where |
| --- | --- | --- |
| Heartbeat cadence | 60 s, plus or minus 10 s | `loops.go` `defaultHeartbeatEvery`, `heartbeatJitter` |
| Retry while the console does not answer | backoff 1 min doubling, clamped at 5 min (the jitter never takes it above) | `uplink/backoff.go` `BackoffMax` |
| Retry when the console sends Retry-After | the console's value, honoured exactly, up to 1 h | `uplink/backoff.go` `Delay` |
| Retry with a refused device token | one probe an hour, plus or minus 20 % (at most 72 min) | `loops.go` `refusedProbeEvery` |
| A restore that stops moving | stopped after 15 min | `restore.go` `restoreIdleBound` |
| Fetch of the selection | retried for at most 30 min | `restore.go` `restoreFetchFor` |
| Waiting behind a running backup | up to `sla.max_run_hours` (12 h by default); heartbeats continue | `restore.go`, policy defaults |
| The console's offline rule | no heartbeat for 2 min | `state/classify.go`; `ui.restoreOfflineAfter` |

## Decision

### 1. The status page says when the device has gone silent

A RUNNING device restore whose device is offline by the console's rule (no heartbeat for 2 minutes, or never heard from) shows a
warning banner, never the plain "Files are being restored.":

> **<host> has not been heard from since <time>.** The outcome is unknown until it reports. If it stays silent, the console
> closes the restore as interrupted at about <time>, and <host> can take another restore.

with a "Last seen" line (age and time, or "Never"). The chip still reads RUNNING, without the pulse, in the warning colour.

### 2. LIMIT: 24 hours of silence closes the restore as interrupted

The sweeper gains a third case, checked first: **`agent_silent`**, nothing from the device (no heartbeat) for
**`store.DeviceRestoreSilenceLimit` = 24 hours**, counted from the later of the fetch and the device's newest heartbeat.

Reasoning: no restore still running on a device that can reach the console goes silent for longer than 72 minutes (the refused-
token probe; a Retry-After of up to 1 hour when the console asks for one; 5 minutes otherwise), because the agent heartbeats
throughout the restore, including the fetch retries and the wait behind a backup. 24 hours is that bound twenty times over, and it is the same day a QUEUED restore waits for its device (ruling
C6), so "a device not heard from for a day" ends both kinds of wait the same way. What 24 hours does not cover, a machine asleep
or cut off from the console while its restic can still reach the gateway, is the case section 4 exists for. It is a named
constant, not a setting: no configuration key fits it today.

The two P22-E cases stay as they were: `agent_restarted`, and `no_run_record`, 48 hours after the fetch
(`store.DeviceRestoreRunningBound`), which for a restore is a ceiling (see Context).

### 2a. No close while the agents reconnect after a console outage (fix F1 of the blind check)

A console that was down heard nothing from anyone, so the silence it measures across its own outage is not the device's: after a
24-hour outage every running restore's newest heartbeat is more than 24 hours old, and the first sweep runs seconds after the
start, while the agents are still waiting to try again. So after an outage the sweep closes no device restore, for any of the three
reasons, until the console has been listening again for **`store.DeviceRestoreReconnectGrace` = 2 hours**: an agent's longest wait
before it tries again is a Retry-After of up to 1 hour (its own backoff is clamped at 5 minutes), and its next heartbeat follows
within 70 seconds; 2 hours is nearly twice that. A heartbeat inside the grace makes the device heard, as always; a device still
silent when the grace ends is closed then, because it has now been silent while the console listened.

The start of the grace is `SweepOptions.ConsoleStartedAt` when a caller sets it. Otherwise the sweep works it out from its own
record: when the previous sweep (the sweeper's `job_state` row, which the job runner writes after every run) finished more than
15 minutes before this process's first sweep, the console or its job runner was not listening, and the grace starts at that first
sweep; the process remembers it. A quick restart (sweeps paused under 15 minutes) starts no grace, since the fleet's heartbeats
are fresh. Left open: a second restart within 15 minutes of a first one that followed an outage does not restart the grace. All
times are the store's clock, so tests drive them.

### 3. How the console closes it

One transaction per row, as P22-E built it: status FAILED, class `interrupted`, the wrapped selection cleared, finish time now,
and the audit row `restore.finish` (actor system, result error) whose details carry the status, the class and the reason word,
never a path. New: the reason is also kept in the row's progress document (`closed_by`), so the status page says what happened,
in the reason's words:

- `agent_silent`: "Nothing came from <host> for 24 hours while the restore was open, so the console closed it."
- `agent_restarted`: "The agent on <host> restarted during the restore and never reported it, so the console closed it."
- `no_run_record`: "<host> sent no result 48 hours after it started the restore, so the console closed it."

The outcome sentence is "FAILED: the device stopped reporting this restore, so the console closed it as interrupted. What it
restored is unknown." A FAILED row is not active, so the device can take another device restore at once.

### 4. The late-report rule

A run record that arrives after the console closed the row is ingested as any run record is (`AgentIngestRuns`: created, or
updated when the stored run was open or "abandoned"). For the restore row:

- **The row is not reopened.** Status, class and finish time stay the console's verdict. A FAILED restore never turns quietly into
  RESTORED; this differs on purpose from the run row, where a late real outcome overwrites "abandoned" (`AgentIngestRuns`
  rule), because an operator may already have acted on the FAILED restore (run it again, told the customer).
- **The report is kept beside it.** The progress document gains `late`: the outcome, the status and class the record would have
  closed the row with, the counts by class and the time it arrived. The audit row `restore.late_report` (actor the device,
  severity warning) carries the same, path-free.
- **The page shows both.** The outcome sentence adds "The device reported it later: restored, N restored, M skipped because they
  already exist." (or "failed (<class>)", "restored with errors ...", "cancelled"); when the late report says files were restored
  (fully or with errors) the banner turns from critical to warning, and a late failure or cancel keeps it critical; a "Late
  report" line gives the time; the chip stays FAILED.
- Only the first closed record is kept: a resend is a duplicate (`AgentIngestRuns`) and changes nothing. An open record changes
  nothing. A row the agent closed itself, and a row the purge job expired or cancelled before the agent fetched it, are left alone
  as before (no `closed_by`).

## Consequences

- A dead or uninstalled agent no longer leaves a restore RUNNING forever: within 24 hours of its last heartbeat (48 hours of the
  fetch at most; plus up to 2 hours after a console outage) the row is FAILED, the device is free, the audit trail says why, and
  the page warned meanwhile.
- A restore that is genuinely still moving at 48 hours is closed by the ceiling while it runs; its real outcome arrives as a late
  report and is shown beside FAILED. The fix for that is a heartbeat that names the running restore (agent work, not done here).
- The restore runbook's status table gains the silent-device line and the console-closed line.

## What is deliberately left out and why

- **No reopen on a late success.** Reopening would hide the console's verdict after the operator may have acted on it.
- **No restore progress in the heartbeat.** It would let the ceiling exempt a moving restore; it is agent and wire work, outside
  ticket P22-R2a.
- **No setting for the limit.** No configuration key fits; a constant with its numbers in the code and here is easier to audit.
