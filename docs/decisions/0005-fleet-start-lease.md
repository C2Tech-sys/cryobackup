# ADR 0005 - Fleet concurrency cap by console start-lease

**Date:** 2026-10-24 - **Status:** Accepted - **Decider:** operator

Feature row r30 ("start jitter to spread fleet load, plus fleet concurrency cap", M1) had jitter but no design for the cap.

## Decision

- Before a **scheduled** backup (triggers tick, boot, catch-up, resume, connectivity, retry) the agent asks the console for a start lease: `POST /api/v1/agent/start-lease` (device token) with the run id. The console grants while fewer than `max_concurrent_backups` leases are live for the device's storage target, and otherwise answers "not granted" with a `retry_after_s`.
- The cap is per storage target, configured on the target (`0` = unlimited, the default, so an upgrade changes nothing until the operator sets it).
- A lease lives 15 minutes and is renewed by every heartbeat sent while the run is active; the run upload releases it. A crashed agent's lease expires on its own.
- **Fail open.** If the console is unreachable, times out, or answers 5xx, the agent starts anyway and the run records `lease: bypassed`. A backup is never blocked because the control plane is down ("fail loudly, degrade gracefully": the bypass is visible on the run, not silent).
- A refused lease is not a failure: the run is journaled as `deferred` with reason `fleet_cap`, retried after `retry_after_s` plus the existing jitter; the SLA clock keeps running, so a cap set too low surfaces as STALE, not as quiet green.
- Operator-initiated runs (`backup_now` command) skip the lease; the operator asked for that run.
- Leases are stored in a new table `start_leases` (one row per live lease, keyed by run id) added by a migration; expired rows are deleted on read and by the existing trim job.

## Why

The gateway and the storage provider see the fleet's load; only the console sees the fleet. An agent-only cap cannot exist, and a cap in the gateway would reject restic mid-handshake and look like a storage error. Asking first keeps the refusal clean and classifiable.

## Consequences

- One new agent route, one table, one deferred reason (`fleet_cap`), one storage-target field.
- A console outage removes the cap, by design; the run rows show how many starts bypassed it.
- `backup_now` storms are not capped; that is an operator action, audited.
