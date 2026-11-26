# P22 pilot exit: lab acceptance script (restore, console self-backup, escrow recheck, console drill, pilot client)

For the operator. Run it against a real console, a real gateway, real storage and real Windows machines, never fixtures:
CLAUDE.md requires a phase to be exercised on real machines before it is called done. Hosts are described generically;
keep concrete addresses, bucket names and customer names outside the repository (write them in your own copy of the
evidence lines, not in a commit).

Decisions this script checks: [ADR 0026](../decisions/0026-the-pilot-slice.md) (the slice and its exit criteria),
[ADR 0027](../decisions/0027-restore-to-the-original-device.md) and [0028](../decisions/0028-a-zip-of-several-paths-is-one-stream.md)
(restore), [ADR 0029](../decisions/0029-console-self-backup-is-a-restic-repository.md) (self-backup and the drill),
[ADR 0030](../decisions/0030-escrow-rechecked-and-retired-devices-restorable.md) (escrow recheck, retired devices),
[ADR 0031](../decisions/0031-implementation-rulings-for-the-pilot-slice.md) (where the build differs from their wording).
What an operator does with the restore pages is in [`docs/runbooks/restore.md`](../runbooks/restore.md); the console rebuild
is [`docs/runbooks/console-disaster-recovery.md`](../runbooks/console-disaster-recovery.md); the upgrade is
[`docs/upgrade-notes.md`](../upgrade-notes.md), "0.4.0".

**[OP]** marks a step only the operator can do: it needs the production host, the storage account, a second machine's
power button or the client. Every other step can be run by anyone with a lab console. Each step has one **Evidence** line:
fill it in when you run the step (PASS or FAIL, date, what you saw, the restore id or the file you compared). A step with
an empty evidence line has not been run.

## Exit criteria (written before day 1)

The pilot has passed when all five hold. They are written here, before the pilot client's first backup, and are not changed
afterwards. Day 1 is the day of that first backup; the exception of ADR 0026 (no forget, prune or check on device
repositories) ends on day 45, and continuing needs a new ADR.

1. **Zero silent misses.** Every outage induced during the pilot raised an alarm: the board showed it and, where mail is on,
   a mail said so, within the time the banner's own text promises. No outage left the board green. Induce at least once each:
   stop the agent service on the pilot PC; stop `rclone-gateway`; stop `cryo-maintain`; make the mail relay unreachable; let
   the pilot PC sleep past a scheduled run. Record each below.
2. **At least 95 % of scheduled runs inside the policy's SLA**, with the time the device was off excluded (offline time comes
   from the console's own heartbeat record). Count from the run history over the 45 days.
3. **Every restore drill passed:** steps 6 to 13 on the lab VM and step 16 on the pilot client, each compared by SHA-256.
4. **The console drill passed:** step 14, repeated after any change to the offline kit.
5. **No path in any log, mail or audit row:** step 17's search finds none of the sentinel names in the console and worker
   journals, the device's `agent.log`, any mail the console sent, or the audit log.

| Outage induced | When (UTC) | Alarm or banner (code) | Seen on the board at | Mail at | Silent? |
| --- | --- | --- | --- | --- | --- |
| | | | | | |
| | | | | | |
| | | | | | |

SLA tally over the pilot: scheduled runs ____ , in SLA ____ , device-off windows excluded ____ , result ____ %.

## Setup

- A production console built from the 0.4.0 release commit, its gateway and worker, and real object storage. The console
  is **not** put at risk by this script: step 1 is an ordinary upgrade; steps 2 to 4 only turn the self-backup on.
- A lab console and a **lab VM** (Windows 10 or 11, NTFS, a local administrator `labadmin`, an ordinary user `labuser` with a
  profile, a second NTFS volume `E:`), enrolled to the lab console, in a customer with an **escrowed** key (a customer-held
  key is refused by every restore path). The lab console may be the production console with a lab customer; keep the lab
  device in its own customer so nothing of the pilot client is touched.
- The agent 0.4.0 MSI, from the build report (version, sha256, signed or not).
- A second Linux VM, clean, for step 14.
- A terminal on the production host: `journalctl -fu cryo-console -u cryo-maintain`.
- A browser signed in to the console as an owner with an authenticator, which a restore needs fresh (5 minutes).

