# ADR 0002 — Architecture: one Go module, three binaries, an embedded database, pull-based agents

**Date:** 2026-10-20 · **Status:** Accepted 2026-10-20 (approval gate 2; SLA/grace defaults and seed-extension=off taken as the init-wizard defaults, to be tuned in the pilot) · **Decider:** operator · **Supersedes:** the "console evolves from the existing dashboard" wording in ADR 0001 (see D2)

## Context

Approval gate 1 (2026-10-20) accepted `replica/recon.md` and `replica/features.csv` at **full-featured scope**: must + should + every feasible could, built in vertical slices ordered must → should → could so a pilot-able build exists early. The engine capability sheet (`docs/recon-notes/engine_capabilities.md`) fixes the hard limits: one restic repository per device, append-only gateway semantics, no safe Object Lock compliance mode, Windows fidelity limits, and the fact that `kardianos/service` drops power events. The project conventions (`CLAUDE.md`) require: restic never reimplemented; endpoints never hold the storage credential; one binary per tier where possible; no telemetry or CDN assets; nothing C2-specific; HIPAA posture; a stranger must be able to build and run it.

This ADR records the decisions that `replica/architecture.md` and `replica/schema.sql` elaborate. They are decisions, not sketches: changing one means a new ADR.

## Decisions

### D1 — One language, one module, three binaries

Everything CryoBackup ships is Go, in one module, producing:

| binary | runs on | role |
| --- | --- | --- |
| `cryo-agent` | Windows endpoints, as a service | scheduler, restic supervisor, journal, heartbeat, command executor |
| `cryo-console` | the operator's Linux host (container or bare) | web UI + admin API, agent API, state engine, alerting, digests, enrollment, key escrow, **and** the maintenance worker (`cryo-console maintain`) and admin CLI (`cryo-console init|user|token|backup|doctor`) as subcommands of the same binary |
| `cryo-tray` | Windows endpoints, per user session (should) | status icon, "back up now", last result; talks to the service over a named pipe |

The gateway is **rclone** (`rclone serve restic`), run as a sidecar from the compose stack. It is configuration, not code; CryoBackup never forks or wraps it beyond generating its config and htpasswd.

Shared packages (`internal/restic`, `internal/policy`, `internal/api`, `internal/state`) are imported by both agent and console, so the restic JSON types, exit-code mapping, policy schema and wire types are defined once and tested once.

Module path: `github.com/c2tech-sys/cryobackup` (lower-case; GitHub owner names are case-insensitive and the go command resolves github.com paths directly, so the mixed-case account name never appears in imports). *Decided 2026-10-20 by the operator: the repository stays under the existing GitHub account rather than a separate organization.*

### D2 — The console is a Go rewrite; the Python dashboard is the specification, not the codebase

ADR 0001 said the console "evolves from" the existing C2 UrBackup dashboard. Clarified: what carries over is the **design** — the classification precedence and its 161 tests (ported as table-driven Go tests), the fail-loud rules (unreachable = banner, no-data = alarm, freshness stamp on every page), the digest layout, the Healthchecks heartbeat, the expected-customers alarm, the branding tokens and the start-backup CSRF guard. The Python code does not carry over. Reasons: one toolchain for the maintainer and for strangers; a single static binary with embedded assets; shared types with the agent; no interpreter to patch on the server. The Python dashboard stays in service, frozen except for bug fixes, until UrBackup is decommissioned.

### D3 — Database: embedded SQLite, deliberately

The console's database is **SQLite in WAL mode via `modernc.org/sqlite`** (pure Go, no cgo, cross-compiles). One write connection **per process** and a pool of read connections, `busy_timeout` set, every write in `BEGIN IMMEDIATE` with millisecond transactions; `cryo-console serve` and `cryo-console maintain` share the file under WAL and are separate processes on purpose, so the master key and the S3 credential never pass through the web-facing process. Migrations are embedded in the binary and applied at start; a nightly `VACUUM INTO` self-backup is bundled with the escrow vault (D9). *(Amended 2026-10-20 from "one write connection".)*

Why not Postgres: the console holds **metadata only** — no file index (restic keeps that, per repo, in object storage). The largest table is `runs`: 200 devices × ~24 runs/day × 90-day retention ≈ 430 k rows; at 5 000 devices it is ~11 M rows, still trivial. Heartbeats are an upserted latest row plus a 24-hour ring. A single file that a tired operator can copy is worth more than a second service. The UrBackup failure this project escapes was a 72 GB single-writer *file index* doing random I/O on spinning disk; nothing of that shape lives here.

Not supported in v1: Postgres, multi-node, HA. SQL is hand-written in `internal/console/store` (no ORM); it stays standard enough to port if adoption ever demands it, but there is one database target and one test surface.

The agent uses the same library for its local journal (D11), so the project has one database dependency.

### D4 — Web UI: server-rendered Go templates + htmx + SSE, assets embedded

`html/template` with a small component library, **htmx** for partial updates and **SSE** for live run progress and the state ticker. All CSS and JS are vendored and embedded with `go:embed`; the page makes no third-party request. No SPA framework, no Node toolchain at runtime or build time (hand-written CSS using the design tokens that `/replica-design` produces). Polish is a first-class requirement and is delivered by the design system, not by a framework.

