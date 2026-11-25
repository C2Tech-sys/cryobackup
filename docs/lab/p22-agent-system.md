# P22-D - restore to the device as SYSTEM: lab checks of ADR 0027

Status: **every check below is NOT RUN.** The build PC of ticket P22-D gave no SYSTEM context that could be used
without a password or a download (the worker's shell was not elevated, so `schtasks /create /ru SYSTEM` was not
available). This file is the runnable script and the checklist for the lab VM. The operator runs it, records the
evidence in the table at the end and commits it. If a check fails, the device mode concerned does not ship in the
pilot and a new ADR records why; for check 10 the `GetFinalPathNameByHandle` fallback of ADR 0027 applies instead
(ADR 0027, "What spike P22-S1 showed, and what it could not").

What the non-elevated tests already show on NTFS (package `internal/agent/restoreplace`, `go test`): a junction on the
destination path is refused and nothing is written through it; a held parent and grandparent cannot be renamed
during a move; a lost no-replace race is skipped and never overwritten; an 8.3 short-name match and a stripped
trailing dot are refused; a file/folder type mismatch is refused; replace mode skips read-only and system files and
counts a locked file as `target_locked`; the moved file's descriptor drops staging's inherited entry and picks up the
new parent's, a protected one is unchanged; the clean-up walk and the side-folder reset delete junctions without
following them and leave the target's DACL unchanged; the root rule creates, verifies, refuses (grants others, not
protected, junction, wrong owner) and never repairs. Those are run as an ordinary user; the checks below are the
ones that need SYSTEM, a second account, a second volume or a real profile.

## The lab VM

- Windows 10 or 11, NTFS, a local administrator account `labadmin`, an ordinary local user `labuser` with a
  profile (log on once), a second NTFS volume `E:` (a second virtual disk), and, for check 11, a file-sync client
  with a cloud-only placeholder in `labuser`'s profile.
- The agent 0.4.0 MSI (P22-J) installed and enrolled against the lab console, its gateway reachable.
- Go and this repository at the commit under test (only to build the lab test binary).

## Script: the SYSTEM test binary through a one-shot scheduled task

Run in an **elevated** PowerShell 5.1 on the lab VM, from the repository root. Plain variables at the top; it
creates one scheduled task, runs it once, waits, prints the log, and deletes the task. Nothing else on the machine is
changed; the scratch folder is removed at the end.

```powershell
# --- settings ---------------------------------------------------------------
$Scratch   = 'C:\p22-lab'                         # removed at the end
$TaskName  = 'CryoBackup-P22-Lab-OneShot'
$UserSid   = (New-Object System.Security.Principal.NTAccount('labuser')).Translate([System.Security.Principal.SecurityIdentifier]).Value
$WaitMax   = 300                                  # seconds

# --- discovery ----------------------------------------------------------------
if (-not ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Output 'not elevated: run from an elevated PowerShell'; exit 2
}
if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) { Write-Output "task $TaskName exists already"; exit 2 }
New-Item -ItemType Directory -Force $Scratch | Out-Null

# --- act ----------------------------------------------------------------------
& go test -c -o "$Scratch\restoreplace.test.exe" ./internal/agent/restoreplace
if ($LASTEXITCODE -ne 0) { Write-Output 'build failed'; exit 3 }
$cmd = "/c set CRYO_P22_LAB_SYSTEM=1&& set CRYO_P22_LAB_DIR=$Scratch\work&& set CRYO_P22_LAB_USER_SID=$UserSid&& " +
       "`"$Scratch\restoreplace.test.exe`" -test.v -test.count=1 > `"$Scratch\system.log`" 2>&1"
$action = New-ScheduledTaskAction -Execute 'cmd.exe' -Argument $cmd
Register-ScheduledTask -TaskName $TaskName -Action $action -User 'SYSTEM' -RunLevel Highest | Out-Null
Start-ScheduledTask -TaskName $TaskName
$t = 0
do { Start-Sleep -Seconds 2; $t += 2; $info = Get-ScheduledTaskInfo -TaskName $TaskName } while ($info.LastTaskResult -eq 267009 -and $t -lt $WaitMax)

# --- scrub --------------------------------------------------------------------
Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false

