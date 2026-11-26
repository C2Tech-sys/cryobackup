# Runbook: restoring files from the console

How an operator gets files back: as a zip, onto the device they came from, or into a side folder on that
device. It also says what every status on the restore page means and what to do about it, which devices cannot
be restored to, and what the agent refuses to do on the machine and why.

Design: [ADR 0008](../decisions/0008-no-restored-data-at-rest-on-the-console-host.md) (nothing restored is
stored on the console host), [ADR 0027](../decisions/0027-restore-to-the-original-device.md) (restore to the
device), [ADR 0028](../decisions/0028-a-zip-of-several-paths-is-one-stream.md) (the zip),
[ADR 0030](../decisions/0030-escrow-rechecked-and-retired-devices-restorable.md) (retired devices), and
[ADR 0031](../decisions/0031-implementation-rulings-for-the-pilot-slice.md) (where the build differs from the
wording of 0027). The lab script that proves all of this before a pilot is
[`docs/lab/p22-acceptance.md`](../lab/p22-acceptance.md).

## 1. Before you start

- **Who.** The operator role, and an authenticator check made in the last 5 minutes, for every restore of every kind.
  If the session's last check is older, the review step asks for the current authenticator code. A recovery code is
  not accepted there.
- **Where.** Device page, **Restore files**. The wizard has four steps: snapshot, select, destination, review. It
  works without JavaScript.
- **Which snapshot.** The list comes from what the agent last reported. **Refresh from repository** asks the
  maintenance worker for the real list, and the page does that by itself when it has none. Choosing another snapshot
  clears your selection and says so.
- **Select.** Tick files and folders. A folder is listed one level at a time, 500 entries a page; your selection
  survives paging and moving between folders (it travels inside the page, never in a URL, and is capped at 64 KiB).
  A folder you tick already holds everything below it, so ticking something inside it folds into it. **At most 50
  items.** Listing a repository is slow when it holds many backup runs (every listing reads every index file); a
  folder of 2 000 entries still comes back in one listing.
- **Names the restore refuses:** a name that begins or ends with a space, a name that ends with a dot, and a
  Windows device name (`NUL`, `COM1`, ...). Windows would silently change such a name when the file is written, so it could
  land on a different file. Restore the folder above it instead.
- **What gets written to the audit log:** that someone browsed (once per person, device, snapshot and hour), created or
  downloaded a restore, and which restore. Never a path.

## 2. The four destinations

| Destination | What it does | Use it when |
| --- | --- | --- |
| **Download as a zip** | The zip streams from the repository to your browser. Nothing is stored on the console host. Works while the device is off. | You need the files now, or the device is off, retired or gone. |
| **Original location, keep existing files** (the default for a device) | Every item goes back where it was. A file that already exists is left as it is. Nothing is deleted or replaced. | A file was deleted or lost. |
| **A new folder on the device** | Everything lands in `C:\CryoBackup-Restore\<restore id>\<drive letter>\...` on the system drive. Nothing that exists is touched. | You want to look first, compare, or put files somewhere else. |
| **Original location, replace changed files** | As keep, but a file that differs from the snapshot's copy is replaced by the snapshot's copy. You type the device's host name to confirm. | A file was damaged or changed and you want it back as it was. |

### Zip

- One zip for the whole selection, up to 50 items. A selected folder `Documents` becomes `Documents/...` at the top of the
  zip, including empty folders. A selected file is at the top. Two items with the same name become `name (2)`.
- **Links are left out.** Symbolic links, junctions, hard links and special entries are not in the zip, and the zip
  ends with `CRYOBACKUP-SKIPPED.txt`, which counts them by kind and names nothing.
- **A cut download leaves no zip.** The zip's table of contents is written last, after every part has been read; a
  download that stops, or a part that fails, never gives a file a zip program will open. Download again from the
  beginning (no resume). The status page says "Download cut off".
- **The link works once.** **Download** on the status page makes a single-use link for your session. A new one can be
  made until the restore expires (24 hours after it was created).
- A name in the snapshot that Windows would extract under another name (a trailing dot or space, a device name, a
  colon, a backslash, a control character) aborts the whole download. Restore that folder to a new folder on the device.

### Original location, keep existing files

- Files that do not exist are put back. Files that exist are counted as **skipped**. Folders that exist are never
  changed (no permission, owner or attribute); Windows may update a folder's last-write time because a file was added
  to it.
- A folder that was deleted is recreated and **gets its parent's permissions, not the snapshot's**.
- A placed file keeps the snapshot's own permission entries and owner, and takes what its new parent hands down.
- **Alternate data streams are not restored.** restic 0.19.1 does not back them up. This is a known limit.

### A new folder on the device

