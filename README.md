# CryoBackup

**Frozen in time. Back in seconds.**

Open-source workstation backup for MSPs and small IT shops. A rock-solid Windows agent, an append-only gateway in front of your own S3 storage, and a console that answers "who failed last night, and whose machine is it?" — grouped by customer.

> **Status: pre-release.** The agent, gateway and console run end to end (milestones M0–M2: backup, restore, enrollment,
> policies, MFA, audit). Real-machine acceptance of M2 is in progress; there is no tagged public release yet.

## Why this exists

The market has no free, self-hosted, good-looking, multi-tenant Windows workstation backup manager. UrBackup is the closest, and its single-writer SQLite file index stalls a 200-machine fleet on spinning disk; the FOSS alternatives have no fleet view; everything with a real console costs $2–12 per endpoint per month. The founding decision is [`docs/decisions/0001-origin.md`](docs/decisions/0001-origin.md); every later decision is an ADR in [`docs/decisions/`](docs/decisions/).

## Architecture

| Tier | What | Built on |
|---|---|---|
| **Agent** | Windows service with its own scheduler (missed-run catch-up, run-on-wake), VSS, throttling, policy pull, result reporting, self-update | Go, wrapping [restic](https://restic.net) for all data work |
| **Gateway** | Append-only storage front end: endpoints never hold S3 credentials; one storage key server-side; per-device logins; ransomware-resistant by construction | `rclone serve restic --append-only` |
| **Console** | Device registry, per-customer policy, fleet status, alerting, daily digest, heartbeat monitoring, missing-customer alarm | Evolves from C2's UrBackup visibility dashboard |

The backup engine is **restic** and will stay restic. CryoBackup never reimplements chunking, deduplication, encryption or integrity checking.

## Roadmap

- **Phase 0** — Replica-style recon of the field (Comet, N-able Cove, MSP360, IDrive, Acronis, Datto, Axcient, Veeam Agent, UrBackup) → feature matrix → architecture document.
- **Phase 1** — Agent + gateway + console, built with blind verification at every stage.
- **Phase 2** — 30-day pilot on one real customer alongside the incumbent.
- **Phase 3** — Fleet rollout via RMM; incumbent decommissioned.
- **Later** — Microsoft 365 and Google Workspace backup as a separable module.

## License

[AGPL-3.0](LICENSE). Contributions will require a contributor agreement (details to come) so the project can offer dual licensing later. "CryoBackup" is a trademark of C2 Technology Systems.

Built by [C2 Technology Systems](https://c2techsys.com), Cullman, Alabama.
