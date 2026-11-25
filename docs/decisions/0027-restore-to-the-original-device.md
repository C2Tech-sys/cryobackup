# ADR 0027 - Restore to the original device: the agent restores with its own login, and the bytes never pass the console

**Date:** 2026-11-24 - **Status:** Accepted (operator, 2026-11-24) - **Decider:** the operator (rulings C1, C2, C5 and C6 of plan P22, 2026-11-24), the foreman (rulings marked below, after spike P22-S1 and the blind checks of this ADR), recorded by the foreman's worker (ticket P22-D0)

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
  none after, and the gateway accepts the lock refresh every 5 minutes (`docs/lab/p22-spike.md:56-80`).
- **restic rewrites folders that already exist.** When restic restores into a folder tree that exists, it re-applies
  the snapshot's timestamps and permissions to every existing folder below its target on the way to a selected item:
  a permission added after the backup is removed, and files in branches nobody selected can change
  (`docs/lab/p22-spike.md:94-122`). `--overwrite never` does not prevent it: the blind check of this ADR's first
  version restored a selected existing folder that way, and restic reset the timestamps of that folder and of its
  existing subfolder, removed a permission added after the backup, and reported both folders as `restored`
  (`p22-spike.md:415-433`).
- **Restored files carry the snapshot's permissions as explicit entries**, and a folder's own permissions are only
  added to them (`p22-spike.md:205-207`).
- **`icacls <folder> /reset /T` follows junctions.** The same blind check put a junction inside a side folder; the
  reset removed an explicit permission entry from a file outside the side folder (`p22-spike.md:435-441`).
- **restic restores a junction as a symbolic link.** It backs a junction up as a symlink node that carries no security
  descriptor, and restores it as a symbolic link to the junction's original target. As a non-elevated user the link
  cannot be created: one error line, exit 1, the rest restored (`p22-spike.md:466-481`).
- **A move by path follows a link.** With a destination folder replaced by a junction, a same-volume move by path, and
  Go's `os.Rename`, put the file into the junction's target (`p22-spike.md:483-487`). `os.Rename` replaces an existing
  target, because Go passes `MOVEFILE_REPLACE_EXISTING` (`p22-spike.md:509-510`, `p22-spike.md:537`).
- **A moved file keeps what staging handed down.** A file moved from a staging folder kept the staging folder's
  inherited entry and did not get the entry its new parent hands down. The existing folder it went into kept its
  permissions, but its last-write time changed, because an entry was added to it (`p22-spike.md:496-507`).
- **A staging root an ordinary user created first** let that user rename the agent's child folder while the child was
  still readable by the user; with the child set to Administrators and SYSTEM only, the rename was refused
  (`p22-spike.md:489-494`).
- **The Windows calls the placement needs**, checked as a non-elevated user (`p22-spike.md:513-573`): a folder held
  through a handle with a data right and without `FILE_SHARE_DELETE` can be neither renamed nor removed by another
  process, nor can its parent be renamed, while the holder can still move a file into it; a handle with attribute
  rights only does not protect the folder (`p22-spike.md:525-531`, `p22-spike.md:560-563`). A no-replace rename
  (`MoveFileEx` without `MOVEFILE_REPLACE_EXISTING`, `FileRenameInfoEx` without the replace flag) refuses an existing
  target; a replacing `MoveFileEx` is refused on a read-only file but not on a system file (`p22-spike.md:532-540`).
  A junction carries a name-surrogate tag, `IO_REPARSE_TAG_MOUNT_POINT`, the tag a mounted folder carries too; an
  ordinary folder or file carries none; a symbolic link could not be created without the privilege
  (`p22-spike.md:541-547`). A delete walk that opens every entry without following it removed two junctions and left
  their target untouched (`p22-spike.md:548-553`).

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
existing folder's permissions or attributes are changed; its last-write time can change when an item is added to it.
`--delete` is never passed: a restore never removes a file from the machine.

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

1. **Free space.** The check of the shared rules below, against the destination volume.
2. **Staging.** The agent makes the restore's staging folder `<L>:\.cryobackup-restore\<restore id>\` on the same
   volume as the destination, under the root rule of the shared rules below. restic restores the selection into it:
   `restic restore --json -vv <snapshot>:/<L> --target <L>:\.cryobackup-restore\<restore id> --include-file <F>`.
   Every folder below that target is created by restic, so restic changes nothing that existed before.
