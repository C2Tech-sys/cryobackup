# ADR 0033 - Rulings from the review of the escrow verifier and the drill: no lock, a recheck floor, one drill mode for both processes

**Date:** 2026-11-26 - **Status:** Accepted (foreman, 2026-11-26; the operator was told) - **Decider:** the foreman, for the operator, on the evidence of the blind review of tickets P22-H and P22-K; built by ticket P22-R2b

ADRs are not edited once accepted (`CONTRIBUTING.md`, "Decisions"). The blind review of the escrow verifier (ADR 0030) and the drill
(ADR 0029, ruling C8) found places where the code did not do what those ADRs and ADR 0031 say, and one question ADR 0030 left to the
build. This ADR records the rulings. It changes no operator decision (rulings C1 to C8 of plan P22 stand).

## Decision

### 1. `restic cat config` runs with `--no-lock` in every mode (makes ADR 0031, section 5, true)

ADR 0031 says an escrow check "reads only the repository's config". It did not: `restic cat config` without `--no-lock` creates a lock
object in the repository and deletes it again (restic 0.19.1, proven with `locks/` write-denied: the call failed on the lock write;
with `--no-lock` it returned the config). The weekly check therefore wrote to every repository it checked, retired ones included, and
on retention-locked storage the lock objects would have piled up. Now the escrow verifier, "Verify now" and enrollment's config read
all run `cat config` with `--no-lock` and no `--retry-lock`, in production mode as in drill mode (`Worker.noLockCommon`,
`internal/console/maintain/worker.go`). Other reads keep `--retry-lock`: they need the repository's lock protocol.

### 2. `sla.escrow_recheck_days` is 0 (recheck off) or at least 7 (closes the open point of ADR 0030)

ADR 0030 left to the build whether the recheck gets a floor. The verifier re-proves a key weekly, so a recheck shorter than a week
raises ESCROW_UNVERIFIED between two checks of a healthy key. Policy validation now refuses 1 to 6 and names
`sla.escrow_recheck_days` (`internal/policy/validate.go`). 0 stays legal and now means what the field's other zeros mean, "off": a
proven key's age never raises ESCROW_UNVERIFIED; a recorded failure and a never-verified key still do. (Before, 0 made every key
overdue at once; ADR 0014 had kept the rule off, so nothing showed it.) The default stays 30.

### 3. The verifier's due time is exact, and a failed key is retried hourly

The verifier's due time is the proof plus a week less the device's stable stagger offset (up to a day sooner). A recorded failure
(`wrong_password`, `repo_missing`) is due again as soon as the one-hour retry hold after the last attempt has passed, not at the next
weekly time: a repaired repository clears its alarm within the hour. A proof time more than five minutes in the future (a console
clock that ran ahead) proves nothing: the verifier re-proves it, and the state rules treat it as unverified, so it never reads green.
The device page words "Due now" from the same function (`maintain.EscrowVerifyDueAt`), never a day early.

### 4. The candidate set is checked against the devices that should be in it

The verifier's candidates are every confirmed, non-purging device with an escrowed key. A second query counts those devices by another
route; fewer candidates than that is a read failure (`store.ErrEscrowCandidates`): the verifier logs it, the System page's escrow row
says "not checked" and banner C22b is raised. An empty candidate set while such devices exist is never a quiet green.

### 5. serve and maintain must agree about drill mode (amends ADR 0029, "Drill mode")

Both processes read the drill switch from their own environment, so one can be on while the other is off (`CRYO_DRILL=false` in a unit's
`EnvironmentFile` beats `"drill": true` in the file): a drill console in front of a worker that runs the scheduler, the escrow verifier
and the self-backup against production storage. The worker's ping now reports `drill`. A console whose mode differs from its worker's
raises a critical banner on every page (`worker_mode_mismatch`) and logs an error; `cryo-console doctor` fails the maintenance-worker
line. An unreachable worker is not a mismatch (banner C07 says so). A worker too old to report the field reads as not in drill.

### 6. The offline `self-backup list` and `restore` take `--drill`

In a drill the production self-backup repository is live under another console. `cryo-console self-backup list` and `restore` now take
`--drill` (default `$CRYO_DRILL`) and then read with `--no-lock` and no `--retry-lock`, so they leave no lock object there; the restore
writes only to its local `--out`. Without it they keep `--retry-lock`.

## Consequences

- A check of an escrowed key never writes to the repository. A drill copy still writes nothing to storage (ADR 0031, section 6).
- A policy that sets `escrow_recheck_days` between 1 and 6 is refused wherever it is validated: on save, and when the store or an agent
  reads one. No shipped policy does (the default is 30); a stored one is not rewritten, so check the pilot's policies before upgrading.
- The ping's new field is additive; an older console ignores it.
