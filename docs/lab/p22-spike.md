# P22-S1 spike - restic and rclone behaviours the pilot slice assumes

Spike for the P22 pilot slice (plan section F). No product code. Run on 2026-11-24 on one
Windows 11 workstation, as a NON-elevated user, against a throwaway rig. The rig scripts are in
[`test/lab/p22/`](../../test/lab/p22/); every command below is quoted from the rig's call log
with rig paths made generic (`<rig>` = the scratch directory, `R:` = a `subst` drive standing in
for a volume such as `C:`). Passwords were random, made at run time, passed in the environment
and never printed; the scratch directory was deleted at the end.

**Timing caveat.** The workstation ran other build and test work during the whole spike (CPU at
100 % on 8 logical processors, a Dropbox client and several editors busy). Every millisecond
figure below is inflated by that load and is useful only for comparisons inside one table.
Object-storage latency is NOT measured anywhere in this document: the rig stores on local disk.

## Versions

| component | version | where |
| --- | --- | --- |
| restic | 0.19.1 (`restic version`: `restic 0.19.1 compiled with go1.26.4 on windows/amd64`) | winget package `restic.restic`, file `restic_0.19.1_windows_amd64.exe` |
| rclone | v1.75.1 (`rclone version`) | winget package `Rclone.Rclone`, `rclone-v1.75.1-windows-amd64\rclone.exe` |
| OS | Windows 11 Enterprise 26H2, build 10.0.26300 | workstation, NTFS, `LongPathsEnabled` = 0 |
| Go (htpasswd helper only) | 1.27.0 toolchain, module `go 1.26.10`, `GOPROXY=off` | `go run ./test/lab/p22/htpasswd.go` |
| tar / python (archive readers) | GNU tar 1.35 (Git for Windows), Python 3.12 `tarfile`/`zipfile` | item 6 |

Both binaries equal the pins (`deploy/windows/restic.pin`, `docs/lab/m0-run.md:13`).

## The rig

| part | how |
| --- | --- |
| storage | an rclone `local` remote: `[store]` (gateway) and `[admin]` (maintain path) both point at `<rig>\store\bucket\cryo` |
| gateway | `rclone serve restic --addr 127.0.0.1:19443 --cert <rig>\secrets\gateway.crt --key <rig>\secrets\gateway.key --min-tls-version tls1.2 --htpasswd <rig>\secrets\htpasswd --append-only --private-repos -vv --log-file <rig>\log\gateway.log store:<rig>/store/bucket/cryo` - the flags of `deploy/rclone/gateway-flags.md`; differences: loopback address instead of `:9443` (no firewall prompt on a workstation), `-vv --log-file` added for evidence |
| TLS | self-signed P-256 pair for `IP:127.0.0.1` (openssl), `RESTIC_CACERT` on the client |
| logins | two device logins, `spike-dev-01` and `spike-dev-02`, hashed by `internal/console/gateway.HashPassword` (bcrypt cost 10, `$2y$`) through `test/lab/p22/htpasswd.go` |
| device repository | `rest:https://spike-dev-01:<pw>@127.0.0.1:19443/spike-dev-01/`, created through the gateway (`init`, exit 0) |
| maintain path | `rclone:admin:<rig>/store/bucket/cryo/spike-dev-01` with `--no-cache -o rclone.program=<rclone, forward slashes> -o "rclone.args=serve restic --stdio"` (`internal/restic/env.go` AdminOptions, `maintain/worker.go:324-329`) |
| sample data (`R:\data`) | nested folders; a 378-character path; names with `[ ] $ % ! { }` and a backtick, spaces and non-ASCII (`café üñï 日本`); a file and a folder with a protected custom DACL; a file with an alternate data stream; an empty folder; a 1 GiB random file; 20 000 small files (`many\flat` = 2 000, `many\d01..d18` = 1 000 each). `*` and `?` are not legal in NTFS names, so no file can carry them. |
| first backup | `restic backup --host spike-dev-01 --json -- R:\data` through the gateway: exit 0, `files_new` 20024, `dirs_new` 39, `data_added_packed` 1 076 730 649, 1 636 s under the load above |

## Summary

