# ADR 0026 - The pilot slice: four steps before a first client, and what the pilot runs without

**Date:** 2026-11-24 - **Status:** Accepted (operator, 2026-11-24) - **Decider:** the operator (slice order of 2026-11-23, rulings C1-C8 of plan P22 of 2026-11-24), recorded by the foreman's worker (ticket P22-D0)

## Context

architecture.md puts the pilot after M6: "**PILOT-ABLE = M6.**" (line 1381, milestone table line 1367). By then
retention and integrity checks run server-side, the console can be restored, and a file can be restored with the
device on, off or dead. M5 (gateway, server-side maintenance and keys, line 1366) and M6 (restore, line 1367) are
both still open. What the build has today:

- **Restore is a single-path download.** One file or one folder per request, streamed and never staged (ADR 0008;
  `internal/console/maintain/dump.go:39-158`). The UI lets the operator select files only
  (`internal/console/ui/restore.go:168`), although the backend takes a folder (`dump.go:143-157`).
- **Restore to the device does not exist.** The wire types do (`internal/api/restore.go:14-46`,
  `internal/api/commands.go:108-112`); the console registers no agent restore route
  (`internal/console/agentapi/service.go:117-127`), and the agent answers every restore command with
  `not_implemented_m0` (`internal/agent/commands/commands.go:154-157`).
- **A retired device cannot be restored** from the console (`internal/console/restores/restores.go:217-219`; ADR 0016,
  "Operator-facing limits").
- **The console database has no backup.** The CLI has no backup command (`internal/console/cli/cli.go:23-35`); the
  state engine's self-backup causes of C11 exist (`internal/state/banners.go:533-550`) but the loader marks them
  not in this build (`internal/console/health/probes.go:98-101`).
- **An escrowed key is verified once.** The scheduler only verifies keys whose `verified_at` is NULL
  (`internal/console/maintain/escrowverify.go:45-49`), skips retired devices
  (`internal/console/maintain/loop.go:323`), and the 30-day recheck is switched off
  (`internal/state/rules.go:83`; ADR 0014).
- **No forget, prune or check.** The worker answers "unavailable, M5" (`internal/console/maintain/methods.go:322-348`).

On 2026-11-23 the operator ordered a narrower route to a first client: what makes a pilot safe to run, not
everything M5 and M6 list.

## Decision

**The order.** The pilot slice is four steps, in this order:

1. **Restore in the console**: several files and folders as one download (ADR 0028), and restore to the original
   device (ADR 0027).
2. **The console's self-backup and a drill** that restores it on a clean machine (ADR 0029).
3. **The escrow verifier** that rechecks every escrowed key, retired devices included, and **restore from a retired
   device** by download (ADR 0030).
4. **A pilot on one client**, under the conditions below.

**What moves out of M5 and M6 into the slice.** From M6: multi-path download, restore to the original device, restore
to a side folder on the device. From M5: `db-self-backup` and its weekly verify (architecture.md job row 1038), the
periodic escrow verification ADR 0014 waits for, and matrix row r7's "archive state stays restorable"
(architecture.md line 1791; ADR 0016).

**What stays behind, built after the pilot:** device-token rotation, the rest of the usage sampler (step 8 already
samples every 6 h, `internal/console/maintain/config.go:26`), the retention jobs, key reveal, the replacement-PC relay
(DH28), the overwrite variants not named in ADR 0027, bulk actions and the complete OpenAPI.

**The pilot runs without forget, prune and check on device repositories** (ruling C7):

- Without forget and prune, a device repository only grows: nothing is removed from storage during the pilot.
- Without check, the evidence that the device repositories are intact is the restore drills: a file restored to the device
  and a zip of two folders and a file, each compared by sha256 with the original.
- The exception has a time box: **45 days** from the pilot client's first backup. Forget, prune and check are the
  first tickets built after the pilot. If day 45 comes and they are not in production, the exception has ended, and
  continuing needs a new decision, recorded as a new ADR.
- Rejected: adding check before the pilot. It is about a week of work, and the restore drills give direct evidence
  for the files that matter to the client. Rejected too: adding prune before the pilot. Prune has never run against a device
  repository in this product, its memory and time are unmeasured (lab item L-PRUNE-1, architecture.md line 2179), and a client
  repository is the wrong place to run it first.

