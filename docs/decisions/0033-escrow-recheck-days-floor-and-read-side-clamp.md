# ADR 0033 - `sla.escrow_recheck_days` has a floor of 8 days on write, and a stored lower value is clamped on read

**Date:** 2026-11-26 - **Status:** Accepted (foreman, 2026-11-26; operator told) - **Decider:** the foreman, for the operator, on the evidence of the blind review of ticket P22-H; built by ticket P22-R2b

ADRs are not edited once accepted (`CONTRIBUTING.md`, "Decisions"). ADR 0030 left one question to the build: whether the policy field
`sla.escrow_recheck_days` gets a minimum ("Open points for the build"; its Consequences say a recheck below 7 days "would raise the
alarm between checks"). This ADR answers it. It changes no other decision of ADR 0030: the recheck stays enforced
(`state.EscrowRecheckEnforced`), the verifier stays weekly.

## Decision

1. **The floor is 8 days.** A write that sets `sla.escrow_recheck_days` to 1 to 7 is refused, naming `sla.escrow_recheck_days`. 8 is
   `state.EscrowVerifyMaxAge` (a week and a day): the weekly verifier needs a day of slack, or a healthy key is overdue between two checks.
   7 would be a floor with no slack. The default stays 30.
2. **0 is not changed and is not "off".** Policy validation has always accepted 0 (`>= 0`), and the engine has no special case for it: a
   recheck interval of 0 days makes every proven key overdue at once (`state.escrowIssueRecheck`, `now - verified > 0`), so ESCROW_UNVERIFIED
   stays raised. That is its meaning at `fa3b4c0` and it is still its meaning; this ADR does not add a "use the default" or an "off". Nothing
   turns the recheck off except removing `EscrowRecheckEnforced` by a new ADR.
3. **Write paths refuse; read paths never fail on it.**
   - Write: `policy.ValidateWrite` (the policy editor, `policy.Patch`) and `policy.CheckEscrowRecheckWritten` on the input of a save
     (policy create and update, override set) refuse 1 to 7 with the field named. A policy update merges the stored sections into
     what it checks, so on a policy whose stored value is below the floor any save through the API (a rename, a retention edit) is
     refused, loudly and naming `sla.escrow_recheck_days`, until the save also sets that field to 0 or at least 8. The web policy editor
     starts from the clamped value, so a save there writes 8.
   - Read: the console's effective-policy composition (`effectiveDoc`, `composeLayers`, the enrollment grace read, hence the agent policy
     and the facts load), and the agent's policy decode and its last-known-good copy, raise a stored 1 to 7 to 8 (`Policy.ClampStored`) and
     go on. The first clamp in a process logs one warning of class `policy_clamped` with the stored and the used number (no path, no name).
     `policy.Validate`, the read-side check, does not refuse the value.

## Consequences

- A policy stored with 1 to 7 before this ADR keeps working, and is judged as 8 days. Reads never rewrite it; an API save of that policy is
  refused until the value is 0 or at least 8, and a save from the web policy editor writes 8. Look for the `policy_clamped` warning after upgrading.
- An agent older than this ADR still refuses nothing new (it never had the floor); an agent with it accepts an older console's value.
- `policy.EscrowRecheckMinDays` is tied to `state.EscrowVerifyMaxAge` by a test.
