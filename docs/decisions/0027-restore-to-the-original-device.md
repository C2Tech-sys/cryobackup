# ADR 0027 - Restore to the original device: the agent restores with its own login, and the bytes never pass the console

**Date:** 2026-11-24 - **Status:** Accepted (operator, 2026-11-24) - **Decider:** the operator (rulings C1, C2, C5 and C6 of plan P22, 2026-11-24), recorded by the foreman's worker (ticket P22-D0)

## Context

ADR 0026 puts restore in the console first in the pilot slice. Today the only restore is a download of one path, made
by the maintenance worker and streamed through the console (ADR 0008). A restore that puts files back on the machine
they came from has most of its parts already defined, but none of them run:

- **Wire.** `RestoreSelection` (`internal/api/restore.go:14-46`) is the one path-bearing type of the agent protocol.
  It travels only on the device-authenticated `GET /api/v1/agent/restores/{id}`
  (`internal/api/routes.go:61`), and the agent writes it to a SYSTEM-only temporary file and deletes it after the
  restore (`internal/api/restore.go:19-22`). The restore command carries only the request id (`internal/api/commands.go:108-112`).
- **Schema.** Commands may be of type `restore` (`internal/console/migrations/0001_initial.sql:622`). Restore
  requests have the modes `device_original` and `device_path` (`0001_initial.sql:650`), the statuses this ADR needs
  (`0001_initial.sql:657`), a free-text `error_class` (`0001_initial.sql:663`) and a `command_id`
  (`0001_initial.sql:659`). Runs may be of kind `restore` with trigger `manual` and outcome `interrupted`, and carry
  their stats as JSON (`internal/console/migrations/0003_fleet_start_lease.sql:53-62`).
- **Not wired.** The console does not register the agent restore route (`internal/console/agentapi/service.go:117-127`);
  the agent answers a restore command with `not_implemented_m0` (`internal/agent/commands/commands.go:154-157`); an
  operator can queue only four command types (`internal/console/store/commands_m2.go:46`; ADR 0010).
- **The restic builder exists** and never emits `--delete` (`internal/restic/commands.go:32-85`).
- **Cancel by run id works** in the agent (`internal/agent/commands/commands.go:122-130`).
- **The agent's command executor is synchronous.** It runs a command and only then returns the result to post, and a
  command cut short by a restart is run again (`internal/agent/commands/commands.go:58-77`). A long restore cannot run
  inside it, and a restore must never be run twice by accident.
- **Credentials.** The device reads its repository through the gateway with its own login. The gateway runs with
  `--append-only --private-repos`; the flag refuses deletion (except of locks) and overwrite of existing objects and
  confines a login to its own repository (`deploy/rclone/gateway-flags.md:25-26`). A live test proves backup,
  snapshots and unlock through such a gateway, and that a second login is refused on the first login's repository
  (`internal/console/maintain/doc.go:254-261`, `TestLiveAppendOnlyGateway`). No test in the repository proves a
  restore through it.

## Decision

### Where the files go (rulings C1 and C2)

The operator chooses one of three destinations. The first is the default:

1. **Original location, keep what exists.** Files go back where they were. A file that already exists is left alone
   (`--overwrite never`). Nothing on the machine is replaced.
2. **Side folder.** Files go to `<SystemDrive>\CryoBackup-Restore\<restore id>\<drive>\...`, a folder that only
   Administrators and SYSTEM can open (ruling C2).
3. **Original location, replace changed files** (`--overwrite if-changed`). The operator must type the device's host
   name to confirm.

`--delete` is never passed: a restore never removes a file from the machine.

### How a request travels

- **The console** creates the `restore_requests` row and the `restore` command in one transaction. The selection is
  wrapped under the master key, the command's parameters carry only the request id, and the audit row
  `restore.create` carries no path. One active device restore per device; a second answers 409. A restore command is
  created only through this path, never through the operator command queue of ADR 0010.
- **A queued request waits 24 hours** for the device (ruling C6). After that it is EXPIRED.
- **The agent** accepts the command at once and runs the restore in its own goroutine, outside the command executor.
  It fetches the selection with its device token. The console answers it only when the device matches, the mode is a
  device mode, the request is not finished or expired, and the device is neither retired nor purging. The fetch writes
  the audit row `restore.fetch`.
- **The agent runs restic** through the gateway, with its own login and its DPAPI-protected repository password. It
  runs one `restic restore --json` per drive root: `<snapshot>:/<drive letter>` with `--target`, `--overwrite` as the
  destination demands, and `--include-file` naming a file in a SYSTEM-only temporary folder, deleted on every exit.