- `C:\CryoBackup-Restore\<restore id>\C\...` (one folder per drive letter of the selection). The folder is readable only by
  Administrators and SYSTEM, and **every file in it is owned by Administrators**, not by the user it belonged to: the
  snapshot's owner would otherwise be able to read and re-grant access to what is in there. Open it from an elevated
  prompt, or copy the files out as an administrator.
- It is for administrators. The agent resets the permissions after restic has written the files.
- A restore into a new folder that **does not succeed removes its folder again** (see section 4), so nothing half-secured
  stays on the disk. Run it again.
- Removing the folder later is yours to do; nothing expires it.

### Original location, replace changed files

- Type the device's host name exactly. Nothing is replaced until it matches.
- A replaced file is replaced whole: its alternate data streams are dropped and it takes the snapshot's owner and permissions.
- A file that is **read-only or marked system is never replaced** (counted `access_denied`). A file **held open** by a
  program cannot be replaced (counted `target_locked`); the rest is placed.
- Changes made to a replaced file since the snapshot are lost. Use a new folder first if you are not sure.

### How a device restore runs

- The console records the request and queues one command. The agent accepts it at once and runs it on its own, one
  repository task at a time: **it waits for a running backup**, then starts.
- **One device restore per device at a time.** A second is refused (409). There is no cancel button; the `cancel` command
  of the command queue ([ADR 0010](../decisions/0010-device-commands.md)) names the restore's id as the run id, or let the
  request expire.
- **A restore never resumes.** If the agent or the machine restarts mid-restore, it ends FAILED, "interrupted", and is not
  run again by itself. Files already placed stay. Run it again; files already in place are kept.
- **Queued for 24 hours.** A request whose device does not come online within 24 hours expires.
- The device restores with its own login through the gateway. The bytes never pass the console.
- **Free space is checked first**, on the volume restic writes to (the destination volume, or the system drive for a new
  folder). If it is short, the restore fails at once before anything is written (class `disk`).
- Nothing leaves the device but counts and classes. The device's own log has the detail: `C:\ProgramData\CryoBackup\logs\agent.log`
  and the Application Event Log, source `CryoBackup`, event 1020 (finished) and 1021 (failed, cancelled, interrupted).

## 3. The status page and what to do

`/restores/<id>`. It refreshes itself while a restore can still change.

### A device restore

| Status | Meaning | What to do |
| --- | --- | --- |
| **QUEUED**, device online | The device picks it up at its next check-in, normally within a minute. | Wait. |
| **QUEUED**, device offline | "Last seen ...  Starts when online, until <expiry>." | Wait for the device, or use **Download as zip** for the files now (the page links back to the wizard). |
| **RUNNING** | Files are being restored. Counts arrive when it finishes. | Wait. |
| **WAITING** | "Waiting for the running backup." The device runs one repository task at a time. | Wait; it starts when the backup ends. |
| **RESTORED** | Done. "N restored, M skipped because they already exist." | Check the files. In keep mode the skipped ones were left as they are. |
| **RESTORED WITH ERRORS** | Some files could not be written. The counts by class are shown. | See the class table below, fix the cause, run it again for the rest; what is in place is kept. |
| **FAILED**, `interrupted` | "Cut short." The agent or the machine restarted. | Run the restore again. Files already placed stay. |
| **FAILED**, `auth` | The gateway refused the device's login. | Check the device's gateway login on the device page, then run it again. |
| **FAILED**, `disk` | Not enough free space; nothing was written. | Free space on the device (the log line gives the numbers) or restore fewer items. |
| **FAILED**, `guard_refused` | The safety check refused the folder the restore writes into (section 4). Nothing was written. | The log names the failed check. Remove the folder it names and run the restore again. |
| **FAILED**, `nothing_matched` | Nothing in the snapshot matched the selection. | Pick the items again from the snapshot listing. |
| **FAILED**, `repo_unreachable` | The repository could not be reached, or restic stopped making progress for 15 minutes and the agent stopped it. | Check the gateway and the device's network, then run it again. |
| **FAILED**, other | The device's log has the detail. | Read `agent.log` on the device. |
| **CANCELLED** | The command was cancelled before or while the device ran it. | Start a new restore. |
| **EXPIRED** | The device never picked it up within 24 hours. | Start a new restore, or download the files as a zip. |

The class table under **RESTORED WITH ERRORS** and **FAILED**:

| Class | Meaning |
| --- | --- |
| `access_denied` | Windows refused access (for example a read-only or system file in replace mode). |
| `target_locked` | A file held open by another program could not be replaced. |
| `guard_refused` | An item was refused by the safety check (a link on the path, a wrong type, an odd name, see section 4). |
| `nothing_matched` | Items not found in the snapshot. |
| `interrupted` | Cut short. |
| `disk` | Not enough free space. |
| `other` | Anything else; the device's log. |