**The pilot client holds no PHI and its keys are escrowed.** Restore to the device and the console self-backup are new
paths that will first run in production during the pilot. Until the pilot has passed, no PHI depends on them.
Escrowed keys are a requirement because every console restore path refuses a customer-held key
(`restores.go:224-226`, `internal/console/maintain/worker.go:356-362`), so a pilot on customer-held keys could not run
its own drills.

**The exit criteria are written down before day 1**, as architecture.md line 1528 already requires: every induced outage
raised an alarm (zero silent misses), at least 95 % of scheduled runs within SLA (device-off time excluded), every
restore drill passed, the console drill passed, and no path in any log, metric, mail or audit row.

**Version numbers: PROPOSED, not ruled.** The operator has not ruled on what "v1" means. The proposal, recorded here
only so the slice has a name:

| Version | Content |
|---|---|
| v0.4.0 | the pilot slice (this ADR and ADRs 0027-0030) |
| v0.5.x | the rest of M5 and M6 |
| v1.0 | every must row of M0-M7, a passed pilot and a passed console drill; the first release offered to PHI clients |
| v1.x | the should rows |
| v2 | the could rows |

Until the operator rules, nothing in the build or the docs uses these numbers except the agent's 0.4.0 version
(ADR 0027).

## Consequences

- This ADR supersedes PILOT-ABLE at architecture.md line 1381 for the first pilot. Line 1381's definition still
  describes the end of M6.
- The console's own self-backup repository is pruned by its retention (ADR 0029); the exception covers device
  repositories only.
- A pilot repository is never pruned during the pilot, so its storage use is what was written, plus the provider's
  own rules. The first prune after the pilot will remove more at once than a steady schedule would.
- An integrity fault in a file that is not drilled can go unseen for up to 45 days. This is the known cost of C7.
- Browsing slows down during the pilot. Each backup run adds one index file to the repository, only prune merges
  them, and every uncached browse call reads them all. A policy of 4 runs a day gives some 180 index files by day 45
  (`docs/lab/p22-spike.md:277-279`). This is one more reason forget and prune are the first tickets after the pilot.
- The milestone table (architecture.md lines 1366-1367) is not edited; this ADR records which of its rows run earlier.

## What is deliberately left out and why

- **A PHI client in the pilot.** Not before the new paths have run once in production.
- **The should and could rows.** The pilot proves the must path first.
- **A ruling on version numbers.** The operator has not made one. The table above is a proposal.

## What spike P22-S1 settled, and the checks before the pilot starts

The spike ran restic 0.19.1 and rclone 1.75.1 on one Windows workstation as a non-elevated user, against a gateway
and a maintain path on local disk (`docs/lab/p22-spike.md`, lines 1-52). Its results are recorded where they decide
something:

- restore and reads through the append-only gateway with the device login work (item 1); the drive-root form of an
  original-location restore is rejected and replaced by a restore per anchor folder (item 2); include files, counts,
  locked files, ACLs and alternate data streams set the agent's rules (items 3-5). All in ADR 0027.
- the tar route for folders in a multi-path zip holds, so no single-path cut line is needed (item 6). ADR 0028.
- the self-backup through the worker's rclone backend, its prune, its full read check and a bare-host restore with
  only the rclone file and the password all work (item 9). ADR 0029.

**Pilot-entry checks.** The pilot client is not enrolled until each of these has passed:

1. **Browse latency on the real bucket.** The spike measured 10 to 60 seconds per browse call on a heavily loaded PC
   against local disk, which says nothing about object storage (`p22-spike.md:250-282`). Three cold
   `ls --json --no-cache` calls on a real device repository through the admin path give the figure. If it is too
   slow to use, the worker gets an in-memory cache (tmpfs), the only cache ADR 0008 allows.
2. **The object-storage checks of ADR 0029** (spike item 8): the admin credential's rights on the self-backup
   prefix, the gateway credential's policy, and the cost of prune under the bucket's versioning.
3. **The restore checks that need the agent's account** (ADR 0027): everything the spike could not run as SYSTEM.
4. **The console drill** of ADR 0029 on a throwaway machine.