| item | answer | one-line evidence | design consequence (plan section B) |
| --- | --- | --- | --- |
| 1 restore / ls / dump / snapshots through the append-only gateway, device login | **YES** | all exit 0; 1 lock object while running, 0 after; lock refresh every 5 min accepted; `forget` and another login's read = 403; the gateway log at `-vv` records no 403 | Mode B can use the device's own login unchanged; refusals are visible only in restic's stderr |
| 2 original-location forms; include relativity; existing parents | **NO** (drive-root form) | includes are relative to the subfolder; the drive-root form reset mtime and DACL of every existing folder on the path (an ACE added after the backup was removed) and changed DACLs in an unselected branch; the restore target itself is never touched | restore per anchor folder (`<snap>:/<L>/<anchor> --target <L>:\<anchor>`), not per drive root |
| 3 include patterns with `[ * ? $` | **PARTIAL** | no escape char; `[[]` and `[$]` work; matching is case-sensitive; `--include-file` expands `$VAR` / `${VAR}` (not `%VAR%`); CRLF, BOM, comments handled; lines trimmed | include file: UTF-8, LF, `/relative`, `[`->`[[]`, `$`->`[$]`, exact case; refuse leading/trailing-space names |
| 4 `--overwrite` modes, JSON summary, locked file | **PARTIAL** | fields `total_files files_restored files_skipped total_bytes bytes_restored bytes_skipped`, zeros omitted; directories counted; no match = exit 0 empty summary; locked file = error line with absolute path, exit 1, rest restored; `never` does not open it | count from `-vv` `verbose_status` on the agent; "nothing matched" = FAILED; classify errors locally, send counts only |
| 5 ACL, owner, long path, ADS (non-elevated; SYSTEM not tested) | **PARTIAL** | DACL + own-SID owner identical; 378-char path and empty folder restored; ADS lost; under a new folder the snapshot DACL stays explicit | side-folder ACL does not cover restored files: reset the subtree after restore, or reword C2; document ADS loss |
| 6 `dump -a tar` / `-a zip` | **PARTIAL** | streams into a pipe, PAX for long / non-ASCII names, names = full snapshot path without `/`, no entry for the dumped folder, one path per call, file ignores `-a`, junction = symlink; gateway killed: restic retried 20+ min without exiting; rclone child killed: exit 1 in 2 s; partial tar detectably truncated | Mode A tar route holds (no cut line); rename entries, add the folder's own entry, skip symlinks, idle watchdog on stdout, abort on exit != 0 or tar error |
| 7 browse latency, `ls --no-cache`, 5 and 30 snapshots | **PARTIAL** | 10-60 s per click on this loaded PC through both paths; load noise exceeds the 5 -> 31 index-file effect; 2 000 entries come back in one call; object storage NOT measured | one folder per call, console pages the stream; operator measures on the bucket (item 8.4); tmpfs cache stays the lever |
| 8 real object storage | **not tested here** | ticket: operator only | checklist in "Item 8" below; note the IAM prefix-wildcard trap for `<prefix>-console` |
| 9 self-backup via rclone:, forget --prune, check --read-data, bare-host restore | **YES** | all exit 0 with the maintain options; prune removed 2 snapshots / 4 packs; bare restore sha256-equal with only the rclone file and the password | P22-C / P22-F as planned; restore with `latest:<staging path>` to avoid the nested absolute path |

## Item 1 - restore and reads through the append-only gateway with the device login

All calls below ran with `RESTIC_REPOSITORY=rest:https://spike-dev-01:<pw>@127.0.0.1:19443/spike-dev-01/`.
Lock objects were counted every 20-50 ms on the disk behind the gateway (`.../spike-dev-01/locks`).

| command | exit | evidence |
| --- | --- | --- |
| `restic backup --host spike-dev-01 --tag item1 --force --json -- R:\data\many\d01 ... d04` | 0 | 158 s; at most **1** lock object while running (2 084 samples), **0** after exit |
| `restic snapshots --json` | 0 | 0 lock objects after |
| `restic ls <snap> /R/data/nested` | 0 | `/R/data/nested`, `/R/data/nested/a`, `/R/data/nested/top.txt` (one level) |
| `restic dump <snap> /R/data/nested/top.txt` | 0 | stdout `top` |
| `restic restore --json <snap> --target <rig>\restore\item1 --include /R/data/nested` | 0 | 1 lock object while running, 0 after; the 3 files restored |
| `restic forget <snap>` | 3 | `Remove(<snapshot/90766e961f>) failed: unexpected HTTP response (403): 403 Forbidden` |
| second login `spike-dev-02` on `/spike-dev-01/`: `restic cat config` | 1 | `Stat(<config/>) failed: unexpected HTTP response (403): 403 Forbidden` (`--private-repos`) |

- The 27-minute first backup shows the lock refresh through `--append-only`: the gateway log has a new
  `spike-dev-01/locks/<id>: size = 186 OK` every 5 minutes (11:36:53, 11:41:55, 11:46:55, ...) and the
  lock directory never held more than one object, so the old lock's `DELETE /locks/<id>` was accepted
  each time.
- The gateway log at `-vv` holds **no line for a refused request**: 0 of 522 lines contain `403`
  although two requests above were refused. Lock deletions are not logged either. A refusal is
  visible only in restic's stderr on the client.

**Answer: YES.** Restore, ls, dump and snapshots work through the append-only gateway with the device
login; locks are created and removed; reading another device's repository is 403.

## Item 2 - original-location forms, include relativity, existing parents

