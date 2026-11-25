# ADR 0030 - Every escrowed key is rechecked weekly, retired devices included, and a retired device can be restored by download

**Date:** 2026-11-24 - **Status:** Accepted (operator, 2026-11-24) - **Decider:** the operator (slice order of 2026-11-23, plan P22 of 2026-11-24), recorded by the foreman's worker (ticket P22-D0)

This ADR completes ADR 0014: it adds the periodic verification that ADR 0014 left to M5, and it turns
`EscrowRecheckEnforced` on. It replaces the *Restore* limit in ADR 0016's "Operator-facing limits": a retired device
can now be restored from the console, by download only.

## Context

- **A key is verified once.** Enrollment's `cat config` sets `verified_at` (ADR 0014). The scheduler verifies only keys
  whose `verified_at` is NULL, at most once an hour per device (`internal/console/maintain/escrowverify.go:15`,
  `escrowverify.go:45-49`), at most 5 per pass and none after 2 minutes (`escrowverify.go:29-30`). Its candidates
  exclude retired devices (`internal/console/maintain/loop.go:323`).
- **The recheck is switched off.** `state.EscrowRecheckEnforced` is false (`internal/state/rules.go:83`). ADR 0014
  accepted the gap: a key verified once is trusted indefinitely, and a replaced or unreadable repository shows up only
  at the next restore or browse.
- **A retired device keeps its data and its key.** Retire deletes nothing: not the repository, the snapshots, the
  escrowed key or the snapshot rows (ADR 0016). But its restore is refused: `restores.Service.Create` answers a
  retired device with a generic invalid-request error (`internal/console/restores/restores.go:217-219`), the restore
  page shows "This device is retired" (`internal/console/ui/restore.go:96-97`) and the device page hides the action
  (`internal/console/ui/device.go:384-385`).
- **The worker does not refuse a retired device.** Its `prepare` resolves the device, refuses a customer-held key and
  unwraps the password, with no retired check (`internal/console/maintain/worker.go:346-387`). A download of a device
  being purged is refused (`restores.go:476-482`; ADR 0019).
- **A retired device's gateway login is revoked** (ADR 0016). Only the console's path, through the worker's admin
  remote, can read its repository.

## Decision

### The verifier

- **Its own candidate query.** Every device with an active escrowed key and a confirmed enrollment that is not being
  purged, **retired devices included**. It no longer reuses the maintenance candidates.
- **Due** when `verified_at` is NULL or older than 7 days, staggered by device id so the fleet does not come due on
  the same pass.
- **Same bounds:** at most 5 checks a pass, none started after 2 minutes.
- **Same proof:** unwrap the key and run `restic cat config` through the admin path, as enrollment does.
  `wrong_password` and `repo_missing` are failures. Anything else (storage unreachable, a timeout, a busy worker)
  records only the attempt.
- **`EscrowRecheckEnforced` becomes true.** The change updates case B37 and
  `TestEscrowRecheckIsNotEnforcedUntilTheVerifyJobExists`, as ADR 0014 says. Architecture §4.2 row 12 then reads as
  written.
- **Retired devices.** RETIRED suppresses every device alarm (ADR 0016), so a retired device whose check fails cannot
  raise its own. Instead a fleet banner says "Escrow verification failing for N retired devices", and the System page
  shows the count.
- **On demand.** `POST /api/v1/devices/{id}/escrow/verify` (admin role) and a "Verify now" button on the device page
  run one check now.
- **What it proves:** that the escrowed key opens the repository's config. It does not prove that the data is intact.

### Restore from a retired device

- `restores.Service.Create` allows `download_zip` for a retired device that is not being purged.
- Device modes (ADR 0027) are refused by name: "This device is retired: download only." The agent's login is gone, so
  it could not read the repository anyway.
- The retired device's page gets the Restore action and the snapshot refresh (the worker's snapshot list, ADR 0027),
  since the agent that used to fill the snapshot cache no longer reports
  (`internal/console/store/runs.go:670`).
- A retired device with a customer-held key stays refused, as every customer-held device is.

## Consequences

- A repository that is replaced, emptied or no longer opened by its escrowed key is found within about a week, on
  the board for an active device and on the fleet banner for a retired one, instead of at the next restore.
- The default `escrow_recheck_days` is 30 (`internal/policy/policy.go:396`), so a weekly check keeps a healthy key
  clear of ESCROW_UNVERIFIED with room for three missed weeks. Policy validation accepts any value from 0
  (`internal/policy/validate.go:271-276`); a policy that sets the recheck below 7 days would raise the alarm between
  checks.
- A retired device costs one `cat config` a week until it is purged.
- Retire is no longer a reason to restore first: "restore first, or retire only when the backups are no longer
  needed" in ADR 0016 now applies to purge (ADR 0019), not to retire.
- Matrix row r7's "archive state stays restorable" (architecture.md line 1791) is met for download.

## What is deliberately left out and why

- **A data-integrity check of escrowed repositories.** That is `restic check`, which the pilot runs without
  (ADR 0026).
- **Device restore for a retired device.** Its gateway login is revoked by design (ADR 0016), and giving it back for a
  restore would undo the retire.
- **Alarms on the retired device itself.** RETIRED suppresses device alarms by design. The fleet banner carries the
  signal instead.

## Spike P22-S1

This ADR rests on no new restic behaviour: `cat config` with the escrowed key through the admin path is already in
production use (enrollment and the scheduler, ADR 0014), and so is the streamed download (ADR 0008). The spike
confirmed the tar route of ADR 0028 (`docs/lab/p22-spike.md:363-376`), so a retired device's download is the same
multi-path zip as any other device's.

## Open points for the build (P22-H)

- The fleet banner for retired devices whose verification fails has no code yet; the build ticket assigns one.
- Whether `escrow_recheck_days` gets a minimum of 7 (see Consequences) is left to the build ticket (foreman ruling).
