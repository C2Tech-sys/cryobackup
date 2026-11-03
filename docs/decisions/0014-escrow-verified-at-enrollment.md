# ADR 0014 - The escrow is verified at enrollment; the 30-day recheck waits for its job

**Date:** 2026-11-02 - **Status:** Accepted - **Decider:** foreman (M2 lab fix batch, ticket M2-FIX-B1), recorded for the operator

Architecture §4.2 row 12 raises ESCROW_UNVERIFIED for an escrowed key when there is no active escrow row,
`verified_at` is NULL after `escrow_verify_grace_hours`, the verification is older than `escrow_recheck_days`, or
the last verification failed. It clears on "successful `cat config` verification". In the M2 lab every escrowed
device turned ESCROW_UNVERIFIED 24 h after enrollment (finding F7). Two things were missing: nothing ever wrote
`repo_key_escrow.verified_at`, and the periodic verification job that row 12 assumes is scheduled for M5 and does
not exist yet.

## Decision

- **Enrollment's check is the verification.** Enrollment already runs `restic init` and then `restic cat config`
  through the admin path with the escrowed key before it answers 200. When that cat config succeeds, the same
  request sets `verified_at` (and `last_verify_attempt_at`) and clears `last_verify_error_class`. It writes no
  audit row of its own; the `agent.enroll` row covers it. A failed write is logged at error level and does not
  fail the enrollment: the device then alarms after the grace, which is loud.
- **No "recheck overdue" before M5.** Until the periodic verification job exists, the state engine does not
  raise ESCROW_UNVERIFIED for a verification that is only older than `escrow_recheck_days`. The switch is the
  constant `state.EscrowRecheckEnforced` (false); the rule stays implemented and tested with the switch on.
  The change that adds the M5 job sets it to true and updates `TestEscrowRecheckIsNotEnforcedUntilTheVerifyJobExists`
  and case B37. Missing escrow, a recorded verification failure, and "never verified after the grace" are
  raised as before.
- **Devices enrolled before this fix recover through the maintenance scheduler.** On a pass, the scheduler
  runs `cat config` for any maintenance candidate (escrowed, confirmed, not retired) whose active key has
  `verified_at` NULL, at most once an hour per device, and records the outcome. A success sets `verified_at`.
  `wrong_password` or `repo_missing` is recorded as `last_verify_error_class` (reason "verify failed"). Anything
  else (storage unreachable, a timeout, a busy worker) records only the attempt, so the device stays "never
  verified". A key verified once is not checked again by this path. No migration backfills `verified_at`: the
  only trace of the earlier check is the `agent.enroll` audit row, which says the enrollment finished but not
  which key opened which repository, and a real check costs one cat config per device, once.

## Consequences

- A freshly enrolled escrowed device is green on escrow from its first minute, and so is a lab device enrolled
  before the fix, after one maintenance pass.
- Until M5, a key that was verified once is trusted indefinitely. If the repository is later replaced, or its
  key no longer opens it, that shows up on the next restore or browse, not on the board. This is a known gap.
  Closing it is the M5 job's purpose. It is accepted because the alternative, a device that alarms on day 31
  with no means of clearing the alarm, teaches operators to ignore the alarm.
- Architecture §4.2 row 12 reads as written once `EscrowRecheckEnforced` is true. Until then, the "older than
  `escrow_recheck_days`" clause is suspended by this ADR.
