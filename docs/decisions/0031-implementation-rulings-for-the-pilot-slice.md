# ADR 0031 - Implementation rulings of the pilot slice that change the wording of ADRs 0027, 0029 and 0030

**Date:** 2026-11-26 - **Status:** Accepted (foreman, 2026-11-26; the operator was told) - **Decider:** the foreman, for the operator, on the evidence of the build and blind checks of tickets P22-C, P22-D, P22-F, P22-H and P22-K; recorded by ticket P22-J

ADRs are not edited once accepted (`CONTRIBUTING.md`, "Decisions"). Building ADRs 0027, 0029 and 0030 showed seven places where
the sentence of the ADR and the code that shipped differ. The code is right in each place and each was ruled by the foreman
while the ticket ran. This ADR records the seven rulings so that a reader of 0027, 0029 or 0030 finds what is true. It changes no
operator decision (rulings C1 to C8 of plan P22 stand) and adds no behaviour: it states what the build does.

## Decision

### 1. The free-space size comes from a dry run of the restore, per volume (amends ADR 0027, "Rules shared by both device destinations", *Free space*)

ADR 0027 says the agent "sums the selection's sizes from `restic ls`". It does not. The agent runs the same restore with
`--dry-run -vv` once per volume the selection touches, with the same include file and a target on that volume, and reads
`total_bytes` of restic's summary (`internal/agent/restore_run.go:60-82`; the call and the comparison in
`internal/agent/restore.go:399-422`; the builder option, `internal/restic/commands.go:27-29` and `:79-80`). The sizes of
all groups on one volume are added and compared with that volume's free space; the destination volume for the original
location, the system drive for a side folder.

Why: the product's `ls` builder lists one folder, one level, and never emits `--recursive`
(`internal/restic/commands.go:103-108`), so it cannot give the size of a folder tree without a walk of its own. A dry run counts
what restic would write, writes nothing, needs no target folder, and runs under the same watchdog as the restore
(`superviseRestore`): its failures end the restore with the class they would have had (`auth`, `repo_unreachable`, `other`).
Cost: one more restic run per volume, and each run reads the repository's index files (ADR 0028 states the same cost for a
download). Behaviour that stays as ADR 0027 says: a short volume is refused at once, FAILED with class `disk`, before
anything is written.

### 2. The owner of everything in a side folder is Administrators (amends ADR 0027, "Side folder")

ADR 0027 resets the side folder's subtree to inherit from the side folder, so that only Administrators and SYSTEM can open
it. A restored file still carries the snapshot's owner after that reset, and an owner holds `READ_CONTROL` and `WRITE_DAC`
implicitly: the user who owned the file could read its new permissions and grant themselves access back, which would
break the guarantee of ruling C2. So the same walk that resets each entry also sets its owner to `BUILTIN\Administrators`
(`internal/agent/restoreplace/side_windows.go:27-34`, foreman ruling D1, 2026-11-25), and the agent's check after the
walk fails the restore if an entry is not owned that way.

The snapshot's owner is therefore **not** kept for files in a side folder. It is kept for files placed at the original
location (ADR 0027, consequences). Lab check 6 of `docs/lab/p22-agent-system.md` proves it as SYSTEM: a user the file
once belonged to is refused both the read and a grant.

### 3. A side restore that does not succeed removes its folder (amends ADR 0027, "Side folder" and "What the operator sees when it does not work")

ADR 0027 removes the staging folder of the original-location restore whatever the outcome. It does not say what happens to a
side folder whose restore stopped halfway. A half-restored side folder can hold files that never received the reset and the
owner change, so it is not a folder only Administrators can open. The rule that shipped
(`internal/agent/restore_plan.go:126-152`):

- A side restore that ends **cancelled, failed or interrupted** after restic wrote something removes its folder, with the
  clean-up walk of ADR 0027 (never following a reparse point), **before** its run is closed. Its four counts are then 0.
- A **partial** side restore (some files placed, some errors) is removed too, unless every group's reset and owner change
  completed. A partial one that is removed ends FAILED with class `other`. A partial one that is fully secured stays: it is
  the operator's result, shown as RESTORED WITH ERRORS.
- If the removal itself fails, the restore ends FAILED `other`, Event Log 1021 is written, and the next agent start tries again
  (`internal/agent/restore.go:178-210`).
- At every agent start the sweep removes the side folder of each restore whose journalled run closed non-success or was
  abandoned (`internal/agent/restore_run.go:323-345`). A side folder of a successful or partial-and-secured restore, and one
  the journal does not know, is never touched.

What the operator does: run the restore again. The status page already says so for `interrupted`.

### 4. The disk space needed and free stays in the device log; no wire field carries it (amends ADR 0027, "What the operator sees when it does not work")

