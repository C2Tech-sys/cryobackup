# ADR 0008 - No backup or restored data at rest on the console host

**Date:** 2026-10-28 - **Status:** Accepted - **Decider:** operator

The production console host is an unmanaged VPS under a subcontractor BAA whose security schedule requires that
the host keeps no backup payload on persistent disk: no repository copies, staging files or caches, and no
payloads or secrets in swap, crash dumps, temporary files or diagnostics. Two M0/M1 designs break that:

- the maintenance worker keeps restic's cache, its temp directory and rclone's cache on the persistent
  `maintain-state` volume (repository copies on disk);
- technician restores are built as plaintext zip archives on the `artifacts` volume and kept until
  `purge_after` (DH27; decrypted client files on disk).

## Decision

- **Restores stream; they are never staged.** A technician download runs `restic dump` when the download is
  requested and streams its output through the admin path to the HTTP response. Nothing restored is written to
  the console host's disk, not even briefly. A dropped transfer starts again from the beginning (no resume, no
  Range). The same rule binds the M6 replacement-PC relay (DH28): it streams to the agent too.
- This supersedes the staging part of DH27 and the restore-artifact use of the `artifacts` volume. A multi-path
  selection (M6) must be produced as a stream as well (restic restore to a pipe-backed archive or one dump per
  path into one zip stream); restore-to-temp is no longer allowed.
- **Maintenance keeps no repository data on disk.** restic runs with `--no-cache`; restic's and rclone's temp
  and cache directories live in memory (tmpfs). The persistent state volume keeps only the worker's own
  bookkeeping, never repository bytes.
- **No crash dumps.** Console, gateway and maintenance containers run with a core-file limit of 0.
- Hosts with swap must encrypt it, or run without it; `doctor` reports swap it can see.

## Why

Encryption at rest of a staged archive would still be a payload copy on disk and needs a written BAA
exception; RAM staging caps the restore size. Streaming removes the copy entirely and costs only resume. The
cache rule costs maintenance speed (restic downloads index files on every run), which is acceptable for a job
that runs on a schedule.

## Consequences

- `restore_requests` no longer carries an artifact on disk; size and SHA-256 are known only after a download
  finishes and are recorded then.
- The admin path gains a streaming response; its "bytes never travel over the socket" rule is replaced by
  "restore bytes travel only over the socket, never to disk".
- Maintenance gets slower; prune and check download what the cache used to hold.
