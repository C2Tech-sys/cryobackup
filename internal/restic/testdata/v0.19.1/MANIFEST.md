# Fixtures captured from restic 0.19.1

Regenerate with `go run ./internal/restic/testdata/regen -restic <restic 0.19.1> -out ./internal/restic/testdata/v0.19.1`.

<!-- BEGIN GENERATED (regen rewrites everything between the markers) -->

Captured with restic 0.19.1. Environment of every run: `RESTIC_REPOSITORY` (a temporary local repository), `RESTIC_PASSWORD=fixture-password`, `RESTIC_CACHE_DIR`, `TEMP`/`TMP` (temporary directories), `RESTIC_PROGRESS_FPS=60` (so that short runs print status lines). The temporary root appears as `C:\fixture`.

| file | exit | command |
| --- | --- | --- |
| `version.json` | 0 | `restic version --json` |
| `init.json` | 0 | `restic init --json` (raw argv (no builder for init)) |
| `backup_skipped.stdout.jsonl` | 0 | `restic backup --json --host fixture-host --retry-lock 30s --skip-if-unchanged --no-scan -- C:\fixture\skiptree` (second run of an unchanged tree in a repository of its own: no snapshot_id in the summary) |
| `backup_ok.stdout.jsonl` | 0 | `restic backup --json --host fixture-host --tag cb.customer=fixture-customer --tag cb.policy=fixture-policy.1 --tag cb.run=fixture-run-1 --retry-lock 30s -- C:\fixture\tree` |
| `backup_ok.stderr.jsonl` | 0 | `restic backup --json --host fixture-host --tag cb.customer=fixture-customer --tag cb.policy=fixture-policy.1 --tag cb.run=fixture-run-1 --retry-lock 30s -- C:\fixture\tree` |
| `backup_partial.stdout.jsonl` | 3 | `restic backup --json --host fixture-host --tag cb.customer=fixture-customer --tag cb.policy=fixture-policy.1 --tag cb.run=fixture-run-1 --retry-lock 30s -- C:\fixture\tree C:\fixture\missing` (missing root: plain-text stderr line, no JSON error line) |
| `backup_partial.stderr.jsonl` | 3 | `restic backup --json --host fixture-host --tag cb.customer=fixture-customer --tag cb.policy=fixture-policy.1 --tag cb.run=fixture-run-1 --retry-lock 30s -- C:\fixture\tree C:\fixture\missing` (missing root: plain-text stderr line, no JSON error line) |
| `backup_locked.stdout.jsonl` | 3 | `restic backup --json --host fixture-host --tag cb.customer=fixture-customer --tag cb.policy=fixture-policy.1 --tag cb.run=fixture-run-1 --retry-lock 30s -- C:\fixture\tree` (notes/locked.docx held open by a PowerShell child with FileShare None) |
| `backup_locked.stderr.jsonl` | 3 | `restic backup --json --host fixture-host --tag cb.customer=fixture-customer --tag cb.policy=fixture-policy.1 --tag cb.run=fixture-run-1 --retry-lock 30s -- C:\fixture\tree` (notes/locked.docx held open by a PowerShell child with FileShare None) |
| `backup_dry_run.stdout.jsonl` | 0 | `restic backup --json --host fixture-host --tag cb.customer=fixture-customer --tag cb.policy=fixture-policy.1 --tag cb.run=fixture-run-1 --retry-lock 30s --dry-run -vv -- C:\fixture\tree` |
| `snapshots.json` | 0 | `restic snapshots --json --host fixture-host --retry-lock 30s` |
| `ls_roots.jsonl` | 0 | `restic ls --json c4709fdaf86559b31287cd581af7076550d83b73b567458d851dcbdbb8d975cd / --retry-lock 30s` (empty dir argument becomes /) |
| `ls.jsonl` | 0 | `restic ls --json c4709fdaf86559b31287cd581af7076550d83b73b567458d851dcbdbb8d975cd /C/fixture/tree --retry-lock 30s` (one level: the directory and its direct children) |
| `find.json` | 0 | `restic find --json -H fixture-host --ignore-case --snapshot c4709fdaf86559b31287cd581af7076550d83b73b567458d851dcbdbb8d975cd --retry-lock 30s *.TXT` (stderr carries the notice: Ignoring "filters": explicit snapshot ids are given) |
| `stats_raw_data.json` | 0 | `restic stats --json --mode raw-data --host fixture-host --retry-lock 30s` |
| `stats_restore_size.json` | 0 | `restic stats --json --mode restore-size --host fixture-host --retry-lock 30s` |
| `restore.stdout.jsonl` | 0 | `restic restore --json --target C:\fixture\restore\verify --overwrite always --verify --retry-lock 30s c4709fdaf86559b31287cd581af7076550d83b73b567458d851dcbdbb8d975cd:/C/fixture/tree` |
| `restore_dry_run.stdout.jsonl` | 0 | `restic restore --json --target C:\fixture\restore\simulate --dry-run -vv --retry-lock 30s c4709fdaf86559b31287cd581af7076550d83b73b567458d851dcbdbb8d975cd:/C/fixture/tree` (--dry-run -vv: one verbose_status line per item) |
| `restore_error.stdout.jsonl` | 1 | `restic restore --json --target C:\fixture\restore\full --retry-lock 30s c4709fdaf86559b31287cd581af7076550d83b73b567458d851dcbdbb8d975cd` (whole-snapshot restore: a summary is printed although the run exits 1) |
| `restore_error.stderr.jsonl` | 1 | `restic restore --json --target C:\fixture\restore\full --retry-lock 30s c4709fdaf86559b31287cd581af7076550d83b73b567458d851dcbdbb8d975cd` (whole-snapshot restore: a summary is printed although the run exits 1) |
| `cat_config.json` | 0 | `restic cat config --retry-lock 30s` (restic does not take --json here; the output is pretty-printed JSON) |
| `forget_dry_run.json` | 0 | `restic forget --json --dry-run --host fixture-host --group-by host,paths --keep-last 1 --keep-within 1d --retry-lock 30s` |
| `forget.json` | 0 | `restic forget --json --host fixture-host --group-by host,paths --keep-last 1 --keep-within 1d --retry-lock 30s` |
| `prune_dry_run.stdout.txt` | 0 | `restic prune --max-unused 0% --dry-run --retry-lock 30s` |
| `prune.stdout.txt` | 0 | `restic prune --max-unused 0% --retry-lock 30s` (no JSON: plain text) |
| `check.stdout.jsonl` | 0 | `restic check --json --retry-lock 30s` |
| `check_damaged.stdout.jsonl` | 1 | `restic check --json --read-data-subset 100% --retry-lock 30s` (copy of the repository with 24 bytes overwritten inside the largest pack; check errors use a top-level message field) |
| `check_damaged.stderr.jsonl` | 1 | `restic check --json --read-data-subset 100% --retry-lock 30s` (copy of the repository with 24 bytes overwritten inside the largest pack; check errors use a top-level message field) |
| `exit10_repo_missing.stderr.jsonl` | 10 | `restic snapshots --json --retry-lock 30s` (repository points at a directory that does not exist) |
| `exit12_wrong_password.stderr.jsonl` | 12 | `restic snapshots --json --retry-lock 30s` (a --json command: the error arrives as an exit_error line) |
| `exit12_wrong_password_plain.stderr.txt` | 12 | `restic cat config --retry-lock 30s` (cat config has no --json: plain-text stderr) |
| `exit11_lock.stderr.jsonl` | 11 | `restic forget --json --host fixture-host --group-by host,paths --keep-last 1 --keep-within 1d` (produced while 'backup --stdin --stdin-filename held.bin --host held' holds a non-exclusive lock) |
| `exit11_prune_lock.stderr.txt` | 11 | `restic prune` (no --json: plain-text stderr) |

