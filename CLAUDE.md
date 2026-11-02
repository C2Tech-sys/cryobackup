# Project conventions — CryoBackup

Read this before writing code. These are the operator's standing rules.

## Who you're building for

The operator: one person running a small MSP who maintains everything built here alone, and, because this is open source, so do strangers. Technically deep; does not need concepts explained; needs precise, structured output and working code. Optimize for "still comprehensible in eight months" and "a stranger can read it." A git-ignored `CLAUDE.local.md`, when present, adds the operator's own profile.

## Communication style

Direct, structured, lead with the answer. No padding. Label uncertainty as uncertainty.

## What CryoBackup is — and is not

- **The engine is restic.** Chunking, dedup, encryption, integrity, repository format: restic's, unmodified. CryoBackup orchestrates; it never reimplements the engine. Any proposal to "just write our own X" where X is in restic is wrong by default.
- **Three tiers:** Agent (Go Windows service wrapping `restic.exe`), Gateway (`rclone serve restic --append-only`, holds the only storage credential), Console (device registry, policy, fleet status, alerting, digest).
- **Endpoints never hold storage credentials.** Non-negotiable; it is the ransomware and credential-limit answer in one.
- **File-level backup first.** Image/bare-metal restore is a later, separate engine decision — never bolted onto restic.
- **Feature-complete is the bar.** The Phase 0 matrix against Comet, N-able Cove, MSP360, IDrive, Acronis, Datto, Axcient, Veeam Agent and UrBackup defines "done" for v1. Polish is first-class: it must look as good as it works.

## Engineering conventions

- **Fail loudly, degrade gracefully.** A backup system that goes quietly green when it cannot see its agents is worse than none. Every "no data" path is an alarm condition. (Documented case: an 18-day silent outage behind a certificate problem on another product.)
- **Small and boring beats large and clever.** One binary per tier where possible. Dependencies are things the operator patches.
- **No telemetry, no analytics, no CDN assets.** Vendor everything. The console lists client hostnames; it makes no third-party requests.
- **Secrets never in the repo.** Environment or mounted files, `.gitignore`d, with `.example` templates committed.
- **Nothing C2-specific hardcoded.** Customer names, storage endpoints, branding, thresholds: configuration. The repo must build and run for someone who has never heard of C2.
- **HIPAA posture by default.** No file contents or client-identifying paths in logs, metrics or crash reports. The gateway and console must be deployable so that PHI only ever lands on BAA-covered storage.
- **Timezone:** store UTC; display the operator's configured zone.
- **Decisions are recorded** as ADRs in `docs/decisions/NNNN-*.md`. Changing a decision means a new ADR, not an edit.

## Process

- Multi-file or multi-stage work runs under the fable-foreman skill: tickets, disjoint write sets, blind verification before anything is called done.
- The product follows the Replica pipeline: recon → architect → design → build → test → diff. Each stage's output is reviewed by the operator before the next starts.
- Every tier ships with unit tests, a fixture/mock mode, and explicit tests for the unreachable/partial-data paths.
- Before declaring a phase done: run it against real machines, not just tests.

## Syncro / PowerShell rules (for deployment scripts)

No `param()`, no `[CmdletBinding()]`; plain variables at the top with sane defaults; discovery → quiesce → act → scrub → verify → report; dry-run by default for destructive actions; idempotent; meaningful exit codes.