## Pilot-entry checks (ADR 0026): all four before step 15

The pilot client is not enrolled until each has passed.

| # | Check | Where it is proven | Evidence |
| --- | --- | --- | --- |
| E1 | **Browse latency on the real bucket.** Time three cold listings of one folder (open the wizard on a real device with a few weeks of backups, choose a snapshot, open a large folder; reload it). Each listing reads every index file. Record the three times. If they are too slow to use, the worker gets an in-memory cache, the only cache ADR 0008 allows. | the wizard on production | |
| E2 | **The four object-storage checks of ADR 0029** on the real account: the admin key's rights on the self-backup prefix (`init`, `backup`, `forget --prune`, `check --read-data` all exit 0), the gateway key refused on the self-backup repository, the cost of `forget --prune` every 6 hours under the bucket's versioning and minimum-duration rules, and the gateway policy being prefix-scoped. | storage console and step 4 | |
| E3 | **The restore checks that need the agent's account (SYSTEM):** the whole of [`docs/lab/p22-agent-system.md`](p22-agent-system.md). | the gate after step 5 | |
| E4 | **The console drill** on a throwaway Linux VM. | step 14 | |

## Step 1. Upgrade the production console (operator [OP], sudo)

| | |
| --- | --- |
| Do | On the production host, with the new `cryo-console` binary staged and its sha256 known: `sudo cryo-upgrade ~/cryo-console <sha256>` (dry run, changes nothing), then `sudo cryo-upgrade --apply ~/cryo-console <sha256>`, then `sudo -u cryobackup cryo-console doctor --config /etc/cryobackup/config.json`. Add `admin_url` and `trusted_proxies` only after this has been checked (upgrade notes, "Rolling back"). |
| Expect | The dry run lists its checks and changes nothing. The apply saves the database and the old binary under `/var/backups/cryobackup/`, restarts both units, and `doctor` prints no `FAIL`. `cryo-console version` prints the version you installed and `doctor` shows schema 8. The board, devices and history are as before. C11 now reads "no self-backup has been written" (step 2 clears it). |
| Look | **System health** (every row says why), the journal, `cryo-console doctor` line by line. |
| Fail if | A device or customer disappears; any `FAIL` line; the old binary is not saved; the console does not start. Roll back with what the script prints, and read "What a rollback must remove first" before you touch config.json. |
| Evidence | |

## Step 2. Give the self-backup a prefix (operator [OP], sudo)

| | |
| --- | --- |
| Do | Give the admin storage key its rights on `<bucket>/<self-backup prefix>/*` (upgrade notes, "Turn the self-backup on"). In `/etc/cryobackup/maintain.env` add `CRYO_MAINTAIN_SELF_PREFIX=<self-backup prefix>` (not beginning with the device prefix). `sudo systemctl restart cryo-maintain`. Leave `CRYO_MAINTAIN_MAX_STREAMS` at its default of 2. |
| Expect | With a valid prefix the worker starts and the "Database self-backup" row stops saying `not_configured`. A prefix that breaks the rule is a configuration error: **`cryo-maintain` does not start**, prints the reason on stderr (exit 2), and systemd keeps restarting it, so the journal shows the same refusal again and again. The rule refuses a prefix equal to the device prefix, inside it, beginning with it or containing it; any prefix while `CRYO_MAINTAIN_PREFIX` is empty; and a segment with characters other than letters, digits, `.`, `_` and `-`. A refused prefix does not reach the job row or C11: there is no worker running to write them. |
| Look | `journalctl -u cryo-maintain` (the refusal line, or a clean start); `systemctl status cryo-maintain`; the System page's "Database self-backup" row. |
| Fail if | The worker accepts a prefix that begins with the device prefix; a refused prefix lets the worker start; a valid prefix leaves the worker failing; the row stays "no self-backup has been written (not_configured: ...)" after a pass with a valid prefix. |
| Evidence | |

## Step 3. Wait for a pass (operator or anyone)