<!-- END GENERATED -->

## Divergences from the ticket and the architecture

Written by hand; regen preserves everything below the generated block. Each item
says what restic 0.19.1 does, which fixture shows it, and what the package does
about it. The package follows the binary.

1. **`--` before the roots is accepted.** Every backup row above runs with it.
   `Backup` renders `-- ROOT...`, so a root that starts with `-` cannot be read as a flag.
2. **`-o vss.exclude-all-mount-points` needs a value.** The bare form fails with
   `strconv.ParseBool: parsing "": invalid syntax` (probed with `--use-fs-snapshot --dry-run`,
   no fixture: it needs no admin rights to fail). `Backup` renders `-o vss.exclude-all-mount-points=true`.
   The other two vss options (`vss.timeout=<n>s`, `vss.exclude-volumes=a;b`) are accepted and an
   unknown `vss.*` name is rejected by restic (`option vss.bogus is not known`).
3. **`ls` without a directory walks the whole snapshot.** `restic ls --json <id>` prints every
   node of every directory (not just the roots). With a directory argument it prints that
   directory and its direct children only (`ls.jsonl`: the tree directory and `invoices`, `media`,
   `notes`; `ls_roots.jsonl`: `/` gives the single node `/C`). Directories must start with `/`
   (`Fatal: All path filters must be absolute`). `Ls(id, "")` therefore sends `/` and lists the roots.
   The first line is the snapshot record (`message_type: snapshot`, plus `struct_type`), the
   following lines are nodes (`message_type: node`). `mode` is a number in the JSON
   (`Node.Mode` is a `uint64`, the ticket drafted a string).