### Include patterns are relative to the restored subfolder

| restore source and include | restored |
| --- | --- |
| `<snap>:/R/data/nested --include /a/b` | `\a\b\c\deep.txt` |
| `<snap>:/R/data/nested --include a/b` (no leading slash) | `\a\b\c\deep.txt` |
| `<snap>:/R/data/nested --include /R/data/nested/a/b` | nothing; exit 0, summary `{"message_type":"summary"}` |
| `<snap>:/R --include /data/nested/a/b` (drive-root form) | `\data\nested\a\b\c\deep.txt` |
| `<snap>:/R --include /R/data/nested/a/b` | nothing; exit 0, empty summary |

### restic re-applies snapshot metadata to EXISTING directories on the path

Controlled run (`02b-parent-acl.ps1`) on a fresh tree `R:\fresh\{p1\{p2\f.txt, sib.txt}, other\o.txt}`:
back up, then delete `f.txt`, add an extra ACE (Guests read) to `p1`, set the mtime of `fresh`, `p1`,
`p2` to 2001-02-03, restore ONLY `f.txt`.

What the backup recorded (`restic cat tree <snap>:/R/fresh`, `generic_attributes.windows.security_descriptor`
decoded): `D:(A;;0x1301bf;;;AU)(A;OICIIO;SDGXGWGR;;;AU)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)`
for `p1`, whose live DACL was entirely inherited (`ID` flags). **The inherited ACEs are recorded as
explicit ones** (run as a non-elevated user; SYSTEM not tested).

| form | command | `fresh` | `p1` | `p2` | `other\o.txt` (not selected) |
| --- | --- | --- | --- | --- | --- |
| D drive-root | `restic restore --json <snap>:/R --target R:\ --include /fresh/p1/p2/f.txt` (exit 0) | mtime reset to the snapshot value; DACL rewritten (explicit copies + inherited) | mtime reset; DACL rewritten, **the Guests ACE added after the backup is gone** | mtime reset; DACL rewritten | **DACL changed** (explicit copies of its inherited ACEs added) |
| E parent folder | `restic restore --json <snap>:/R/fresh/p1/p2 --target R:\fresh\p1\p2 --include /f.txt` (exit 0) | untouched | untouched (Guests ACE kept) | DACL untouched (Guests ACE kept); mtime = now (the file was created in it) | untouched |
| F grandparent | `restic restore --json <snap>:/R/fresh/p1 --target R:\fresh\p1 --include /p2/f.txt` (exit 0) | untouched | untouched | mtime reset; DACL rewritten (Guests ACE gone) | untouched |

The restore target directory itself is never changed (drive root in D, `p2` in E, `p1` in F); every
directory BELOW the target on the way to a selected item is. `restore -vv --json` lists them as
`{"action":"restored","item":"\\R\\data","size":0}` and `{"action":"restored","item":"\\R","size":0}`.
The first, uncontrolled runs showed the same at scale: after the drive-root restores of items 2
(`one.txt` into `R:\data\parent\keep`) and 5, the never-restored `R:\data\many\flat\f00001.txt` carried explicit copies of
its inherited ACEs.

**Answer: NO** for the drive-root form `<snap>:/<L> --target <L>:\` as an original-location restore:
include patterns are relative to the subfolder (as expected), but restic resets mtime and DACL of
every existing parent directory below the target and the change reaches files in unselected
branches. With `<snap>:/C --target C:\` and a selection under `C:\Users\x\Documents`, the parents are
`C:\Users`, `C:\Users\x` and `Documents`; as SYSTEM those writes succeed (inference, SYSTEM not tested).

## Item 3 - include patterns with special names

`P22VAR=p22value` was in restic's environment. Every case restored into its own empty side folder.

| pattern (`--include` argument unless marked "file") | exit | restored |
| --- | --- | --- |
| `/R/data/names/bracket [ab].txt` | 0 | nothing (`[ab]` is a character class) |
| `/R/data/names/bracket \[ab].txt` | 0 | nothing (no backslash escape on Windows) |
| `/R/data/names/bracket [[]ab].txt` | 0 | `bracket [ab].txt` |
| `/R/data/names/bracket [[]ab[]].txt` | 1 | `Fatal: --include: invalid pattern(s) provided` |
| `/R/data/names/[x].txt` / `[[]x].txt` | 0 / 0 | nothing / `[x].txt` |
| `/R/data/names/x]y.txt` | 0 | `x]y.txt` (a lone `]` is literal) |
| `$P22VAR`, `%P22VAR%`, `!`, space, `{a,b}`, non-ASCII in arguments | 0 | the literal file each time |
| `/r/data/names/a.txt` (drive letter lower case) | 0 | nothing: **matching is case-sensitive** |
| `/R/data/names/A.TXT` | 0 | nothing: case-sensitive for file names too |
| file: `/R/data/names/$P22VAR.txt` | 0 | `p22value.txt`: **`--include-file` expands `$VAR`** |
| file: `/R/data/names/dollar ${P22VAR}.txt` | 0 | nothing (`${VAR}` expanded too) |
| file: `/R/data/names/dollar $$P22VAR.txt` | 0 | `dollar $P22VAR.txt` (`$$` is a literal `$`) |
| file: `/R/data/names/dollar [$]P22VAR.txt` | 0 | `dollar $P22VAR.txt` |
| file: `/R/data/names/percent %P22VAR% 100%.txt` | 0 | the literal file (`%VAR%` is not expanded) |
| file with LF, CRLF, UTF-8 BOM, BOM + CRLF (two names) | 0 | both names in all four |
| file: non-ASCII name (UTF-8) | 0 | the file |
| file: `# comment`, blank line, then a name | 0 | the name (comments and blank lines skipped) |
| file: `/R/data/names/a.txt ` (trailing space) | 0 | `a.txt` (line is trimmed) |
| file: exact list `bracket [[]ab].txt`, `[[]x].txt`, `dollar [$]P22VAR.txt`, `percent %P22VAR% 100%.txt`, non-ASCII | 0 | all five, nothing else |

