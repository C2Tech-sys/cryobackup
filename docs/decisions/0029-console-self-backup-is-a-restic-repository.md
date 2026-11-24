# ADR 0029 - The console's self-backup is a restic repository written by the maintenance worker

**Date:** 2026-11-24 - **Status:** Accepted (operator, 2026-11-24) - **Decider:** the operator (rulings C3, C4 and C8 of plan P22, 2026-11-24), recorded by the foreman's worker (ticket P22-D0)

This ADR supersedes architecture.md job row 1038 (`db-self-backup`: nightly, an encrypted copy written to a backup
directory, a weekly tested restore in a temporary directory, M5).

## Context

- **Nothing backs up the console database.** The CLI has no backup command (`internal/console/cli/cli.go:23-35`).
- **The alarm is ready.** `state.SelfBackupFacts` carries `LastAt` and `VerifyFailed` (`internal/state/facts.go:327-335`).
  C11 is raised for a self-backup that was never written or is older than 26 h, and is critical when the newest
  tested restore failed (`internal/state/banners.go:533-550`, `DBSelfBackupMaxAge` at `banners.go:57`). The System
  page has its row (`internal/console/adminapi/m3_system.go:218`, judged at `m3_system.go:776-794`). The loader marks
  all of it as not in this build (`internal/console/health/probes.go:98-101`), so nothing is judged.
- **The maintenance worker has what a backup needs:** the admin storage credential (an rclone remote that is never
  internet-facing), the master key and the database. The probe repository (ADR 0017) already shows the pattern: a
  restic repository on the worker's storage target, a password derived from the master key and never stored, and a
  settings row that guards a one-time `restic init`.
- **Row 1038 keeps its copies on the console host** ("write to the backup directory (and optionally a second
  location)"). A copy on the host it protects is lost with that host, so it cannot be the disaster-recovery copy.
- **What the database holds.** Client host names, backup source roots (`snapshots.paths`,
  `internal/console/migrations/0001_initial.sql:563`), wrapped keys, the audit trail. A copy of it is client-identifying
  data and belongs on the same storage as the device repositories.

## Decision

**Where (ruling C3).** The self-backup is a restic repository in the same bucket as the device repositories, under its own
prefix that the gateway credential's policy does not cover (for example `<prefix>-console`). It is written through the
worker's admin remote, never through the gateway.

**Password.** HKDF-SHA256 of the master key with info `cryobackup/self-backup/v1`, never stored, as for the probe
repository (ADR 0017). The repository is created once, guarded by a settings row; a repository missing after that row
exists is a failure, never re-created.

**When (ruling C4).** A scheduler step in the worker every 6 hours, with `job_state` rows `db-self-backup` and
`db-self-backup-verify`. Only the scheduler's lease holder writes them.

**One run:**

1. Check free disk. The run needs room for one copy of the database.
2. `VACUUM INTO` a staging folder under the data directory, mode 0700. Leftovers from an earlier run are swept first.
3. Copy `config.json` beside it.
4. Write `manifest.json`: the time, schema version, build, database bytes and sha256, row counts per table, the master
   key's id, and the bucket and prefix.
5. `restic backup` of the staging folder, host `console`.
6. `forget --prune` by the retention below.
7. Delete the staging folder.

**Retention (ruling C4).** Keep every snapshot of the last 7 days, one a day for 30 days, one a month for 12 months.

**Verify, weekly and on demand.** Restore the newest snapshot into the staging folder (removed afterwards, as
after a run), run `PRAGMA integrity_check`, compare schema version, row counts and sha256 with the manifest, unwrap one
escrow row with the master key, and run `restic check --read-data` on the self-backup repository. A failure sets `VerifyFailed`: C11 critical. The loader stops marking the self-backup
not in this build.

**CLI.** `cryo-console self-backup run|list|verify`, and
`cryo-console self-backup restore --rclone-config F --remote R --bucket B --prefix P --master-key K --out DIR`, which
needs no database and works on a bare host.

**What the self-backup does not hold: the operator's offline kit.** The master key, both rclone configurations (the
worker's admin remote and the gateway's) and the mail password file are not in the self-backup. A backup encrypted
with a key derived from the master key cannot be what recovers that key. The operator keeps them offline, and the
disaster-recovery runbook names each one. TLS keys are not kept: they are issued again.

**Drill mode (ruling C8).** A console restored for a drill runs with `"drill": true` in `config.json`. It starts no agent
listener and none of these jobs: notify, digest, health ping, htpasswd sync, device purge. The worker answers only its
read methods (`ls`, `dump`, `cat_config`, `snapshots`), runs no scheduler pass and refuses `purge_repo`. Every page
shows a "DRILL COPY" banner. The drill is done on a throwaway machine in a lab: restore with the offline kit, start in
drill mode, log in, browse a real snapshot, download one file, confirm no mail arrived, destroy the machine.

**What a real restore loses.** Everything after the last self-backup: runs and audit rows since, and every device
enrolled since. Those devices' agents are refused by the restored console and go REFUSED (ADR 0018). They must enroll
again, and their old repositories remain in storage as orphans.

## Consequences

- C11 now judges the self-backup. A console whose self-backup stops raises C11 at alarm level once the newest copy
  is older than 26 h (about four missed runs), and a failed verify raises it at critical level.
- The staging copy sits on the console host's disk for the length of one run. It is the console's own database, which
  already lives on that disk: no restored client data and no key that is not already wrapped there. ADR 0008 is not
  affected. The copy is deleted at the end of a run and swept at the start of the next.
- The free-disk check means a host needs room for a second copy of the database. A run without that room fails loudly
  and writes nothing.
- The worker runs `forget --prune` and `check --read-data` on this one repository, while device repositories run
  neither during the pilot (ADR 0026). The self-backup repository is not behind the gateway, and the worker holds the
  credential that may delete in it.
- Two consoles on one bucket are unsupported. Drill mode is the guard that keeps a restored copy from acting as a
  second production console.

## What is deliberately left out and why

- **A copy on the console host or a second location.** Row 1038 allowed both. The first is lost with the host. The
  second waits until the first location has run through the pilot.
- **The offline kit inside the backup.** It would make the backup self-locking.
- **Backing up TLS keys.** They can be issued again, so a copy is only one more secret to protect.

## To be proven by spike P22-S1

| # | Assumed | Fallback if the spike says no |
|---|---|---|
| 8a | The admin credential may read, write and delete under the new prefix. | The operator grants it on the storage side before the first run. Until then every run fails with its class, and C11 reports that no self-backup has been written. |
| 8b | `forget --prune` every 6 hours costs little under bucket versioning and a minimum storage duration. | Prune runs less often than forget (for example weekly). The schedule change is recorded in a new ADR with the measured cost. |
| 8c | The gateway credential's policy is really scoped to its own prefix, so it cannot reach the self-backup prefix. | The self-backup moves to its own bucket that the gateway credential has no right to. |
| 9a | `restic backup` of the staging folder through the worker's rclone backend works, and `check --read-data` runs through it. | The self-backup does not ship. The pilot (ADR 0026, step 2) waits for a working design. |
| 9b | A bare host with only the binary, restic, rclone and the offline kit restores a working console. | The runbook and the CLI are fixed until it does. The pilot does not start before a drill passes. |

Not settled by the spike: whether an offline copy of the master key and both rclone configurations exists today (the
operator confirms before step 2 of ADR 0026), and the production database's size, which sets the free disk a run needs.
