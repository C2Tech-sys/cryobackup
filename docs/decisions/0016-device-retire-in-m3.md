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

A failed audit write refuses the whole change. Retiring a retired device answers the same state with `changed: false`,
whatever the reason says, writes nothing, and the first reason stands.

**Revoked in the database is not gone from the gateway.** The gateway reads a file, and the `htpasswd-sync` job writes it.
That job can fail, for example on a disk or permission fault, and the login then still works. A retire during that fault
is the worst place for a false calm, because it may be a compromised endpoint. So:

- **The file is the only witness.** `HtpasswdSync.Check` reads the deployed file and returns which retired devices' logins
  are in it: usernames only, never hashes, never logged. It changes nothing. Nothing is inferred from when the job ran: an
  earlier design compared the job's start with the retire and answered `removed` with the login in the file.
- The handler signals the job and reads the file for at most 5 s. `gateway_login: removed` means a read after the retire
  committed found the login gone. Anything else is `pending` with a reason class, and a read that fails is `pending` at
  once. The notice and device page say plainly that the gateway may still accept the login until the sync succeeds.
- Banner **C17** (critical) is up while the newest clean read found a retired device's login, and only a clean read clears
  it. **C17b** (not checked) is up when there are retired devices and the file cannot be read or has not been read lately,
  when the retired devices cannot be read, or when a retired device has an unusable `retired_at`. The measurement runs on
  the health-prober's tick (60 s by default) and at the end of every run of the job, so a failed sync and an old copy put
  back are both seen within about a minute. A second console's prober does not measure, so it reads C17b.
- **Runs do not overlap.** The job has one slot in-process, whoever starts a run (signal, timer, Run now). A run takes it
  before it reads the logins, so a later run can never be overwritten by an earlier one. A check takes it too, so it waits
  for a run that may hold logins read before the commit; a login a check finds gone therefore stays gone. No new lease
  across consoles: the runner's lease and the generation header already stand guard, and two consoles on one gateway is
  unsupported (ADR D3).
- **Two consoles on one file (known limit, recorded by M3-HARDEN).** A second console sharing the gateway file can write a
  retired login back after `removed`: its sync may hold logins read before the retire. The next tick of the first console's
  login check reads the file and raises C17, and its sync repairs it. One console per gateway is the supported setup.
- **How long the answer can take.** After the commit: 5 s for the reads, then at most 2 s for the outcome's audit row, so
  7 s on a working disk. A COMMIT cannot be cancelled, so a disk that hangs inside that row's commit holds the answer past
  7 s for as long as it hangs (amended by M3-HARDEN; the store's own shutdown bound still ends the process). Before the commit a retire queues for the store's one writer like any other write, so that part is bounded only by
  the request. A read of a hung mount cannot be cancelled, so it runs apart from the answer.
- **Two audit rows, not one (foreman ruling).** `device.retire` commits with the retire, so an unwritable audit table
  refuses it. Its outcome cannot be known until the file has been read after that commit, and `trg_audit_events_immutable`
  forbids amending the row. So `device.retire_sync` records `removed` or `pending` when the wait ends. A later successful
  sync writes no retire row; the job audits its own run (`gateway.htpasswd.sync`).

**What "stops schedules" means here.** The console's own schedule stops: `maintain` selects only non-retired devices
(`maintain/loop.go`). An agent still running on a machine cannot be told anything, because the token that would carry
the message is gone. It keeps its local schedule until someone uninstalls it, and every attempt fails at the console
and at the gateway with a 401. No data moves.

**What retire does not delete.** Nothing: not the repository, the snapshots, the escrowed key (`repo_key_escrow`), the
run history or any audit row. Purging is the separate owner action with fresh MFA (`DELETE /devices/{id}`, M10).

**Operator-facing limits, stated in the retire dialog or on the device page:**

- *Restore.* `restores.Service.Create` refuses a retired device today, so its data is kept but cannot be restored from
  the console. Matrix row r7 ("archive state stays restorable") stays with M5. Retire is terminal, so retire a device
  only when its backups are no longer needed from the console, or restore first.
- *Retention and purge.* §7.3 step 0 skips retired devices only past retention, but `maintain` skips every retired device
  (`maintain/loop.go:323`). Retention therefore does not run on a retired device, and its data stays until the owner's purge
  (M10) rather than until retention expires. It errs towards keeping data. M5 owns making it match §7.3.
- *A live agent.* It keeps trying until it is uninstalled: the console cannot tell it to stop. While an enrollment token is
  active, a recurring RMM script enrolls the same machine again, with a new nonce, as a new device. Stop the script or revoke
  the token.

**Final backup is a separate step, not a deferred retire.** `final_backup: true` queues a `backup_now` and answers 202 with
the device not retired; the admin retires it after that run. A backup needs the credentials a retire takes away, so the
two cannot share a transaction. A deferred retire would need a pending state, a job to finish it and a rule for a device
that never comes back, and the devices this exists for are the ones that never come back. M5 can add that on top.

**The state engine's device rules do not change**, and its one addition is banner C17 over the measured fact above.
`internal/state` already has the rules: RETIRED never alarms and suppresses
everything (§4.2 row 1, §4.3, B14), is excluded from rings and rollups (B15), and leaves the health denominator (§4.5,
D10). "Zero active devices" is judged on non-retired devices, so retiring an expected customer's last device reads
MISSING and critical, or ONBOARDING while its onboarding window is open (§4.5, D02, §9). That is deliberate: silence is an
alarm. To retire a customer's last device without the alarm, clear `expected` first.

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

## Rollback

Migration 0006 takes the database to version 6, and an older binary refuses a newer database loudly at open
(`ErrMigrationAhead`). There is no down migration, so rolling back means restoring a copy of the database taken before the
upgrade, and whatever changed after that copy is lost. Take the copy first.

## Consequences

- architecture.md rows 707 and 922 and matrix row r7 still read M5; this ADR overrides their milestone for the retire
  action. r7's "stays restorable" and row 711's bulk retire (M7) are not done here.
- `GET /enroll` no longer carries the form. Anything linking there to create a token links to `/enroll/new`.
- §4.6 of architecture.md does not list C17 and C17b yet; this ADR defines them. They sit on the system health page's
  Gateway row.
- Retire is terminal. A machine that comes back enrolls as a new device, as §4.2 row 1 says.