`*` and `?` cannot occur in an NTFS name, so they need no escape on a Windows device. Not tested: names
with leading spaces (legal on NTFS; the trimming above suggests they cannot be matched literally).

**Answer: PARTIAL.** There is no escape character; `[`, `$` need the class forms `[[]` and `[$]`,
patterns are case-sensitive and `--include-file` expands environment variables. Safest form for an
exact list: one `--include-file`, UTF-8 without BOM, LF, one line per selected item, each written
relative to the restore subfolder with a leading `/`, the exact case from the snapshot listing,
every `[` written `[[]` and every `$` written `[$]` (in that order); no other character needs
treatment. A selection whose name begins or ends with whitespace is refused (or tested first).

## Item 4 - `--overwrite` modes, the `--json` summary, a locked target file

Folder `/R/data/names` (13 files) restored again and again into one side folder:

| run | exit | `--json` summary |
| --- | --- | --- |
| first, empty target (default `always`) | 0 | `{"total_files":14,"files_restored":16,"total_bytes":253,"bytes_restored":253}` |
| again, `--overwrite never` | 0 | `{"total_files":1,"files_restored":3,"files_skipped":13,"bytes_skipped":253}` |
| again, `if-changed` / `always` (nothing changed) | 0 | same as `never` |
| `a.txt` changed, `never` | 0 | skipped 13; `a.txt` keeps the changed content |
| `a.txt` changed, `if-changed` | 0 | `{"total_files":2,"files_restored":4,"files_skipped":12,"total_bytes":11,"bytes_restored":11,"bytes_skipped":242}`; content back |
| same size, mtime 2001, `if-newer` | 0 | restored (snapshot copy is newer) |
| `a.txt` held open by another process with `FileShare.None`, `always` | **1** | summary as `if-changed` above; stderr `{"message_type":"error","error":{"message":"open \\\\?\\<rig>\\restore\\item4\\R\\data\\names\\a.txt: The process cannot access the file because it is being used by another process."},"during":"restore","item":"\\R\\data\\names\\a.txt"}` then `{"message_type":"exit_error","code":1,"message":"Fatal: There were 1 errors"}` |
| same lock, `if-changed` | 1 | same error |
| same lock, `never` | 0 | skipped, no error (the file is not opened) |

Field names: `total_files`, `files_restored`, `files_skipped`, `total_bytes`, `bytes_restored`,
`bytes_skipped` (plus `seconds_elapsed`; `files_deleted` exists only with `--delete`, never used).
Fields that are zero are left out: a restore whose include matched nothing prints
`{"message_type":"summary"}` and exits **0**. `total_files` and `files_restored` count directories:
the 13-file folder gives `files_restored: 16` (13 files + `\R`, `\R\data`, `\R\data\names`), and every
repeat run reports those 3 directories as restored again. `restore -vv --json` adds one
`verbose_status` line per item with `action` `restored` / `unchanged` and the item path.

**Answer: PARTIAL.** The modes behave as documented and a locked file is a per-file error with exit 1
while the rest is restored; but the summary cannot be shown as "N restored, M skipped" raw: it counts
directories, and "nothing matched" is exit 0. The error text carries the absolute target path.

## Item 5 - ACL, owner, long path, ADS, empty folder (as a non-elevated user; SYSTEM NOT tested)

Restore of `acl`, `long`, `ads`, `empty` into a NEW side folder, then of `custom.txt` and the ADS
file into the original location:

