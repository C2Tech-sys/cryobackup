# CryoBackup engine capability sheet

**Scope:** what the open-source engine stack can and cannot deliver, so the CryoBackup feature matrix promises only what the engine does.
**Stack surveyed:** restic 0.19.1 (latest release, 2026-07-05), rclone `serve restic` and rclone S3 backend (current docs/master source), restic `rest-server` (master), `golang.org/x/sys/windows/svc`, `github.com/kardianos/service`.
**Date compiled:** 2026-10-20. Every claim carries a URL. Items marked **UNCERTAIN** are inference, forum-sourced, or not verified in a lab; test before promising them.

Legend: `[docs]` restic docs, `[src]` source file read directly, `[rel]` release notes / changelog, `[forum]` restic/rclone forum (community, not authoritative), `[ms]` Microsoft Learn.

---

## 1. RESTIC ON WINDOWS

### 1.1 VSS: `--use-fs-snapshot`

| Item | Finding | Source |
|---|---|---|
| Availability | Flag is registered only when `runtime.GOOS == "windows"`. | [src] https://raw.githubusercontent.com/restic/restic/master/cmd/restic/cmd_backup.go |
| What it snapshots | "Restic will transparently create a VSS snapshot for each volume that contains files to backup." Files are read from the snapshot, so files "exclusively locked by another process" can be read. | [docs] https://restic.readthedocs.io/en/stable/040_backup.html |
| Options | `-o vss.timeout` (default 120 s), `-o vss.exclude-all-mount-points`, `-o vss.exclude-volumes` (semicolon list), `-o vss.provider` (GUID, name, or `MS`). Added 0.17.0 (#3067). | [docs] same; [rel] https://github.com/restic/restic/releases/tag/v0.17.0; [src] https://raw.githubusercontent.com/restic/restic/master/internal/fs/fs_local_vss.go |
| Mount points | restic cannot enumerate which mount points it needs, so it snapshots every mount point under a volume unless `vss.exclude-all-mount-points`; unneeded ones can point at "locked, filled or otherwise unsupported volumes" and stall. | https://github.com/restic/restic/pull/3067 |
| Failure behaviour | **Per-volume fallback, not abort.** `snapshotPath` doc: "If creation of a snapshot fails the file's original path is returned as a fallback." On failure the volume is recorded in `failedSnapshots`, an error message is emitted, and backup continues reading the live filesystem ("try to backup without a snapshot"). Locked files then fail to read and the run ends with exit code 3. The PR author deliberately removed a provider fallback because a shadow copy can "run without errors" and still not work. | [src] fs_local_vss.go (above); https://github.com/restic/restic/pull/3067 |
| Admin requirement | restic docs do not state it. **UNCERTAIN (Windows semantics, not restic docs):** VSS requester calls need Backup Operators/Administrators; as a LocalSystem service this is satisfied. Test once as LocalSystem. | (no restic source; see 5.3) |
| Restore | VSS is not used for restore at all. | https://github.com/restic/restic/pull/3067 |
| Outlook OST | Skipped by Windows itself via registry `FilesNotToSnapshot` under `BackupRestore`, not by restic. | [docs] 040_backup |
| Known fixes | 0.17.2 #5063: extended metadata now read from the snapshot path, not the live path. 0.17.3 #5107: metadata error on VSS paths fixed. 0.18.0 #5169: Event Log 8194 warnings suppressed. | [rel] https://raw.githubusercontent.com/restic/restic/master/CHANGELOG.md |
| OneDrive / cloud files | `--exclude-cloud-files` (0.18.0, #3697) skips online-only files, which "previously caused VSS problems and forced downloads". Windows + macOS only. **Required for workstation profiles.** | [rel] CHANGELOG.md; [docs] 040_backup |

### 1.2 Long paths and odd names

- restic prepends `\\?\` to paths to exceed MAX_PATH (issue #2745 discusses side effects for reserved names like `con`; **UNCERTAIN whether #2745 is fully fixed**). https://github.com/restic/restic/issues/2745
- 0.17.1 #4953: long paths on older Windows (Server 2012) fixed. 0.18.0 #1843: timestamps for paths >256 chars restored correctly on Windows <10 1607. 0.18.0 #4938: **minimum Windows 10 / Server 2016**. https://raw.githubusercontent.com/restic/restic/master/CHANGELOG.md
- Files with invalid characters in the name cannot be read: "If the name of a file in Windows contains an invalid character, Restic will not be able to read the file." Fix is renaming. https://restic.readthedocs.io/en/stable/faq.html
- 0.19.1 #5667: inaccessible/malformed source paths are skipped; backup aborts only if no source path remains. https://github.com/restic/restic/releases
- 0.17.1 #2004: `C:` volume-name targets handled; older snapshots need `<snapshot>:<subfolder>` restore syntax. CHANGELOG.md

### 1.3 NTFS metadata: what is and is not preserved

| Object | Backup | Restore | Source |
|---|---|---|---|
| Security descriptor (owner, group, DACL, SACL) | Since 0.17.0 (#4708). Full SD needs `SeBackupPrivilege` or admin; otherwise only owner, group, DACL. | Full SD needs `SeRestorePrivilege`, `SeSecurityPrivilege`, `SeTakeOwnershipPrivilege` or admin; otherwise only DACL. 0.19.0 #5427 fixed ACL inheritance (previously restored as explicit ACEs). 0.19.0 #5424 enables privileges before any file access. | [docs] https://restic.readthedocs.io/en/stable/040_backup.html ; https://restic.readthedocs.io/en/stable/050_restore.html ; [rel] CHANGELOG.md |
| File attributes + creation time (hidden, read-only, encrypted flag, ctime) | Since 0.17.0 (#4611). | Restored. | [rel] https://github.com/restic/restic/releases/tag/v0.17.0 |
| NTFS Extended Attributes (EAs, not ADS) | Since 0.17.0 (#4807). Skipped on volumes without EA support, e.g. some network drives (0.17.1 #4980). | Restored; `--exclude-xattr/--include-xattr` filters (0.18.0 #5089). | CHANGELOG.md |
| **Alternate Data Streams (ADS)** | **NOT supported.** PR #4614 "Back up and restore Windows Alternate Data Streams" was closed unmerged on 2024-12-01 (maintainer: restore ordering constraints unsolved). No 0.17–0.19 changelog entry mentions ADS. ADS are not enumerated by normal directory reads, so they are silently omitted (no error). Zone.Identifier MoTW streams, some app metadata streams are lost. | n/a | https://github.com/restic/restic/pull/4614 ; CHANGELOG.md |
| Symlinks | Archived as symlinks, not followed. | Restoring on Windows since 0.15.0 (#1078) requires `SeCreateSymbolicLinkPrivilege` or admin. | [docs] 040_backup, 050_restore; CHANGELOG.md |
| **Junctions / reparse points** | **Target path only, never contents** (official builds treat junctions as symlinks). With Go >= 1.23 semantics (`winsymlink=1`) junctions surface as `irregular` and error "unsupported file type \"irregular\""; 0.17.2 #5057 stopped including irregular files in snapshots. Maintainer: "The link targets were never backed up"; junction support "can be added if someone comes up with a proper concept". restic 0.19.0 builds with Go 1.25. **UNCERTAIN how 0.19.1 release binaries report junctions — lab-test `%LOCALAPPDATA%\Microsoft\WindowsApps` and user-profile junctions; expect warnings, never data.** | Not recreated as junctions. | [forum] https://forum.restic.net/t/windows-junction-backup-with-go1-23-or-later/8940 ; CHANGELOG.md ; https://go.dev/issue/61893 |
| Windows Dedup (SIS) reparse files | Backed up since 0.16.3 (#4574). | — | CHANGELOG.md |
| Hard links | Restic stores them; `mount` does not restore them (use rsync -H). | **UNCERTAIN on Windows restore.** | [docs] 050_restore |
| Sparse files | Backed up as content; zero runs dedupe naturally. | `--sparse` is **off by default**; when enabled "long zero-byte runs become holes. Hole locations may differ from the original." | [docs] 050_restore |

### 1.4 Open / locked files without VSS

Unreadable files produce warnings and **exit code 3**: "restic still completes the backup, creating a snapshot that then contains all but the unreadable files." https://restic.readthedocs.io/en/stable/040_backup.html, https://restic.readthedocs.io/en/stable/075_scripting.html. Change detection on Windows is path + size + mtime only; `--ignore-inode`/`--ignore-ctime` are ignored, only `--force` has effect. [docs] 040_backup.

### 1.5 Exclusion syntax

Flags (all repeatable where noted): `--exclude`, `--iexclude` (case-insensitive), `--exclude-file`, `--iexclude-file`, `--exclude-if-present <file>[:header]` (no wildcards in file name), `--exclude-caches` (CACHEDIR.TAG; tag file itself kept), `--exclude-larger-than <size>` (bytes default; k/m/g/t = KiB/MiB/GiB/TiB), `--exclude-cloud-files`. Source: https://restic.readthedocs.io/en/stable/040_backup.html

Pattern rules (same page):
- Go `filepath.Match` syntax plus `**`; `**` must be a complete path component (`foo/**/bar`).
- Matched against the full path; patterns match whole components (`foo` matches `/dir/foo`, not `/dir/foobar`).
- Leading `/` anchors at root; trailing `/` ignored; single `*` does not cross `/`.
- `!pattern` re-includes, but files inside an excluded directory cannot be re-included (gitignore semantics).
- `$VAR` expanded with `os.ExpandEnv` (`$$` literal); `~` is **not** expanded (FAQ: `--exclude "~/documents"` silently fails).
- Exclude files: `#` comments, blank lines ignored, UTF-16/BOM accepted (0.9.0 #1433).
- 0.19.0 change #5767: excludes no longer apply to paths passed explicitly on the command line (they still filter contents inside those paths).
- **Recommendation (inference):** on NTFS use `--iexclude`/`--iexclude-file` since the filesystem is case-insensitive.

`--one-file-system`: **"Unsupported [on Windows]; the backup fails immediately with an error."** Do not emit it on Windows. [docs] 040_backup.

### 1.6 Performance flags

| Flag | Default / notes | Source |
|---|---|---|
| `--pack-size` / `RESTIC_PACK_SIZE` (MiB) | 16 MiB. Temp space = pack size x (connections + 1); memory "can also increase by a similar amount". Larger packs = fewer objects, better upload throughput on object stores; invalid values error since 0.19.0. `prune --repack-smaller-than` consolidates small packs later. | https://restic.readthedocs.io/en/stable/047_tuning_parameters.html ; 060_forget |
| `--compression` / `RESTIC_COMPRESSION` | `auto` default; `off`, `fastest`, `better` (0.19.0 #4728), `max`; repo format v2 only; per-run setting. | 047_tuning_parameters; https://github.com/restic/restic/releases |
| `--read-concurrency` / `RESTIC_READ_CONCURRENCY` | 2 (help text in cmd_backup.go); raise for NVMe. | [src] cmd_backup.go; 047_tuning_parameters |
| `--limit-upload`, `--limit-download` | KiB/s, unlimited by default; global flags, fixed for the run (no schedule). | https://restic.readthedocs.io/en/stable/manual_rest.html |
| `-o <backend>.connections` | 5 per backend (local: 2); e.g. `-o rest.connections=5`. | 047_tuning_parameters |
| `GOMAXPROCS` | All cores by default; limiting slightly reduces memory. | 047_tuning_parameters |
| `--no-extra-verify` | Disables pre-upload decode verification; then run `check --read-data[-subset]` regularly. Keep verification on. | 047_tuning_parameters |
| `--no-scan` | Skip the pre-scan size estimate (faster start, no ETA). | [src] cmd_backup.go |

### 1.7 Snapshot identity: `--host`, `--group-by`, `--tag`

- `--host` (`-H`, env `RESTIC_HOST`; CLI wins since 0.19.0 #5440) sets the hostname stored in the snapshot; default `os.Hostname()`. [src] cmd_backup.go
- `--group-by` default `host,paths` (alternatives `paths`, `host,tags`, `''`) selects the parent snapshot; `forget --group-by` "should normally match". [docs] 040_backup, 060_forget
- `--tag` adds tags; `restic tag` edits later (0.18.0 #5137 reports changed snapshots). [docs] 040_backup
- **Design consequence:** per-device identity should be an explicit `--host <stable-device-id>` (users rename PCs, reimages reuse names); customer/site/policy go in `--tag`. Changing `--host` or paths breaks parent selection and forget grouping.

### 1.8 `--dry-run`

`backup --dry-run/-n` writes nothing; with `--verbose` lists changes. `restore --dry-run` (0.17.0 #4839) with `--verbose=2` labels files unchanged/updated/restored; `forget --dry-run`; `prune --dry-run`. [docs] 040_backup, 050_restore, 060_forget

### 1.9 JSON output (`--json`) for a reporting agent

Source for all fields: https://restic.readthedocs.io/en/stable/075_scripting.html (stability: backward compatible; new fields/message types may appear; enums may gain values).

- **Streams:** JSON goes to stdout; backup/check/restore `error` messages and the final `exit_error` (`message_type`, `code`, `message`; 0.18.0 #4948) go to **stderr**. Parse both.
- **backup** (JSON lines): `status` {`seconds_elapsed`, `seconds_remaining`, `percent_done`, `total_files`, `files_done`, `total_bytes`, `bytes_done`, `error_count`, `current_files`}; `error` {`error.message`, `during`, `item`}; `verbose_status` {`action` new/unchanged/modified/scan_finished, `item`, `duration`, `data_size`, `data_size_in_repo`, `metadata_size`, `metadata_size_in_repo`, `total_files`}; `summary` {`dry_run`, `files_new`, `files_changed`, `files_unmodified`, `dirs_new`, `dirs_changed`, `dirs_unmodified`, `data_blobs`, `tree_blobs`, `data_added`, `data_added_packed`, `total_files_processed`, `total_bytes_processed`, `backup_start`, `backup_end`, `total_duration`, `snapshot_id` (omitted when `--skip-if-unchanged` skipped)}.
- **snapshots** (array): `id`, `short_id` (deprecated), `time`, `parent`, `tree`, `paths`, `hostname`, `username`, `uid`, `gid`, `excludes`, `tags`, `program_version`, `summary` (same stats as backup summary; present only for snapshots made by >= 0.17.0).
- **ls** (lines): `snapshot` record then `node` records {`name`, `type`, `path`, `uid`, `gid`, `size`, `mode`, `permissions`, `atime`, `mtime`, `ctime`, `inode`}; `--sort` since 0.18.0.
- **stats** (object): `total_size`, `total_file_count`, `total_blob_count`, `snapshots_count`, `total_uncompressed_size`, `compression_ratio`, `compression_progress`, `compression_space_saving`. Modes `--mode restore-size` (default), `files-by-contents`, `blobs-per-file`, `raw-data`, `debug` (blob histograms, good for sizing prune memory). [src] https://raw.githubusercontent.com/restic/restic/master/cmd/restic/cmd_stats.go
- **find**: array of {`hits`, `snapshot`, `matches`[{`path`, `permissions`, `name`, `type`, `atime`, `mtime`, `ctime`, `user`, `group`, `inode`, `mode`, `device_id`, `links`, `linktarget`, `uid`, `gid`, `size`}]}.
- **restore** (lines): `status` {`seconds_elapsed`, `percent_done`, `total_files`, `files_restored`, `files_skipped`, `files_deleted`, `total_bytes`, `bytes_restored`, `bytes_skipped`}; `error`; `verbose_status` (`--verbose=2`); `summary` (same counters). Verify progress bar is suppressed under `--json`.
- **forget**: array of ForgetGroup {`tags`, `host`, `paths`, `keep`[], `remove`[], `reasons`[{`snapshot`, `matches`}]}; empty when IDs passed. **`prune` has no JSON output**; `forget --prune` mixes JSON and text — run them as two invocations.
- **check** (0.18.0): `summary` {`num_errors`, `broken_packs`, `suggest_repair_index`, `suggest_prune`}; `error` messages free-text.
- **diff**: `change` {`path`, `modifier` +/-/T/M/U/?}, `statistics`. **init**: {`id`, `repository`}. **key list**: {`current`, `id`, `userName`, `hostName`, `created`}. **version**: {`version`, `go_version`, `go_os`, `go_arch`}.

### 1.10 Exit codes

| Code | Meaning | Since |
|---|---|---|
| 0 | success | |
| 1 | command failed | |
| 2 | Go runtime error | |
| 3 | `backup`: some source data unreadable (incomplete snapshot created); `forget`: one or more snapshots could not be removed (0.19.0 #5233); any missing source path (0.19.0 #4467) | |
| 10 | repository does not exist | 0.17.0 |
| 11 | failed to lock repository (conflicting lock) | 0.17.0 |
| 12 | wrong password | 0.17.1 |
| 130 | cancelled (SIGINT/SIGTERM; 0.19.0 #5258) | |

"If an unknown exit code is returned, then it MUST be treated as a command failure." https://restic.readthedocs.io/en/stable/075_scripting.html ; CHANGELOG.md

### 1.11 Resumability of interrupted backups

- No partial snapshot is ever created. restic periodically writes index files for data uploaded so far; the next run finds that data via the index and references it instead of re-uploading, "effectively picking up from the last saved index (a few minutes ago)". Until a first snapshot exists the file scan repeats. https://restic.readthedocs.io/en/stable/faq.html
- Leftover unreferenced packs are harmless; `check` reports them, `prune` removes them. faq
- `--skip-if-unchanged` (0.17.0 #662) omits the snapshot when content equals the parent; with absolute paths, parent-folder metadata changes count as changes. [docs] 040_backup
- Lock refresh: a lock that cannot be refreshed before going stale aborts the run ("Fatal: failed to refresh lock in time"); routine refresh failures are only logged. [src] https://raw.githubusercontent.com/restic/restic/master/internal/repository/lock.go
- `prune` resumes an interrupted run (0.17.0 #3806); `copy` resumes. [rel] v0.17.0; [docs] 045_working_with_repos

### 1.12 `restic restore`

Source: https://restic.readthedocs.io/en/stable/050_restore.html and https://raw.githubusercontent.com/restic/restic/master/cmd/restic/cmd_restore.go

- `--target/-t` required. Snapshot ID or `latest` (`--host`, `--path`, `--tag` pick the snapshot; `--path` does not filter files).
- Subset: `--include`/`--exclude` (mutually exclusive), `--iinclude`/`--iexclude`, `--include-file`/`--exclude-file` (0.17.0 #4781). Single file: `--include /full/snapshot/path` (find it with `ls`/`find`). Subfolder: `<snapshot>:<subfolder>` (paths then relative to it).
- `--overwrite always|if-changed|if-newer|never` (0.17.0 #4817); default `always` verifies existing content and rewrites only mismatched parts.
- `--delete` (0.17.0 #2348): removes target files absent from the snapshot; with a target of `/` requires include/exclude; use `--dry-run -vv` first.
- `--verify` ("verify restored files content"; cannot combine with `--dry-run`); `--sparse`; `--include-xattr/--exclude-xattr`; `--ownership-by-name` is non-Windows.
- Takes a read (non-exclusive) lock; `--no-lock` honoured.
- Windows: symlink and full-SD restore need privileges (1.3). An interrupted in-place restore can leave files partially written.
- Restore to an alternate location = `--target <dir>`; original path structure is recreated under it.

### 1.13 `restic mount` and `restic dump`

- `mount` is FUSE: "Supported on Linux, macOS, and FreeBSD only". **Not available on Windows.** Third-party WinFsp wrappers exist (not official). https://restic.readthedocs.io/en/stable/050_restore.html ; https://forum.restic.net/t/restic-mount-windows/898
- `dump` streams a file to stdout; directories as tar by default, `-a zip` (0.18.0 #5054), `--target` writes to a file; `<snapshot>:<subfolder> /` puts folder contents at archive root; chunks fetched in parallel (0.17.0 #3406). This is the server-side building block for "download a file/zip" restores. 050_restore

### 1.14 `restic forget` policies (GFS)

Source: https://restic.readthedocs.io/en/stable/060_forget.html ; https://raw.githubusercontent.com/restic/restic/master/cmd/restic/cmd_forget.go

- `--keep-last n`, `--keep-hourly/daily/weekly/monthly/yearly n` (`unlimited` allowed): keep the newest snapshot in each of the last n calendar periods **that contain a snapshot** (a missed day does not consume a slot; weeks Mon–Sun).
- `--keep-within 2y5m7d3h` and `--keep-within-{hourly,daily,weekly,monthly,yearly}` relative to the latest snapshot. Future-dated snapshots are ignored, not removed.
- All options OR together. Oldest snapshot is kept when a duration option has too few snapshots.
- `--keep-tag`; `--group-by` default `host,paths`; `--host/--tag/--path` filters.
- Refuses to delete the last snapshot of a group ("refusing to delete last snapshot of snapshot group") unless `--unsafe-allow-remove-all` plus a filter (0.17.0).
- `--prune` runs prune if anything was removed. Takes an **exclusive lock**; `--no-lock` only with `--dry-run`. Exit 3 if a removal fails.
- Security section: append-only protects against a compromised client; `forget`/`prune` need delete rights so they belong on "a separate, well-secured client"; an attacker adding fake snapshots can push real ones out of `--keep-*` windows, so pair policies with `--keep-within`.

### 1.15 `restic prune` and `restic check` cost

- prune: `--max-unused` (default 5%; absolute, percent or `unlimited`), `--max-repack-size`, `--repack-cacheable-only`, `--repack-smaller-than`, `--repack-uncompressed`, `--dry-run`. Exclusive lock: "During a prune operation, the repository is locked and backups cannot be completed." 0.17.0: up to 60% less memory, resumable. 0.19.0 #5293: removes small packs more aggressively. No JSON. 060_forget; v0.17.0; releases
- check: default verifies structure only (downloads snapshots/index/trees into a temporary cache deleted afterwards; `--with-cache` reuses the normal cache). `--read-data` downloads **every pack** (egress); `--read-data-subset n/t | x% | 10G`. Exclusive lock. 0.19.0 #3326 limits check to selected snapshots; #5610 lower memory. https://restic.readthedocs.io/en/stable/045_working_with_repos.html ; [src] https://raw.githubusercontent.com/restic/restic/master/cmd/restic/cmd_check.go

### 1.16 Cache

- Default on Windows: `%LOCALAPPDATA%\restic`; override `--cache-dir` / `RESTIC_CACHE_DIR`; `--no-cache`; `--cleanup-cache`. One subdirectory per repository ID with `snapshots`, `data`, `index` subdirs; the directory mtime is bumped on use. https://restic.readthedocs.io/en/stable/manual_rest.html ; https://raw.githubusercontent.com/restic/restic/master/doc/cache.rst
- `restic cache [--cleanup] [--max-age days(30)] [--no-size]` lists/removes old caches. [src] https://raw.githubusercontent.com/restic/restic/master/cmd/restic/cmd_cache.go
- Growth: proportional to repository metadata (index + tree packs); not documented numerically. **UNCERTAIN:** expect tens to hundreds of MB per workstation repo; a shared multi-TB repo pushes GBs onto every client. **Inference:** under LocalSystem `%LOCALAPPDATA%` resolves to `C:\Windows\System32\config\systemprofile\AppData\Local` — set `RESTIC_CACHE_DIR` to `%ProgramData%\CryoBackup\cache` explicitly.

---

## 2. REPOSITORY MODEL

### 2.1 Locks

| Fact | Source |
|---|---|
| Exclusive vs non-exclusive locks; only one exclusive and no others may coexist; many non-exclusive may. Lock files live in `locks/`. Removing/rewriting data needs exclusive. | https://restic.readthedocs.io/en/stable/100_references.html |
| Stale after 30 min (plus dead-PID check on the same host). Refresh every 5 min; refresh failure close to staleness aborts the operation. Lock-removal errors are only logged, never fatal. | 100_references; [src] internal/repository/lock.go |
| `backup` = `openWithAppendLock` (non-exclusive). `restore` = read lock (`--no-lock` OK). `forget`, `prune`, `check` = exclusive. `--retry-lock <duration>` waits (0.16.0 #719). `unlock` removes stale locks, `unlock --remove-all` forces. Exit 11 on conflict. | [src] cmd_backup.go, cmd_restore.go, cmd_forget.go, cmd_check.go; v0.16.0 release |

### 2.2 Concurrent backups into one repository

Supported: multiple clients hold non-exclusive locks simultaneously; each uploads its own packs and index files, and they dedupe against the shared index. The problem is maintenance: any `prune`/`check`/`forget` needs the repo to itself, so every client of that repo fails with exit 11 (or waits with `--retry-lock`) for the duration — on a shared multi-TB repo that is hours, and laptops have no predictable quiet window. [forum] "simultaneous prune/check and backup is the problem": https://forum.restic.net/t/any-way-to-get-restic-to-work-with-s3-object-locking-or-a-read-only-key/6858

### 2.3 Memory and index per client

- Every restic run loads the full repository index into RAM; memory scales with **blob count**, not snapshot count or raw size. Community rule of thumb: ~1 GB RAM per 7 million blobs; one 1.2 TB many-small-file repo had a 6–7 GB index; 80 M blobs OOM'd prune at ~40 GB. **UNCERTAIN (forum numbers, pre-0.17 for some):** https://forum.restic.net/t/memory-usage-oom-etc-on-a-big-repository/8758 ; https://forum.restic.net/t/restic-prune-runtime-out-of-memory/6902
- Blobs average ~1 MiB (512 KiB–8 MiB) for large files, but small files are one blob each, so workstation profiles with hundreds of thousands of small files generate blob counts out of proportion to bytes. 100_references
- 0.19.0 #5713 "significantly faster index loading"; 0.16.0 index 25% leaner (`GOGC=50`). releases
- Index files are kept under 8 MiB each; `supersedes` chains; prune/`repair index` rebuild them. 100_references

### 2.4 Prune at 1–10 TB

- Per-run: loads full index, classifies packs, repacks partially-used packs (download + re-upload = egress + PUTs), deletes unused. `--max-unused` trades leftover space for repack work; `--max-repack-size` bounds a run; `--repack-cacheable-only` is the cheap metadata-only path. 060_forget
- Sizing (**UNCERTAIN**, from 2.3): 1–10 TB of mixed workstation data ~ 2–20 M blobs ~ 0.3–3 GB RAM on the prune host; time is dominated by repack bandwidth. Run prune on the admin VPS, never on a workstation.
- Interrupted prune resumes (0.17.0). Run `check` after prune. 060_forget

### 2.5 Keys

- One master key pair per repository; each password is a key file that wraps the master key (scrypt-derived AES-256 + Poly1305-AES). Changing the master key requires `copy`/new repo. 100_references
- `restic key list/add/remove/passwd`, `--new-password-file`. Any key holder decrypts everything in the repo. https://restic.readthedocs.io/en/stable/070_encryption.html
- Consequence: a per-customer repo means every device of that customer holds a credential that decrypts every other device's data; adding per-device key files does not change that. A per-device repo bounds compromise to that device. (Design doc; inference stated plainly.)

### 2.6 Repository format v2 / compression

Default since 0.14.0; zstd for data and tree blobs plus index/lock/snapshot files; `--compression` per run; migrate with `migrate upgrade_repo_v2` then `prune --repack-uncompressed`. 030_preparing_a_new_repo; 045_working_with_repos; 100_references

### 2.7 Tradeoff matrix

| Dimension | One repo per device | One repo per customer | One repo for all |
|---|---|---|---|
| Dedup scope | Device only (no cross-device OS/app dedup; mitigated by excludes + zstd) | All devices of a customer | Global |
| Lock contention | prune/check block one device only | prune/check block every device of the customer for the run | Everyone, for hours |
| Client RAM / cache | Proportional to the device's own data | Proportional to the customer's total blob count, paid on every laptop | Worst |
| Prune cost | Many small prunes (parallelisable, each cheap) | Fewer, larger prunes | One huge prune |
| Key blast radius | One device | All devices of customer | Everything |
| Append-only / private-repos mapping | 1 gateway user per device = 1 repo (clean) | Shared user or per-device users on a shared path; a compromised device can still write junk/fake snapshots into siblings' history | Same, worse |
| Admin ops scale | N repos to forget/prune/check (scripted loop) | fewer | one |
| Wasabi 90-day min / repack churn | Independent per device | Shared packs repacked for any device's churn | Same |

**Recommendation:** one repo per device, one gateway user per device (`--private-repos`), one bucket (or prefix) per customer for billing, forget/prune/check run from the admin host in a loop. The cross-device dedup loss is the price; everything else gets simpler and safer.

---

## 3. RCLONE SERVE RESTIC GATEWAY

Sources: https://rclone.org/commands/rclone_serve_restic/ ; https://raw.githubusercontent.com/rclone/rclone/master/cmd/serve/restic/restic.go

### 3.1 Flags

| Flag | Semantics |
|---|---|
| `--addr` | default `127.0.0.1:8080`; repeatable; `:0` picks a port. Listening on LAN/public requires auth. |
| `--baseurl` | URL prefix when behind a proxy. |
| `--cert`, `--key`, `--client-ca`, `--min-tls-version` | TLS; `--min-tls-version` default `tls1.0` — set `tls1.2`. |
| `--user`/`--pass`, `--htpasswd` (MD5/SHA1/bcrypt; bcrypt recommended; hot-reloaded), `--realm`, `--salt`, `--user-from-header` (trusted reverse proxy only) | Auth. With auth on, every request incl. OPTIONS must authenticate. |
| `--append-only` | "Disallow deletion of repository data" — exact behaviour in 3.2. |
| `--private-repos` | Routes nest under `/{userID}`; middleware `checkPrivate` allows only when the URL user segment equals the authenticated user; anything else 403. Repo URL = `rest:https://gw/<username>/` (trailing slash mandatory; sub-repos `/<username>/sub/` allowed). |
| `--stdio` | HTTP/2 server on stdin/stdout for restic's built-in `rclone:` backend (restic spawns `rclone serve restic --stdio --b2-hard-delete <remote>`); refuses to run on a terminal. Only relevant for local/SSH use, not a network gateway. restic side: https://restic.readthedocs.io/en/stable/030_preparing_a_new_repo.html |
| `--cache-objects` (default true) | Caches objects returned by List so later HEAD/GET resolve without a backend call; `--cache-objects=false` disables. |
| `--server-read-timeout`/`--server-write-timeout` | 1h each (total transfer time). `--max-header-bytes` 4096. `--allow-origin` CORS (avoid `*`). |
| Not present | No `--max-size` quota, no Prometheus metrics, no upload integrity verification (all of which rest-server has). |

### 3.2 What `--append-only` actually prevents (from source)

- `DELETE`: rejected with **403 unless the path ends in `/locks/<name>`**. So clients can still create and remove lock files (required for normal operation), but cannot delete `snapshots/`, `data/`, `index/`, `keys/` or `config`.
- `POST` of an object that already exists: **403 "file already exists, refusing to overwrite in append-only mode"** (guarded by a per-remote upload lock).
- `POST ?create=true` (repo creation) still works; listing (`Accept: application/vnd.x.restic.rest.v2`) works.
- Effects on restic commands from a client: `backup`, `restore`, `ls`, `find`, `snapshots`, `stats`, `check` (read), `unlock` succeed; `forget` fails (snapshot delete 403 -> exit 3/1), `prune` fails, `rewrite` fails (old snapshot delete), `key remove` fails, `key passwd` fails. A compromised client can still: fill the bucket (no quota), create arbitrary new snapshots (polluting `--keep-*` windows -> use `--keep-within`), and delete *any* lock in its own repo, including an admin's exclusive lock mid-prune (narrow but real; run admin prune when the device is quiesced or accept the risk). (Inference from the source paths above.)
- rest-server `--append-only` is byte-for-byte the same policy: delete 403 unless type `locks`; overwrite of any existing blob is 403 in **all** modes; config delete 403. https://raw.githubusercontent.com/restic/rest-server/master/repo/repo.go
- Known issue (historic): the env-var form of the flag (`RCLONE_APPEND_ONLY`) once failed to enforce append-only while the CLI flag worked; use the CLI flag and lab-test with `restic forget` from a client. https://forum.rclone.org/t/serve-restic-append-only-environment-variable/17050

### 3.3 Performance characteristics (architecture, not benchmarked)

- Every restic HEAD/GET/POST/DELETE becomes one rclone operation on the remote (S3: HeadObject/GetObject/PutObject/DeleteObject); directory lists use `walk.ListR` (ListObjectsV2, 1000 keys/page) and prime the object cache. No local disk, no local copy. [src] restic.go
- restic packs (16 MiB default, up to 128 MiB) are below rclone's 200 MiB `--s3-upload-cutoff`, so each pack is a single PUT; multipart tuning is irrelevant unless pack size is raised past the cutoff. https://rclone.org/s3/
- Latency: a workstation's backup is bound by WAN upload; the gateway adds one hop. **UNCERTAIN:** no published throughput numbers for `serve restic` -> S3; measure with 20 concurrent clients in the lab.
- `--stdio` uses the deprecated `http2.Server.ServeConn` path (works; noted in source).

### 3.4 Running it

- Linux: plain systemd unit running `rclone serve restic --addr ... --cert ... --htpasswd ... --append-only --private-repos remote:bucket/path`; rclone has no serve-restic-specific service docs. rsync.net runs `rclone serve restic --stdio --append-only` behind a restricted SSH key in production. https://www.rsync.net/resources/notes/2025-q4-rsync.net_technotes.html
- Admin path for forget/prune/check: a **second, non-append-only** access path with delete rights, never exposed to clients: either `rclone serve restic` without `--append-only` bound to localhost on the admin host, or `restic -r s3:...` with the full S3 key on the admin host. The client-facing gateway's S3 credential can itself be delete-capable (restic deletes locks) — the append-only enforcement is in rclone, so protect the gateway host and its rclone.conf as the trust boundary.

### 3.5 rest-server vs rclone serve restic

| | restic rest-server (local disk) | rclone serve restic (object storage) |
|---|---|---|
| Storage | Local filesystem `--path`; needs a second copy (e.g. `rclone sync` to S3) for off-site | Direct to S3-compatible; no disk |
| Append-only | `--append-only` (locks deletable) | same |
| Private repos | `--private-repos` `/<user>/` | same |
| Auth | `.htpasswd` bcrypt/SHA; refuses to start if file missing unless `--no-auth`; `--proxy-auth-username` | htpasswd (bcrypt/MD5/SHA1), `--user/--pass`, `--user-from-header` |
| Quota | `--max-size` (0.18.1 restic stops retrying on "insufficient storage") | none |
| Integrity | Verifies uploaded pack hashes by default (`--no-verify-upload` to disable) | none at gateway (restic verifies on its own side) |
| Metrics | `--prometheus` (`/metrics`, `metrics` user or `--prometheus-no-auth`), Grafana example | none |
| TLS | `--tls --tls-cert --tls-key --tls-min-ver` (1.2 default) | `--cert/--key`, min TLS default 1.0 |
| Packaging | Single Go binary, systemd + Docker examples, `--group-accessible-repos` | rclone binary |
| Performance notes | README: better than SFTP, "simple enough for low-end devices" | one S3 call per restic object op |

Sources: https://raw.githubusercontent.com/restic/rest-server/master/README.md ; https://github.com/restic/rest-server ; https://rclone.org/commands/rclone_serve_restic/

**Verdict:** rclone serve restic for cost and zero local disk; rest-server if you want quotas, Prometheus, and server-side upload verification and are willing to run `rclone sync` to S3 as the off-site copy (3-2-1 bonus).

---

## 4. S3 BACKENDS

### 4.1 Provider facts

| | Wasabi | Backblaze B2 (S3 API) | IDrive e2 |
|---|---|---|---|
| Storage | "$7.99/TB/month" starting price on the pricing page fetched 2026-10-20; 1 TB minimum billed | $6.95/TB/month; first 10 GB free | $6/TB/month PAYG (1 TB minimum); **MSP/Veeam/Reseller plan $5/TB/month**; yearly plans 50% off first year |
| Egress | $0, no API fees | Free up to 3x average monthly storage, then $0.01/GB | Free up to 3x active storage, then $0.01/GB (page also says "free egress"; inconsistent) |
| API requests | $0 | Class A, B, C **free for pay-as-you-go** per current pricing page; Class D (event notifications) 2,500/day free then $0.004/10k | "does not impose any additional charges for your API calls" |
| Minimum storage duration | **90 days PAYG, 30 days Reserved Capacity**; early delete = "Timed Deleted Storage charge equal to the storage charge for the remaining days" | "No minimum storage duration fees", no minimum file size | No minimum duration |
| Object Lock | COMPLIANCE/GOVERNANCE; bucket default via `?object-lock` (Days or Years); legal hold; `x-amz-bypass-governance-retention` | Governance/compliance/legal hold; default retention only if Object Lock enabled at bucket creation; compliance "cannot be removed by any user" (dates extendable); no extra charge; app-key caps `readBucketRetentions`/`writeBucketRetentions` | Compliance (nobody incl. IDrive can modify/delete; cannot shorten) and Governance (owner/root or `s3:BypassGovernanceRetention` can bypass); bucket default retention; versioning; legal hold |
| Endpoint | `s3.wasabisys.com` + regional | `https://s3.<region>.backblazeb2.com`, HTTPS + SigV4 only; no IAM roles, object tagging, website config, POST uploads | provider `IDrive`, regional endpoint |
| Sources | https://wasabi.com/pricing ; https://docs.wasabi.com/docs/how-does-wasabis-minimum-storage-duration-policy-work ; https://docs.wasabi.com/docs/object-lock-with-the-wasabi-s3-api ; https://docs.wasabi.com/apidocs/object-lock-on-buckets-with-the-wasabi-s3-api | https://www.backblaze.com/cloud-storage/pricing ; https://www.backblaze.com/cloud-storage/transaction-pricing ; https://www.backblaze.com/docs/cloud-storage-object-lock ; https://www.backblaze.com/docs/cloud-storage-s3-compatible-api ; https://www.backblaze.com/docs/cloud-storage-call-the-s3-compatible-api | https://www.idrive.com/s3-storage-e2/pricing ; https://www.idrive.com/e2/object-lock ; https://idrive.com/e2/faq-dashboard |

Caveats: pricing pages change; B2's "A/B/C free" contradicts older B2 docs (2,500/day free tiers) — re-check before quoting customers. Wasabi's egress fair-use clause was not found on the pricing page fetched (**UNCERTAIN**).

### 4.2 Wasabi 90-day minimum vs prune

Every object restic deletes before 90 days (unused packs, repacked packs, superseded index files, every lock file) is billed for the remainder of 90 days; repack also writes **new** packs that start a fresh 90-day clock. Mitigations: prune no more than ~quarterly, raise `--max-unused` (e.g. 20–30%) to minimise repack, use `--repack-cacheable-only` for routine runs, and expect lock-file churn to be a rounding error (bytes). Wasabi minimum-duration policy: https://docs.wasabi.com/docs/how-does-wasabis-minimum-storage-duration-policy-work ; prune options: https://restic.readthedocs.io/en/stable/060_forget.html

### 4.3 B2 versioning and restic

restic's S3 backend only hides files on delete in B2; set lifecycle "Keep only the last version of the file" so hidden versions are purged; restic recommends the S3 API over the native B2 backend. https://restic.readthedocs.io/en/stable/030_preparing_a_new_repo.html. restic's default rclone args include `--b2-hard-delete` (native `b2:` remotes only). https://rclone.org/b2/

### 4.4 rclone S3 flags that matter

| Flag | Default | Relevance |
|---|---|---|
| `--s3-chunk-size` | 5 MiB (auto-raised to stay under 10,000 parts) | Only for objects > `--s3-upload-cutoff` (200 MiB); restic packs are smaller -> single PUT |
| `--s3-upload-concurrency` | 4 | Same; memory = transfers x concurrency x chunk size |
| `--s3-no-check-bucket` | false | Skip bucket HEAD/create; required when the gateway key lacks `CreateBucket`; `serve restic`'s create-repo calls Mkdir, so set this |
| `--s3-no-head` | false | Skip HEAD after PUT (saves a request; "increases the chance for undetected upload failures") — leave off |
| `--fast-list` | — | ~1 call per 1,000 objects at ~1 GiB RAM per million objects; `serve restic` already lists recursively |
| `--s3-list-chunk` | 1000 | Listing page size |
| `--s3-disable-checksum` | false | Skips MD5 metadata; leave on |
| `--s3-versions` / `--s3-version-at` | — | Point-in-time view of a versioned bucket (recovery after a bad prune) |
| Object Lock | `--s3-object-lock-mode` + `--s3-object-lock-retain-until-date` (both required; `365d`/RFC3339), `--s3-object-lock-set-after-upload`, `--s3-bypass-governance-retention`, `--s3-bucket-object-lock-enabled` | Applies to every upload through that remote, lock files included |

Source: https://rclone.org/s3/

### 4.5 Can restic + rclone use S3 Object Lock safely?

- restic itself has no Object Lock support; the only lever is bucket default retention or rclone's per-upload flags, which lock **every** object including `locks/*`. forum consensus: "restic uses locks which do need a deletion after the operation is done. If you can configure your object lock on subpaths, exclude /locks" — S3 default retention cannot be scoped to a prefix. https://forum.restic.net/t/restic-malware-and-object-locks/7079 ; https://forum.restic.net/t/wasabi-compliance-bucket-and-lockfiles/2318
- **COMPLIANCE mode: not safe.** Lock deletes fail (non-fatal, only logged — locks pile up until stale at 30 min and are then ignored), but `prune`/`forget`/`rewrite`/`key remove` cannot delete and there is no bypass; the repo accumulates unreferenced data until retention expires. Restorability guarantee is also weaker than it looks: a snapshot is restorable only while *every* pack it references is still locked, and repack issues new objects with new clocks. https://forum.restic.net/t/anyone-using-restic-with-b2-backblaze-and-object-locks/4277
- **GOVERNANCE mode: plausible, untested here.** Client-facing gateway without bypass (cannot delete anything, locks included -> stale-lock litter, trivial cost on B2/IDrive, 90-day billed bytes on Wasabi); admin host with `--s3-bypass-governance-retention` runs forget/prune/unlock. **UNCERTAIN**: whether an exclusive lock can be acquired with undeletable stale locks present (design doc says stale locks are ignored; `unlock` with bypass clears them). Lab-test with a 1-day retention before promising.
- **v1 recommendation:** append-only gateway (3.2) + bucket versioning with a lifecycle delete window (B2 "hide" + lifecycle; Wasabi/IDrive versioning) + gateway host hardening. Advertise Object Lock immutability as "roadmap, governance mode", not v1.

---

## 5. GO WINDOWS SERVICE

### 5.1 `golang.org/x/sys/windows/svc`

Sources: https://pkg.go.dev/golang.org/x/sys/windows/svc ; https://raw.githubusercontent.com/golang/sys/master/windows/svc/service.go ; https://learn.microsoft.com/en-us/windows/win32/api/winsvc/nc-winsvc-lphandler_function_ex

- Pattern: implement `Handler.Execute(args []string, r <-chan ChangeRequest, s chan<- Status) (svcSpecificEC bool, exitCode uint32)`; call `svc.Run(name, handler)` (which calls `StartServiceCtrlDispatcher`; `serviceMain` registers the control handler via `RegisterServiceCtrlHandlerEx`). Report `Status{State: Running, Accepts: ...}` on `s`; loop on `r`.
- `ChangeRequest{Cmd, EventType uint32, EventData uintptr, CurrentStatus, Context}`; the control handler forwards `(ctl, evtype, evdata)` verbatim and always returns 0 (NO_ERROR), so the service cannot veto a power event (irrelevant for a backup agent).
- `Cmd` values include `Stop, Pause, Continue, Interrogate, Shutdown, ParamChange, PowerEvent, SessionChange, PreShutdown, DeviceEvent, HardwareProfileChange`; `Accepted` flags include `AcceptPowerEvent`, `AcceptSessionChange`, `AcceptPreShutdown`, `AcceptPauseAndContinue`.
- **Yes, a Go service receives suspend/resume and session changes** once it sets `AcceptPowerEvent`/`AcceptSessionChange`: for `SERVICE_CONTROL_POWEREVENT`, `dwEventType` is the `WM_POWERBROADCAST` wParam (`PBT_APMSUSPEND` 0x4, `PBT_APMRESUMESUSPEND` 0x7, `PBT_APMRESUMEAUTOMATIC` 0x12, `PBT_POWERSETTINGCHANGE` 0x8013 with `EventData` -> `POWERBROADCAST_SETTING`); for `SERVICE_CONTROL_SESSIONCHANGE`, `dwEventType` is the `WM_WTSSESSION_CHANGE` wParam (`WTS_SESSION_LOGON` 5, `WTS_SESSION_LOGOFF` 6, `WTS_SESSION_LOCK` 7, `WTS_SESSION_UNLOCK` 8, ...) and `EventData` -> `WTSSESSION_NOTIFICATION{cbSize, dwSessionId}`. HandlerEx doc (above); constants from https://learn.microsoft.com/en-us/windows/win32/power/wm-powerbroadcast and https://learn.microsoft.com/en-us/windows/win32/termserv/wm-wtssession-change. **UNCERTAIN whether x/sys/windows exports `PBT_*`/`WTS_*` constants — define them locally.**
- Handler must return within 30 s; do work on another goroutine. `SERVICE_CONTROL_SHUTDOWN` leaves ~20 s; `PreShutdown` buys more (use it to let restic finish its current pack and release the lock). HandlerEx remarks.
- `svc.IsWindowsService()` to detect service vs console; `svc/debug` runs the handler on a console; `svc/mgr` installs/queries services.
- Keeping a laptop awake during backup: `SetThreadExecutionState` is not exported by x/sys/windows (index check; **UNCERTAIN**) — call via `NewLazySystemDLL("kernel32")`. Modern Standby devices may ignore it from a service; treat "backup while lid closed" as not promised.

### 5.2 `kardianos/service`

Source: https://pkg.go.dev/github.com/kardianos/service ; https://raw.githubusercontent.com/kardianos/service/master/service_windows.go

- Gives `Install/Uninstall/Start/Stop/Restart/Status`, `Config{Name, DisplayName, Description, UserName, Arguments, Executable, Dependencies, EnvVars, Option}`; Windows options `StartType` (automatic), `DelayedAutoStart`, `Password`, `Interactive`, `OnFailure` (restart default), `OnFailureDelayDuration` (1s), `OnFailureResetPeriod` (10). Installs via `mgr.CreateService` with `ServiceStartName: UserName`, sets recovery actions, registers an Event Log source with `eventlog.InstallAsEventCreate`, and `SystemLogger()` writes to it. `Shutdowner` interface distinguishes machine shutdown.
- **Limitation:** its Windows `Execute` accepts only `svc.AcceptStop | svc.AcceptShutdown` and ignores every other command (`default:`). **No power or session events reach your code.** To get `PBT_APMRESUMEAUTOMATIC` you must use `x/sys/windows/svc` directly (or fork). Use kardianos only for install/recovery conveniences, or reimplement those ~100 lines with `svc/mgr`.
- `WorkingDirectory`/`ChRoot` unsupported on Windows.

### 5.3 LocalSystem vs service account

- Empty `ServiceStartName` = LocalSystem (Win32 `CreateService` semantics; https://learn.microsoft.com/en-us/windows/win32/api/winsvc/nf-winsvc-createservicew). LocalSystem already holds `SeBackupPrivilege`, `SeRestorePrivilege`, `SeSecurityPrivilege`, `SeTakeOwnershipPrivilege`, `SeCreateSymbolicLinkPrivilege` and can create VSS snapshots — all of which restic needs for full-fidelity Windows backup/restore (1.1, 1.3). restic 0.19.0 enables those privileges itself before file access (#5424).
- A dedicated service account would need "Back up files and directories", "Restore files and directories", "Manage auditing and security log", "Take ownership", symlink rights, and VSS access — effectively Backup Operators + Administrators. Not worth it for v1; run LocalSystem and treat the agent binary + config ACLs as the trust boundary. (Inference from the privilege lists in restic docs.)
- Under LocalSystem, `%LOCALAPPDATA%` is the system profile; set `RESTIC_CACHE_DIR`, `RESTIC_PASSWORD_FILE`/`RESTIC_PASSWORD_COMMAND` explicitly to `%ProgramData%\CryoBackup\...` with admin-only ACLs.

### 5.4 Launching restic.exe as a child

- `exec.CommandContext` with `SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}`; read stdout (JSON lines) and stderr (JSON `error`/`exit_error`) concurrently; map exit codes per 1.10.
- Tie the child's lifetime to the service with a **job object**: `windows.CreateJobObject` -> `SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, &JOBOBJECT_EXTENDED_LIMIT_INFORMATION{BasicLimitInformation.LimitFlags: JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE})` -> `AssignProcessToJobObject(job, procHandle)` right after start (small race before assignment; acceptable). Constants/functions confirmed in https://pkg.go.dev/golang.org/x/sys/windows and https://raw.githubusercontent.com/golang/sys/master/windows/types_windows.go (`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE = 0x2000`, `JobObjectExtendedLimitInformation = 9`, `CREATE_BREAKAWAY_FROM_JOB`, `CREATE_NO_WINDOW`). Job memory limits (`JOB_OBJECT_LIMIT_PROCESS_MEMORY`) can cap restic RAM on 8 GB laptops.
- On `Stop`/`PreShutdown`: cancel context (restic exits 130 and removes its lock); on hard kill the lock goes stale after 30 min and the next run proceeds (2.1).

### 5.5 Event Log

`svc/eventlog`: `InstallAsEventCreate(src, Error|Warning|Info)` at install time (registry write, admin), `Open(src)`, `Log.Info/Warning/Error(eid 1..1000, msg)` (EventCreate.exe message file). Three levels, generic message template; good enough for Syncro's Event Log monitors. https://pkg.go.dev/golang.org/x/sys/windows/svc/eventlog

### 5.6 Self-update of a running service

- Windows cannot delete or overwrite a running .exe but **can rename it**. `minio/selfupdate` does: verify checksum/signature -> write `target.new` -> rename target to `target.old` -> rename `.new` into place -> try to remove `.old`; "On Windows, the removal of /path/to/target.old always fails, so instead Apply hides the old file"; remove `.old` on next start. `Options{TargetPath, TargetMode, Checksum, Hash, Verifier, OldSavePath}`, `RollbackError()`. https://raw.githubusercontent.com/minio/selfupdate/master/apply.go
- Then restart: exit and rely on SCM recovery (`OnFailure=restart`, 1 s delay — recovery fires on abnormal exit, so exit non-zero deliberately), or spawn a detached helper that calls `mgr`/`sc` to restart the service. kardianos `Restart()` from an external process also works.
- **v1 alternative that fits an MSP:** ship MSI upgrades through the RMM (Syncro script: `msiexec /i agent.msi /qn`) with a fixed `UpgradeCode`; WiX `ServiceControl` stops/starts the service around the file swap. Self-update becomes a v2 feature.

### 5.7 MSI / installer packaging for a Go binary

| Tool | Notes | Source |
|---|---|---|
| **WiX** (v4/v5, `wix` .NET tool) | `ServiceInstall` (Name, DisplayName, Description, Type=ownProcess, Start=auto, ErrorControl, Account, Password, Arguments, Vital, Interactive, LoadOrderGroup) under a Component whose KeyPath is the .exe; `ServiceDependency`, `ServiceConfigFailureActions`; `ServiceControl` for stop/start/remove. Industry standard; MSI = silent install, upgrade codes, rollback, RMM-friendly. | https://docs.firegiant.com/wix/schema/wxs/serviceinstall/ |
| go-msi | Wraps WiX 3.10 (`heat/candle/light` must be on PATH); 335 commits, no releases visible, maintenance **UNCERTAIN** — treat as unmaintained; write WiX directly. | https://github.com/mh-cbon/go-msi |
| NSIS | Produces .exe not .msi; SimpleSC plugin for install/start/stop/remove/failure actions/description; fine for silent `/S` but loses MSI upgrade semantics and Intune/GPO MSI handling. | https://nsis.sourceforge.io/NSIS_Simple_Service_Plugin |
| msitools/`wixl` (Linux) | Builds MSIs from WiX-like XML without Windows; limited schema. **UNCERTAIN** support for ServiceInstall — verify or build on a Windows runner. | (not verified) |

### 5.8 Code signing and SmartScreen (reality, 2026)

Sources: https://learn.microsoft.com/en-us/windows/apps/package-and-deploy/code-signing-options ; https://learn.microsoft.com/en-us/windows/apps/package-and-deploy/smartscreen-reputation

- **EV certificates no longer bypass SmartScreen** (removed 2024); EV = OV for reputation purposes; "Paying the EV premium ($400+/year) solely to avoid SmartScreen warnings is no longer justified."
- **Azure Artifact Signing (formerly Trusted Signing):** ~$9.99/month, no hardware token, CI-friendly; organisations in USA/Canada/EU/UK, individuals USA/Canada; identity validation takes days; reputation still builds over time ("initial warnings expected"), but a consistent signing identity carries reputation to later releases.
- OV: $150–300/year; since June 2023 CA/B Forum requires keys on HSM/hardware token.
- Unsigned: "Windows protected your PC"; enterprise policy can block entirely; **Windows 11 Smart App Control blocks unsigned files without reputation for all executables, not just downloads.** Sign the agent, the installer, and the bundled restic.exe (BSD-2 permits redistribution; upstream restic Windows builds ship GPG-signed SHA256SUMS, **not Authenticode — UNCERTAIN**, verify on the next release).
- Inference: silent RMM-driven `msiexec` installs do not trigger SmartScreen's download prompt (no Mark-of-the-Web), but Defender ASR/AppLocker/Smart App Control still evaluate signatures, so sign anyway.
- Open-source option: SignPath Foundation offers free OV-level signing for qualifying OSS projects. (code-signing-options page)

### 5.9 Embedding restic.exe vs bundling

- Go `embed` (`//go:embed restic.exe`, https://pkg.go.dev/embed) makes a single agent binary (~+25–30 MB, **UNCERTAIN size**); extract to `%ProgramData%\CryoBackup\bin\restic-<ver>.exe` at start, verify SHA256 against restic's published SHA256SUMS, Authenticode-sign restic.exe *before* embedding (bytes stay identical). Downsides: runtime extraction of an executable resembles dropper behaviour to EDR (inference), doubles disk, complicates MSI component tracking/rollback.
- Bundling as a second MSI component (`restic.exe` in the same directory, pinned version per agent release, `restic version --json` checked at start) is simpler, EDR-friendlier and lets MSI handle upgrades. **Recommend bundle, not embed.**

---

## 6. GAPS vs the big-player feature list

| Commercial feature | Engine reality | Honest position |
|---|---|---|
| Image / bare-metal restore | restic is file-level only; VSS is used for reads, never for block images (PR #3067). | **Not in v1.** Position CryoBackup as file-level off-site; keep UrBackup (already deployed) for images. Backing up a wbadmin VHDX via restic is possible but churn-heavy and unproven here. |
| Continuous / real-time (CDP) backup | Snapshot-per-run; each run walks the tree (path/size/mtime compare on Windows); no USN-journal integration. | Hourly/15-min scheduled runs with `--skip-if-unchanged` and `--no-scan`; scan cost grows with file count; say "near-continuous (interval)", not CDP. |
| Cross-client dedup with per-device repos | Dedup scope is the repository (2.7). | **Not in v1**: accept; mitigate with aggressive OS/app excludes and zstd. Per-customer repos buy dedup at the cost of lock contention, client RAM, and key blast radius. |
| Native GFS archiving to cold tiers | restic stores one tier; 0.18.0 added only *experimental* S3 cold storage (#5173); via rclone `--s3-storage-class` applies to every object incl. locks/index, and Glacier-class objects must be restored before any read, which breaks restore/prune. Wasabi/B2/IDrive have no cold tier anyway. | **Not in v1.** Later: `restic copy` into an archive repo on cheaper storage (copy needs `--copy-chunker-params` for dedup). |
| In-place "download link" / web restore | No server API; but `ls --json`, `find --json`, `dump -a zip` give everything a server-side restore service needs if the admin host holds the repo key. | v1: technician-driven restore (`dump` to temp + signed URL / `restore` to a share). Self-service portal is v2 and implies key escrow on the server. |
| Browse snapshots as a drive on Windows | `restic mount` is FUSE; no Windows support. | **Not in v1**; use `ls`/`find` + restore to folder. |
| Alternate Data Streams, junction contents | Not supported (1.3). | State it. |
| Application-aware (SQL/Exchange) backups | restic's VSS requester snapshots volumes; it does not coordinate writer-specific truncation or verify application consistency (**UNCERTAIN**: writers are involved in the snapshot but no app-level handling exists). | Workstations: fine. Servers with DBs: dump first, then back up the dumps. |
| Immutability / ransomware retention lock | Append-only gateway (3.2) = clients cannot delete; Object Lock compliance mode is incompatible, governance mode untested (4.5). | v1: append-only + versioning + `--keep-within`; immutability "governance mode, roadmap". |
| Bandwidth schedules | `--limit-upload` is fixed per run. | Agent restarts restic with a new limit at schedule boundaries (loses in-flight pack only; index checkpoints keep progress). |
| Built-in alerting / central policy | None; all agent responsibility. | CryoBackup's job: JSON summaries + Syncro/SMTP2Go; this is the product. |
| Key escrow / lost-password recovery | Lose the password, lose the repo (030_preparing_a_new_repo). | Design an escrow (per-device password stored encrypted on the admin host) before v1 ships. |
| Multi-tenant RBAC, customer self-service | None in the engine. | Product layer. |

---

## Sources index (all cited above)

restic docs: https://restic.readthedocs.io/en/stable/030_preparing_a_new_repo.html , 040_backup.html , 045_working_with_repos.html , 047_tuning_parameters.html , 050_restore.html , 060_forget.html , 070_encryption.html , 075_scripting.html , 100_references.html , manual_rest.html , faq.html ; https://raw.githubusercontent.com/restic/restic/master/doc/cache.rst
restic releases/changelog: https://github.com/restic/restic/releases ; /releases/tag/v0.17.0 ; /releases/tag/v0.16.0 ; https://raw.githubusercontent.com/restic/restic/master/CHANGELOG.md
restic source: cmd/restic/cmd_backup.go , cmd_restore.go , cmd_forget.go , cmd_check.go , cmd_cache.go , cmd_stats.go ; internal/fs/fs_local_vss.go ; internal/repository/lock.go (all under https://raw.githubusercontent.com/restic/restic/master/)
restic PRs/issues: https://github.com/restic/restic/pull/3067 ; https://github.com/restic/restic/pull/4614 ; https://github.com/restic/restic/issues/2745
restic forum: https://forum.restic.net/t/windows-junction-backup-with-go1-23-or-later/8940 ; /t/memory-usage-oom-etc-on-a-big-repository/8758 ; /t/restic-prune-runtime-out-of-memory/6902 ; /t/any-way-to-get-restic-to-work-with-s3-object-locking-or-a-read-only-key/6858 ; /t/restic-malware-and-object-locks/7079 ; /t/wasabi-compliance-bucket-and-lockfiles/2318 ; /t/anyone-using-restic-with-b2-backblaze-and-object-locks/4277 ; /t/restic-mount-windows/898
rest-server: https://github.com/restic/rest-server ; https://raw.githubusercontent.com/restic/rest-server/master/README.md ; https://raw.githubusercontent.com/restic/rest-server/master/repo/repo.go
rclone: https://rclone.org/commands/rclone_serve_restic/ ; https://raw.githubusercontent.com/rclone/rclone/master/cmd/serve/restic/restic.go ; https://rclone.org/s3/ ; https://rclone.org/b2/ ; https://forum.rclone.org/t/serve-restic-append-only-environment-variable/17050 ; https://www.rsync.net/resources/notes/2025-q4-rsync.net_technotes.html
Providers: https://wasabi.com/pricing ; https://docs.wasabi.com/docs/how-does-wasabis-minimum-storage-duration-policy-work ; https://docs.wasabi.com/docs/object-lock-with-the-wasabi-s3-api ; https://docs.wasabi.com/apidocs/object-lock-on-buckets-with-the-wasabi-s3-api ; https://www.backblaze.com/cloud-storage/pricing ; https://www.backblaze.com/cloud-storage/transaction-pricing ; https://www.backblaze.com/docs/cloud-storage-object-lock ; https://www.backblaze.com/docs/cloud-storage-s3-compatible-api ; https://www.backblaze.com/docs/cloud-storage-call-the-s3-compatible-api ; https://www.idrive.com/s3-storage-e2/pricing ; https://www.idrive.com/e2/object-lock ; https://idrive.com/e2/faq-dashboard
Go/Windows: https://pkg.go.dev/golang.org/x/sys/windows/svc ; https://raw.githubusercontent.com/golang/sys/master/windows/svc/service.go ; https://pkg.go.dev/golang.org/x/sys/windows/svc/eventlog ; https://pkg.go.dev/golang.org/x/sys/windows ; https://raw.githubusercontent.com/golang/sys/master/windows/types_windows.go ; https://pkg.go.dev/github.com/kardianos/service ; https://raw.githubusercontent.com/kardianos/service/master/service_windows.go ; https://raw.githubusercontent.com/minio/selfupdate/master/apply.go ; https://pkg.go.dev/embed
Microsoft: https://learn.microsoft.com/en-us/windows/win32/api/winsvc/nc-winsvc-lphandler_function_ex ; https://learn.microsoft.com/en-us/windows/win32/power/wm-powerbroadcast ; https://learn.microsoft.com/en-us/windows/win32/termserv/wm-wtssession-change ; https://learn.microsoft.com/en-us/windows/win32/api/winsvc/nf-winsvc-createservicew ; https://learn.microsoft.com/en-us/windows/apps/package-and-deploy/code-signing-options ; https://learn.microsoft.com/en-us/windows/apps/package-and-deploy/smartscreen-reputation
Packaging: https://docs.firegiant.com/wix/schema/wxs/serviceinstall/ ; https://github.com/mh-cbon/go-msi ; https://nsis.sourceforge.io/NSIS_Simple_Service_Plugin