### D5 — Two listeners, one binary: the agent API is public, the admin surface is not

Agents live on customer networks and cannot reach a mesh interface, so the **agent API must be internet-reachable**. The **admin UI/API must not be**. `cryo-console serve` therefore opens two separate `net.Listener`s with two separate routers:

| listener | default bind | auth | routes |
| --- | --- | --- | --- |
| agent | `0.0.0.0:8443`, TLS required (own cert/key, or ACME as a should) | device token (bearer; hashed at rest; rotatable) | `/api/v1/agent/*` only |
| admin | `127.0.0.1:8080` (operator binds it to a mesh IP) | session cookie + MFA; API tokens | web UI and `/api/v1/*` |

The admin router is never mounted on the agent listener; this is enforced by construction (different `http.Handler` trees) and by a test. Agent routes are rate-limited per device and per IP and accept only the JSON shapes in `internal/api`. The gateway (`rclone serve restic`) is likewise internet-facing with TLS ≥ 1.2 and bcrypt htpasswd.

### D6 — Agent ↔ console protocol: HTTPS/JSON, pull-based, idempotent

Endpoints never open an inbound port. The agent initiates everything:

| call | purpose |
| --- | --- |
| `POST /api/v1/agent/enroll` | one-time: enrollment token + device facts → device id, device token, repo URL, gateway login, repo password (D9), initial policy |
| `GET  /api/v1/agent/policy` | ETag'd policy pull; the agent keeps the last known good policy |
| `POST /api/v1/agent/heartbeat` | every 60 s ± jitter: version, power, network, queue depth, clock, service state |
| `POST /api/v1/agent/runs` | run results, batched from the journal, idempotent on the agent-generated run id |
| `POST /api/v1/agent/snapshots` | snapshot-list cache after each successful backup |
| `GET  /api/v1/agent/commands` | long-poll (≤ 30 s) for queued commands; `POST …/commands/{id}/result` acknowledges |
| `GET  /api/v1/agent/release` | signed release manifest for self-update (should) |

Run ids and command ids are idempotency keys. The console records `clock_offset` from the heartbeat and uses **server time** for every state decision. The protocol is versioned in the path; an unknown agent version is reported, not rejected.

### D7 — Storage layout and the gateway

One restic repository per device at `<bucket>/<prefix>/<device_id>/`. The gateway runs
`rclone serve restic --append-only --private-repos --htpasswd <file> --min-tls-version tls1.2 --cert … --key … <remote>:<bucket>/<prefix>`;
`--private-repos` confines the login `<device_id>` to `/<device_id>/`. The agent's repo URL is `rest:https://<device_id>:<password>@<gateway>/<device_id>/`.

The console owns the htpasswd file (bcrypt entries; rclone hot-reloads it). In v1 the console and the gateway are **co-located** in one compose stack and share that file on a volume; a split deployment is a later feature. The single S3 credential is held by the gateway container and by the maintenance subcommand (D8); it exists nowhere else.

### D8 — Retention and integrity run server-side through a second, non-append-only path

