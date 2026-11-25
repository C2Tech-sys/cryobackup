# ADR 0028 - A download of several paths is one zip stream, built from one restic run per path

**Date:** 2026-11-24 - **Status:** Accepted (operator, 2026-11-24) - **Decider:** the operator (plan P22, 2026-11-24), recorded by the foreman's worker (ticket P22-D0)

This ADR completes ADR 0008. That ADR says a multi-path selection "must be produced as a stream as well (restic restore
to a pipe-backed archive or one dump per path into one zip stream); restore-to-temp is no longer allowed"
(`docs/decisions/0008-no-restored-data-at-rest-on-the-console-host.md:20-22`). This ADR chooses between those two.

## Context

- **One path today.** A download is one file or one folder (`internal/console/restores/restores.go:209-259`, one
  selection). The worker streams it (`internal/console/maintain/dump.go:39-158`) and writes nothing to disk.
- **The worker asks what a path is first.** `restic dump` of a single file writes the file's raw bytes whatever
  archive format is asked for (`internal/console/maintain/doc.go:84-86`; spike P22-S1, `docs/lab/p22-spike.md:224`).
  So the worker runs `ls`, copies a folder's zip through unchanged, and wraps a file's raw bytes into a one-entry zip
  on the fly (`dump.go:60-157`).
- **Fail closed, in code.** The one-entry zip's central directory is written only after restic exited 0
  (`dump.go:124-141`), so a failed run never ends in a readable archive. A failed write to the browser cancels restic
  at once (`dump.go:104-108`).
- **Streams have their own slots.** A download over the worker's stream budget is refused at once, never queued
  (`dump.go:43-50`).
- **The restore command writes into a directory.** The product's builder for `restic restore` requires `--target`
  (`internal/restic/commands.go:46-48`). Restoring into a pipe-backed archive would need a filesystem that is an
  archive writer, which this product does not have.
- **Why folders come as tar.** A zip is read reliably from its central directory, which comes at its end, so restic's
  zip of a folder cannot be merged into a larger zip as it arrives. A tar is read forwards, entry by entry.
- **What spike P22-S1 showed about `restic dump -a tar`** (restic 0.19.1, `docs/lab/p22-spike.md:209-250`):
  - it streams into a pipe without seeking, and carries PAX records for long and non-ASCII names;
  - entry names are the full snapshot path without the leading `/` (`R/data/names/<name>`), and the dumped folder
    itself has no entry, so an empty selected folder gives an empty archive;
  - one path per call: a second path is refused (`p22-spike.md:225`);
  - a junction comes out as a symlink entry; restic's own `-a zip` writes the link target as the content of a
    47-byte file instead (`p22-spike.md:234-235`);
  - a cut stream is detectably truncated: the tar reader fails with "unexpected end of data";
  - with the gateway gone restic retried for more than 20 minutes without exiting; with the maintain path's rclone
    child gone it exited 1 within 2 s and left its lock object in the repository (`p22-spike.md:241-242`).

## Decision

- **One zip stream per download.** Each selected path is its own restic run, in the order selected. Raw `dump` or
  `dump -a tar` is chosen from the node type the browse listing already has:
  - a **file** is `restic dump` of that file, whose raw bytes become one zip entry;
  - a **folder** is `restic dump -a tar` of that folder, read with Go's `archive/tar` (which reads PAX records) and
    written entry by entry into the same zip. A single selected folder goes the same way, so restic's own folder zip
    is no longer passed through.
- **Entry names.** Each tar entry is renamed from its full snapshot path to a path relative to the selected item's
  parent: a selected folder `.../<parent>/<name>` gives `<name>/...`. The selected folder's own directory entry is
  written first, so an empty folder survives.
- **Zip-slip guard** (foreman ruling). After renaming, every entry name must be relative, with no drive letter, no leading `/` and no
  `..` segment. An entry that breaks this aborts the download.
- **Symlinks and junctions are skipped and counted, never named** (foreman ruling). The zip carries no link and no link target as file
  content. The count goes into the download's record; the names do not.
- **The central directory comes last.** It is written only after every restic run has exited 0 and every tar stream
  has been read to its end without error. A restic exit other than 0, or a tar read error, aborts the response, and
  the browser keeps no complete zip.
- **An idle watchdog on restic's output** (foreman ruling; the agent has its own for device restores, ADR 0027).
  If restic writes no byte for a bound the build sets, the worker stops
  restic and aborts the response. Then it runs `restic unlock` on that repository through the admin path, because a
  stopped restic leaves its lock object behind.
- **Limits.** At most 50 paths. A path inside another selected folder is dropped, because the folder already holds it.
  One download holds one stream slot for all its runs, one run at a time.
- **No disk, no cache.** Nothing is staged, and restic runs with `--no-cache` (ADR 0008). The bytes cross the admin
  path once, to the HTTP response.
- **The record.** As today: the request stores the selection wrapped under the master key and a hash and count of
  the paths, never the paths. Size and sha256 are recorded once the download finishes (ADR 0008).

## Consequences

- A download of several files and folders is one click and one file, with nothing written on the console host.
- A dropped download starts again from the beginning: no resume, no Range (ADR 0008).
- Each path is a separate restic run, and every uncached run reads every index file of the repository
  (`p22-spike.md:267-269`). A download of many paths pays that once per path. The 50-path cap bounds it.
- A download keeps one stream slot for as long as its paths take.
- A dead gateway or storage cannot hold a stream slot for 20 minutes: the watchdog frees it.
- Fixture mode and tests cover: a failing run in the middle never gives a readable zip; a truncated tar never gives a
  readable zip; a dropped client cancels restic within 2 s; the watchdog fires and the lock is removed; an entry that
  would leave the archive root aborts; a junction is skipped and counted; an empty folder keeps its entry; nothing is
  left in scratch or artifact directories; the 51st path is refused.

## What is deliberately left out and why

- **A tar download** (`download_tar` exists in the schema, `internal/console/migrations/0001_initial.sql:650`). One
  download format is enough for the pilot, and the single-path download is already a zip.
- **One `restic restore --include-file` into a pipe.** The restore command writes into a directory, and a directory on
  the console host is what ADR 0008 forbids.
- **Links in the zip.** A link target written as file content, as restic's own zip does, is wrong data, and a link
  points at a location on the original machine, not at anything in the download.
- **Resume.** It would need a staged copy, which ADR 0008 forbids.

## What spike P22-S1 showed, and what it did not

The tar route holds; the single-path cut line the plan held in reserve is not needed (`p22-spike.md:363-376`). Not
tested: an object-storage outage behind a live rclone, where rclone's own retries decide how long restic waits
(`p22-spike.md:244`); the watchdog covers that case as well. File symlinks in a snapshot were not tested either, and
are skipped by the same rule as junctions.
