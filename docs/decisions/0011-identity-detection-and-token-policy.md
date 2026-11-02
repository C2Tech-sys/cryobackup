# ADR 0011 - Identity-conflict detection, and what an enrollment token's policy means

**Date:** 2026-10-31 - **Status:** Accepted - **Decider:** foreman (M2 build), recorded for the operator

M2 has to detect identity conflicts as well as resolve them (ADR 0010 covers resolution), and it has to give the
policy chosen on an enrollment token a meaning. Architecture §14 names two signals for a conflict: a fingerprint
mismatch and two simultaneous sessions. Nothing in the spec says how a token's policy relates to the customer's.

## Decision

**Detection, on heartbeat**

- A heartbeat whose `hw_fingerprint` differs from the one recorded for the device sets `identity_conflict_at`
  once. Raising it again is idempotent. It writes a system audit row of critical severity, which never contains
  the fingerprint, and the device shows as IDENTITY_CONFLICT on the board.
- The heartbeat is always answered. Detection never locks a device out: a backup machine that cannot check in is
  worse than a flagged one.
- A heartbeat with no fingerprint (an older agent) is not compared.
- A device with no recorded fingerprint, such as an imported row, adopts the first fingerprint it sends. That is
  audited as `device.identity_adopt` and raises no conflict.
- **Keep** clears the recorded fingerprint, so the next machine to heartbeat becomes the device's identity. If two
  machines are really both alive under one identity, the conflict comes back on the next heartbeat from the other
  one. That is correct: the operator has to retire one of them, or re-key it when M5 brings token rotation.
- **Two simultaneous sessions are not detected yet.** That is the second signal in §14, and it is deferred to M3,
  the state engine and monitoring milestone. The fingerprint check covers the common cases: a cloned disk and a
  reimaged machine whose state directory was restored.

**Token policy, at enrollment**

- A token's policy P becomes the enrolled device's **device override layer** (DH6). P's sparse sections are
  written as the device's `device_policy_overrides` row, with the reason "Enrollment token policy: <name>". The
  device composes default, then customer policy, then P. P wins on the fields it sets, and the customer's policy
  still applies to everything else.
- The override is a **snapshot**. Later edits to the template P do not move devices that already enrolled with it,
  so a template edit's blast radius (ADR 0009) is not hidden behind enrollment history. To move those devices,
  edit their overrides or reassign.
- Enrollment is never refused because of the token's policy. The outcome is recorded in the enrollment audit row
  as one of three values:
  - `same`: P equals the customer's policy, or is empty, so no override is written;
  - `applied`;
  - `skipped`: P does not compose to a valid policy, is the default policy, or belongs to another customer.
- The device has one override row. An admin who sets an override later replaces the token's. The device's Policy
  tab shows the source as the enrollment token.

## Consequences

- Cloned and reimaged machines are flagged without breaking their backups.
- The token's policy is honoured without a new layer in the composition model or a schema change.
- Follow-ups: two-session detection (M3). If operators need a template edit to also update devices enrolled from
  it, that is a "re-apply template" action, not a live link.
