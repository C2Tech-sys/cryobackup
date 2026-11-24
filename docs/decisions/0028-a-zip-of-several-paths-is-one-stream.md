# ADR 0028 - A download of several paths is one zip stream, built from one restic run per path

**Date:** 2026-11-24 - **Status:** Accepted (operator, 2026-11-24) - **Decider:** the operator (plan P22, 2026-11-24), recorded by the foreman's worker (ticket P22-D0)

This ADR completes ADR 0008. That ADR says a multi-path selection "must be produced as a stream as well (restic restore
to a pipe-backed archive or one dump per path into one zip stream); restore-to-temp is no longer allowed"
(`docs/decisions/0008-no-restored-data-at-rest-on-the-console-host.md:20-22`). This ADR chooses between those two.

## Context

- **One path today.** A download is one file or one folder (`internal/console/restores/restores.go:209-259`, one
  selection). The worker streams it (`internal/console/maintain/dump.go:39-158`) and writes nothing to disk.
- **What restic does, proven for 0.19.1.** `restic dump -a zip` of a folder writes a zip; of a single file it writes the
  file's raw bytes (verified live, `internal/console/maintain/doc.go:84-86`). The worker therefore asks `ls` what the
  path is, copies a folder's zip through unchanged, and wraps a file's raw bytes into a one-entry zip on the fly
  (`dump.go:60-157`).
- **Fail closed, proven in code.** The one-entry zip's central directory is written only after restic exited 0
  (`dump.go:124-141`), so a failed run never ends in a readable archive. A failed write to the browser cancels restic
  at once (`dump.go:104-108`).
- **Streams have their own slots.** A download over the worker's stream budget is refused at once, never queued
  (`dump.go:43-50`).
- **The restore command writes into a directory.** The product's builder for `restic restore` requires `--target`
  (`internal/restic/commands.go:46-48`). Restoring into a pipe-backed archive would need a filesystem that is an
  archive writer, which this product does not have.
- **Why a folder cannot come as restic's zip into the larger zip.** A zip is read reliably from its central directory,
  which comes at its end. Merging restic's folder zip into a larger zip stream would mean holding it until its end, or
  parsing it forwards. A tar is read forwards, entry by entry.

## Decision

- **One zip stream per download.** Each selected path is its own restic run, in the order selected:
  - a **file** is `restic dump` of that file, whose raw bytes become one zip entry (proven behaviour above);
  - a **folder** is `restic dump -a tar` of that folder, read with Go's `archive/tar` and written entry by entry into
    the same zip.
- **The central directory comes last.** It is written only after every restic run has exited 0. If any run fails, the
  response is aborted, and the browser keeps no complete zip. The single-path rule of `dump.go` extends to the whole
  download.
- **Limits.** At most 50 paths. A path inside another selected folder is dropped, because the folder already holds it.
  One download holds one stream slot for all its runs, one run at a time.
- **No disk, no cache.** Nothing is staged, and restic runs with `--no-cache` (ADR 0008). The bytes cross the admin
  path once, to the HTTP response.
- **The record.** As today: the request stores the selection wrapped under the master key and a hash and count of
  the paths, never the paths. Size and sha256 are recorded once the download finishes (ADR 0008).

## Consequences

- A download of several files and folders is one click and one file, with nothing written on the console host.
- A dropped download starts again from the beginning: no resume, no Range (ADR 0008).
- Each path is a separate restic run. With `--no-cache` each run is expected to load the repository index again
  (assumed, not measured), so a download of many paths from a large repository may pay that load once per path. The
  50-path cap bounds it.
- A download keeps one stream slot for as long as the slowest path takes.
- Fixture mode and tests cover: a failing run in the middle never gives a readable zip; a dropped client cancels
  restic within 2 s; nothing is left in scratch or artifact directories; the 51st path is refused.

## What is deliberately left out and why

- **A tar download** (`download_tar` exists in the schema, `internal/console/migrations/0001_initial.sql:650`). One
  download format is enough for the pilot, and the single-path download is already a zip.
- **One `restic restore --include-file` into a pipe.** The restore command writes into a directory, and a directory on
  the console host is what ADR 0008 forbids.
- **Several paths in one `dump -a tar` run.** The design does not depend on it. If the spike shows it works, it can
  reduce the index loads later. That would be an optimisation, not a new decision.
- **Resume.** It would need a staged copy, which ADR 0008 forbids.

## To be proven by spike P22-S1

| # | Assumed | Fallback if the spike says no |
|---|---|---|
| 6 | `restic dump -a tar <snapshot> <folder>` streams (writes as it reads, holds no whole archive in memory); its entry names are relative and stable; non-regular entries (links, empty folders) are recognisable so they can be carried or skipped by rule. | **The cut line:** a download is one file or one folder, as today, and the wizard says so before the operator selects a second path. The folder still comes as restic's own zip (proven). |
| 7 | Browsing with `--no-cache` answers in a usable time on about 50 GB and 200 000 files (shared with ADR 0027). | An in-memory (tmpfs) cache for the worker. |

Not settled by the spike: the cost of one index load per path on the production repositories' sizes. It is measured
in the pilot.