3. **Reparse points.** Every reparse point that restic restored into staging (a junction in
   the snapshot comes back as a symbolic link) is deleted there by the clean-up walk of the shared rules, never
   followed, never moved into place, and counted as skipped.
4. **Placement never resolves through a link the agent did not make** (foreman ruling). For each item:
   - **The destination path is checked from the drive root down.** The agent opens each existing component of the
     destination path itself, with `FILE_FLAG_OPEN_REPARSE_POINT | FILE_FLAG_BACKUP_SEMANTICS`, so that nothing is
     followed, and reads its reparse tag. A tag for which `IsReparseTagNameSurrogate` is true (a junction, a symbolic
     link, a mount point) anywhere on the path: the item is not placed and is counted under `guard_refused`, and no
     path leaves the device. A reparse point whose tag is not a name surrogate, such as a cloud-file placeholder or a
     deduplicated file, is an ordinary folder or file and is allowed: a user folder kept by OneDrive Files On-Demand
     is full of placeholders, and refusing them would refuse the folders users keep their documents in.
   - **Each checked folder is held until the item is placed.** The agent keeps the handle of every verified ancestor
     open, opened with a data right (`FILE_LIST_DIRECTORY | FILE_TRAVERSE`) and without `FILE_SHARE_DELETE`, so no
     ancestor can be renamed or removed between the check and the move. A handle with attribute rights only does not
     do this (`p22-spike.md:525-531`). The hold is not credited with stopping a held folder from being turned into a
     reparse point in place, so immediately before each move the agent re-reads the parent's reparse tag through its
     held handle, and the move itself is made relative to that handle (below). Assumed for the agent's account: the addendum test proves
     it for a non-elevated user (`p22-spike.md:560-563`), and the lab proves it as SYSTEM (below). If the lab says no,
     the fallback is: after each move the agent reads the placed file's final path through
     `GetFinalPathNameByHandle` on its own handle, and a path other than the intended one is counted under
     `guard_refused` and raises an alarm.
   - **A mounted folder is a name surrogate**, so no placement crosses a mount point, and "the same volume" is the
     drive letter of the destination path.
   - **A folder that does not exist** is created by the agent and inherits its permissions from its parent. The agent
     then opens and holds it like any other component. If something appeared at that name in the meantime, the
     creation fails and the existing item is checked like any other.
   - **A folder that exists** is never touched by the agent: it changes no permission, owner, timestamp or attribute
     on it. The file system still updates its last-write time when an item is added to it.
   - **One handle per placed file** (foreman ruling). The agent opens the staged file once, without following, renames
     it through that handle with `FileRenameInfoEx` whose `RootDirectory` is the held parent handle, and applies the
     descriptor below through the same handle. No placement step reopens a file or folder by name.
   - **Names are compared exactly** (foreman ruling). Every path the agent opens is in the `\\?\` form, and after
     opening a component or the item being replaced the agent compares the item's real long name, read through the
     handle, with the expected name. A mismatch (a name that only matched an 8.3 short name, or one whose trailing dot
     or space was dropped) is counted under `guard_refused` and nothing is placed or replaced.
   - **A file that does not exist** at its destination is moved in with a no-replace rename: `FileRenameInfoEx`
     without `FILE_RENAME_FLAG_REPLACE_IF_EXISTS` (foreman ruling). A file that appears at that name between the check
     and the move makes the rename fail; the item is skipped and counted, never overwritten. Go's `os.Rename` and a
     path-based `MoveFileEx` are not used.
   - **A file that exists** is skipped in keep mode. In replace mode, the agent uses the replace flag only for an item
     it checked: a regular file with no name-surrogate tag.
   - **A file where a folder exists, or a folder where a file exists**, is never placed, in any mode, and is counted
     under `guard_refused` (foreman ruling).
   - **Replace over a read-only or system file** is skipped and counted under `access_denied`; the agent checks the
     attributes itself and never clears them (foreman ruling). Windows refuses a replacing rename onto a read-only
     file but not onto a system file (`p22-spike.md:532-540`), so the check is the agent's.
   - **The descriptor of a moved file** (foreman ruling). A move keeps the inherited entries staging handed down and
     does not pick up the new parent's (`p22-spike.md:496-507`). So after the move, if the snapshot's descriptor is
     not protected, the agent re-applies the file's explicit entries with `UNPROTECTED_DACL_SECURITY_INFORMATION`
     through its no-follow handle: staging's inherited entries are dropped and the new parent's are picked up. A
     protected descriptor is kept as restored.
5. **Clean up.** The staging folder is removed when the restore ends, whatever the outcome, by the clean-up walk of
   the shared rules. A staging folder left by a restore that was cut short (a restart) is removed the same way when the
   agent starts, and that restore ends as `interrupted`.

### Side folder (ruling C2, made safe by a foreman ruling)

- The free-space check of the shared rules applies here too, against the system drive (foreman ruling).
- restic restores into the fresh side folder, made under the root rule of the shared rules:
  `<snapshot>:/<L> --target <SystemDrive>\CryoBackup-Restore\<restore id>\<L>`. Every folder below the target is
  created by restic.
- Before any permission is changed, the agent deletes every reparse point in the side folder with the clean-up walk,
  and counts each as skipped.
- Then the agent resets the subtree to inherit from the side folder (Administrators and SYSTEM only). It does this
  with its own walk that never follows a reparse point, or with an API call that gives the same guarantee, never with
  `icacls /T` on a tree that may hold reparse points. It then checks one file.
- **The guarantee:** nothing outside the side folder can be changed by a side-folder restore. The reset is what makes
  ruling C2 true.

### Rules shared by both device destinations

- **Free space** (foreman ruling). In both destinations the agent sums the selection's sizes from `restic ls` and
  checks the free space on the volume restic will write to: the destination volume for the original location, the
  system drive for the side folder. If it is short, the restore is refused at once, FAILED with class `disk`, before
  anything is written.
- **The staging and side roots** (foreman ruling). `<L>:\.cryobackup-restore` and `<SystemDrive>\CryoBackup-Restore`:
  if a root is absent, the agent creates it with a protected descriptor that grants Administrators and SYSTEM only. If
  it is present, it must be a real directory (no name-surrogate tag), owned by Administrators or SYSTEM, with a
  protected DACL that grants only those two. Otherwise the restore is refused under `guard_refused`, with a log line
  on the device that names the failed check, and the agent does not repair the root: an operator removes it. Each
  restore's own `<restore id>` folder is created fresh; if it already exists, the restore is refused. The agent holds
  the root's handle (as for an ancestor above) for the whole restore, and the start-up sweep applies this root rule
  before it walks anything: a root that fails it is left alone and logged.
- **The clean-up walk** (foreman ruling). Every removal (staging at the end of a restore, the sweep when the agent
  starts, the removal of links in the side folder) is the agent's own walk. It opens each entry with
  `FILE_FLAG_OPEN_REPARSE_POINT | FILE_FLAG_BACKUP_SEMANTICS`, deletes a reparse point itself through that handle,
  and never descends through one (`p22-spike.md:548-553`). Go's `os.RemoveAll` is not relied on for this.
- **The include file.** UTF-8 without BOM, LF line ends, one line per selected item, written relative to the drive
  root with a leading `/`, in the exact case of the snapshot listing. Every `[` is written `[[]`, then every `$` is
  written `[$]`. restic has no escape character, matches case-sensitively (the drive letter too), and expands `$VAR`
  and `${VAR}` in an include file; `*` and `?` cannot occur in an NTFS name (`p22-spike.md:124-158`). restic trims
  each line, so a selected name that begins or ends with a space is refused when the selection is made (foreman
  ruling). Names stay byte-exact: no case folding, no Unicode normalisation.
- **Guards on the device.** restic's target is always a folder the agent created for this restore: the staging folder,
  or a folder under `<SystemDrive>\CryoBackup-Restore\<restore id>\`. The placement step writes only at the original
  location of a selected item. Anything else ends as `guard_refused`. A restore waits for a running backup. A restore
  never resumes after the agent restarts; it ends as `interrupted`.
- **A restore that stops moving is stopped** (foreman ruling). With the gateway gone, restic retried for more than 20
  minutes without exiting (`p22-spike.md:241`). If no new status line arrives within a bound the build sets, the
  agent stops restic and the restore is FAILED with a storage class. After any restic it stopped (watchdog or cancel),
  the agent runs `restic unlock`, because a killed restic leaves its lock object in the repository
  (`p22-spike.md:347-351`); the append-only gateway allows it.
- **The run.** The agent journals a run of kind `restore`, trigger `manual`, whose run id is the restore id, so a
  `cancel` names it and a restore runs at most once. Its record goes through the outbox like every run, path-free, with
  three new stats: `files_restored`, `files_skipped`, `bytes_restored`.
- **Where the counts come from.** restic's summary has `total_files`, `files_restored`, `files_skipped`,
  `total_bytes`, `bytes_restored` and `bytes_skipped`; it leaves out every field that is zero, and it counts folders as
  files (`p22-spike.md:176-182`), so it is used only for what happened in staging or the side folder. The counts the
  operator sees come from the agent's own step: files placed (or kept in the side folder) are restored, existing files
  kept and reparse points removed are skipped, and every error is counted by class. No path leaves the device.
- **Nothing matched is judged per restic run** (foreman ruling). A restic run whose include file matched nothing exits
  0 with an empty summary (`p22-spike.md:178-179`). If every run matched nothing, the restore is FAILED with the class
  `nothing_matched`, never delivered. If at least one run restored files and another matched nothing, the restore ends
  RESTORED WITH ERRORS (class `partial`), and the number of runs that matched nothing is reported under the class
  `nothing_matched` with the other per-class counts, never their paths.
- **Errors are classified on the device.** restic's error lines carry absolute paths (`p22-spike.md:172`), and so do
  the agent's own move errors. The agent classifies each locally (a file in use by another program, `access_denied`,
  `guard_refused`, other) and sends only the count per class. In replace mode, an existing file held open by another
  program cannot be replaced; it is counted as an error and the rest is placed.
- **The outcome.** The console closes the request from the run. A success is `delivered`. Errors with some files
  restored are `delivered` with `error_class` `partial`, shown as RESTORED WITH ERRORS with the counts by class.
  Anything else is `failed` or `cancelled`.
- **Classes and migrations.** The runs table's `error_class` is a closed CHECK list
  (`internal/console/migrations/0003_fleet_start_lease.sql:65`). P22-E decides whether `runs.error_class` needs a
  migration (foreman ruling: the restore classes ride in the run's stats JSON, `runs.stats`, so likely none; this
  replaces the earlier ruling of one additive migration on the run side). The `restore_requests` side needs none: its
  `error_class` is free text (`internal/console/migrations/0001_initial.sql:663`).
- **Bytes never pass the console.** The agent reads its own repository through the gateway.

### What both restore modes share

This applies to this ADR and to the download of ADR 0028:

- **One wizard page.** It works without JavaScript and runs in four steps: point in time, browse and select (files
  and folders), destination, review. The selection travels as hidden form fields, capped at 64 KiB of POST body,
  never in a URL or browser storage. One browse call is one `ls --json` of one folder, never recursive. restic does
  not page: a folder of 2 000 entries came back in one call (`p22-spike.md:276-278`). The worker pages restic's stream
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
gateway logs no line for a refused request (`p22-spike.md:75-77`). Files kept: "N restored, M skipped because they
already exist". Items under a link or of the wrong type: counted under `guard_refused`, "restore these to the side
folder". A staging or side root that fails the root rule: FAILED, class `guard_refused`, "remove the folder and run
the restore again". Nothing matched: FAILED, "nothing in the backup matched the selection". restic stopped moving:
FAILED, with the storage class. Interrupted: FAILED, "the restore was cut short; files already placed stay, run the
restore again". Past 24 hours: EXPIRED.

## Consequences

- An operator can restore to a machine that is on, or that comes on within 24 hours, without the files passing the
  console host. ADR 0008 holds.
- **Recreated folders get their parent's inherited permissions, not the snapshot's.** A deleted folder comes back
  with the permissions it would get if someone created it today.
- **A placed file keeps the snapshot's explicit entries and inherits from its new parent.** A file whose snapshot
  descriptor was protected keeps it as restored. No entry of the staging folder stays on a placed file.
- **An existing folder's permissions and attributes are never changed by the agent.** The file system still updates a
  folder's last-write time when an entry is added to it, as it does for any new file; the agent sets nothing on the
  folder.
- **Replace mode replaces the whole file.** The replaced file's alternate data streams are dropped, and the file takes
  the snapshot's owner and DACL. Read-only and system files are never replaced.
- **Nothing is placed through a link.** An item whose destination path holds a junction, a symbolic link or a mount
  point that the agent did not make is counted under `guard_refused`; the operator restores it to the side folder.
- **While an item is placed, its destination folders are held.** A program that tries to rename or remove one of them
  at that moment is told the folder is in use.
- A restore needs free space for the selection on the volume restic writes to, in both destinations. A short volume
  is refused before anything is written.
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
- **Placing through a link at the destination**, even one that looks harmless, such as an old profile junction. The
  agent cannot tell a link a user made from one an attacker planted; the side folder covers those items.
- **Repairing a staging or side root that fails the root rule.** Taking ownership of, or rewriting, a folder the agent
  did not make is the kind of change this ADR forbids; an operator looks at it and removes it.
- **A target typed by the operator.** The guard is simpler when there are two places a restore may write.
- **`if-newer`, `--verify`, a dry run, search, live progress and a cancel button.** None is needed to put a file back.
  `cancel` by run id already works for the operator's command.
- **The replacement-PC relay** (DH28). It is a separate path; ADR 0008 already says it must stream.
- **A tar download.** The download is a zip (ADR 0028).

**Known limit: alternate data streams are not restored.** restic 0.19.1 did not back up the spike's stream, so no
restore can bring it back (`p22-spike.md:200`, `p22-spike.md:205-207`). The restore page's help and the runbook say
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
| Addendum 2 | A junction restores as a symbolic link; a move by path follows a link; a moved file keeps staging's inherited entries; an existing folder's last-write time changes when an item is added. | The placement guards, the descriptor rule, the wording of C1. |
| Addendum 3 | Held folders cannot be renamed or removed; no-replace renames refuse; a junction is a name surrogate; the delete walk leaves a junction's target alone (non-elevated). | The hold, the no-replace rename, the surrogate check, the clean-up walk. |

**Lab checks as SYSTEM, the agent's account, before the pilot (ADR 0026; the spike's list is
`p22-spike.md:575-586`):**

1. the case of the blind check (a selected existing folder and its existing subfolder) under a user profile, showing
   that no existing folder's permissions or attributes change;
2. owner restore with the restore privilege (SeRestorePrivilege);
3. which permissions a backup made as SYSTEM records for inherited entries;
4. profile junctions in both destinations;
5. the reparse-point rule: links in staging and in the side folder are removed by the clean-up walk without being
   followed, including the symbolic link restic makes as SYSTEM for a junction in the snapshot, and whether restic, as
   SYSTEM, applies that junction node's attributes or times through the link it created;
6. creating the staging and side roots with an Administrators and SYSTEM protected descriptor, refusing a root that
   fails the root rule, and whether a user can open a restored file in the side folder by its full path;
7. the descriptor rule after a move: staging's inherited entries gone, the new parent's present, a protected
   descriptor unchanged;
8. a destination on a volume other than the system drive, with staging on that volume;
9. the free-space refusal, in both destinations;
10. the hold as SYSTEM, with the agent's open flags and its backup and restore privileges enabled: another process,
    as an ordinary user and as an administrator, cannot rename or remove a held folder; whether a held, empty folder
    can be turned into a reparse point in place (the re-read before each move covers it either way); and a hard link
    to a file outside the destination, placed by a user on the restore path, is not replaced in replace mode;
11. the name-surrogate check on a real symbolic link and a mounted folder, and that a cloud-file placeholder is not a
    name surrogate.

Also untested: file symlinks in a snapshot (`p22-spike.md:387-396`). If a lab check fails, the device mode concerned
does not ship in the pilot, and a new ADR records why; for check 10 the `GetFinalPathNameByHandle` fallback above
applies instead.
