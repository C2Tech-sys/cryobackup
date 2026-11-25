# ADR 0027 - Restore to the original device: the agent restores with its own login, and the bytes never pass the console

**Date:** 2026-11-24 - **Status:** Accepted (operator, 2026-11-24) - **Decider:** the operator (rulings C1, C2, C5 and C6 of plan P22, 2026-11-24), the foreman (rulings marked below, after spike P22-S1 and the blind check of this ADR), recorded by the foreman's worker (ticket P22-D0)

## Context

ADR 0026 puts restore in the console first in the pilot slice. Today the only restore is a download of one path, made
by the maintenance worker and streamed through the console (ADR 0008). A restore that puts files back on the machine
they came from has most of its parts already defined, but none of them run:

- **Wire.** `RestoreSelection` (`internal/api/restore.go:14-46`) is the one path-bearing type of the agent protocol.
  It travels only on the device-authenticated `GET /api/v1/agent/restores/{id}`
  (`internal/api/routes.go:61`), and the agent writes it to a SYSTEM-only temporary file and deletes it after the
  restore (`internal/api/restore.go:19-22`). The restore command carries only the request id
  (`internal/api/commands.go:108-112`).
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
- **restic rewrites folders that already exist.** When restic restores into a folder tree that exists, it re-applies
  the snapshot's timestamps and permissions to every existing folder below its target on the way to a selected item:
  a permission added after the backup is removed, and files in branches nobody selected can change
  (`docs/lab/p22-spike.md:92-120`). `--overwrite never` does not prevent it: the blind check of this ADR's first
  version restored a selected existing folder that way, and restic reset the timestamps of that folder and of its
  existing subfolder, removed a permission added after the backup, and reported both folders as `restored`.
- **Restored files carry the snapshot's permissions as explicit entries**, and a folder's own permissions are only
  added to them (`p22-spike.md:203-205`).
- **`icacls <folder> /reset /T` follows junctions.** The same blind check put a junction inside a side folder; the
  reset rewrote a protected permission set on a file outside the side folder.

## Decision

### Where the files go (rulings C1 and C2)

The operator chooses one of three destinations. The first is the default:

1. **Original location, keep what exists.** Files go back where they were. A file that already exists is left alone.
   Nothing on the machine is replaced.
2. **Side folder.** Files go to `<SystemDrive>\CryoBackup-Restore\<restore id>\<drive>\...`, a folder that only
   Administrators and SYSTEM can open (ruling C2).
3. **Original location, replace existing files.** An existing file in the selection is replaced by the restored one.
   The operator must type the device's host name to confirm.

The intent of ruling C1 holds in every mode: nothing that exists is replaced unless the operator chose replace, and no
existing folder is ever changed. `--delete` is never passed: a restore never removes a file from the machine.

### restic restores; the agent places files

restic remains the engine: it reads the repository and writes the restored files, always into a fresh folder the
agent has just created, never into one that already existed. Putting those files at their original locations is a
separate step the agent does itself. That step moves files; it reads no repository, decrypts nothing and replaces no
part of restic.

### How a request travels

- **The console** creates the `restore_requests` row and the `restore` command in one transaction. The selection is
  wrapped under the master key, the command's parameters carry only the request id, and the audit row
  `restore.create` carries no path. One active device restore per device; a second answers 409. A restore command is
  created only through this path, never through the operator command queue of ADR 0010. A selection of a whole drive
  is refused by the wire contract.
- **A queued request waits 24 hours** for the device (ruling C6). After that it is EXPIRED.
- **The agent** accepts the command at once and runs the restore in its own goroutine, outside the command executor.
  It fetches the selection with its device token. The console answers it only when the device matches, the mode is a
  device mode, the request is not finished or expired, and the device is neither retired nor purging. The fetch writes
  the audit row `restore.fetch`.
