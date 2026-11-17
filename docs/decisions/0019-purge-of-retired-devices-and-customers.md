# ADR 0019 - Purging a retired device and deleting a customer move from M10 to the release after M3

**Date:** 2026-11-17 - **Status:** Proposed - **Decider:** the operator (request of 2026-11-16), recorded by the foreman

architecture.md rows 688 (`DELETE /api/v1/customers/{id}`) and 710 (`DELETE /api/v1/devices/{id}`), the dialogs of row
911 and matrix row r243 put purge in M10. ADR 0016 moved retire to M3 and left purge where it was, so a retired device
stays for good: its row under "Retired (n)", its repository in storage, its escrowed key. The M3 lab run ended with 14
retired lab devices on three customers and one customer that exists only to hold retired rows. The operator asked for a
way to clear retired machines permanently and to delete customers, and ruled that a purge removes the backups too, not
only the record.

## Decision: purge a retired device

`DELETE /api/v1/devices/{id}` (owner role, fresh MFA, a typed confirmation, a reason) and the UI action on the retired
device's page go through one handler. Only a retired device can be purged: an active one answers 409 and says to retire
it first. Purge is one-way and the dialog says so in those words.

It runs in two parts, because storage can fail and the database cannot be allowed to forget a repository that still
exists:

1. **Mark.** One transaction sets `devices.purge_requested_at`, `purge_requested_by` and the reason, and writes the
   audit row `device.purge.request`. From here the device shows as "purging" and nothing else can be done to it.
2. **Remove the repository.** A job (`device-purge`) asks the maintenance worker, over the admin path, to remove the
   device's repository prefix. This is a new admin-path method, `purge_repo`, and the only one that deletes outside
   restic: the worker runs `rclone purge` on `<remote>:<bucket>/<prefix>/<device id>` with the admin credential it
   already holds, then lists the prefix and reports what is left. The console never holds the credential (the rule
   that endpoints and the console do not hold storage credentials is unchanged).
3. **Remove the record.** Only when the worker reports the prefix empty does a second transaction delete the device
   row. The foreign keys already cascade to everything a device owns (runs, snapshot cache, heartbeats, tokens, gateway
   login, escrow, commands, restores, acknowledgements). One audit row `device.purge.done` carries the device id, the
   customer id, the object and byte counts the worker removed, and no host name.

Audit rows have no foreign key and stay. After a purge they are the only record that the device existed, which is the
point of them.

**When it fails.** A storage or worker failure leaves the device in "purging", raises a banner (`Purge not finished`,
critical, with the device and the error class) and the job retries on its normal schedule. There is no "forget it
anyway" button: a record deleted while its data remains is an orphan nobody will find. An operator who wants the row
gone must get the storage step to succeed.

**What the dialog must say.**

- The backups are deleted and cannot be restored afterwards, by anyone.
- The escrowed repository key is deleted with them.
- Storage with versioning, object lock or a minimum storage duration may keep or bill the removed objects until the
  provider's own window ends; the console cannot shorten that. (architecture.md already says this of the purge path.)
- An agent still installed on the machine stays REFUSED (ADR 0018) and should be uninstalled.

## Decision: retention does not gate the purge

architecture.md says purge comes "after the retention window". ADR 0016 recorded that retention does not run on a
retired device at all, so that window never closes by itself. This ADR drops the gate: an owner may purge a retired
device at any time. The protections are the owner role, fresh MFA, the typed confirmation and the audit rows. A
per-customer "do not purge before" hold, for customers whose contract or law demands one, stays with M10.

## Decision: delete a customer

`DELETE /api/v1/customers/{id}` (owner role, fresh MFA, typed confirmation of the customer's name, a reason) deletes a
customer that has **no devices at all**, active or retired. The schema already refuses otherwise (`devices` to
`customers` is `RESTRICT`), and the handler answers 409 with the counts instead of a database error. So the path for a
customer with history is: retire its devices, purge them, then delete the customer. There is no cascade that purges a
customer's devices in one click; a "purge all retired devices of this customer" action on the customer page queues the
individual purges, each with its own audit rows.

Enrollment tokens of the customer are deleted with it (they are useless without it); the audit rows that name them
stay. One audit row `customer.delete`.

## Decision: fresh MFA arrives with this ADR

No step-up exists yet. This work adds it once, for these two actions: the session must have passed TOTP within the
last 5 minutes, otherwise the dialog asks for a code. Key reveal and the other owner actions adopt the same helper in
their own milestones.

## Consequences

- New: migration (purge columns), admin-path method `purge_repo` in the maintenance worker, job `device-purge`, banner,
  two API routes, two UI dialogs, the fresh-MFA helper, OpenAPI rows, the scope-matrix rows.
- The maintenance worker gains its first delete outside restic. Its tests must show it refuses any prefix that is not
  exactly one device id under the configured bucket and prefix.
- Fixture mode covers: storage unreachable during purge, worker down, partial removal, purge of an active device,
  customer with devices.
- architecture.md rows 688, 710, 911 and r243 move to the release after M3. Archive, suspend and the retention hold
  stay in M10.

## Open questions for the operator

1. Typed confirmation: the device's host name, or the word `PURGE`?
2. Is a cooling-off delay wanted (for example the purge starts 24 h after the request and can be cancelled until
   then), or does it start at once? This ADR assumes at once.
3. The lab customer that only holds retired rows is the first user of this. Confirm that deleting its repositories from
   production storage is intended.