| object | result |
| --- | --- |
| `acl\custom.txt` (protected DACL, explicit ACEs) | owner and SDDL identical to the source (`D:PAI(A;;FR;;;WD)...`), new folder and original location |
| `acl\customdir` (protected, inheritable ACEs) and its child | SDDL identical |
| `acl\plain.txt` (inherited-only at backup time) | restored with the recorded ACEs as **explicit** entries plus the new parent's inherited ones |
| 378-character path (`LongPathsEnabled` = 0) | restored, content equal |
| empty folder | restored |
| alternate data stream `withstream.txt:extra` (23 bytes) | **not restored**: only `:$DATA` (11 bytes), new folder and original location; the stream is not in the snapshot (item 6 tar shows 11 bytes too) |

Owner was the same user that ran the restore; setting a different owner needs SeRestorePrivilege
and was not tested.

**Answer: PARTIAL.** DACL, owner (own SID), long path and empty folder are restored; alternate data
streams are lost (not backed up by restic 0.19.1 on this run). Files restored under a NEW folder keep
the snapshot's DACL as explicit ACEs: the side folder's own ACL is appended, it does not replace them.

## Item 6 - `restic dump -a tar` / `-a zip`

Each archive was written by `restic dump` into a Git Bash pipe and parsed by
`test/lab/p22/archive_list.py` (Python `tarfile` in stream mode `r|`, `zipfile`):

| command | restic exit | result |
| --- | --- | --- |
| `restic dump -a tar <snap> /R/data/names` | 0 | 13 entries `R/data/names/<name>`: full snapshot path **without** the leading `/`; the dumped folder and its parents are not entries; mode 0666, empty uname; the non-ASCII name carries a PAX `path` record |
| `restic dump -a tar <snap> /R/data/long` | 0 | 4 directory entries + the file; names longer than 100 bytes carry PAX `path` records (no GNU LongLink) |
| `restic dump -a tar <snap> /R/data/nested` | 0 | sub-folders are `dir` entries (`R/data/nested/a`, `.../a/b`, `.../a/b/c`), files after their folders |
| `restic dump -a tar <snap> /R/data/empty` | 0 | a valid empty archive (1 024 bytes, 0 entries) |
| `restic dump -a tar <snap> /R/data/ads` | 0 | 1 entry, 11 bytes: no ADS |
| `restic dump -a zip <snap> /R/data/names` | 0 | 13 entries, same names; every file entry uses a data descriptor (written without seeking); UTF-8 flag only on the non-ASCII name; `testzip` clean |
| `restic dump -a zip <snap> /R/data/long` / `nested` | 0 | directory entries end in `/`, files deflated |
| `restic dump -a zip <snap> /R/data/empty` | 0 | a valid empty zip (22 bytes) |
| `restic dump -a tar <snap> /R/data/nested/top.txt` (a FILE) | 0 | the raw 3 bytes `top`: `-a` is ignored for a file, no archive |
| `restic dump -a tar <snap> /R/data/nested /R/data/names` | 1 | `Fatal: no file and no snapshot ID specified`: one path per call |