- **Guards on the device.** The target must be the original location, or a folder under
  `<SystemDrive>\CryoBackup-Restore\<restore id>\`; anything else ends as `guard_refused`. One restic process at a
  time: a restore waits for a running backup. A restore never resumes after the agent restarts; it ends as
  `interrupted`.
- **The run.** The agent journals a run of kind `restore`, trigger `manual`, whose run id is the restore id, so a
  `cancel` names it and a restore runs at most once. Its record goes through the outbox like every run, path-free, with
  three new stats: `files_restored`, `files_skipped`, `bytes_restored`.
- **The outcome.** The console closes the request from the run. A success is `delivered`. Errors with some files
  restored are `delivered` with `error_class` `partial`, shown as RESTORED WITH ERRORS with the counts by class.
  Anything else is `failed` or `cancelled`. No migration is needed.
- **Bytes never pass the console.** The agent reads its own repository through the gateway.

### What both restore modes share

This applies to this ADR and to the download of ADR 0028:

- **One wizard page.** It works without JavaScript and runs in four steps: point in time, browse and select (files
  and folders), destination, review. The selection travels as hidden form fields, capped at 64 KiB of POST body,
  never in a URL or browser storage. Browsing gets an offset, so a folder is read in pages of 500
  (today it stops at 500 and says "truncated", `internal/console/ui/restore.go:149-150`). "Refresh from repository"
  asks the worker for the snapshot list (the worker's `Snapshots` method, unused today,
  `internal/console/maintain/methods.go:285-301`), and the page does this by itself when the cache is empty. Today
  the list comes only from the cache the agent fills (`ui/restore.go:103-111`).
- **Who may restore (ruling C5).** The operator role and a fresh MFA check (the 5-minute window of ADR 0019,
  `internal/console/auth/stepup_adr19.go:35`) for every restore, whichever mode. Browsing writes the audit row
  `restore.browse` once per admin, device, snapshot and hour.
- **A customer-held key is refused** at every layer, device modes included.

A device restore to an agent older than 0.4.0 is refused when the request is created, because that agent cannot run it.

### What the operator sees when it does not work

Worker or storage down, the master key missing, a customer-held key or a device being purged: a banner that says
which. Device offline: "Queued. Last seen ... Starts when online, until <expiry>. For files now, use Download as zip."
A backup running: "Waiting for the running backup". The gateway refuses the login: FAILED, class `auth`. Files kept:
"N restored, M skipped because they already exist". Interrupted: FAILED, "files being written may be incomplete; run
the restore again". Past 24 hours: EXPIRED.

## Consequences

- An operator can restore to a machine that is on, or that comes on within 24 hours, without the files passing the
  console host. ADR 0008 holds.
- The agent ships as 0.4.0. Its path-free test (`TestNoPathLeavesTheDevice`, `internal/agent/leak_test.go:23`) is
  extended to the restore records.
- ADR 0010's list of command types grows by `restore`, created only by the restore service with the 24-hour lifetime
  above.
- A restore run does not count as a backup. The backup freshness queries select `kind = 'backup'`
  (`internal/console/store/facts.go:326-330`), and a test pins that.

## What is deliberately left out and why

- **A target typed by the operator.** The guard is simpler when there are two places a restore may write.
- **`if-newer`, `--verify`, a dry run, search, live progress and a cancel button.** None is needed to put a file back.
  `cancel` by run id already works for the operator's command.
- **The replacement-PC relay** (DH28). It is a separate path; ADR 0008 already says it must stream.
- **A tar download.** The download is a zip (ADR 0028).

## To be proven by spike P22-S1

Each of these is assumed, not known (restic 0.19.1, rclone 1.75.1). If the spike says no, the fallback applies, and
the change is recorded in a new ADR.

| # | Assumed | Fallback if the spike says no |
|---|---|---|
| 1 | A device login can restore through the append-only gateway: restic reads the repository and handles its lock files. | Device modes do not ship in the pilot slice. Restore is the download of ADR 0028. |
| 2 | `<snapshot>:/C` with `--target C:\` restores to the original location; include patterns are relative to that root; existing parent folders keep their timestamps and ACLs. | The original-location modes are withheld. The side folder (ruling C2) is the only device destination. |
| 3 | Include patterns survive names with `[`, `*` and `?`; `--include-file` does not expand `$VAR`. | Paths go as `--include` arguments, or a name that cannot be expressed is restored through its parent folder into the side folder. |
| 4 | `--overwrite never` and `if-changed` report skipped files in the JSON summary; a target file held open is reported as an error, not a crash. | The status gives the restored count only and says that existing files were kept. |
| 5 | restic running as SYSTEM restores ACLs, owner and long paths; the side folder's ACL holds. | The status page and the runbook say what is not restored. Files that fail count as errors (RESTORED WITH ERRORS). |
| 7 | Browsing with `--no-cache` (ADR 0008) of a repository of about 50 GB and 200 000 files answers in a usable time. | An in-memory (tmpfs) cache for the worker, the only cache ADR 0008 allows. |