- **The agent runs restic** through the gateway, with its own login and its DPAPI-protected repository password, one
  restic process at a time: `restic restore --json -vv` with `--include-file` naming a file in a SYSTEM-only
  temporary folder, deleted on every exit. One run per volume the selection touches.

### Original location: restore into staging, then place (foreman ruling)

1. **Size check.** The agent sums the selection's sizes from `restic ls` and checks the free space on each
   destination volume. If a volume is short, the restore is refused at once, FAILED with class `disk`, before
   anything is written.
2. **Staging.** The agent creates a fresh staging folder on the same volume as the destination, for example
   `<L>:\.cryobackup-restore\<restore id>\`, open to Administrators and SYSTEM only. restic restores the selection into
   it: `restic restore --json -vv <snapshot>:/<L> --target <L>:\.cryobackup-restore\<restore id> --include-file <F>`.
   Every folder below that target is created by restic, so restic changes nothing that existed before.
3. **Reparse points.** Every symlink or junction restic restored into staging is deleted there without being followed,
   never moved into place, and counted as skipped.
4. **Placement, item by item.**
   - A **file** that does not exist at its destination is moved in. A move within one volume keeps the restored
     file's security descriptor, which is the snapshot's.
   - A **file** that exists is skipped (keep), or, in replace mode, the restored file is moved over it.
   - A **folder** that does not exist is created by the agent. It inherits its permissions from its parent.
   - A **folder** that exists is never touched: the agent changes no permission, owner, timestamp or attribute on it.
5. **Clean up.** The staging folder is removed when the restore ends, whatever the outcome. A staging folder left by a
   restore that was cut short (a restart) is removed when the agent starts, and that restore ends as `interrupted`.

### Side folder (ruling C2, made safe by a foreman ruling)

- restic restores into the fresh side folder:
  `<snapshot>:/<L> --target <SystemDrive>\CryoBackup-Restore\<restore id>\<L>`. Every folder below the target is
  created by restic.
- Before any permission is changed, the agent deletes every reparse point in the side folder without following it,
  and counts each as skipped.
- Then the agent resets the subtree to inherit from the side folder (Administrators and SYSTEM only). It does this
  with its own walk that never follows a reparse point, or with an API call that gives the same guarantee, never with
  `icacls /T` on a tree that may hold reparse points. It then checks one file.
- **The guarantee:** nothing outside the side folder can be changed by a side-folder restore. The reset is what makes
  ruling C2 true.

### Rules shared by both device destinations

- **The include file.** UTF-8 without BOM, LF line ends, one line per selected item, written relative to the drive
  root with a leading `/`, in the exact case of the snapshot listing. Every `[` is written `[[]`, then every `$` is
  written `[$]`. restic has no escape character, matches case-sensitively (the drive letter too), and expands `$VAR`
  and `${VAR}` in an include file; `*` and `?` cannot occur in an NTFS name (`p22-spike.md:122-156`). restic trims
  each line, so a selected name that begins or ends with a space is refused when the selection is made (foreman
  ruling). Names stay byte-exact: no case folding, no Unicode normalisation.
- **Guards on the device.** restic's target is always a folder the agent created for this restore: the staging folder,
  or a folder under `<SystemDrive>\CryoBackup-Restore\<restore id>\`. The placement step writes only at the original
  location of a selected item. Anything else ends as `guard_refused`. A restore waits for a running backup. A restore
  never resumes after the agent restarts; it ends as `interrupted`.
- **A restore that stops moving is stopped** (foreman ruling). With the gateway gone, restic retried for more than 20
  minutes without exiting (`p22-spike.md:239`). If no new status line arrives within a bound the build sets, the
  agent stops restic and the restore is FAILED with a storage class. After any restic it stopped (watchdog or cancel),
  the agent runs `restic unlock`, because a killed restic leaves its lock object in the repository
  (`p22-spike.md:345-349`); the append-only gateway allows it.
- **The run.** The agent journals a run of kind `restore`, trigger `manual`, whose run id is the restore id, so a
  `cancel` names it and a restore runs at most once. Its record goes through the outbox like every run, path-free, with
  three new stats: `files_restored`, `files_skipped`, `bytes_restored`.
- **Where the counts come from.** restic's summary has `total_files`, `files_restored`, `files_skipped`,
  `total_bytes`, `bytes_restored` and `bytes_skipped`; it leaves out every field that is zero, and it counts folders as
  files (`p22-spike.md:174-180`), so it is used only for what happened in staging or the side folder. The counts the
  operator sees come from the agent's own step: files placed (or kept in the side folder) are restored, existing files
  kept and reparse points removed are skipped, and every error is counted by class. No path leaves the device.
- **Nothing matched is judged per restic run** (foreman ruling). A restic run whose include file matched nothing exits
  0 with an empty summary (`p22-spike.md:176-177`). If every run matched nothing, the restore is FAILED with the class
  `nothing_matched`, never delivered. If at least one run restored files and another matched nothing, the restore ends
  RESTORED WITH ERRORS (class `partial`), and the number of runs that matched nothing is reported under the class
  `nothing_matched` with the other per-class counts, never their paths.
- **Errors are classified on the device.** restic's error lines carry absolute paths (`p22-spike.md:170`), and so do
  the agent's own move errors. The agent classifies each locally (a file in use by another program, access denied,
  other) and sends only the count per class. In replace mode, an existing file held open by another program cannot be
  replaced; it is counted as an error and the rest is placed.
- **The outcome.** The console closes the request from the run. A success is `delivered`. Errors with some files
  restored are `delivered` with `error_class` `partial`, shown as RESTORED WITH ERRORS with the counts by class.
  Anything else is `failed` or `cancelled`.
- **Classes and migrations.** The runs table's `error_class` is a closed CHECK list
  (`internal/console/migrations/0003_fleet_start_lease.sql:65`). It already holds `locked`, `permission`, `unknown`
  and `disk`; of the restore classes only `nothing_matched` is missing. The wire ticket (P22-A) carries the restore
  classes in the run's stats JSON, so the run side needs no migration if the classes ride there, and otherwise one
  additive migration of that CHECK list; the console ticket (P22-E) decides. The `restore_requests` side needs none:
  its `error_class` is free text (`internal/console/migrations/0001_initial.sql:663`).
- **Bytes never pass the console.** The agent reads its own repository through the gateway.

### What both restore modes share

This applies to this ADR and to the download of ADR 0028:

- **One wizard page.** It works without JavaScript and runs in four steps: point in time, browse and select (files
  and folders), destination, review. The selection travels as hidden form fields, capped at 64 KiB of POST body,
  never in a URL or browser storage. One browse call is one `ls --json` of one folder, never recursive. restic does
  not page: a folder of 2 000 entries came back in one call (`p22-spike.md:274-276`). The worker pages restic's stream
  itself, 500 entries a page, and keeps no copy (today the listing stops at 500 and says "truncated",
  `internal/console/ui/restore.go:149-150`). "Refresh from repository" asks the worker for the snapshot list (the
  worker's `Snapshots` method, unused today, `internal/console/maintain/methods.go:285-301`), and the page does this
  by itself when the cache is empty. Today the list comes only from the cache the agent fills
  (`ui/restore.go:103-111`).
- **Who may restore (ruling C5).** The operator role and a fresh MFA check (the 5-minute window of ADR 0019,
  `internal/console/auth/stepup_adr19.go:35`) for every restore, whichever mode. Browsing writes the audit row
  `restore.browse` once per admin, device, snapshot and hour.
- **A customer-held key is refused** at every layer, device modes included.

A device restore to an agent older than 0.4.0 is refused when the request is created, because that agent cannot run it.

### What the operator sees when it does not work

Worker or storage down, the master key missing, a customer-held key or a device being purged: a banner that says
which. Device offline: "Queued. Last seen ... Starts when online, until <expiry>. For files now, use Download as zip."
A backup running: "Waiting for the running backup". Not enough free space: FAILED, class `disk`, with the space
needed and free. The gateway refuses the login: FAILED, class `auth`, read from restic's own error output, because the
gateway logs no line for a refused request (`p22-spike.md:73-75`). Files kept: "N restored, M skipped because they
already exist". Nothing matched: FAILED, "nothing in the backup matched the selection". restic stopped moving: FAILED,
with the storage class. Interrupted: FAILED, "the restore was cut short; files already placed stay, run the restore
again". Past 24 hours: EXPIRED.

## Consequences

- An operator can restore to a machine that is on, or that comes on within 24 hours, without the files passing the
  console host. ADR 0008 holds.
- **Recreated folders get their parent's inherited permissions, not the snapshot's.** A deleted folder comes back
  with the permissions it would get if someone created it today. Restored files keep the snapshot's permissions.
- **An existing folder is never changed by the agent.** The file system still updates a folder's last-write time when
  an entry is added to it, as it does for any new file; the agent sets nothing on the folder.
- A restore to the original location needs free space for the selection on each destination volume while staging
  exists. A short volume is refused before anything is written.
- The agent ships as 0.4.0. Its path-free test (`TestNoPathLeavesTheDevice`, `internal/agent/leak_test.go:23`) is
  extended to the restore records.
- ADR 0010's list of command types grows by `restore`, created only by the restore service with the 24-hour lifetime
  above.
- A restore run does not count as a backup. The backup freshness queries select `kind = 'backup'`
  (`internal/console/store/facts.go:326-330`), and a test pins that.

## What is deliberately left out and why

- **restic restoring straight into the original location**, in either form tried. One run per drive root rewrites
  every existing folder on the way (spike item 2). One run per anchor folder, the deepest existing folder above each
  item, still rewrites the existing folders below the anchor, even with `--overwrite never` (the blind check above).
- **Restoring reparse points.** A link restored at its original location could point anywhere, and following one
  during placement or a permission reset can change files outside the restore.
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
| 2 | Drive-root form: no. It rewrites existing parents and unselected branches. | restic restores only into fresh folders; the agent places files. |
| 3 | No escape character; `[[]` and `[$]` work; case-sensitive; `$VAR` expanded in include files; lines trimmed. | The include-file rules; names with leading or trailing spaces refused. |
| 4 | Summary omits zero fields and counts folders; nothing matched = exit 0; a locked file = exit 1 with an absolute path. | Counts from the agent's own step; nothing matched judged per run; errors classified on the device. |
| 5 | Permissions and own-SID owner restored; 378-character path and empty folder restored; alternate data streams lost; under a new folder the snapshot's permissions stay explicit. | The side-folder reset; the known limit. |
| 7 | Browse took 10-60 s per call on a loaded PC on local disk: not a measure of object storage. | ADR 0026 pilot-entry check; the in-memory cache stays the lever. |

**Lab checks as SYSTEM, the agent's account, before the pilot (ADR 0026):**

1. the case of the blind check (a selected existing folder and its existing subfolder) under a user profile, showing
   that no existing folder changes;
2. owner restore with the restore privilege (SeRestorePrivilege);
3. which permissions a backup made as SYSTEM records for inherited entries;
4. profile junctions in both destinations;
5. the reparse-point rule: links in staging and in the side folder are removed without being followed;
6. creating the side folder with an Administrators and SYSTEM permission set, and whether a user can open a restored
   file in it by its full path;
7. a move within one volume keeping the restored file's security descriptor;
8. a destination on a volume other than the system drive, with staging on that volume;
9. the free-space refusal.

Also untested: file symlinks in a snapshot (`p22-spike.md:385-394`). If a lab check fails, the device mode concerned
does not ship in the pilot, and a new ADR records why.