### A zip

| Status | Meaning | What to do |
| --- | --- | --- |
| **QUEUED** / **RUNNING** | The maintenance worker is starting. | Wait. |
| **READY** | "Ready to download." | Press **Download**. If the page says other restores are downloading, try again in a moment. |
| **DELIVERED** | The zip was downloaded. Nothing was kept on the server. | Open it. You can download again until it expires; it streams again from the snapshot. |
| **Download cut off** (a failed finish) | The download did not finish. No zip was saved: a part-file in the browser's downloads is not usable. | Download again from the beginning. |
| **FAILED**, `network` / `storage` / `unavailable` / `timeout` | The storage or the maintenance worker could not be reached. | Wait until the banners above clear, then start again. |
| **FAILED**, `wrong_password` / `auth` | The escrowed key did not open the repository. | Use **Verify now** on the device page. If that fails too, the repository was replaced or the escrowed key no longer opens it. Nothing was restored. |
| **FAILED**, `repo_missing` | The device's repository does not exist. | Nothing to restore from. |
| **FAILED**, `not_found` | The file or snapshot is not in the repository. | Choose again. |
| **FAILED**, `custody_customer_held` | The device's key is held by the customer. | No technician restore is possible (section 5). |
| **Expired** | 24 hours have passed. | Start another restore. |

## 4. What the agent refuses, and why

The agent restores as SYSTEM, so a restore is a way to write files anywhere on the machine. These refusals keep it from being
steered out of the place it was asked to restore into. Each is counted in `guard_refused` (or removed and counted skipped),
never named in the console.

- **Links.** A junction, symbolic link or mount point anywhere on a destination path: the item is not placed and is counted
  `guard_refused`. A link could point anywhere, and the agent cannot tell one a user made from one an attacker planted;
  restore those items to a new folder. A link **inside the snapshot** (restic restores a junction as a symbolic link) is
  deleted from the restore's working folder without being followed and counted skipped. Cloud-file placeholders (OneDrive
  Files On-Demand) are ordinary files and folders and are placed normally.
- **Hard links.** A restored file with more than one name is not placed, because its permissions are shared with its other
  names, which may lie outside the restore. In a new folder such a file is unlinked there; its other names keep the file.
- **Odd names.** The agent compares every name it opens with the name it expects. A name that only matched an 8.3 short
  name, or whose trailing dot or space Windows dropped, is refused. A file where a folder is (or a folder where a file is)
  is never placed.
- **The two working roots.** `<L>:\.cryobackup-restore` (the working folder next to an original-location restore) and
  `C:\CryoBackup-Restore` must be real folders owned by Administrators or SYSTEM and open to nobody else. If one is absent
  the agent creates it that way; if it exists and is not, the restore fails `guard_refused` and the agent does not repair
  it: look at the folder and remove it.
- **No drive-root selection.** "Everything on C:" is not offered.
- **A running restore blocks nothing silently.** It waits for a backup, never runs two at once, and never resumes after a
  restart.
- **A restore never deletes** a file from the machine, in any mode.

While an item is placed, the folders above it are held open: a program that tries to rename or remove one at that moment is
told it is in use.

## 5. Devices that cannot be restored to

- **A retired device: download only.** The wizard says so and offers only the zip. The device's gateway login was revoked
  when it was retired, so it could not read its repository, and giving it back would undo the retire. The device page
  keeps **Restore files** and **Refresh from repository**. If the escrowed key stops opening the repository, a fleet banner
  says "Escrow verification failing for N retired devices".
- **A customer-held key: refused everywhere**, device modes included, by the wizard, the API, the worker and the agent
  route. The console cannot read such a repository. Restore it with the customer's key, outside the console.
- **A device being purged:** blocked until the purge ends.
- **An agent older than 0.4.0:** the device destinations are greyed out with the version. The zip works. Upgrade the agent.
- **A device with a restore already queued or running:** the device destinations are greyed out; the page links to the
  active restore.
- **The master key not loaded, the worker down, storage unreachable:** a banner on the wizard says which; nothing is queued.
- **A device with no snapshot known** and **Refresh from repository** failing: the wizard says why (usually the worker).

## 6. After a restore: the drill

A restore is proven by comparing files, not by the status. The pilot's drills restore a file to the device and a zip of two
folders and a file, and compare each with the original by `Get-FileHash -Algorithm SHA256`. Steps 6 to 11 and 16 of
[`docs/lab/p22-acceptance.md`](../lab/p22-acceptance.md) are exactly this.