| | |
| --- | --- |
| Do | Wait for the worker's next scheduler pass (a minute or two). Do nothing else. |
| Expect | The System page shows the time of the first self-backup. The C11 banner is gone. The job row `db-self-backup` shows success with a size. |
| Look | The System page; `sudo -u cryobackup sh -c 'set -a; . /etc/cryobackup/maintain.env; RCLONE_CONFIG=/etc/cryobackup/rclone-admin.conf cryo-console self-backup list --master-key /etc/cryobackup/master.key'` prints one snapshot with a time (UTC), a size and an id, never file contents. |
| Fail if | C11 stays raised without a stated class; the list is empty; a job row says `no_slot`. |
| Evidence | |

## Step 4. Verify the self-backup (operator [OP], sudo)

| | |
| --- | --- |
| Do | `sudo -u cryobackup sh -c 'set -a; . /etc/cryobackup/maintain.env; RCLONE_CONFIG=/etc/cryobackup/rclone-admin.conf cryo-console self-backup verify --config /etc/cryobackup/config.json'`. Then do the four object-storage checks of ADR 0029 (E2). |
| Expect | Exit 0. The verify restored the newest copy, ran `PRAGMA integrity_check`, compared schema, row counts and sha256 with the manifest, unwrapped one escrowed key with the master key and ran `restic check --read-data` on the self-backup repository. The System row shows the verdict and its age. |
| Look | The command's output (counts, never contents); the System page. |
| Fail if | Exit 1 or 2; a wrong master key does not give class `wrong_password`; the row reads ok without a verdict. |
| Evidence | |

## Step 5. Install the agent on the lab VM and seed samples (operator [OP])

| | |
| --- | --- |
| Do | From an elevated prompt on the lab VM: `msiexec /i cryobackup-agent-0.4.0.msi /qn TOKEN=<enrollment token> SERVER=https://<console agent address> /l*v C:\Windows\Temp\cryobackup.log` (the token is hidden in the log). Run the seed script below. Give the device a policy that backs up `C:\LabRestore`, start a backup now, and wait for it to finish. |
| Expect | The device enrolls (escrowed key), `Get-Service CryoBackup` is Running, the device page shows agent 0.4.0, the first backup is green. The seed script printed a manifest of 9 files and the ACL of the custom-ACL file. |
| Look | The device page; `C:\ProgramData\CryoBackup\logs\agent.log`; `Get-AuthenticodeSignature` of the MSI says Valid (a release build) or the build report says unsigned. |
| Fail if | Enrollment fails; the backup is not green; the version is not 0.4.0. |
| Evidence | |

The seed script (run elevated on the lab VM; creates the sample tree, a long path over 260 characters, names with `[` and
`$`, and a file with a custom ACL, and writes a SHA-256 manifest **outside** the backed-up tree):