`cryo-console maintain` runs on the admin host with the S3 credential and the escrowed repo key. It addresses each repo through restic's **rclone backend** (`-r rclone:<admin-remote>:<bucket>/<prefix>/<device_id>`, `-o rclone.program=…`) so that the same path can pass `--s3-bypass-governance-retention` when the storage target has Object Lock in governance mode. Per device, serialized, scheduled away from the device's backup window where the policy allows, it runs `forget --json` (with the policy's keep rules **always including `--keep-within`**), then `prune` (text output, parsed), then `check` on its own cadence (`--read-data-subset` as configured). restic exit 11 (lock held by a running backup) is a deferral, never a failure. Every outcome is a `Run` of kind `forget|prune|check` and appears in the device history.

Prune cadence is a property of the storage target (default quarterly where minimum-storage billing applies, monthly otherwise), overridable per policy.

### D9 — Keys: per-device password, escrowed by default, customer-held as a warned opt-out

At enrollment the console generates a 32-byte random repo password per device, sends it once to the agent inside the enrollment response, and stores it **wrapped** (AES-256-GCM under a key derived by HKDF from the console master key) in `repo_key_escrow`. The master key is 32 bytes generated by `cryo-console init`, supplied by environment or mounted file, never in the database. Escrow is marked `verified_at` after the maintenance path has successfully run `restic cat config` with the escrowed key. Revealing a key requires MFA re-authentication, writes an audit event, and shows the key once. Master-key rotation re-wraps every row.

**Customer-held custody** (opt-out at enrollment, with the warning on screen) means the console never stores the key. Consequences, stated to the operator at the moment of choice: no server-side retention (the device is append-only and the server has no key, so nothing can prune — growth is reported but not controlled), no technician restore, no escrow-health check; retention becomes a manual, key-in-hand operation on the admin host. Browse, technician restore, escrow reveal/verification and server-side forget/prune/check refuse a customer-held device explicitly (HTTP 409 `custody_customer_held`) rather than failing opaquely.

On the device, the repo password and device token live under `C:\ProgramData\CryoBackup\secrets\` with an ACL of SYSTEM and Administrators only, additionally DPAPI-protected (machine scope).

### D10 — State is a pure function evaluated at read time

`internal/state.Classify(facts, policy, now)` is the product. It is pure, table-driven, and the only place a device state is decided. Inputs are persisted facts (last success, last run outcome, last seen, enrolled at, first-backup deadline, paused until, retired at, escrow verified, agent version vs release) plus policy thresholds (SLA days, unseen days, grace, partial tolerance) and server `now`. Nothing persists a status column; the UI, the API, the digest and the alarm evaluator all call the same function. The alarm evaluator runs it on a schedule only to decide what to *notify* (deduplicated through `notification_log`), never to store state. The precedence table and its exhaustive tests — including the unreachable-gateway, unreachable-storage, empty-poll and partial-response paths — are part of the architecture document and are the first tests written.

### D11 — The agent

`golang.org/x/sys/windows/svc` directly (not `kardianos/service`), accepting Stop, Shutdown, PreShutdown, PowerEvent and SessionChange. Runs as LocalSystem. Owns its schedule: a 60-second tick plus event triggers (resume, logon, boot, command), computing due-ness from last success against the policy's interval and window — there is no stored next-run and no Task Scheduler. Start conditions (battery floor, metered, user-idle, Wi-Fi/VPN) **delay** a due run and a "run anyway after N" fallback bounds the delay. `restic.exe` is bundled, pinned, hash-checked at start, launched under a job object (kill-on-close, memory cap), with `--json`, an explicit `RESTIC_CACHE_DIR` under ProgramData, `--use-fs-snapshot` by default and `--exclude-cloud-files` on. Results go to a durable local journal (SQLite) and are delivered at-least-once; gateway or console unreachability is a deferral with backoff. Logs are rotating files plus the Windows event log (so an RMM can see them). Device-local logs and per-run records may name paths: they stay on the machine that already holds the files, under an ACL of SYSTEM and Administrators, and never enter a diagnostics bundle or an upload. Everything sent to the console is path-free (error class, phase, extension, counts). *(Amended 2026-10-20 from "nothing in them contains a user path".)*

### D12 — Packaging, signing, releases

Agent: WiX MSI with a fixed UpgradeCode, `cryo-agent.exe` and `restic.exe` as separate components, service install, silent install (`/qn`) with `TOKEN=` and `SERVER=` properties, in-place upgrade preserving `ProgramData`. Both executables and the MSI are Authenticode-signed (any PFX or a cloud signing service; the pipeline takes a signing command, not a vendor). Console: single Linux binary, container image, and a reference compose stack (console, rclone gateway, maintenance timer). Releases are built by GitHub Actions on tags; checksums and signatures published; a release manifest feeds the agent's self-update channel (should).

### D13 — Observability and health

Structured logging (`log/slog`), path-free by construction (paths are hashed or counted, never logged). A `/metrics` Prometheus endpoint and `/healthz` on the admin listener. A system-health page probing gateway, storage, SMTP, maintenance worker and database. Optional outbound heartbeat ping (Healthchecks-style URL) as configuration. No other outbound call exists.

### D14 — Open-source hygiene

`cryo-console init` produces a working configuration interactively; every secret comes from the environment or a mounted file with a committed `.example`; `cryo-console serve --fixtures` boots with canned customers, devices and runs for UI work and screenshots; the test suite needs no network. No customer name, hostname, endpoint or brand of the original operator appears in the repository.

## Consequences

- Two internet-facing services (agent API, gateway) exist by design; both are TLS-only, credential-scoped per device, rate-limited, and covered by the hardening section of the architecture document.
- The Python dashboard is frozen; its tests become the Go state engine's acceptance suite.
- Customer-held key custody disables server-side retention and technician restore; the UI says so at the moment of choice.
- SQLite means single-node; HA and multi-node are out of scope for v1 and would need a new ADR.
- `rclone` and `restic.exe` are pinned, hash-checked third-party binaries; upgrading either is a release of CryoBackup.
- The module path `github.com/c2tech-sys/cryobackup` is permanent from the first Go file; a later move to a dedicated organization would be a path change for every importer and is not planned.

## Alternatives considered

| alternative | why not |
| --- | --- |
| Evolve the Python/Flask dashboard into the console | two toolchains; interpreter on the server; no shared types with the agent; JSON-file state cannot carry 24 entities |
| Next.js / TypeScript console (replica-architect default) | Node toolchain and hosting assumptions that do not fit a self-hosted single binary; adds a runtime the maintainer must patch |
| Postgres | a second service for a metadata store that fits in a file; dual test surface if offered alongside SQLite |
| `kardianos/service` for the agent | drops power and session events; run-on-wake is a must |
| rest-server instead of rclone as the gateway | needs local disk; rclone fronts S3 directly and carries the Object Lock governance flags |
| Shared repo per customer | exclusive locks during prune block every device; one key for all; index RAM scales with the customer |
| Embedding restic as a Go library | restic's internals are not a stable API; the CLI JSON contract is |