4. **A missing backup root is not a JSON error line.** `backup_partial.stderr.jsonl` holds the
   plain-text line `<path> does not exist, skipping` followed by the exit_error
   `Warning: at least one source file could not be read` (exit 3). The decoder reports the first as
   `Unparsed{Class: missing_source}`; the ticket's `ErrorEvent{missing_source}` only arises for a
   file that exists but cannot be read. `backup_locked.*` (a file held open with FileShare None)
   shows that case: `error` with `during: archival`, an `item`, and the message
   `The process cannot access the file because it is being used by another process.`
   (class `locked`, extension `docx`).
5. **`Unparsed` carries a class.** The ticket specified `Unparsed{}`. Plain-text stderr is how
   restic reports errors for commands run without `--json` (`cat config`, `prune`:
   `exit12_wrong_password_plain.stderr.txt`, `exit11_prune_lock.stderr.txt`), so the decoder keeps
   the path-free class of the dropped text. The text itself is gone.
6. **Restore always prints a summary, even when it exits 1.** `restore_error.stdout.jsonl` has a
   `summary` while the run exits 1 with `Fatal: There were 1 errors`. `Map` does not require a
   summary for restore, as the ticket says. The error is real: restoring a whole snapshot to a
   target recreates every ancestor directory, and Windows refuses to set the timestamp of the
   read-only `Users` directory (`Access is denied.`, `during: restore`, class `permission`). The
   subfolder form `<id>:<path>` used for `restore.stdout.jsonl` avoids it.
7. **`restore --dry-run -vv --json` prints one `verbose_status` per item** with `action: "restored"`,
   `item` and `size` (`restore_dry_run.stdout.jsonl`); `VerboseStatus.Size` holds the size.
   Backup and restore also leave out zero-valued counters (a status line may carry only
   `percent_done`), and short runs print no status line at all unless `RESTIC_PROGRESS_FPS` is
   raised: the fixtures were captured with `RESTIC_PROGRESS_FPS=60`.
8. **Check errors have a different shape.** `check_damaged.stderr.jsonl`: `{"message_type":"error",
   "message":"..."}` (top-level `message`, no `error` object, no `during`, no `item`); the summary's
   `broken_packs` is an array of pack ids (or `null`), not a number. `CheckSummary.BrokenPacks` is
   its length. The decoder sets `During: "check"` only when told `ForKind(KindCheck)`.
9. **`cat config` has no `message_type` and is multi-line.** `cat_config.json` is pretty-printed
   JSON `{version, id, chunker_polynomial}` and restic does not take `--json` for it.
   `init --json` prints `message_type: initialized` with `id` and `repository`.
10. **`find --json` matches have no `name`.** Fields: `path, permissions, type, mode, mtime, atime,
    ctime, uid, gid, size` (`FindMatch.Name` stays empty). With explicit `--snapshot` ids restic
    prints `Ignoring "filters": explicit snapshot ids are given` to stderr; the document decoders also
    skip leading text lines in case the streams were merged.
11. **`forget --json` and `snapshots --json` list snapshots oldest first**; `tags` is `null` when a
    group has none; `username` is `DOMAIN\name` on a domain-joined Windows host.
12. **`prune` writes its summary to stdout** between progress lines (`prune.stdout.txt`,
    `prune_dry_run.stdout.txt`); a real run adds `rebuilding index`, `removing N old packs`,
    `done`. The conflict message of exit 11 reads `repository is already locked by PID ...` (not
    `exclusively`, because the holder's lock is shared); the classifier matches `already locked`.
13. **`--retry-lock` and `vss.timeout` render as `<n>s`** (`30s`, `120s`), matching the architecture;
    both accept Go durations. A sub-second value falls back to Go syntax (`1.5s`).
14. **Name clash in the ticket.** `func Stats(mode, host, c)` (builder) and `DecodeStats(r) (Stats, error)`
    cannot both exist in one package; the result type is `StatsResult`.
15. **Exit 1 carries little in its exit_error.** For restore and check the message is
    `Fatal: There were 1 errors` / `Fatal: repository contains errors`, class `unknown`; the useful
    class is on the preceding `error` lines. `StderrClass(exit, counts)` therefore falls back to
    the dominant counted class when the exit_error class is `unknown`.
