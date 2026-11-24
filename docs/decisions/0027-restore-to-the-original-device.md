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
  (`internal/console/maintain/doc.go:254-261`, `TestLiveAppendOnlyGateway`). Spike P22-S1 proved restore, `ls`,
  `dump` and `snapshots` through it with a device login: each exits 0, restic keeps one lock object while it runs and
  none after, and the gateway accepts the lock refresh every 5 minutes (`docs/lab/p22-spike.md:54-78`).

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
- **The agent runs restic** through the gateway, with its own login and its DPAPI-protected repository password, one
  restic process at a time: `restic restore --json -vv` with `--overwrite` as the destination demands and
  `--include-file` naming a file in a SYSTEM-only temporary folder, deleted on every exit.
- **Original location: one restore per anchor folder.** The anchor of a selected item is the deepest folder above it
  that exists on the device, normally its parent. Selected items are grouped by anchor, and each group is one run:
  `restic restore --json -vv <snapshot>:/<L>/<anchor> --target <L>:\<anchor> --include-file <F>`. restic never
  changes its target folder, so the anchor and everything above it keep their timestamps and permissions; folders
  restic creates below the anchor get their metadata from the snapshot, which is what restoring a deleted folder
  should do. **Rejected: one run per drive root** (`<snapshot>:/<L> --target <L>:\`). The spike showed that it resets
  the timestamps and permissions of every existing folder between the drive root and the selected file, removing a
  permission added after the backup, and changes permissions of files in branches nobody selected
  (`docs/lab/p22-spike.md:92-120`).
- **Side folder: one restore per drive.** `<snapshot>:/<L> --target <SystemDrive>\CryoBackup-Restore\<restore id>\<L>`.
  Every folder below the target is created by restic, so the drive-root finding does not apply
  (`p22-spike.md:329-330`). restic gives restored files the snapshot's permissions as explicit entries, and the side
  folder's own permissions are only added to them (`p22-spike.md:203-205`). So after a side-folder restore the agent
  resets the whole subtree to inherit from the side folder (Administrators and SYSTEM only) and checks one file. That
  reset is what makes ruling C2 true and is part of this decision.
- **The include file.** UTF-8 without BOM, LF line ends, one line per selected item, written relative to the
  run's source folder with a leading `/`, in the exact case of the snapshot listing. Every `[` is written `[[]`, then
  every `$` is written `[$]`. restic has no escape character, matches case-sensitively (the drive letter too), and
  expands `$VAR` and `${VAR}` in an include file; `*` and `?` cannot occur in an NTFS name
  (`p22-spike.md:122-156`). restic trims each line, so a selected name that begins or ends with a space is refused
  when the selection is made. Names stay byte-exact: no case folding, no Unicode normalisation.
- **Guards on the device.** The target must be an anchor folder of the original location, or a folder under
  `<SystemDrive>\CryoBackup-Restore\<restore id>\`; anything else ends as `guard_refused`. A restore waits for a
  running backup. A restore never resumes after the agent restarts; it ends as `interrupted`.
- **A restore that stops moving is stopped.** With the gateway gone, restic retried for more than 20 minutes without
  exiting (`p22-spike.md:239`). If no new status line arrives within a bound the build sets, the agent stops restic and
  the restore is FAILED with a storage class. After any restic it stopped (watchdog or cancel), the agent runs
  `restic unlock`, because a killed restic leaves its lock object in the repository (`p22-spike.md:345-349`); the
  append-only gateway allows it.
- **The run.** The agent journals a run of kind `restore`, trigger `manual`, whose run id is the restore id, so a
  `cancel` names it and a restore runs at most once. Its record goes through the outbox like every run, path-free, with
  three new stats: `files_restored`, `files_skipped`, `bytes_restored`.
- **Where the counts come from.** restic's summary has `total_files`, `files_restored`, `files_skipped`,
  `total_bytes`, `bytes_restored` and `bytes_skipped`; it leaves out every field that is zero, and it counts folders as
  files (`p22-spike.md:174-180`). So the agent does not copy the file counts. It counts the `verbose_status` lines of
  `-vv`: `restored` or `updated` on an item that is not a folder (checked locally) is restored, `unchanged` is skipped.
  `bytes_restored` comes from the summary. The item paths in those lines never leave the device.
- **Nothing matched is judged per anchor group.** A restic run whose include file matched nothing exits 0 with an
  empty summary (`p22-spike.md:176-177`), so the agent checks each group's run. If every group matched nothing, the
  restore is FAILED with the class `nothing_matched`, never delivered. If at least one group restored files and
  another matched nothing, the restore ends RESTORED WITH ERRORS (class `partial`), and the number of groups that
  matched nothing is reported under the class `nothing_matched` with the other per-class counts, never their paths.
- **Errors are classified on the device.** A target file held open by another program is a per-file error: restic
  exits 1 and restores the rest; with `--overwrite never` the file is not opened and is simply skipped
  (`p22-spike.md:170-172`). restic's error line carries the absolute path, so the agent classifies each error locally
  (in use, access denied, other) and sends only the count per class.
- **The outcome.** The console closes the request from the run. A success is `delivered`. Errors with some files
  restored are `delivered` with `error_class` `partial`, shown as RESTORED WITH ERRORS with the counts by class.
  Anything else is `failed` or `cancelled`.
- **One additive migration, on the run side only.** The runs table's `error_class` is a closed CHECK list
  (`internal/console/migrations/0003_fleet_start_lease.sql:65`) without the restore classes (`nothing_matched` and the
  per-file classes above). The console ticket of wave 2 (P22-E) adds them to that list in one additive migration. The
  `restore_requests` side needs none: its `error_class` is free text
  (`internal/console/migrations/0001_initial.sql:663`) and keeps the same class.
- **Bytes never pass the console.** The agent reads its own repository through the gateway.

### What both restore modes share

This applies to this ADR and to the download of ADR 0028:

- **One wizard page.** It works without JavaScript and runs in four steps: point in time, browse and select (files
  and folders), destination, review. The selection travels as hidden form fields, capped at 64 KiB of POST body,
  never in a URL or browser storage. One browse call is one `ls --json` of one folder, never recursive. restic does
  not page: a folder of 2 000 entries came back in one call (`p22-spike.md:274-276`). The worker pages restic's stream
  itself, 500 entries a page, and keeps no copy (today the listing stops at 500 and says "truncated",
  `internal/console/ui/restore.go:149-150`). "Refresh from repository"
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
A backup running: "Waiting for the running backup". The gateway refuses the login: FAILED, class `auth`, read from
restic's own error output, because the gateway logs no line for a refused request (`p22-spike.md:73-75`). Files kept:
"N restored, M skipped because they already exist". Nothing matched: FAILED, "nothing in the backup matched the
selection". restic stopped moving: FAILED, with the storage class. Interrupted: FAILED, "files being written may be incomplete; run
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

**Known limit: alternate data streams are not restored.** restic 0.19.1 did not back up the spike's stream, so no
restore can bring it back (`p22-spike.md:198`, `p22-spike.md:203-205`). The restore page's help and the runbook say
so. The feature matrix's row for alternate data streams stays open.

## What spike P22-S1 showed, and what it could not

The spike ran restic 0.19.1 and rclone 1.75.1 as a **non-elevated user** on a local-disk rig
(`docs/lab/p22-spike.md:1-52`). Shown there, and built on above:

| Item | Result | Where it lands in this decision |
|---|---|---|
| 1 | Restore and reads through the append-only gateway with a device login: yes. | Device modes ship. Refusals are read from restic's error output. |
| 2 | Drive-root form: no. It rewrites existing parents and unselected branches. | One run per anchor folder; the side folder keeps the drive-root form. |
| 3 | No escape character; `[[]` and `[$]` work; case-sensitive; `$VAR` expanded in include files; lines trimmed. | The include-file rules; names with leading or trailing spaces refused. |
| 4 | Summary omits zero fields and counts folders; nothing matched = exit 0; a locked file = exit 1 with an absolute path. | Counts from `-vv`; nothing matched judged per anchor group; errors classified on the device. |
| 5 | Permissions and own-SID owner restored; 378-character path and empty folder restored; alternate data streams lost; under a new folder the snapshot's permissions stay explicit. | The side-folder reset; the known limit. |
| 7 | Browse took 10-60 s per call on a loaded PC on local disk: not a measure of object storage. | ADR 0026 pilot-entry check; the in-memory cache stays the lever. |

**Not tested, and checked in the lab before the pilot (ADR 0026):** everything as SYSTEM, the agent's account. That
is restoring owner and permissions with the restore privilege, what a backup made as SYSTEM records for inherited
permissions, writes to folders at the level of `C:\Users`, creating the side folder with an Administrators and SYSTEM
permission set, and whether a user can open a restored file in it by its full path (`p22-spike.md:385-394`). Also
untested: file symlinks in a snapshot, and selected names with leading spaces. If the SYSTEM checks show that the
anchor form or the side-folder reset does not hold under that account, the device mode concerned does not ship in the
pilot, and a new ADR records why.