# --- verify / report ------------------------------------------------------------
Get-Content "$Scratch\system.log"
$ok = Select-String -Path "$Scratch\system.log" -Pattern '^ok ' -Quiet
Remove-Item -Recurse -Force $Scratch
if ($ok) { exit 0 } else { exit 1 }
```

The binary runs every test of the package as SYSTEM: the ordinary ones (they pass the lab trustee, which is then
SYSTEM's own SID) and the three `TestLabSystem*` tests, which use the production options (no lab trustee) and the
agent's backup and restore privileges. Their `LAB n` log lines are the evidence for checks 5, 6, 7 and 11.

## Checklist (ADR 0027 lab checks 1-11)

Each restore below is started from the lab console (P22-E) against the lab VM's device, unless the row says
otherwise. "Descriptor" means `icacls <path>` output before and after, or `(Get-Acl <path>).Sddl`.

| # | Check (ADR 0027) | How | Expected evidence |
|---|---|---|---|
| 1 | The blind-check case under a user profile: a selected existing folder and its existing subfolder | As `labuser`, create `C:\Users\labuser\Documents\p1\p2\f.txt` and `g.txt`; back up; delete `f.txt`; add a Guests read ACE to `p1` and `p2`; set the mtime of `p1`, `p2` to 2001-02-03; restore `.../Documents/p1` (original, keep) | `f.txt` back; `p1` and `p2`: DACL identical to before (Guests ACE kept), attributes unchanged; their mtime may change only by the add of `f.txt` to `p2`; record success, `files_skipped` counts `p1`, `g.txt` |
| 2 | Owner restore with SeRestorePrivilege | Back up a file owned by `labuser`; delete it; restore it (original) | the restored file's owner is `labuser` (`(Get-Acl).Owner`), not SYSTEM |
| 3 | What a SYSTEM backup records for inherited entries | `restic cat tree <snap>:/C/Users/labuser/Documents/p1` through the maintain path; decode `windows.security_descriptor` | note whether inherited ACEs are stored as explicit ones (spike item 2 showed explicit for a non-elevated user) |
| 4 | Profile junctions in both destinations | Select `/C/Users/labuser/Documents` and the legacy junction `/C/Users/labuser/My Documents` (or `/C/Documents and Settings`) in one restore, original then side | original: items under the junction counted `guard_refused`, nothing written through it; side: the junction node is removed, counted skipped, its target untouched |
| 5 | Reparse rule: links in staging and side folder removed unfollowed, incl. the symbolic link restic makes as SYSTEM for a snapshot junction; whether restic applies the junction node's attributes or times through the link | Back up a folder holding a junction to `C:\p22-victim`; record `C:\p22-victim`'s DACL, mtime, attributes; restore the folder (original, then side) | `C:\p22-victim` DACL, mtime and attributes unchanged; record counts the link as skipped; `TestLabSystemSymlink` `LAB 11`/clean-up lines pass |
| 6 | Creating the roots with the protected Administrators + SYSTEM descriptor; refusing a bad root; can a user open a restored side-folder file by full path | `TestLabSystemRootRule` (`LAB 6` lines); then a side restore; as `labuser`, `Get-Content C:\CryoBackup-Restore\<id>\C\...\file` | created root `O:BA D:P(SY FA)(BA FA)`; user-owned root refused (`check=owner_not_administrators_or_system`), not changed; `labuser` gets access denied on the side file (see concern: the owner of a restored file keeps implicit READ_CONTROL/WRITE_DAC; record what happens) |
| 7 | Descriptor rule after a move | `TestLabSystemDescriptorRule` (`LAB 7` lines); and one real original restore of a file whose parent hands down an extra ACE | staging's inherited entry gone, the new parent's present; a protected descriptor unchanged |
| 8 | Destination on another volume, staging on that volume | Back up `E:\data`; delete a file; restore it (original) | `E:\.cryobackup-restore` created with the root descriptor; restic target under it; nothing written to `C:\.cryobackup-restore`; staging removed after |
| 9 | Free-space refusal, both destinations | Fill `E:` to within 10 MB (a large file); restore a 50 MB file to `E:` (original); then fill `C:` and restore the same to the side folder | FAILED `disk`; `agent.log` line with `needed_bytes` and `free_bytes`; no staging or side root written |
| 10 | The hold as SYSTEM: another process, as `labuser` and as `labadmin`, cannot rename or remove a held folder; can a held empty folder be turned into a reparse point in place; a hard link to an outside file planted on the restore path is not replaced in replace mode | Start a replace-mode restore of a large selection under `C:\Users\labuser\Documents\big`; while the agent places (watch `agent.log`), as `labuser` and as `labadmin`: `Rename-Item C:\Users\labuser\Documents\big x`, `Remove-Item`; `fsutil reparsepoint` / `mklink /J` on an empty held folder; before the restore, `mklink /H C:\Users\labuser\Documents\big\h.txt C:\p22-outside.txt` | rename and remove refused (in use); the reparse attempt either refused or caught by the re-read (`guard_refused`); `C:\p22-outside.txt` content unchanged (the link is replaced as a directory entry, the outside file keeps its bytes) |
| 11 | Name-surrogate check on a real symbolic link and a mounted folder; a cloud-file placeholder is not a surrogate | `TestLabSystemSymlink` (`LAB 11`); mount `E:` at `C:\mnt\e` and restore an item under `/C/mnt/e/...` (original); restore a file into a OneDrive folder holding placeholders | symbolic link and mount point refused (`guard_refused`); the placeholder folder is placed into normally |

Also untested in the spike and to be noted here: file symbolic links in a snapshot (`p22-spike.md:387-396`).

## Evidence (to be filled on the lab VM)

| # | Result | Evidence (log lines, descriptors, record) | Date / operator |
|---|---|---|---|
| 1 | NOT RUN | | |
| 2 | NOT RUN | | |
| 3 | NOT RUN | | |
| 4 | NOT RUN | | |
| 5 | NOT RUN | | |
| 6 | NOT RUN | | |
| 7 | NOT RUN | | |
| 8 | NOT RUN | | |
| 9 | NOT RUN | | |
| 10 | NOT RUN | | |
| 11 | NOT RUN | | |
