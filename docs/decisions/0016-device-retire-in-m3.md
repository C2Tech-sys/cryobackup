# ADR 0016 - Device retire moves from M5 to M3, and deleting a token hides it

**Date:** 2026-11-06 - **Status:** Accepted - **Decider:** the operator (request of 2026-11-04, ticket M3-T6), recorded by the foreman's worker

architecture.md rows 707 (`POST /api/v1/devices/{id}/retire`) and 922 (`POST /ui/devices/{id}/retire`) and matrix row r7
put device retire in M5. The dead lab devices of the M2 and M3 runs stay on the board until then: UNSEEN and STALE,
critical, every day. The 7-day run must start from a fleet where every red device means something, so the operator
pulled retire into M3, together with a way to clear old enrollment tokens.

## Decision: retire

`POST /api/v1/devices/{id}/retire` (admin role, a reason, one audit row `device.retire`) is the one handler; the UI's
`POST /ui/devices/{id}/retire` goes through it in process. One transaction:

- sets `retired_at`, `retired_reason` and, when given, `replaced_by_device_id`;
- revokes every device token and the gateway login, and clears any pending wrapped gateway secret;
- ends a pause (the schema refuses retired and paused together);
- cancels commands still queued, since nothing can collect them any more.

After the commit the handler signals `htpasswd-sync`, so the login leaves the file at once rather than at the next
five-minute run (§7.2 "revoke"). A failed audit write refuses the whole change. Retiring a retired device answers the
same state with `changed: false`, writes nothing, and the first reason stands.

**What "stops schedules" means here.** The console's own schedule stops: `maintain` selects only non-retired devices
(`maintain/loop.go`). An agent still running on a machine cannot be told anything, because the token that would carry
the message is gone. It keeps its local schedule until someone uninstalls it, and every attempt fails at the console
and at the gateway with a 401. No data moves.

**What retire does not delete.** Nothing: not the repository, the snapshots, the escrowed key (`repo_key_escrow`), the
run history or any audit row. Purging is the separate owner action with fresh MFA (`DELETE /devices/{id}`, M10).

**Two limits the operator should know, both stated in the retire dialog or on the device page:**

- *Restore.* `restores.Service.Create` refuses a retired device today, so its data is kept but cannot be restored from
  the console. Matrix row r7 ("archive state stays restorable") stays with M5. Retire is terminal, so retire a device
  only when its backups are no longer needed from the console, or restore first.
- *Retention.* §7.3 step 0 skips retired devices only past retention, but `maintain` skips every retired device. Retention
  therefore does not run on a retired device, and its data stays until the purge rather than until retention expires. It
  errs towards keeping data. M5 owns making it match §7.3.

**Final backup is a separate step, not a deferred retire.** `final_backup: true` queues a `backup_now` and answers 202 with
the device not retired; the admin retires it after that run. A backup needs the credentials a retire takes away, so the
two cannot share a transaction. A deferred retire would need a pending state, a job to finish it and a rule for a device
that never comes back, and the devices this exists for are the ones that never come back. M5 can add that on top.

**The state engine does not change.** `internal/state` already has the rules: RETIRED never alarms and suppresses
everything (§4.2 row 1, §4.3, B14), is excluded from rings and rollups (B15), and leaves the health denominator (§4.5,
D10). "Zero active devices" is judged on non-retired devices, so retiring an expected customer's last device reads
MISSING and critical (§4.5, D02, §9). That is deliberate: silence is an alarm. To retire a customer's last device
without the alarm, clear `expected` first.

**Lists.** A retired device is on no list as an ordinary row. The board's customer groups, `/devices`, a customer's Devices
tab, Overdue, Not seen, Activity and a token's device list hold them under a collapsed "Retired (n)".

## Decision: deleting a token hides it

`POST /api/v1/enrollment-tokens/{id}/delete` and `POST /ui/enroll/tokens/{id}/delete` set a new column,
`enrollment_tokens.deleted_at` (migration 0006). It is a POST and not `DELETE` on the token because §5.2 already gives
that verb to revoke.

- Only a token that can no longer enroll may be hidden: revoked, or expired. An active or used-up token answers 409
  (`token_live`); revoke it first. A schema CHECK holds the same rule below the store, so a live credential cannot leave
  the operator's sight.
- The row stays. Each device keeps `enrollment_token_id`, a direct read still finds the token with `deleted_at` set, and
  every audit row stays. One new audit row, `enrollment_token.delete`. A repeat writes nothing.

`/enroll` becomes the token table, active tokens first; "New enrollment" opens the wizard at `GET /enroll/new`.

## Consequences

- architecture.md rows 707 and 922 and matrix row r7 still read M5; this ADR overrides their milestone for the retire
  action. r7's "stays restorable" and row 711's bulk retire (M7) are not done here.
- `GET /enroll` no longer carries the form. Anything linking there to create a token links to `/enroll/new`.
- Retire is terminal. A machine that comes back enrolls as a new device, as §4.2 row 1 says.
