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

**Where (ruling C3).** The self-backup is a restic repository in the same bucket as the device repositories, under its
own prefix. It is written through the worker's admin remote, never through the gateway. The self-backup prefix must
not be equal to the device prefix, must not lie inside it, and must not be the device prefix with more characters
appended. An access policy whose resource is written `<prefix>*` (no slash) also matches `<prefix>-console/...`
(`docs/lab/p22-spike.md:290`), so a name of that shape could fall inside the gateway credential's rights. The example
`<prefix>-console` given while this was planned is withdrawn. The worker refuses to start the self-backup when the
configured self-backup prefix breaks this rule, and says why (foreman ruling: fail loudly). An unset prefix means the
self-backup is not configured: nothing runs, and C11 raises "no self-backup has been written" (foreman ruling).

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

**Verify, weekly and on demand.** Restore `latest:<staging path>` into the 0700 staging folder (removed afterwards,
as after a run), run `PRAGMA integrity_check`, compare schema version, row counts and sha256 with the manifest, unwrap
one escrow row with the master key, and run `restic check --read-data` on the self-backup repository. A failure sets
`VerifyFailed`: C11 critical. The loader stops marking the self-backup not in this build.

**Why `latest:<staging path>`.** A snapshot stores the absolute path of the folder it backed up, so a plain
`restore latest` recreates every folder of that path under the target (`p22-spike.md:311-312`). Naming the staging
path as the snapshot's subfolder puts the database, `config.json` and `manifest.json` at the target's root
(`internal/restic/commands.go:12-14`; the subfolder form in `p22-spike.md:84-87`). The CLI's
restore does the same.

**CLI.** `cryo-console self-backup run|list|verify`, and
`cryo-console self-backup restore --rclone-config F --remote R --bucket B --prefix P --master-key K --out DIR`, which
needs no database and works on a bare host.

**What the self-backup does not hold: the operator's offline kit.** The master key, both object-storage key pairs
(the worker's admin credential and the gateway's) and the whole console configuration directory (both rclone
configurations, the environment files, the mail password file) are kept offline by the operator, not in the
self-backup. A backup encrypted with a key derived from the master key cannot be what recovers that key. The operator
confirmed on 2026-11-24 that this kit is in place. The disaster-recovery runbook names each item. TLS keys are not
kept: they are issued again.

**Drill mode (ruling C8).** A console restored for a drill runs with `"drill": true` in `config.json`. It starts no agent
listener and none of these jobs: notify, digest, health ping, htpasswd sync, device purge. The worker answers only its
read methods (`ls`, `dump`, `cat_config`, `snapshots`), runs no scheduler pass and refuses `purge_repo`. Every page
shows a "DRILL COPY" banner. The drill is done on a throwaway Linux virtual machine in the lab, since the console runs on Linux: restore with the offline kit, start in
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

## What spike P22-S1 showed

Item 9 ran restic 0.19.1 through the rclone backend with the worker's own options (`--retry-lock 30s --no-cache` and
`rclone serve restic --stdio`), on a separate prefix on local disk (`docs/lab/p22-spike.md:295-315`):

- `init`, three `backup` runs of a staging folder (database, `config.json`, `manifest.json`), `forget --prune` with
  the retention of ruling C4, a `forget --prune` that removed two snapshots and their packs, and `check --read-data`
  all exit 0;
- a restore on a bare environment holding only the rclone configuration file and the password gave a database whose
  sha256 equals the last staging copy, and created no cache directory;
- prune without a cache warns that it may be very slow. That is the cost ADR 0008 accepts.

## Checks on the real object storage, before the self-backup ships

The spike's rig had no object storage, so spike item 8 stays with the operator, on the production account
(`p22-spike.md:284-293`). If a check fails, the self-backup does not ship until it passes, and C11 keeps saying that
no self-backup has been written.

1. **The admin credential's rights on the self-backup prefix.** `init`, `backup`, `forget --prune` and
   `check --read-data` through the admin remote all exit 0. This needs list, get, put and delete under the prefix.
2. **The gateway credential cannot reach it.** A device login through the gateway is refused on the self-backup
   repository's `config`, and every resource in the gateway credential's policy ends in a slash.
3. **The cost of `forget --prune` every 6 hours** under the bucket's versioning, its rule for non-current versions,
   any minimum storage duration and any minimum billed object size (restic writes and deletes small lock objects on
   every call). If the estimate is too high, prune runs less often than forget, and a new ADR records the schedule
   with the measured cost.
4. **The gateway credential's policy is prefix-scoped**, read from the storage side, because the repository cannot
   show it.

Still not settled: the production database's size, which sets the free disk a run needs.
