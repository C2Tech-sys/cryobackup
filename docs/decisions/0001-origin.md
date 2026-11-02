# ADR 0001 — Why CryoBackup exists, and what it is

**Date:** 2026-10-20 · **Status:** Accepted · **Decider:** operator

## Context

C2 Technology Systems backs up ~200 remote Windows workstations for its clients as an unbilled value-add, using UrBackup on an HDD-backed storage VPS. In October 2026 UrBackup's file-index SQLite database reached 72 GB on that HDD-backed volume and its nightly cleanup blocked all backups fleet-wide for ~36 hours. A full re-evaluation of the field found:

- No FOSS platform combines a Windows VSS agent, NAT-friendly enrollment at fleet scale, a multi-client status console, and a backend that is not a monolithic server-side index.
- The commercial fit (Comet, self-hosted) costs ~$449/month in licenses at 200 endpoints — pure cost for an unbilled service.
- The UrBackup fix requires the database on SSD, which the existing storage host cannot provide.

## Decision

Build CryoBackup: an open-source (AGPL-3.0) workstation backup platform in three tiers.

1. **Agent** — a Go Windows service with its own scheduler (missed-run catch-up, run-on-wake), VSS, throttling, retries, policy pull, result reporting and self-update, running `restic.exe` for all data work.
2. **Gateway** — `rclone serve restic --append-only` on a C2 VPS holding the single storage credential, issuing per-device logins. Endpoints never hold S3 keys; a compromised endpoint cannot delete history; prune runs server-side.
3. **Console** — the existing C2 UrBackup visibility dashboard evolved into a control plane: device registry, per-customer policy, fleet status, alerting, daily digest, heartbeat, missing-customer alarm.

Product shape is reconned from the public documentation and trial accounts of Comet, N-able Cove, MSP360, IDrive, Acronis, Datto, Axcient, Veeam Agent and UrBackup via the Replica pipeline — never from their binaries. UrBackup's open-source client is studied for laptop and missed-backup scheduling semantics.

## Consequences

- restic is a hard dependency and is never reimplemented.
- C2 becomes the maintainer of a backup agent on 200 machines; the console's fail-loud design is the mitigation.
- Migration from UrBackup requires a full WAN re-seed; UrBackup is bridged (weekend-only cleanup, 48-hour incrementals, 5 concurrent backups) for the 2–3 months this takes.
- Phase 2 (pilot) runs on one real customer alongside UrBackup for 30 days before any fleet rollout.
- Image/bare-metal backup is out of scope for v1 and is a separate engine decision if ever pursued.
- Microsoft 365 / Google Workspace backup is a later, separable module; the console's data model must not preclude it.

## Name, domain, license, money

- **Name:** CryoBackup. Registry-verified clean: no product or trademark of that name; cryobackup.com is a held, empty domain; GitHub `cryobackup` is free. Short name "Cryo". Rejected: Shadowvault (name of a 2023 macOS infostealer), Cryopod (live Azenta lab-equipment trademark).
- **Domain:** cryobackup.cloud (operator purchased 2026-10-20).
- **License:** AGPL-3.0 with a contributor agreement, so the copyright holder can dual-license or offer a paid module later. Trademark registration for "CryoBackup" recommended.
- **Business model:** none planned; donations only. Open-core (the M365/Google module, SSO, compliance reporting, support) is deliberately kept possible and revisited on adoption evidence in six months.