ADR 0027 says a `disk` failure shows "the space needed and free". `RunStats` has no field for it, and the plan's rule is that
the restore record is path-free and carries counts and classes only (ADR 0027, "Where the counts come from"). Two numbers and
a drive letter are not a path, but a new field in the agent protocol is a public-interface change this slice does not need.
The ruling: the console shows FAILED, class `disk`, with the text "Free space on the device, or restore fewer items"
(`internal/console/ui/restore_status.go:132-133`), and the agent writes one line to its own log,
`restore refused: not enough free space`, with the drive letter, `needed_bytes` and `free_bytes`
(`internal/agent/restore.go:419-420`). The operator reads the numbers on the device. A later ADR may add a wire field.

### 5. "Verify now" needs the admin role and no fresh two-factor check (amends ADR 0030, "The verifier", *On demand*)

ADR 0030 says `POST /api/v1/devices/{id}/escrow/verify` is for the admin role. Ruling C5 asks for a fresh two-factor check on
every restore, and ADR 0019 on destructive owner routes. This route is neither: it reads only the repository's config
through the admin path, reveals no key, and changes no device. So it needs the admin role, like retire, and **no** step-up
(`internal/console/adminapi/escrow_p22.go:23-26`). An API token with the admin role may call it. It still writes exactly one
audit row, `device.escrow.verify`, with the verdict and a fixed class and no path, and the record and the row commit together
(a failed audit write refuses the record). It is refused for a customer-held key, a device being purged, a device without an
active escrowed key or a confirmed enrollment, and answered 404 outside the caller's scope; a retired device is verified like
any other.

### 6. A drill copy reads with `--no-lock` and runs no unlock (amends ADR 0029, "Drill mode")

ADR 0029 limits a drill copy to the worker's read methods. A read through restic still **writes** to storage: it creates a lock
object in the repository while it runs, and the idle watchdog's recovery after a stalled download (`restic unlock`, ADR 0028)
deletes. A drill copy holds the production admin credential, so "reads only" has to mean the repository is not touched. The
ruling: in drill mode every restic run carries `--no-lock` and no `--retry-lock` (`internal/console/maintain/worker.go:350-360`,
`internal/console/maintain/drill.go:15-35` and `:75-76`), and the stale-lock unlock never starts (`enterDrill` closes the
worker's background work). The live check of ticket P22-K counted lock objects in the same repository: none in drill mode, one
in the control run, and an identical repository tree after both. A drill copy writes nothing to storage.

Cost: a drill read has no lock, so it is not protected against a prune running at the same moment. The pilot runs no prune
on device repositories (ADR 0026), and a drill read that fails for that reason is run again.

Related and recorded here so ADR 0029's "sends no mail" is exact: `cryo-console doctor` on a drill copy does not probe the mail
relay. It prints a NOT CHECKED line for it (ticket P22-K, fix batch 2).

### 7. The self-backup runs only while the worker has a restic slot to spare, and says `no_slot` when it has not (adds to ADR 0029, "When")

ADR 0029 does not say how the self-backup shares the worker's restic slots. The worker runs at most `MaxConcurrent` restic
processes (4 by default), and technician downloads may hold up to `CRYO_MAINTAIN_MAX_STREAMS` of them (2 by default) for as
long as each takes. The self-backup never takes a stream token, so a technician's download is never refused for it. It runs
only when `MaxConcurrent - MaxStreams >= 2`: one slot for itself and one more that no download can hold, for device
maintenance, browse, snapshots, the storage probe, escrow verify and purge
(`internal/console/maintain/selfbackup.go:56-72` and `:367-371`). Otherwise it runs nothing, records the class `no_slot` on the
`db-self-backup` job row (`selfbackup.go:121`, `:284`) and `cryo-console self-backup run` exits 2.

At the default (2) it runs. At 3 it never runs, and that is loud on purpose: C11 raises at once on a console that has never
written a self-backup, raises after 26 hours on one that has, and turns critical when no verify verdict has been reached for
8 days. The disaster-recovery runbook (section 6) and `deploy/systemd/README.md` say to keep the default.

## Consequences

- A reader of ADR 0027, 0029 or 0030 who finds a sentence that disagrees with the code, in one of the seven places, reads this
  ADR. The rest of those ADRs stands.
- Rulings 1, 3 and 4 touch the agent only; 2 touches the agent's side-folder walk; 5 the admin API; 6 and 7 the maintenance
  worker. None touches the wire contract, the schema or the operator's decisions.
- Agent 0.4.0 and console 0.4.0 are built to this ADR.

## What is deliberately left out and why

- **A wire field for the free space.** Not needed to put a file back, and a change to the agent protocol.
- **Keeping the snapshot's owner in a side folder.** It would reopen the folder to the user the owner names (ruling 2).
- **Keeping a half-restored side folder for the operator to look at.** It would be a folder that is not secured (ruling 3);
  the restore can be run again.
- **A step-up for "Verify now".** It would ask for a second factor to read a repository's config (ruling 5).
- **A drill that takes locks.** A drill that writes to production storage is not a drill (ruling 6).
