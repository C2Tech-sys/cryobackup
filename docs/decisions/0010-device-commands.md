# ADR 0010 - Device commands: lifetimes, coalescing, late results and identity resolution

**Date:** 2026-10-31 - **Status:** Accepted - **Decider:** foreman (M2 build), recorded for the operator

M2 adds operator commands to a device: back up now, cancel a run, refresh the policy and check in. They travel
over the existing agent long-poll (`GET /api/v1/agent/commands`, `POST /api/v1/agent/commands/{id}/result`). The M2
acceptance test requires that back up now reach an online device within 30 s. Three questions needed an answer:

- how long a command stays valid;
- what happens when an operator asks twice;
- what the console does with a result it can no longer apply.

The last one matters most. The agent keeps a result until the console accepts it with a 2xx, and it fetches no
new command while a result is waiting. Agent 0.1.0 is already deployed with that behaviour.

## Decision

- **Only the command types the agent implements:** `backup_now`, `cancel`, `refresh_policy`, `check_in`.
- **Lifetimes.** `backup_now` and `refresh_policy` stay valid for 4 h, so a laptop that comes online later the
  same working day still gets them. `cancel` and `check_in` stay valid for 15 min, because they mean nothing
  later. The API accepts lifetimes from 1 min to 7 days. A queued command past its deadline is shown as expired
  straight away, before the 60 s expiry job runs. A command that was delivered but never answered reads as
  "received, no answer before it expired".
- **Coalescing.** A request equal to a command already queued for the same device returns that command instead
  of adding a second one. Equal means same type and same parameters, and it is checked only after the device's
  scope check. The operator sees the existing command.
- **Cancel.** A command can be cancelled only while it is still queued (409 once delivered). The check runs in the
  same write transaction as delivery, so the two cannot race. Stopping a running backup is a separate `cancel`
  command, which the agent honours.
- **Waking the agent.** Queueing a command wakes an open long-poll for that device after the transaction commits,
  so delivery takes milliseconds rather than up to one poll period. A wake with no open poll is harmless: every
  poll reads the queue first. This holds for one console process, the supported deployment (SQLite).
- **The console never refuses a result in a way that stops an agent.** A result for a command that was delivered
  and has since expired is recorded as a late result: the status stays `expired`, and the outcome is kept and
  shown. Every other result the console cannot apply returns 204 and logs `command result not applied`, with ids
  and status only. That covers a cancelled command, an unknown id and another device's id. It is not an
  error to the agent, because a 4xx would hold up that agent's command loop indefinitely.
- **Identity conflict.** The device page offers two resolutions, each audited. **Keep** clears the flag.
  **Retire** retires the device, revokes its device token and gateway login, and triggers the gateway's htpasswd
  sync at once. In the UI, retire requires typing the hostname. Re-keying one of two machines that share an
  identity is token rotation, which belongs to M5 (F21).

## Consequences

- Back up now is near-instant for online devices and at most one poll period without a wake.
- A late or orphaned result never stops a device from taking commands. A late result is visible on the device.
- Detecting identity conflicts (the console comparing the hardware fingerprint on heartbeat) is a separate M2
  item. Until it ships, only fixtures raise the flag.
- Agent-side follow-ups, as their own tickets: treat a final 4xx on a result as done, and make `cancel` honour
  `run_id` so it cannot stop a newer run.