```powershell
# --- settings ---------------------------------------------------------------
$Root     = 'C:\LabRestore'                  # what the lab policy backs up
$Evidence = 'C:\LabRestore-Evidence'         # NOT backed up: the hash manifest lives here
$GuestSid = '*S-1-5-32-546'                  # BUILTIN\Guests, by SID so the language does not matter

# --- discovery ----------------------------------------------------------------
if (Test-Path -LiteralPath $Root) { Write-Output "$Root exists already: remove it or pick another root"; exit 2 }

# --- act ----------------------------------------------------------------------
New-Item -ItemType Directory -Force $Evidence | Out-Null
$rows = New-Object System.Collections.ArrayList
$rng  = New-Object System.Random 22
function New-LabFile($path, $bytes) {
    $dir = [System.IO.Path]::GetDirectoryName($path)
    [void][System.IO.Directory]::CreateDirectory($dir)
    $buf = New-Object byte[] $bytes
    $rng.NextBytes($buf)
    [System.IO.File]::WriteAllBytes($path, $buf)
    $sha = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash
    [void]$rows.Add([pscustomobject]@{ Path = $path; Sha256 = $sha })
}

New-LabFile "$Root\docs\report.docx" 1048576
New-LabFile "$Root\docs\notes.txt" 4096
# a name with [ and $ : the include file has to escape both
New-LabFile "$Root\data\[Q3] `$budget (final).txt" 8192
# a path over 260 characters, written through the \\?\ prefix
$deep = "$Root\long"
1..12 | ForEach-Object { $deep = "$deep\" + ('d' * 24) }
New-LabFile ("\\?\" + $deep + '\deep-file.bin') 16384
# a custom ACL: an explicit entry added after the folder was created (Guests: read)
New-LabFile "$Root\acl\protected.txt" 2048
& icacls "$Root\acl\protected.txt" /grant "${GuestSid}:(R)" | Out-Null
# the zip drill: two folders and one file
New-LabFile "$Root\zipset\folderA\a1.bin" 300000
New-LabFile "$Root\zipset\folderA\sub\a2.bin" 120000
New-LabFile "$Root\zipset\folderB\b1.bin" 500000
New-LabFile "$Root\zipset\single.bin" 250000

# --- scrub / verify / report ------------------------------------------------------
$rows | Export-Csv -NoTypeInformation -Path "$Evidence\hashes.csv"
Write-Output "seeded $($rows.Count) files; manifest $Evidence\hashes.csv"
Write-Output "ACL of the custom-ACL file (keep this output as evidence):"
& icacls "$Root\acl\protected.txt"
exit 0
```

Save the script as a `.ps1` file and run it with `powershell -NoProfile -ExecutionPolicy Bypass -File <file>.ps1`. Both
helpers end with `exit`, so pasting them into an open console window would close it.

Keep the printed `icacls` output of `C:\LabRestore\acl\protected.txt`: the Guests entry is what steps 6 and 7 look for.

**Pin the snapshot.** Write down the id and time of the snapshot this backup made (device page). Steps 6, 7, 9 and 10 delete
or change files and then restore: **choose that snapshot in the wizard, not the newest.** A scheduled backup that runs after
you delete a file makes a newer snapshot without it, and restoring from that one proves nothing. (Or pause the lab policy's
schedule from here until step 11.)

The compare script, used in steps 6 to 8, 10, 11 and 13. Set `$Where` (and `$From` and `$Only` for a zip) at the top; it prints
`EQUAL`, `DIFFERS` or `MISSING` for every file in the manifest and exits 1 when any file is not equal:

```powershell
# --- settings ---------------------------------------------------------------
$Manifest = 'C:\LabRestore-Evidence\hashes.csv'
$From     = 'C:\LabRestore'          # the folder the manifest paths start with
$Where    = 'C:\LabRestore'          # where to compare: the same folder, a side folder, or an unzipped folder
$Only     = ''                       # optional: only manifest paths containing this text, e.g. 'zipset'

# --- discovery ----------------------------------------------------------------
if (-not (Test-Path -LiteralPath $Manifest)) { Write-Output "no manifest: $Manifest"; exit 2 }
$rows = Import-Csv $Manifest
if ($Only -ne '') { $rows = $rows | Where-Object { $_.Path -like "*$Only*" } }

# --- act / verify -------------------------------------------------------------
$bad = 0
foreach ($r in $rows) {
    $rel = $r.Path
    if ($rel.StartsWith('\\?\')) { $rel = $rel.Substring(4) }
    $rel = $rel.Substring($From.Length)
    $p = $Where + $rel
    if ($p.Length -gt 240) { $p = '\\?\' + $p }
    if (-not (Test-Path -LiteralPath $p)) { Write-Output "MISSING  $rel"; $bad++; continue }
    $h = (Get-FileHash -LiteralPath $p -Algorithm SHA256).Hash
    if ($h -eq $r.Sha256) { Write-Output "EQUAL    $rel" } else { Write-Output "DIFFERS  $rel"; $bad++ }
}

# --- report -------------------------------------------------------------------
Write-Output "$($rows.Count) compared, $bad not equal"
if ($bad -eq 0) { exit 0 } else { exit 1 }
```

## Gate between steps 5 and 6: the agent's SYSTEM checks (operator [OP], elevated)

Run [`docs/lab/p22-agent-system.md`](p22-agent-system.md) on the lab VM: its script builds the restore package's test binary
and runs it once as SYSTEM through a one-shot scheduled task (from an **elevated PowerShell**, repository root, Go installed),
then fill in that file's checklist for ADR 0027 lab checks 1 to 11 (the blind-check case under a profile, owner restore, links
and junctions, the roots, the descriptor rule, a second volume, the free-space refusal, the hold, mount points and cloud
placeholders). Steps 6 to 13 below can be run in parallel with it, but **no device restore is run on a production machine
until all eleven pass.** If one fails, that device mode does not ship in the pilot and a new ADR says why; the zip is not
affected.

| | |
| --- | --- |
| Expect | The test binary ends `ok`; every `LAB n` line of checks 5, 6, 7 and 11 is in the log; the checklist table has a result and evidence for all eleven checks. |
| Evidence | |

## Step 6. Delete a file, restore it to its original place, keep existing files (operator or anyone)

| | |
| --- | --- |
| Do | On the lab VM: record `icacls C:\LabRestore\docs` and `icacls C:\LabRestore\acl`. Delete `C:\LabRestore\docs\report.docx` and `C:\LabRestore\acl\protected.txt`. In the console: device, **Restore files**, the snapshot of step 5 (by its id or time), select the folders `docs` and `acl`, destination **Original location, keep existing files**, review, enter the authenticator code if asked, **Start the restore**. Wait for **RESTORED**. Then set `$Where = 'C:\LabRestore'` in the compare script and run it; run `icacls C:\LabRestore\acl\protected.txt` and `icacls` on both folders again. |
| Expect | Status RESTORED with counts "N restored, M skipped because they already exist": `report.docx` and `protected.txt` restored, `notes.txt` skipped (record the numbers). The compare script prints `EQUAL` for every file. `protected.txt` has the Guests read entry back, and its owner is what it was. The two folders' permissions and attributes are identical to the record from before (their last-write time may have changed: a file was added). |
| Look | The restore status page; `agent.log` (no path in it); Event Log source `CryoBackup`, event 1020. |
| Fail if | A file differs; the custom ACL is lost; a folder's permissions changed; a path appears in the log or the console. |
| Evidence | |

## Step 7. Modify a file, restore with replace (operator or anyone)

| | |
| --- | --- |
| Do | Append a line to `C:\LabRestore\docs\notes.txt`. In the wizard choose the snapshot of step 5, select `docs`, destination **Original location, replace changed files**; on review **type the device's host name** (first type it wrong and see the refusal); start. Wait for the result. Then append another line to `notes.txt`, hold it open (`$f = [IO.File]::Open('C:\LabRestore\docs\notes.txt','Open','Read','Read')` in PowerShell on the VM), restore `docs` again with replace, and only afterwards run `$f.Close()`. |
| Expect | First run: `notes.txt` is back to the manifest hash (`EQUAL`), the unchanged `report.docx` is counted skipped. A wrong host name is refused and nothing is queued. Second run with the file held open: **RESTORED WITH ERRORS**, class `target_locked` count 1, the rest placed; after `$f.Close()` a third restore replaces it. |
| Look | The class table on the status page; the log says only counts. |
| Fail if | The file is replaced before the host name matches; the held file is overwritten or the restore hangs. |
| Evidence | |

## Step 8. Restore into a new folder (operator or anyone)

| | |
| --- | --- |
| Do | Select `acl` and `data`, destination **A new folder on <host>** (the wizard puts the lab VM's host name there), start. Note the restore id on the status page. On the VM, from an elevated prompt, run the commands under this table with that id. Compare with `$Where = 'C:\CryoBackup-Restore\<restore id>\C\LabRestore'` and `$Only = 'acl'`, then `'data'`. As `labuser` (no administrator rights) try `Get-Content` on a file in it. |
| Expect | The layout is `C:\CryoBackup-Restore\<restore id>\C\LabRestore\acl\...` and `...\data\...`. The folder is open only to Administrators and SYSTEM. The owner list is exactly `BUILTIN\Administrators`. Files are `EQUAL`; the name with `[` and `$` is intact. `labuser` is refused (Access is denied). |
| Look | The status page text: "The files are in the CryoBackup-Restore folder ... named after this restore". |
| Fail if | Another owner, another trustee in the permissions, or `labuser` can read. |
| Evidence | |

```powershell
dir C:\CryoBackup-Restore\<restore id>\C\LabRestore
icacls C:\CryoBackup-Restore\<restore id>
Get-ChildItem C:\CryoBackup-Restore\<restore id> -Recurse -Force |
  ForEach-Object { (Get-Acl -LiteralPath $_.FullName).Owner } | Sort-Object -Unique
```

Cancel-and-clean check (optional but worth doing): start a side restore of a few GB, then cancel it. There is no cancel button on
the restore pages; the restore id is its run id, so use the API (operator role; an API token works, no second factor is asked
here): `curl -sS -X POST -H "Authorization: Bearer $CRYO_TOKEN" https://<admin address>/api/v1/runs/<restore id>/cancel`.
Confirm that `C:\CryoBackup-Restore\<restore id>` is gone afterwards (a side restore that does
not succeed removes its folder, ADR 0031) and the run ends cancelled.

## Step 9. Reboot in the middle of a restore (operator [OP], lab VM)

| | |
| --- | --- |
| Do | Put a few GB of incompressible data in `C:\LabRestore\big\` (the seed script's `New-LabFile` with a large size, or any large random files), back it up, delete it, start a restore of `big` from that new snapshot (not the step-5 one, which lacks it) to the original place, and **as soon as the status shows RUNNING** restart the VM (`Restart-Computer -Force`). |
| Expect | After the VM is back and the agent is up: the restore reads FAILED, class `interrupted` ("cut short"), and is **not run again by itself**. `C:\.cryobackup-restore\<restore id>` is gone (the agent removes leftovers at start). Event Log 1021. The next scheduled backup runs normally. Running the restore again places only what is missing. |
| Look | The status page; `agent.log` (start-up sweep line, no path); the list of runs for this device (one restore run with this id). |
| Fail if | The restore starts again after the boot; the status stays RUNNING; a staging folder is left. |
| Evidence | |

## Step 10. VM off: queue a restore (operator [OP], lab VM power)

| | |
| --- | --- |
| Do | Delete `C:\LabRestore\docs\notes.txt`. Shut the VM down. Wait 3 minutes (the console calls a device offline after 2 without a heartbeat). Create a restore of `docs` from the snapshot of step 5 to the original place. Read the review step. Start it. Watch the status page for a few minutes. Power the VM on. |
| Expect | The review step says the device is offline, since when, and until when the request waits (24 hours). The status page reads **QUEUED**, "<host> is offline. Last seen ... Starts when online, until <expiry>. For files now, use Download as zip." After power-on and the agent's check-in (about a minute) it becomes RUNNING, then RESTORED. `notes.txt` is back (`EQUAL`). |
| Look | The status page; the device's last-seen time. |
| Fail if | The page says nothing about the device being offline; the restore does not start after power-on; it starts twice. |
| Evidence | |

## Step 11. VM off: a zip of two folders and one file (operator [OP], lab VM power)

| | |
| --- | --- |
| Do | Shut the VM down. In the wizard select the folders `zipset\folderA` and `zipset\folderB` and the file `zipset\single.bin`, destination **Download as a zip**, continue, press **Download**. Unzip it on your own machine (`Expand-Archive`). Compare: copy the manifest to that machine, set `$From = 'C:\LabRestore\zipset'`, `$Where = '<the unzip folder>'`, `$Only = 'zipset'`. |
| Expect | The zip holds `folderA\a1.bin`, `folderA\sub\a2.bin`, `folderB\b1.bin` and `single.bin` and nothing else (no `CRYOBACKUP-SKIPPED.txt`: there are no links). All four files are `EQUAL`. Status page: DELIVERED. The device was off the whole time. |
| Look | Nothing was left on the console host (`df` before and after; no new file under the data or artifact directories). |
| Fail if | A hash differs; an entry has a drive letter or `..`; the download asks the device for anything. |
| Evidence | |

## Step 12. Stop the worker in the middle of a download (operator [OP], sudo)

| | |
| --- | --- |
| Do | Select the large `big` folder and start its zip download. While it is downloading, on the production host `sudo systemctl stop cryo-maintain`. Look at what the browser saved. Then `sudo systemctl start cryo-maintain` and download again. |
| Expect | The browser reports the download failed. What it saved is **not a usable zip**: opening it with `Expand-Archive` or `unzip -t` fails (the table of contents is only written after every part has been read). The status page reads "The last download did not finish (class: <class>). No zip was saved: a part-file in the browser's downloads is not a usable zip." (class `disconnected` when the console saw the browser stream end). The System page shows the worker banner (C07) while it is stopped and clears when it starts. The second download is complete and `EQUAL`. No path appears in the console or worker journal. |
| Look | `journalctl -u cryo-console -u cryo-maintain`; the status page. |
| Fail if | A cut download opens as a zip; the console keeps part of it; the worker's restic child stays running after the stop. |
| Evidence | |

## Step 13. Retire the device: download works, device restore refused, Verify now passes (operator)

| | |
| --- | --- |
| Do | Retire the lab device (device page, Retire; ADR 0016). Open **Restore files**. Download `zipset\single.bin` as a zip and compare it. Look at the device destinations. Then press **Verify now** on the device page (where your build shows no such button, use the API call below), or from a terminal with an admin token in `$CRYO_ADMIN_TOKEN`: `curl -sS -X POST -H "Authorization: Bearer $CRYO_ADMIN_TOKEN" https://<admin address>/api/v1/devices/<device id>/escrow/verify`. |
| Expect | The wizard says "This device is retired: download only" and offers only the zip. The zip downloads and is `EQUAL`. The device destinations are greyed out with the reason "This device is retired: it can be downloaded from, not restored to". (A device restore posted with an admin API token is not a test of this: a token gets 403 `mfa_step_up_required` before the retired check, because every restore needs a fresh session. Only a signed-in session past the second-factor step reaches 409 `device_retired_download_only`.) Verify now answers `"verdict":"verified"` and says it proves the key opens the repository's config, not that the data is intact. The audit log has `device.escrow.verify` without a path. |
| Look | The device page (escrow row, verification time); the fleet board raises no alarm for the retired device. |
| Fail if | A device restore is accepted for a retired device; the zip is refused; Verify now answers `not_verified` or `failed` on a healthy repository. |
| Evidence | |

Also, once, on any escrowed device: let the verifier run for a pass (or press Verify now) and confirm "verified" with a
fresh time; change nothing else.

## Step 14. The console drill (operator [OP], a clean Linux VM)

Follow [`docs/runbooks/console-disaster-recovery.md`](../runbooks/console-disaster-recovery.md) section 5 exactly, on a new
Linux VM, with the offline kit and nothing else from production.

| | |
| --- | --- |
| Do | (1) Install the binaries and the offline kit (section 3, steps 1 and 2). (2) `cryo-console self-backup restore ...` (section 3, step 3). (3) Place the files (step 4). (4) Set `"drill": true` in `config.json` **before anything starts** and run `cryo-console doctor`. (5) `systemctl start cryo-maintain cryo-console`, never the gateway. (6) Through an SSH tunnel, sign in, open a device that had backups before the snapshot, browse a real snapshot, download one small file as a zip, open it. (7) Check that no mail arrived anywhere. (8) Destroy the VM, disks included. |
| Expect | `self-backup restore` ends `ok` (manifest sha256, integrity check, row counts, master key id, one escrowed key unwrapped). Doctor prints a line that starts `WARN  drill  on: drill copy.` and goes on to say what drill mode does. The journal shows "DRILL COPY: this console is a restored copy." `ss -ltn` shows no listener on the agent port. **Every page, the sign-in page included, carries the red DRILL COPY strip.** The file in the zip equals the original (record both SHA-256 values). No mail from the drill copy reached any recipient and the agents kept reporting to the production console. On the production host, list the browsed device repository's `locks/` folder with rclone before and after the drill's browse and download (command under this table): both listings are empty (reads ran with `--no-lock`). |
| Look | The tunnel's browser; the production host's journal during the drill (nothing from the drill copy); the production console's System page (unchanged). |
| Fail if | The strip is missing on any page; a listener is open on the agent port; a mail arrives; the drill writes anything to storage; the file differs. If drill mode cannot be confirmed in step 4, stop after step 3: steps 1 to 3 already prove the kit opens the backup. |
| Evidence | |

The lock check needs no repository password, only the admin remote (run it on the production host, as the worker's user):

```sh
sudo -u cryobackup rclone --config /etc/cryobackup/rclone-admin.conf lsf admin:<bucket>/<prefix>/<device repository>/locks/
```

Record the date, the snapshot id and the result. Repeat the drill after every change to the offline kit.

## Step 15. The pilot client: install the agent, first backup green (operator [OP])

Only after E1 to E4 have passed. The pilot client holds **no PHI** and its keys are **escrowed** (ADR 0026).

| | |
| --- | --- |
| Do | Create the pilot client as a customer, with an escrowed key and the policy the client will run. Install the agent MSI on the client's PC (`TOKEN` and `SERVER` as in step 5). Start the first backup. **That day is day 1:** write it here and in the exit criteria above. |
| Expect | The device enrolls, the first backup finishes green, the board is green, the self-backup is current (C11 clear), the escrow row says verified. |
| Look | The board; the device page; System health. |
| Fail if | Any alarm stays raised that you cannot explain; the key is customer-held; the client holds PHI. |
| Evidence | Day 1 = |

## Step 16. The pilot client's restore drill (operator [OP])

| | |
| --- | --- |
| Do | Ask the client to name a file. Restore it to its original place (step 6 on the client's PC, after deleting a copy or restoring to a new folder if the client prefers) and download a zip of two folders and a file the client names (step 11). Compare each against the original by SHA-256 (`Get-FileHash -Algorithm SHA256`) with the client present. |
| Expect | Both comparisons are equal. The client confirms the file opens. |
| Look | The restore status pages; `agent.log` on the client's PC. |
| Fail if | A hash differs; the client's file is replaced without being asked; a path is in any log. |
| Evidence | |

Repeat the drill once during the pilot, with another file.

## Step 17. Exit criteria (operator)

Written above, before day 1. At the end of the pilot (day 45 at the latest) fill them in.

| | |
| --- | --- |
| Do | (1) Fill in the outage table. (2) Count the SLA tally from the run history. (3) Confirm steps 6 to 13 and 16 passed. (4) Confirm step 14 passed after the last change to the kit. (5) Search for paths with the commands under this table; the sentinel names are `LabRestore`, `deep-file`, `budget` and the client's file names from step 16. Search every mail the console sent and the audit log (export, or list through the API) for the same words. |
| Expect | No sentinel name and no `X:\` path in the console and worker journals, the audit log or any mail; the device's own `agent.log` carries counts, classes and the restore id, never a file name. All five criteria hold. |
| Fail if | Any criterion does not hold. A silent miss is a failure of the pilot, not a footnote. |
| Evidence | |

On the production host, with the sentinel names filled in:

```sh
sudo journalctl -u cryo-console -u cryo-maintain --no-pager --since <day 1> \
  | grep -n -i -E '<sentinel 1>|<sentinel 2>|(^|[[:space:]"=(])[A-Za-z]:\\'
```

The last alternative matches a Windows drive path (`C:\...`) after the start of the line, a space, a quote, `=` or `(`, so a
URL (`https://...`) does not match it. A path with forward slashes is not caught by it; the sentinel names catch those.

On each device, the same words in `C:\ProgramData\CryoBackup\logs\agent.log`.

## Summary

| Step | Result | Date | Operator |
| --- | --- | --- | --- |
| E1 browse latency | | | |
| E2 storage checks (ADR 0029) | | | |
| E3 SYSTEM checks 1 to 11 | | | |
| E4 = step 14 | | | |
| 1 upgrade, doctor | | | |
| 2 self-backup prefix | | | |
| 3 first self-backup, C11 clear | | | |
| 4 self-backup verify | | | |
| 5 install agent, seed, backup | | | |
| 6 restore original, keep | | | |
| 7 restore replace | | | |
| 8 restore into a new folder | | | |
| 9 reboot mid-restore | | | |
| 10 device off, queue | | | |
| 11 device off, zip | | | |
| 12 worker stopped mid-download | | | |
| 13 retire, download, verify now | | | |
| 14 console drill | | | |
| 15 pilot client, first backup | | | |
| 16 pilot client, restore drill | | | |
| 17 exit criteria | | | |