Non-regular entries: `R:\data\nonreg` held a junction (`mklink /J`, no privilege needed), a hard link
pair, and no file symlink (`mklink` refused: "You do not have sufficient privilege to perform this
operation"; no Developer Mode). `restic backup ... -- R:\data\nonreg` exit 0; `restic ls --json`
shows the junction as `"type":"symlink"` and both hard-link names as ordinary files.

| command | restic exit | result |
| --- | --- | --- |
| `restic dump -a tar --tag nonreg latest /R/data/nonreg` | 0 | `hardlink.txt` file 6 B, `junction` **symlink** entry, `target.txt` file 6 B |
| `restic dump -a zip --tag nonreg latest /R/data/nonreg` | 0 | the junction becomes a 47-byte stored file entry (its link target as content); the hard links two ordinary files |

Repository unreachable mid-stream (`restic dump -a tar <snap> /R/data/big`, the 1 GiB file, to a file):

| path | how | result |
| --- | --- | --- |
| gateway | gateway process killed after 55 009 177 bytes | restic did **not** exit: stderr repeats `Load(<data/f09b0ee7fd>, ...) returned error, retrying after 2.3s: ... connectex: No connection could be made because the target machine actively refused it.` with growing back-off; no byte written after the kill; still running after **1 204 s**, then killed by the rig. The partial file: 1 tar header (`R/data/big/big.bin`, size 1 073 741 824), then `ReadError: unexpected end of data` |
| rclone: backend (maintain path) | the `rclone serve restic --stdio` child of restic killed after 55 858 575 bytes | restic exit **1** after 2 s: `Load(<data/d10b4e4cad>, ...) failed: rclone stdio connection already closed`, then `Remove(<lock/5562613a15>) failed: rclone stdio connection already closed` (the lock object stays in the repository); partial tar truncated the same way |

Not tested: an object-storage outage behind a LIVE rclone (rclone's own retries would then decide).

**Answer: PARTIAL.** tar and zip stream into a pipe without seeking, carry PAX records for long and
non-ASCII names, include sub-folders and non-regular entries, and a cut stream is detectably truncated;
but `dump` takes one path only, a file ignores `-a`, the dumped folder has no entry of its own (an
empty selected folder yields an empty archive), junctions come out as symlinks, and with the gateway
gone restic retries for at least 20 minutes without exiting.

## Item 7 - browse latency, `ls --json --no-cache`, one level (LOCAL DISK; object storage NOT measured)

Each figure is one complete restic process (start, key derivation, snapshot, every index file, the
trees on the path), three cold calls in a row. Through the gateway: `restic --no-cache ls --json
<snap> <dir>`; through the maintain path: `restic --no-cache -o rclone.program=<rclone> -o
"rclone.args=serve restic --stdio" ls --json <snap> <dir>`. Baseline = `cat config` with the same
options (process start + key + one small object). All exits 0; node counts include the folder itself.

| round | repository | path | baseline `cat config` ms | `/R/data` (9 entries) ms | `many\flat` (2 000) ms | `many\d01` (1 000) ms |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | 5 snapshots, 5 index files, 66 packs | gateway | 13 163 / 11 768 / 13 545 | 19 549 / 14 821 / 21 303 | 24 820 / 23 638 / 22 844 | 28 799 / 17 875 / 19 266 |
| 1 | same | rclone: | 6 413 / 5 898 / 7 206 | 21 004 / 17 594 / 17 351 | 17 292 / 19 899 / 27 677 | 20 931 / 16 986 / 27 594 |
| 2 | 30 snapshots, 31 index files, 117 packs | gateway | 4 061 / 5 020 / 3 646 | 11 529 / 13 317 / 12 217 | 15 965 / 16 263 / 14 632 | 16 918 / 18 464 / 17 395 |
| 2 | same | rclone: | 6 390 / 7 582 / 4 888 | 12 100 / 10 182 / 20 599 | 61 580 / 29 033 / 26 716 | 36 186 / 36 327 / 13 009 |

The 25 extra snapshots of round 2 each backed up a small folder with 50 changed files (a full backup
of the 20 000 files took about 10 minutes per run under the load), so each added one index file and
one or two packs: the index-file count, which every uncached call reads in full, grew from 5 to 31.

What the numbers show, and what they do not:

- On this PC every browse click costs 10 to 60 seconds; the `cat config` baseline alone moved from
  12-13 s (round 1) to 4-5 s (round 2) with the background load, so the rounds cannot be compared
  and the effect of 5 -> 31 index files is not separable from the noise.
- `ls` of one folder level costs a few seconds above the baseline even on local disk; the 2 000-entry
  folder returned 2 001 nodes in one call (no paging in restic: the console's offset of B.1 has to page
  the JSON stream itself).
- Each backup run adds an index file and nothing merges them before a `prune` (none in the pilot,
  decision C7): a device policy of 4 backup runs a day gives some 180 index files after the 45-day pilot, all read from
  object storage by every uncached browse call.

**Answer: PARTIAL.** Measured, through both paths, with 5 and 30 snapshots, but the absolute numbers
are dominated by this PC's load and nothing here measures object-storage latency.

## Item 8 - real object storage: NOT tested here (operator)

Nothing in this rig touches Wasabi or any S3 endpoint. The operator must check, on the production
account, before P22-C ships:

1. **Admin key on the self-backup prefix.** With the `[admin]` remote, `restic -o rclone.program=... -o "rclone.args=serve restic --stdio" --no-cache -r rclone:admin:<bucket>/<self_prefix> init`, then `backup`, `forget --prune --keep-last 1`, `check --read-data` (item 9 commands): all exit 0. This needs list, get, put and delete on `<bucket>/<self_prefix>/*`.
2. **The gateway key cannot reach the self-backup prefix.** As a device login through the gateway, `GET /<self_prefix>/config` must fail; and in the IAM policy the gateway key's resources must end in a slash (`arn:aws:s3:::<bucket>/<prefix>/*`). A resource written `<bucket>/<prefix>*` also matches `<bucket>/<prefix>-console/...`: the plan's example name `<prefix>-console` is only safe with the trailing slash, or choose a self-backup prefix that does not start with the gateway prefix.
3. **Cost of `forget --prune` every 6 h under versioning and the 90-day minimum.** Item 9's prune of 2 snapshots deleted 4 packs and rewrote the index (3 old index files deleted). Under bucket versioning each deleted object stays as a non-current version and Wasabi bills deleted objects for 90 days from upload. Read the bucket's versioning state, the lifecycle rule for non-current versions on `<self_prefix>/`, and estimate (database size x copies kept for 90 days). Lock objects (186 bytes) are created and deleted on every call: check Wasabi's minimum-object-size billing for them.
4. **Browse latency against the bucket** (item 7 measured local disk only): `restic --no-cache ls --json <snap> <folder>` through the admin path on a real device repository, three calls, cold.
5. **Production gateway policy is prefix-scoped** (plan F.8): read the policy attached to the gateway key; it is not visible from the repository.

## Item 9 - self-backup through the rclone: backend, prune, check, bare-host restore

Repository `rclone:admin:<rig>/store/bucket/cryo-console` (a separate prefix, decision C3), password
from a file standing in for the HKDF-derived one. Every call carried the maintain worker's options:
`--retry-lock 30s --no-cache -o rclone.program=<rclone> -o "rclone.args=serve restic --stdio"`.

| command | exit | evidence |
| --- | --- | --- |
| `restic ... cat config` | 10 | no repository yet |
| `restic ... init` | 0 | `created restic repository 3a8843bdf1 at rclone:admin:<rig>/store/bucket/cryo-console` |
| `restic ... backup --host console --json -- <rig>\selfstage` (3 rounds, 2 MiB database + config + manifest, all changed each round) | 0 x3 | `files_new` 3, then `files_changed` 3; `data_added_packed` about 2.1 MB each |
| `restic ... forget --prune --keep-within 7d --keep-daily 30 --keep-monthly 12` (C4) | 0 | `keep 3 snapshots` (all within 7 d) |
| `restic ... forget --prune --keep-last 1` | 0 | `remove 2 snapshots`; prune: `to delete: 22 blobs / 4.007 MiB`, `removing 4 old packs`, `3 / 3 old indexes deleted`; warning `running prune without a cache, this may be very slow!` |
| `restic ... check --read-data` | 0 | `no errors were found` |
| bare-host restore: new environment holding only `PATH`, `SystemRoot`, `RCLONE_CONFIG=<copy of the rclone file>`, `RESTIC_PASSWORD_FILE`, an empty `TEMP`; `restic ... -r rclone:admin:<rig>/store/bucket/cryo-console restore latest --target <rig>\bare-host\out` | 0 | `Restored 9 files/dirs (2.000 MiB)`; `console.db` sha256 equal to the last staging copy; no cache directory created |

The snapshot stores the staging directory's absolute path: the bare restore produced
`out\<drive letter>\<every folder of the staging path>\selfstage\console.db`.

**Answer: YES.** Backup, retention with prune, full read check and a restore with nothing but the
rclone file and the password all work through the rclone: backend with the maintain options.

## What each NO / PARTIAL means for plan section B, and the smallest fallback

**Item 2 (NO) - B.1 Mode B "restic restore per drive root `<snap>:/<L>` + `--target`".** As
written, restoring one file to its original location rewrites mtime and DACL of every existing
folder between the drive root and the file, with the snapshot's flattened ACEs, and the change
spreads into unselected branches. Smallest fallback: restore per **anchor folder**, the deepest
folder that exists on the device above the selected item (normally its parent):
`restic restore --json <snap>:/<L>/<anchor> --target <L>:\<anchor> --include-file <F>` with `<F>`
listing the selected items under that anchor relative to it (item 3 form). The anchor itself is never
touched (2b runs E and F); folders restic has to create below it get their snapshot metadata, which is
what a restore of a deleted folder should do. Group selections by anchor (one restic child per anchor,
sequential, as the design already runs one child at a time); each call pays one index load (the same
cost the plan already accepts for the multi-path zip). The side-folder mode may keep the drive-root
form: everything below `<SystemDrive>\CryoBackup-Restore\<restore id>\` is created by restic.

**Item 3 (PARTIAL) - the include file.** The agent writes the `--include-file` as UTF-8 without BOM,
LF, one `/<relative path>` per line, `[` -> `[[]` then `$` -> `[$]`, exact case. P22-A's selection
validation must keep names byte-exact (no case folding, no Unicode normalisation) and should refuse
names with leading or trailing whitespace until a test against the real binary proves a form for them.

**Item 4 (PARTIAL) - RunStats and statuses.** `files_restored` / `files_skipped` cannot be copied from
the summary: directories are counted, and a selection that matched nothing is exit 0 with an empty
summary. Smallest fallback: run `restore --json -vv`, count `verbose_status` lines locally (`restored`
or `updated` on a non-directory target = restored, `unchanged` = skipped; the agent checks the type
with a local `Lstat`), never forward the `item` path; take `bytes_restored` from the summary. A run
whose selection matched nothing is FAILED with its own class (fail loudly), never DELIVERED. Error
lines carry the absolute path: classify them on the agent (`being used by another process` ->
in-use, `Access is denied` -> access, other) and send counts per class only. Exit 1 with files
restored = RESTORED WITH ERRORS, as planned. Related: a killed restic leaves its lock object in the
device repository (seen twice: item 6's capped dump, item 7's stopped backup); later backups and
restores still ran past it (non-exclusive), but the post-pilot `prune` / `check` (exclusive) will
meet it: after a cancel or kill the agent should run `restic unlock` (allowed through the
append-only gateway, L-APPEND-1).

**Item 5 (PARTIAL) - side folder and ADS.** Restored files keep the snapshot DACL as explicit ACEs,
so the C2 side-folder ACL (Administrators + SYSTEM) does not replace the files' own permissions; on
Windows, "Bypass traverse checking" is granted to Everyone by default, so a user whose ACE is on a
restored file may open it by full path even without rights on the side folder (inference, not
tested here). Smallest fallback: after a side-folder restore the agent resets the subtree to inherit
from the side root only (e.g. `icacls <side>\<restore id> /reset /T /C`) and checks one file;
or the operator accepts "a restored copy carries the original's permissions" and C2 is reworded.
Alternate data streams are not restored: say so in the restore page's help and the runbook; the Phase
0 matrix row for ADS stays open.

**Item 6 (PARTIAL) - Mode A zip.** The tar route is suitable; the cut line ("single file/folder
only") is not needed. The builder must: run one `dump` per selected path (as planned; several paths
are refused); choose raw `dump` for a file and `dump -a tar` for a folder from the node type the
browse listing already has (restic ignores `-a` for a file); rename each tar entry from the full
snapshot path `<L>/<parent>/<name>/...` to `<name>/...` (relative to the selected item's parent);
write a directory entry for the selected folder itself (restic emits none, so an empty folder would
otherwise vanish); read PAX records (Go `archive/tar` does); skip symlink entries (junctions) with a
count, rather than writing a link target as file content as `dump -a zip` does. Abort without the
central directory on restic exit != 0 OR a tar read error (both observed on a cut stream). Add an
idle watchdog on restic's stdout (no byte for N seconds -> kill restic -> abort the response): with
the gateway gone restic retried for 20+ minutes without exiting, and an S3 outage behind a live
rclone was not tested. The same watchdog is needed in the agent's Mode B restore through the gateway
(no new `status` line for N minutes -> stop restic, FAILED with a storage class), followed by
`restic unlock` because a killed restic leaves its lock.

**Item 7 (PARTIAL) - browse.** One restic process per click, each loading every index file uncached
(ADR 0008), costs seconds even on local disk; on object storage it is the operator's measurement
(item 8.4). Smallest steps that keep ADR 0008: (a) one browse = one `ls --json` of ONE folder, never
`--recursive`, the console pages the JSON stream (500 per page) and keeps no copy; (b) the UI shows a
"Reading the backup..." state on every click and a timeout that names the storage, not an empty
folder; (c) the plan's tmpfs cache stays the lever if the operator's figure is too slow; (d) the
index-file growth (one per backup run, merged only by `prune`) is one more reason the post-pilot
forget/prune tickets come first.

## Not tested here, and who must

| what | why not here | who |
| --- | --- | --- |
| everything as SYSTEM (the agent's account): ACL and owner restore with SeRestorePrivilege, what the backup records as SYSTEM (inherited ACEs flattened or not), writes to `C:\Users`-level parents | no elevation on this PC (ticket) | P22-D worker on the lab VM, then lab script steps 6-8 by the operator |
| side-folder creation with an Administrators + SYSTEM DACL, and whether a user can open a restored file in it by full path | needs SYSTEM / a second account | P22-D on the lab VM |
| object storage latency, versioning cost, IAM (item 8) | no remote storage (ticket) | operator |
| reboot mid-restore, VM offline queueing | no VM (ticket) | operator, lab script steps 9-10 |
| file symlinks in the snapshot | `mklink` needs SeCreateSymbolicLinkPrivilege or Developer Mode, absent here | lab VM |
| names with leading spaces in `--include-file` | not in the sample set | P22-D unit test against the real binary |

## Rig scripts

`test/lab/p22/`: `rig.ps1` (shared variables and helpers), `00-setup.ps1`, `01-gateway.ps1`,
`02-original-location.ps1`, `02b-parent-acl.ps1`, `03-include-patterns.ps1`, `04-overwrite.ps1`,
`05-acl-ads-long.ps1`, `06-dump.ps1`, `07-browse.ps1`, `09-selfbackup.ps1`, `99-teardown.ps1`,
`htpasswd.go` (build-tagged `ignore`), `archive_list.py`. Windows PowerShell 5.1, no elevation;
`P22_RIG_ROOT` sets the scratch directory (default `%TEMP%\cryo-p22-rig`), `CRYO_RESTIC` and
`CRYO_RCLONE` the binaries. Each script appends to `<rig>\log\calls.md`. Scripts 02, 02b and 05
modify the sample data; run `99-teardown.ps1` and `00-setup.ps1` again for a clean second run.
