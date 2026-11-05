# Architecture: CryoBackup (a rebuild of Comet Backup's core features, feature-union of nine products)

CryoBackup is an open-source (AGPL-3.0) workstation backup platform in three tiers: an **Agent** (a Go Windows service that schedules and supervises `restic.exe`), a **Gateway** (`rclone serve restic --append-only`, the only data-path component that holds the storage credential) and a **Console** (a Go service: device registry, policy, read-time state engine, alerting, digest, key escrow, enrollment, and the server-side maintenance worker). The stack and fourteen architectural decisions (D1-D14) are fixed by ADR 0002 (`docs/decisions/0002-architecture.md`); this document elaborates them and is the **build contract for Phase 1** (`/replica-design`, then `/replica-build`). It contains no decision that contradicts the ADR; where it chooses something the ADR left open, the choice is tagged **Decided here (DHn)** and listed in section 1.3. The companion file `schema.sql` is the executable form of section 3 (migration `0001_initial`).

Inputs: `replica/recon.md` (71 screens S/A/B, 22 flows F01-F22, 24 entities), `replica/features.csv` (258 rows: 121 must, 72 should, 44 could, 21 skip, per the file as it stands), `docs/recon-notes/engine_capabilities.md` (cited as "engine §N"), `CLAUDE.md`, ADR 0001/0002. Scope approved 2026-10-20 at **full-featured**: every must, should and feasible could row is planned; the plan is ordered must, then should, then could, with a pilot-able build early (section 10). Unknowns are labelled **UNKNOWN** or **UNCERTAIN** and collected in section 12; estimates are labelled **estimate**.

Conventions used below: *S/A/B* = recon screen IDs; *F* = recon flows; *rNNN* = a row of `features.csv` by line number (header = line 1); *engine §* = engine capability sheet; *role* abbreviations RO (read_only+), OP (operator+), AD (admin+), OW (owner); times are Unix milliseconds in UTC in storage and the operator's configured IANA zone on screen; "path-free" means no file path, directory name or user-profile name is ever written to a log, metric, e-mail, webhook, audit row or crash report.

---

## 1. Stack

### 1.1 Choices

| layer | choice | why |
| --- | --- | --- |
| web (console UI) | Go `html/template` + **htmx** + **SSE**; CSS/JS/fonts/icons vendored and `go:embed`ded; hand-written CSS from the `/replica-design` tokens | ADR D4. No Node at build or runtime, no CDN request on a page that lists client hostnames, one static binary. Polish comes from the design system, not a framework. |
| HTTP/API | stdlib `net/http` (Go 1.22+ `ServeMux` method+path patterns), JSON, OpenAPI 3.1 generated from `internal/api` types | The API must cover every UI action (matrix: api, must); generating the spec from the same types keeps it true. Zero router dependency. |
| database | SQLite, WAL, `modernc.org/sqlite` (pure Go, no cgo); hand-written SQL in `internal/console/store`; embedded migrations; one write connection + read pool, `busy_timeout` 5 s | ADR D3. Metadata only: the largest table is `runs` (~430 k rows at 200 devices x 90 days; ~11 M at 5 000). One file the operator can copy. Not supported in v1: Postgres, HA, multi-node. |
| auth (admins) | local accounts (argon2id) + **TOTP** (RFC 6238, in-house ~60 lines over `crypto/hmac`) + recovery codes; session cookie (`HttpOnly`, `Secure`, `SameSite=Strict`) + CSRF token on every unsafe request; scoped API tokens (sha256 at rest); **WebAuthn** (`go-webauthn`) and **OIDC** (`coreos/go-oidc`) arrive at should; roles owner / admin / operator / read_only; `customer_scope` on operator and read_only | Matrix security rows; S32/S38. Fresh-MFA step-up (`sessions.mfa_verified_at`) gates key reveal and destructive actions. |
| auth (devices) | 256-bit random bearer **device token**, sha256 at rest, rotatable (`device_tokens`), grace window on rotation; enrollment tokens are separate (hash, expiry, use count) | ADR D5/D6. A stolen token is bounded to one device and one API surface; revocation is one UPDATE. |
| payments | **none** | CryoBackup is a self-hosted tool, not a service: no licensing, billing, prepaid credit or price sheets (matrix skip rows r202-r203, recon "Out of scope"). No payment processor is integrated, and nothing phones home. |
| email | stdlib `net/smtp` + `crypto/tls`, relay-agnostic (STARTTLS 587 / implicit TLS 465 / plain 25 only with an explicit opt-in flag), MIME via `mime/multipart` | Any relay works (a transactional relay, M365, a local MTA). SPF/DKIM/DMARC for the sender domain is the operator's job; delivery failure is itself an alarm (C03). No mail SDK to patch. |
| jobs | in-process scheduler goroutines in `cryo-console serve` (lease-guarded via `job_state`) for alarm evaluation, digest, probes, retention and trims; a separate **maintenance worker** process (`cryo-console maintain`) for everything that needs the S3 admin credential | No external queue or cron to run, monitor or patch. Section 5 lists every job with its idempotency rule. |
| hosting | one Linux host the operator controls (VPS or VM): `docker compose up` starts `console`, `gateway` and `maintenance`; bare binaries + systemd units are the alternative; admin listener on loopback or a mesh IP, agent API and gateway on public TLS ports; storage is any S3-compatible target (Wasabi, Backblaze B2, IDrive e2, AWS S3, MinIO; provider facts in engine §4.1) which may sit with a different provider than the host; endpoints are Windows 10 / Server 2016 and newer | ADR D5, D7, D12. No cloud lock-in; PHI must only land on BAA-covered storage (project rule), which is a deployment decision documented in the README, not code. |
| agent runtime | Go, `golang.org/x/sys/windows/svc` directly (not `kardianos/service`, engine §5.2), LocalSystem, bundled pinned `restic.exe` under a job object, local SQLite journal (same `modernc.org/sqlite`) | ADR D11. Run-on-wake is a must and needs power/session events that `kardianos` drops. |
| gateway | `rclone serve restic --append-only --private-repos --htpasswd <file> --min-tls-version tls1.2 --cert/--key` as a compose sidecar; pinned rclone binary; config = generated htpasswd + a mounted `rclone.conf` | ADR D7. Fronts S3 directly with no local disk; append-only and per-device confinement are rclone's, tested in the lab (engine §3.2). |
| maintenance worker | `cryo-console maintain` (same image, separate container): holds the S3 admin credential and the master key; drives restic through its `rclone:` backend; exposes a unix-socket RPC for interactive admin-path calls (init, ls, find, dump, stats, cat config) | ADR D7/D8: the S3 credential "exists nowhere else" than the gateway and this process. See DH1. |
| packaging / signing | WiX (v4/v5) MSI with a fixed `UpgradeCode`; Authenticode on `cryo-agent.exe`, `cryo-tray.exe`, `restic.exe` and the MSI via a **pluggable signing command** (PFX, Azure Artifact Signing, SignPath for OSS - the pipeline takes a command, not a vendor); console as a static Linux binary + container image + reference compose | ADR D12; engine §5.7-5.9: bundle restic as a second MSI component, do not embed it. |
| CI | GitHub Actions: `windows-latest` job (agent, tray, WiX, signing step), `ubuntu-latest` job (console, images), `go test -race ./...`, `govulncheck`, two custom analyzers (`nopaths`, `nonet`: forbid raw path logging and any outbound HTTP outside the allow-list), OpenAPI diff, SBOM, release on tag with checksums and signatures; a manually-triggered **lab** workflow runs the e2e acceptance suites on a self-hosted Windows VM + compose stack with MinIO | Reproducible releases; the lab job is where VSS, power events and append-only get proven (they cannot be unit-tested). |
| observability | `log/slog` through the path-free `internal/logx` handler; `/metrics` (Prometheus) and `/healthz` on the admin listener; system-health page; optional outbound Healthchecks-style ping as configuration | ADR D13. Nothing else leaves the host. |
| PDF / XLSX (should) | `fpdf`-class pure-Go PDF writer for per-customer status reports; a minimal in-house XLSX writer (`archive/zip` + XML, values only); CSV via `encoding/csv` | No headless browser, no cgo. See DH16. |

### 1.2 Dependency budget (every dependency is one the operator patches)

| dependency | used by | tier of need |
| --- | --- | --- |
| `modernc.org/sqlite` | console, agent journal | must |
| `golang.org/x/sys/windows` (+ `svc`, `svc/mgr`, `svc/eventlog`) | agent, tray | must |
| `golang.org/x/crypto` (argon2, bcrypt, hkdf) | console | must |
| `oklog/ulid` or an in-house 40-line ULID | both | must (in-house preferred) |
| `github.com/go-webauthn/webauthn` | console | should |
| `github.com/coreos/go-oidc/v3` | console | should |
| PDF writer (`go-pdf/fpdf`-class) | console | should |
| htmx (vendored JS file, ~15 KB, committed with its licence) | web | must |
| WiX toolset, rclone, restic (pinned, hash-checked binaries) | build / runtime | must |

Everything else is the standard library. `govulncheck` and Dependabot-style update PRs run weekly; a new dependency requires an ADR note.

### 1.3 ADR 0002 conformance and decisions made here

**Conformance.** Every D-number is elaborated and none is changed. No objection is raised against D1-D14; three **tensions** (not objections) are listed under D3, D11 and D13 and in section 12.1.

| ADR | elaborated in | note |
| --- | --- | --- |
| D1 one module / three binaries | §2, §6, §7 | `cryo-console` subcommands: `init`, `serve`, `maintain`, `user`, `token`, `backup`, `doctor`. |
| D2 Go rewrite; Python is the spec | §4 | Precedence and the 161-test suite are ported as table-driven Go tests (§4.8). |
| D3 SQLite WAL | §3, `schema.sql` | **Tension:** D3 says "one write connection"; D8 puts the escrowed keys and the run records in `cryo-console maintain`, a second process. **Resolved: ADR D3 amended 2026-10-20 to "one write connection per process".** Read as one write connection per process: both processes open the same file under WAL with `BEGIN IMMEDIATE`, millisecond transactions and `busy_timeout` (DH1, "SQLite single-writer discipline" in §9). The alternative (maintain as a pure RPC client of serve, so serve is the only writer) is rejected because it routes the master key and every maintenance step through the web-facing process; listed in §12.1. |
| D4 html/template + htmx + SSE | §1, §5.3 | Screens map 1:1 to server-rendered routes. |
| D5 two listeners | §5, §8 | Different `http.Handler` trees; a test enumerates both muxes and asserts the route sets are disjoint. |
| D6 agent protocol | §5.1, §6 | Seven calls kept verbatim; six additions are DH4. |
| D7 storage layout, gateway | §7 | `rest:https://<device_id>:<pw>@<gw>/<device_id>/` kept; delivered via environment, not argv (DH3). |
| D8 server-side maintenance | §7 | `--keep-within` is enforced by policy validation, not by convention. |
| D9 keys, escrow | §3, §7, §8 | `repo_key_escrow`, `gateway_logins.pending_secret_wrapped`, fresh-MFA reveal. |
| D10 state as pure function | §4 | `Classify`, `Rollup`, `Evaluate`; no status column exists anywhere in `schema.sql`. |
| D11 agent | §6 | **Resolved (foreman ruling 2026-10-20, ADR D11 amended):** device-local logs may contain paths — they stay on the machine that already holds the files, under an ACL of SYSTEM and Administrators, and never enter a diagnostics bundle or an upload; everything sent to the console is path-free (error class, phase, extension, counts). The salted-hash scheme first proposed as DH15 is withdrawn: it added a step with no reduction in exposure. |
| D12 packaging | §1, §10 | |
| D13 observability | §4.6, §5 | **Tension:** D13 says "no other outbound call exists" but the matrix lists a release channel; resolved by DH14 (filesystem-only release check). |
| D14 open-source hygiene | §2, §10 | `init` wizard and `--fixtures` ship in M0. |

**Decided here (DH).** Choices the ADR left open:

| id | decision | reason |
| --- | --- | --- |
| DH1 | `cryo-console serve` holds **no** S3 credential and no repo password in memory beyond a request; every admin-path repo operation (init, ls, find, dump, stats, cat config, forget, prune, check, unlock) runs in the `maintain` process, which serve calls over a unix-domain-socket RPC (`adminpath.sock`, mode 0660, group-shared volume) and which also owns the maintenance schedule. Both processes open the same SQLite file (WAL, `BEGIN IMMEDIATE`, short transactions). | ADR D7: the S3 credential exists only in the gateway and the maintenance subcommand. Keeps a web-facing process away from the bucket. |
| DH2 | Heartbeats are stored twice: `device_presence` (one upserted row per device, the `last_seen_at` of the recon) and `heartbeats` (24 h ring). State reads only `device_presence`. | ADR D3 "upserted latest row plus a 24-hour ring"; avoids a 1-row-per-minute hot path in `devices`. |
| DH3 | The restic child gets `RESTIC_REPOSITORY` (with the gateway password), `RESTIC_PASSWORD`, `RESTIC_CACHE_DIR`, `TEMP` from the agent's DPAPI store through its **environment**; no secret on the command line, no cleartext password file. `RESTIC_PASSWORD_FILE` (engine §5.3) is the documented fallback. | argv is readable by any local process; a SYSTEM child's environment is not. Gateway passwords are hex so they need no URL escaping. |
| DH4 | Agent API additions to the seven D6 calls: `POST /token/rotate`, `GET /restores/{id}`, `GET /restores/{id}/artifact`, `POST /progress`, `GET /release/artifact/{version}/{name}`, `POST /enroll/lobby`, `GET /enroll/lobby/{nonce}`. | Needed by token rotation, restores that must not expose paths in the command queue, relay restore to a replacement device, live progress, and the lobby. |
| DH5 | Run timestamps are stored in **server time**: device time minus `clock_offset_ms` at ingest, clamped to `received_at`. `runs.clock_offset_ms` keeps the correction. | ADR D6: "server time for every state decision". A device clock +3 h cannot make a backup look fresh. |
| DH6 | Effective policy = default policy, then the customer's policy (or template), then the device override, composed as **RFC 7386 JSON merge patches** over the nine policy sections; `policy_version_applied` is the version of the customer/default document the device last pulled. | A "sparse per-device override" needs a defined merge; merge-patch is a standard the maintainer can read. |
| DH7 | There is no `device_facts` cache table. The facts the state function needs are read with indexed queries (`idx_runs_last_success`, `idx_runs_last_backup_done`, `idx_runs_open`, `device_presence`). | State is a read-time function (D10); a cache would be a second source of truth. |
| DH8 | An acknowledgement or snooze **never changes a state** or hides an alarm; it mutes notifications and labels the row. | "Quiet green" is the failure mode; operators must still see what they muted. |
| DH9 | A `UNKNOWN` state exists: any non-retired, non-paused device whose facts could not be loaded or are inconsistent is an alarm, never OK. | Matrix "no-data paths are alarms". |
| DH10 | Customer `suspended` = notifications muted, backups continue, rows labelled; `archived` = every device retired, tokens revoked, hidden by default. Neither can be set on an *expected* customer without clearing `expected` first (DB constraint for archive). | Defines matrix rows r242-r243 without risking silent data loss. |
| DH11 | Pause = no new runs start and a running run is cancelled; `paused_until NULL` is an indefinite pause, allowed but listed in every digest with its age. | Datto/Axcient semantics; indefinite pauses are the usual source of forgotten silence. |
| DH12 | Maintenance defaults: `forget` weekly, `prune` per storage target (quarterly if `min_storage_days > 0`, else monthly), `check` monthly (structure) with a rotating `--read-data-subset` at a longer cadence. All three are **proposed defaults to tune in the pilot** (UNKNOWN real cost). | Engine §4.2 prune economics. |
| DH13 | Gateway-password rotation is delivered through the policy pull: the console generates the password, stores bcrypt in `gateway_logins.password_hash` and the plaintext wrapped in `pending_secret_wrapped` until the agent acknowledges (`delivered_at`), then nulls it. | The console otherwise never holds a plaintext gateway password. |
| DH14 | The "release check" job reads a local releases directory (operator or CI drops signed manifests there); it makes **no** outbound request. | ADR D13: no other outbound call. |
| DH15 | Agent logs and per-run detail records on the device (`logs\agent.log`, `logs\runs\<run_id>.jsonl`) are **path-bearing and local-only**: ACL SYSTEM + Administrators, excluded from `cryo-agent diagnose` bundles, never uploaded. The run record sent to the console is the path-free projection `{during, error class, extension, count}`. A technician reads the local file on the machine (RMM file access or console session). | Matrix row r215 ("full detail stays on the device") and ADR D11 as amended: the device holds the files themselves, so a local path-bearing log adds no exposure; hashing added an operator step for nothing. |
| DH16 | stdlib `net/smtp`, in-house TOTP and XLSX, `fpdf`-class PDF; stdlib `ServeMux`; no ORM, no router, no mail SDK. | Dependency budget. |
| DH17 | Enrollment is idempotent on `devices.enroll_nonce`; the console initialises the repo (`restic init`) during `/enroll` through the maintenance RPC; a retried enroll with the same nonce returns the same device and re-delivers the escrowed key (escrowed custody) until `enroll_confirmed_at` is set. | A lost response must not leak a half-provisioned device or a second repo. |
| DH18 | Compose topology: `console` (agent 8443 public, admin 127.0.0.1:8080), `gateway` (rclone, 9443 public), `maintenance`; shared volumes: `db` (console + maintenance), `gateway-auth` (htpasswd, rw for console, ro for gateway), `artifacts` (restore artifacts, releases). | One `docker compose up` gives a stranger a working stack (D14). |
| DH19 | No separate `release_manifests` table: the signed manifest is a JSON column on `agent_releases`. | One row per release; ADR names only the release channel. |
| DH20 | Migration `0001_initial` **is** `schema.sql`. All 34 tables exist from M0; later milestones add behaviour, not tables. A later schema change is a new numbered migration and `schema.sql` is regenerated by CI from the migrations (a CI step diffs them). | No half-built schema to migrate through during Phase 1. |
| DH21 | Vocabulary additions over recon §6: commands `resume`, `hold` (delay new runs, used by maintenance), `uninstall`; alarm types `partial`, `stuck`, `blank`, `identity`, `outdated`, `quota`, `unknown_state`; restore mode `device_relay`; error classes (path-free) on runs; device `enrolled_via` also `adoption`, `import`. | State and flow requirements (F08, F19, F21). |
| DH22 | Customer-held custody (matrix row r110, **could**) is representable from M0 (`customers.key_custody`, `repo_key_escrow.custody`) but the console rejects it with `501` until M13; ADR D9's consequences are enforced explicitly from M0, not left to fail opaquely: browse, technician restore, escrow reveal, escrow verification and server-side forget/prune/check refuse a `customer_held` device with HTTP 409 `custody_customer_held`, and the device page states the consequence (no server-side retention, no technician restore, growth reported but not controlled). | Priority order must, should, could; the schema must not need a migration. |
| DH23 | Object Lock governance (matrix row r114) is **should** in `features.csv` but **could/untested** in the recon engine gate (engine §4.5). It is scheduled in M9 and gated on a lab test with a 1-day retention before it is exposed. | Resolve the discrepancy conservatively. |
| DH24 | M365 / Google Workspace modules and Linux/macOS agents (could) are planned as milestone M15 behind a **new ADR** (different engine); the model carries `source_kind` / `source_id` so nothing blocks them. | restic cannot back up SaaS tenants; promising dates would be fiction. |
| DH25 | Backups run with `--retry-lock 2m` (policy field `schedule.lock_wait`); a longer exclusive lock yields exit 11 and a *deferral*. | Short wait keeps laptops from idling on a server-side prune. |
| DH26 | Connection, TLS, 401/403 and 5xx failures against the gateway are **deferrals** (`deferred_reason = gateway`) with a path-free `error_class`; they become alarms through staleness (STALE) or the fleet-wide gateway banner (C01), never as a fabricated failed run. | F18; engine §3. |
| DH27 | A multi-path technician download is built by `restic restore --include-file` into a temp directory on the admin host and archived; a single path uses `restic dump -a zip`. Temp space is budgeted and purged (`restore_requests.purge_after`). **[Staging superseded by ADR 0008: restores stream; nothing is staged on the admin host.]** | `restic dump` takes one path (UNKNOWN for multi-path; engine §1.13). |
| DH28 | Restores that must land on a *different* device ("restore to replacement", F08) are **relayed**: the admin host builds the archive, the target agent downloads it through `GET /restores/{id}/artifact` and extracts it. The replacement never gets credentials for the old repo. | Per-device repos and `--private-repos` (engine §2.7, §3.1). |
| DH29 | Replacement has two shapes. **Adoption (F21):** the *same* `devices` row continues: a single-use adoption token re-keys it (device token and gateway password rotated, `hw_fingerprint` updated, `enrolled_at` and `first_backup_due_by` reset, `identity_conflict_at` cleared), the repository and `--host` are unchanged, the old hardware's credentials stop working, and the first run is incremental. **New identity (F08/F22):** a new device is enrolled normally, the old device is retired with `replaced_by_device_id` pointing at it, and data comes across by relay restore (DH28). | The gateway username must equal the repository directory (engine §3.1), so continuity of history requires continuity of identity; a data-only recovery does not. |

---

## 2. Repository layout

One Go module, `github.com/cryobackup/cryobackup` (ADR D1; create the `cryobackup` GitHub organisation before the first Go file lands so the import path never changes). `internal/` packages shared by agent and console are marked **(shared)**: the restic JSON types, exit-code mapping, policy schema, wire types and state engine are defined once and tested once.

```
cryobackup/
  go.mod  go.sum  LICENSE (AGPL-3.0)  README.md  CLAUDE.md
  cmd/
    cryo-agent/        Windows service entrypoint + local CLI (install, uninstall, enroll, status, check-in, diagnose)
    cryo-console/      subcommands: init | serve | maintain | user | token | backup | doctor
    cryo-tray/         per-user-session tray icon (should, M8); talks to the service over a named pipe
  internal/
    agent/
      service/         svc.Handler (Stop, Shutdown, PreShutdown, PowerEvent, SessionChange), svc/mgr install, event log writer
      scheduler/       60 s tick + event triggers; pure due(now, lastSuccess, policy) function; windows, jitter, backoff
      conditions/      battery, metered, idle, Wi-Fi/VPN gates; run-anyway-after-N
      supervisor/      restic child: job object, environment, JSON stdout/stderr readers, graceful cancel, exit-code mapping
      journal/         local SQLite journal + outbox, at-least-once delivery, abandoned-run recovery
      uplink/          HTTPS client: backoff, proxy, SPKI pin, long-poll, clock-offset tracking
      commands/        command executor (backup_now, cancel, refresh_policy, hold, pause, restore, update_agent, ...)
      restore/         restore executor + relay-artifact download/extract
      updater/         manifest verify, staged swap, rollback (should, M11)
      secrets/         DPAPI (machine scope) store, ACL setup under C:\ProgramData\CryoBackup
      tray/            named-pipe server (status, back up now, check in)
      diag/            diagnose bundle (path-free; local logs are excluded, DH15)
    console/
      server/          two listeners, TLS, rate limits, request logging, panic recovery
      agentapi/        /api/v1/agent/* handlers (agent listener only)
      adminapi/        /api/v1/* handlers (admin listener only)
      ui/              page + partial handlers, SSE hub, template functions, form validation
      store/           hand-written SQL, Scope type, transactions, migrations/*.sql (embedded), fixtures loader
      auth/            argon2id, TOTP, WebAuthn, OIDC, sessions, CSRF, API tokens, device tokens
      escrow/          wrap/unwrap (AES-256-GCM, HKDF), reveal, verify, master-key rotation
      gateway/         htpasswd renderer + atomic sync, probe login
      maintain/        worker: scheduler, RPC server, forget/prune/check/stats/dump/init/unlock drivers
      adminpath/       RPC types + client shared by serve and maintain
      alerting/        evaluator, dedupe, digest, SMTP, webhook, RMM adapters
      jobs/            in-process job runner with leases (job_state)
      usage/           usage sampler, quota thresholds, growth
      restores/        restore requests, artifact store, purge
      enroll/          tokens, lobby, adoption, script/MSI-parameter generator
      releases/        releases-directory scan, manifest verification
      health/          probes: gateway, storage, SMTP, certificates; banner inputs
      reports/         renderers: html, pdf, csv, xlsx
      fixtures/        canned customers, devices, runs, probes, clock (for --fixtures and tests)
      importers/       CSV import
    restic/  (shared) argument builders, JSON types, exit-code mapping, stderr classifier
    policy/  (shared) model, defaults, merge-patch, validation (keep-within required), window mini-language, retention presets + preview, selection to exclude-file
    api/     (shared) wire types for agent v1 and admin v1, error model, OpenAPI generator
    state/   (shared) Classify, Rollup, Evaluate, ten-day strip, thresholds - the product (section 4)
    logx/    (shared) path-free slog handler and helpers
    clock/ ulid/ crypto/ version/   (shared) injectable clock, ULID, wrap/hash helpers, build info
  web/
    templates/   layouts/, pages/, partials/, email/ (html + text digest, alert, report)
    static/      css/ (tokens.css from /replica-design), js/htmx.min.js (+ licence), fonts/, icons/
  deploy/
    compose/     docker-compose.yml, .env.example, config.example.toml, rclone.conf.example
    systemd/     cryo-console.service, cryo-maintain.service, rclone-gateway.service (bare-metal alternative)
    rclone/      gateway flags, TLS notes, append-only lab test script
    wix/         Product.wxs, restic component, ServiceInstall/ServiceControl, UpgradeCode
    rmm/         install.ps1.tmpl, uninstall-scrub.ps1, GPO/Intune notes (idempotent, dry-run default, discover/act/verify/report)
    ci/          signing-command contract, release manifest builder
  docs/
    decisions/   ADRs
    runbooks/    restore, key loss and break-glass, master-key rotation, console disaster recovery, gateway certificate renewal
    recon-notes/ research/
  test/
    e2e/ lab/ fixtures/   acceptance suites, lab scripts (Windows VM + compose + MinIO), canned API/restic outputs
  .github/workflows/   ci.yml, release.yml, lab.yml
```

Rules enforced by CI: `cmd/*` contain only wiring; `internal/console/*` never imports `internal/agent/*` and vice-versa; `internal/state` and `internal/policy` import nothing outside the standard library and `internal/restic` types; `internal/logx` is the only package allowed to call `slog` handlers directly; no package outside `internal/console/health` and `internal/console/alerting` and `internal/agent/uplink` may create an `http.Client` (analyzer `nonet`).

---

## 3. Schema

Tables: **34**. Access rules: **data-layer checks** (`customer_scope` on `admin_users` applied in the store layer; no RLS in SQLite). The executable DDL is `schema.sql` (migration `0001_initial`, 103 explicit indexes, 252 DDL assertions in `schema_test.py`, shipped next to it). It requires SQLite 3.38 or newer (built-in JSON functions); it was executed and tested under Python's `sqlite3` (SQLite 3.49.1). It uses only core features (partial indexes, `WITHOUT ROWID`, `json_valid`, triggers), so `modernc.org/sqlite` should run it unchanged - **verified in M0 CI, not yet**.

### 3.1 Conventions

| rule | detail |
| --- | --- |
| ids | `TEXT PRIMARY KEY`, 26-character ULIDs generated by the application (`CHECK (length(id) = 26)`); natural keys only for `settings.key`, `job_state.name`, `snapshots (device_id, snapshot_id)`, `usage_samples (device_id, sampled_at)`, `device_presence.device_id`, `maintenance_schedule.device_id`. The device id is also the restic `--host`, the gateway username and the repo directory name. |
| time | `INTEGER` milliseconds since the Unix epoch, UTC, column names end in `_at` (`audit_events.occurred_at` is the recon's `at`). Agent-supplied timestamps carry `CHECK (>= 1e12)` so a seconds value is rejected at insert. Display zone (`settings.display_timezone`, `customers.display_tz`) is applied only at render time. |
| JSON | `TEXT` with `CHECK (json_valid(col))`. |
| enums | `CHECK (col IN (...))` with the vocabulary of recon §6 plus DH21. |
| FKs | every reference has an explicit `ON DELETE`: devices to customers `RESTRICT` (a customer with devices cannot be deleted); everything a device owns (`runs`, `snapshots`, `heartbeats`, tokens, logins, escrow, commands, restores, acks) `CASCADE`; "who did it" columns (`created_by`, `set_by`, `decided_by`) `SET NULL`; `customers.policy_id` and `customers.storage_target_id` `RESTRICT`; `audit_events` has **no FK at all** (rows must outlive the admin, customer and device they describe); `runs.snapshot_id` is deliberately not an FK (it is a repository id; `snapshots` is a cache that `forget` prunes). |
| indexes | every FK column leads an index (asserted by the test script); runs by `(device_id, started_at DESC)`, heartbeats by `(device_id, received_at DESC)`, notification log by `(dedupe_key, sent_at DESC)`, audit by `occurred_at DESC` (and object / customer / actor), commands by `(device_id, status)`; partial indexes for the "last success", "open run" and maintenance reads. |
| no status column | no table has a persisted device or customer **health or alarm** state; `customers.state` (active / suspended / archived) and `devices.retired_at` / `paused_at` are lifecycle facts that are *inputs* to `Classify`, never its output. The only persisted alarm datum is `alarm_acks` (DH8). |
| non-device sources | `devices.source_kind`, `policies.source_id`, `runs.source_id`, `snapshots.source_id` keep the model from assuming every protected thing is a Windows machine (matrix row r254, must). |
| immutability | `audit_events` rows cannot be updated (trigger); only the retention job deletes, by age. |

### 3.2 Per-table summary

| # | table | purpose | key columns | hard constraints |
| --- | --- | --- | --- | --- |
| 1 | `schema_migrations` | Applied migrations (migration 0001 = schema.sql, DH20) | version, name, checksum, created_at | version > 0, unique |
| 2 | `settings` | Global key/value configuration; secrets are references only | key, value (JSON), updated_by | json_valid(value); non-empty key |
| 3 | `storage_targets` | S3-compatible targets: endpoint, bucket, provider facts, gateway URL, soft quota, prune cadence, governance lock | provider, endpoint, bucket, prefix, gateway_url, min_storage_days, default_prune_cadence_days, object_lock_mode, credential_ref, soft_quota_bytes, is_default | provider enum; lock mode none/governance only (no compliance) and days iff governance; one default target; unique (endpoint, bucket, prefix) |
| 4 | `admin_users` | Operators: role, customer scope, password/OIDC, TOTP, lockout | username, role, customer_scope (JSON ids, NULL = all), password_hash, totp_secret_wrapped, oidc_subject, ip_allowlist, locked_until | role enum; only operator/read_only may be scoped; password or OIDC required; TOTP secret iff enrolled; unique username (nocase); unique OIDC subject |
| 5 | `admin_recovery_codes` | TOTP recovery codes as rows for atomic single use | admin_id, code_hash, used_at | unique code_hash |
| 6 | `admin_webauthn_credentials` | FIDO2 credentials (should) | admin_id, credential_id, public_key, sign_count | unique credential_id |
| 7 | `sessions` | Console sessions with idle and absolute expiry and the fresh-MFA stamp | admin_id, token_hash, mfa_verified_at, expires_at, absolute_expires_at | token_hash lowercase sha256 hex, unique; absolute >= idle expiry |
| 8 | `api_tokens` | Scoped, revocable API tokens | admin_id, name, scopes, token_prefix, token_hash, expires_at, revoked_at | unique token_hash; one active name per admin |
| 9 | `policies` | Default / template / customer policies; nine JSON sections; versioned | scope, customer_id, name, version, schedule, conditions, selection, retention, throttle, sla, maintenance, agent_permissions, notification_overrides, source_id | scope enum; customer scope iff customer_id; exactly one default; unique names; json_valid on all sections |
| 10 | `policy_revisions` | Full policy document per version (drift, rollback, blast-radius audit) | policy_id, version, body, changed_by | unique (policy_id, version) |
| 11 | `customers` | Grouping, policy, cost-attribution and expectation unit | name, slug, state, expected, onboarding_until, contacts, policy_id, storage_target_id, display_tz, device_cap, key_custody | state enum; expected customers cannot be archived; archived_at iff archived; slug charset; unique name (nocase) and slug; policy/target FKs RESTRICT |
| 12 | `enrollment_tokens` | Customer-scoped, expiring, multi-use enrollment tokens (and single-use adoption tokens) | customer_id, token_hash, policy_id, expires_at, max_uses, used_count, adopts_device_id, server_fingerprint, revoked_at | unique token_hash; used_count <= max_uses; adoption tokens single-use and cascade with the device |
| 13 | `devices` | The unit of isolation: one restic repo, one gateway login, one key; no status column | customer_id, source_kind, hostname, device_class, agent_version, update_ring, target_version, enrolled_at, enroll_nonce, enroll_confirmed_at, first_backup_due_by, paused_at/until/reason, retired_at, replaced_by_device_id, repo_path, hw_fingerprint, identity_conflict_at | not both retired and paused; pause needs reason and sane expiry; replaced implies retired; unique repo_path and enroll_nonce; customer FK RESTRICT |
| 14 | `device_tokens` | Hashed, rotatable bearer tokens for the agent API | device_id, token_hash, superseded_at, grace_until, expires_at, revoked_at | unique token_hash; one current token per device; superseded and grace_until together |
| 15 | `device_presence` | Latest heartbeat per device (the recon's last_seen_at), upserted every ~60 s | device_id (PK), received_at, service_state, battery/ac/metered/vpn, clock_offset_ms, progress | service_state enum; battery 0-100; received_at in ms |
| 16 | `heartbeats` | 24-hour heartbeat ring for the device page and C13 | device_id, received_at, same fields as presence | service_state enum; received_at in ms; trimmed by heartbeat-ring-trim |
| 17 | `enrollment_requests` | Lobby: pending-approval enrollment for installs without a valid token (should) | request_nonce, hostname, hw_fingerprint, status, customer_id, device_id, decided_by | status enum; approval requires customer and decision time; unique nonce |
| 18 | `gateway_logins` | Per-device rclone htpasswd credentials; plaintext only transiently wrapped during rotation | device_id, username (= device id), password_hash (bcrypt), pending_secret_wrapped, delivered_at, revoked_at, htpasswd_synced_at | bcrypt format; one active login per device and per username; pending secret cleared once delivered |
| 19 | `repo_key_escrow` | Per-device repo password wrapped under the master key; custody; verification | device_id, key_version, custody, wrapped_key, wrap_alg, master_key_id, verified_at, retired_at | custody coherence (escrowed needs key; customer_held has none); one active key per device; unique (device_id, key_version) |
| 20 | `device_policy_overrides` | Sparse per-device policy patch with reason (history kept) | device_id, patch, reason, set_by, expires_at, cleared_at | one active override per device; reason required; json_valid(patch) |
| 21 | `runs` | Backup, restore and server-side maintenance runs (agent- and console-originated) | device_id, origin, kind, trigger_kind, started_at, finished_at, received_at, clock_offset_ms, outcome, restic_exit_code, snapshot_id, stats, error_counts, error_class, attempt, deferred_reason, policy_version | id = unique run id; outcome/kind/trigger/error_class/deferred_reason enums; open iff no outcome; finished >= started; deferred needs reason; snapshot only on backup runs |
| 22 | `snapshots` | Cache of `restic snapshots --json` (the repo is authoritative); backup roots only | device_id, snapshot_id, taken_at, parent_id, paths (roots), tags, summary | PRIMARY KEY (device_id, snapshot_id); 64-hex snapshot id |
| 23 | `usage_samples` | Hourly repository size / counts per device | device_id, sampled_at, repo_bytes, restore_size_bytes, snapshot_count, blob_count, source | PRIMARY KEY (device_id, sampled_at); non-negative sizes; source enum |
| 24 | `maintenance_schedule` | Per-device forget/prune/check cadence, lease and failure counters | device_id, enabled, last_*_at, next_*_after, deferred_count, consecutive_failures, lease_holder, lease_until | lease holder and lease_until together; one row per device |
| 25 | `commands` | Pull-based, idempotent, expiring command queue | device_id, type, params, idempotency_key, status, created_by, expires_at, delivered_at, acked_at, result | UNIQUE (device_id, idempotency_key); type/status enums; expires_at > created_at |
| 26 | `restore_requests` | Technician and device restores; selection kept only encrypted and purge-dated | device_id, target_device_id, snapshot_id, mode, overwrite, verify, simulate, selection_count, selection_hash, selection_wrapped, status, artifact_ref, purge_after | mode/status/overwrite enums; verify and simulate exclusive; artifact needs purge_after and a download/relay mode; relay mode iff target device (different from source) |
| 27 | `alarm_acks` | Acknowledgements and snoozes - the only persisted alarm datum | subject_type, device_id/customer_id, alarm_type, note, snooze_until, snooze_until_next_success, cleared_at | subject coherence; alarm_type enum; one active ack per subject and alarm type |
| 28 | `notification_rules` | Channels, triggers, thresholds, digest/nag, inheritance scope; secrets wrapped | scope, customer_id/device_id, channel, destination, config, secret_wrapped, triggers, min_severity, failure_threshold, notify_recovery | scope coherence; channel/severity enums; failure_threshold >= 1 |
| 29 | `notification_log` | Every send, failure and suppression; feeds dedupe, nag and the delivery-failure alarm | rule_id, dedupe_key, kind, subject_type/id, channel, result, suppress_reason, attempt, next_retry_at, sent_at | result/kind enums; suppress_reason only when suppressed; rule FK SET NULL |
| 30 | `saved_views` | Saved filters (clause tree), columns, sort | owner_id, name, entity, filter, columns, sort, shared | entity enum; unique (owner, entity, name) |
| 31 | `reports` | Report definitions and schedules | name, scope, customer_id, saved_view_id, kind, format, schedule, recipients, active | scope coherence; kind/format enums; saved-view FK SET NULL |
| 32 | `audit_events` | Immutable, path-free audit trail; no foreign keys by design | occurred_at, actor_type, actor_id, actor_label, method, source_ip, action, object_type, object_id, customer_id, result, severity, details | enums; occurred_at in ms; UPDATE blocked by trigger |
| 33 | `agent_releases` | Release registry and the signed manifest (DH19) | version, channel, sha256, signature, restic_version, restic_sha256, min_os, manifest, artifact_ref, withdrawn_at | unique (version, channel); channel enum; sha256 format |
| 34 | `job_state` | Per-job last run, status, lease and next due; feeds system health and C06/C07 | name (PK), last_started_at, last_finished_at, last_status, lease_holder, lease_until, next_due_at | lease holder and lease_until together; status enum |

### 3.3 Recon entities to tables (all 24) and the tables the ADR added

| # | recon entity | table(s) | note |
| --- | --- | --- | --- |
| 1 | Customer | `customers` | customer-scoped policies are `policies` rows with `customer_id`; custody is `customers.key_custody` |
| 2 | Device | `devices` | `last_seen_at` lives in `device_presence`; auth in `device_tokens`; no status column |
| 3 | EnrollmentToken | `enrollment_tokens` | lobby requests (should) in `enrollment_requests` |
| 4 | Policy | `policies` | versions kept in `policy_revisions` |
| 5 | DevicePolicyOverride | `device_policy_overrides` | history kept; one active |
| 6 | Run | `runs` | console maintenance runs share the table (`origin = 'console'`) |
| 7 | Snapshot | `snapshots` | cache only |
| 8 | Heartbeat | `device_presence` + `heartbeats` | latest row + 24 h ring (DH2) |
| 9 | GatewayLogin | `gateway_logins` |  |
| 10 | RepoKeyEscrow | `repo_key_escrow` | customer-held rows record the choice, store no key |
| 11 | StorageTarget | `storage_targets` |  |
| 12 | AlarmAck | `alarm_acks` |  |
| 13 | NotificationRule | `notification_rules` |  |
| 14 | NotificationLog | `notification_log` |  |
| 15 | Report | `reports` |  |
| 16 | SavedView | `saved_views` |  |
| 17 | AdminUser | `admin_users` | recovery codes and WebAuthn credentials are child tables (`admin_recovery_codes`, `admin_webauthn_credentials`); sessions in `sessions` |
| 18 | ApiToken | `api_tokens` |  |
| 19 | AuditEvent | `audit_events` | `at` is named `occurred_at` to follow the `*_at` rule |
| 20 | AgentRelease | `agent_releases` | also holds the manifest (no `release_manifests` table, DH19) |
| 21 | UsageSample | `usage_samples` |  |
| 22 | Command | `commands` |  |
| 23 | RestoreRequest | `restore_requests` | adds `target_device_id` for relay restores (DH28) |
| 24 | Setting | `settings` |  |

Tables added beyond the 24 entities (ADR 0002 and engine needs):

| table | why |
| --- | --- |
| `device_tokens` | hashed bearer tokens (ADR D5/D6); recon had no entity |
| `sessions` | console sessions (ADR D5: session cookie + MFA) |
| `schema_migrations` | embedded migrations (ADR D3) |
| `settings` | (already the recon's Setting entity) |
| `release_manifests` | **not a separate table** - folded into `agent_releases.manifest` (DH19) |
| `policy_revisions` | policy versioning, drift and rollback (matrix: policy) |
| `enrollment_requests` | lobby / pending-approval enrollment (matrix: should) |
| `admin_recovery_codes` | atomic one-time recovery codes (matrix: security) |
| `admin_webauthn_credentials` | WebAuthn (matrix: should) |
| `maintenance_schedule` | per-device forget/prune/check cadence and lease (ADR D8) |
| `job_state` | job leases and system-health feed (ADR D13) |

### 3.4 Hard constraints as database constraints

Every row below is asserted in `schema_test.py` (shipped next to `schema.sql`; `python -I replica/schema_test.py`) (positive control first, then the violation); the "test" column is a substring of the check label.

| hard constraint (recon / ADR) | enforced by | test label |
| --- | --- | --- |
| one active gateway login per device | partial unique index `uq_gateway_logins_active_device` (`WHERE revoked_at IS NULL`) and `uq_gateway_logins_active_username` | "one active login per device" |
| one active override per device | partial unique index `uq_device_policy_overrides_active` (`WHERE cleared_at IS NULL`) | "one active override per device" |
| unique run id (idempotency key) | `runs.id` PRIMARY KEY (ULID, agent-generated); the store checks `device_id` equality on conflict and answers `duplicate` or `409` | "unique run id" |
| unique `(device_id, snapshot_id)` | `snapshots` PRIMARY KEY `(device_id, snapshot_id)` | "unique (device_id, snapshot_id)" |
| unique command idempotency key | `UNIQUE (device_id, idempotency_key)` on `commands` | "unique idempotency key per device" |
| unique token hashes | `UNIQUE` on `device_tokens`, `enrollment_tokens`, `sessions`, `api_tokens` `token_hash` (all `CHECK` lowercase sha256 hex) | "unique token_hash" |
| one current device token | partial unique `uq_device_tokens_current` (superseded tokens stay valid only until `grace_until`) | "one current token per device" |
| a device may not be both retired and paused | `CHECK (NOT (retired_at IS NOT NULL AND paused_at IS NOT NULL))` | "not both retired and paused" |
| a pause needs a reason and a sane expiry | `CHECK`s on `devices.paused_*` | "pause requires a reason" |
| expected customers cannot be archived while expected | `CHECK (NOT (state = 'archived' AND expected = 1))` | "expected customer cannot be archived" |
| run outcome / kind / trigger vocabularies | `CHECK (... IN (...))`: outcome `success partial failed cancelled skipped deferred interrupted abandoned`; kind `backup restore forget prune check unlock maintenance`; trigger `schedule catchup wake boot manual retry` | "outcome enum", "kind enum", "trigger enum" |
| a run is open iff it has no outcome; a deferred run names its reason | `CHECK ((outcome IS NULL) = (finished_at IS NULL))`; deferred reason coupling | "outcome requires finished_at", "deferred requires deferred_reason" |
| one active escrow key per device; custody coherence | `uq_repo_key_escrow_active`; `CHECK` ties `custody` to `wrapped_key` | "one active key per device", "escrowed key needs wrapped_key" |
| exactly one default policy; customer policies belong to a customer | `uq_policies_one_default`; `CHECK ((scope='customer') = (customer_id IS NOT NULL))` | "exactly one default policy" |
| adoption tokens are single-use and die with their device | `CHECK (adopts_device_id IS NULL OR COALESCE(max_uses,0) = 1)`; FK `ON DELETE CASCADE` | "adoption token must be single-use" |
| token use never exceeds its cap | `CHECK (max_uses IS NULL OR used_count <= max_uses)` | "used_count <= max_uses" |
| one active acknowledgement per subject and alarm type | partial unique indexes on `alarm_acks` | "one active ack per device and alarm type" |
| a restore with `--verify` cannot also simulate | `CHECK (NOT (simulate = 1 AND verify = 1))` (restic: `--verify` cannot combine with `--dry-run`, engine §1.12) | "verify cannot combine with simulate" |
| compliance-mode Object Lock is not representable | `CHECK (object_lock_mode IN ('none','governance'))` (engine §4.5) | "excludes compliance" |
| audit rows are immutable | trigger `trg_audit_events_immutable` | "UPDATE is blocked by trigger" |

A bug the test script caught during authoring: `CHECK (adopts_device_id IS NULL OR max_uses = 1)` accepts `max_uses IS NULL` (SQL three-valued logic); the constraint is now written with `COALESCE`. Every other `CHECK` that mixes a nullable column was reviewed for the same failure and is covered by a violating-row test.

### 3.5 Access rules: data-layer checks

SQLite has no row-level security, so authorisation is one check per query in `internal/console/store`:

1. **`Scope` is a required argument.** Every store method that reads or writes customer-owned data takes a `store.Scope{All bool; CustomerIDs []ULID}` built from the session or API token (`admin_users.customer_scope`; NULL means all). There is no overload without it; a `go vet`-style analyzer (`noscope`) fails the build if an exported store method touching `customers`, `devices` or anything joined to them omits the parameter.
2. **List queries append the scope** (`WHERE customer_id IN (...)`); **by-id queries join through `devices.customer_id`** and return `ErrNotFound` (HTTP 404, not 403) for out-of-scope rows, so existence is not leaked.
3. **Role checks are separate** and sit in the handler layer (RO/OP/AD/OW), but the store re-asserts the minimum role for destructive methods (`Purge*`, `Reveal*`) so a handler bug cannot skip them.
4. **Agent API:** the device id is taken from the authenticated token, never from the request body or path; every agent query is keyed by that id.
5. **Test:** a generated scope matrix runs every store method as (a) all-customer admin, (b) operator scoped to customer A, (c) read_only scoped to customer B and asserts no row of another customer is visible or changeable.
6. **Fixtures mode** uses the same store with an in-memory database and canned rows; there is no second code path.

### 3.6 Policy document shape (the nine JSON sections of `policies`)

The sections are validated by `internal/policy` (reject unknown keys on write, tolerate unknown keys on read for forward compatibility). Defaults below are **proposed and unvalidated**; the `init` wizard asks the operator to confirm or change them, and the pilot (M6) is where they get tuned. No number here is a promise.

| section | fields (default) |
| --- | --- |
| `schedule` | `mode` interval\|fixed (`interval`), `interval_minutes` (1440), `window` mini-language e.g. `Mon-Fri/8-18;Sat,Sun/0-24` (`*/0-24`), `fixed_times[]`, `jitter_minutes` (30), `catchup` (true), `run_on_wake` (true), `startup_delay_s` (120), `retries` {`max` 3, `base_s` 300, `cap_s` 3600}, `lock_wait_s` (120), `max_runtime_hours` (12), `skip_if_running` (true) |
| `conditions` | `battery_floor_pct` (laptop 30 / desktop none), `metered` defer\|allow (defer), `wifi_ssid_block[]`, `vpn_required` (false), `idle_only` (false), `run_anyway_after_minutes` (240) |
| `selection` | `roots[]` or `presets[]` (all user profiles \| documents-desktop \| fixed drives), `includes[]`, `excludes[]` (published baseline: temp, Recycle Bin, pagefile, hiberfil, swapfile, System Volume Information, browser caches), `path_flags` {required, optional, require_snapshot, follow_symlinks}, `exclude_cloud_files` (true), `exclude_larger_than`, `vss` {`enabled` true, `timeout_s` 120, `exclude_all_mount_points` false, `exclude_volumes[]`} |
| `retention` | `preset` (named) **or** `rules` {`keep_last`, `keep_hourly`, `keep_daily`, `keep_weekly`, `keep_monthly`, `keep_yearly`}, **`keep_within` (required; validation rejects a policy without it)**, `keep_tag[]` |
| `throttle` | `upload_kibps`, `download_kibps` (unlimited), `windows[]` {when, up, down} (should) |
| `sla` | `stale_after_hours` (72), `unseen_after_hours` (168), `first_backup_grace_hours` (72), `seed_extension_hours` (0 = off), `partial_tolerance` {`max_failed_files` 20, `max_failed_pct` 0.5}, `consecutive_failures_to_alarm` (1), `max_run_hours` (12), `initial_max_run_hours` (168), `stall_after_minutes` (180), `missed_factor` (2.0), `escrow_verify_grace_hours` (24), `escrow_recheck_days` (30), `maintenance_deferral_alarm_after` (10) |
| `maintenance` | `forget_every_days` (7), `prune_every_days` (target default), `max_unused` ("5%"; "20%" on minimum-storage providers), `repack_cacheable_only` (true on minimum-storage providers), `check_every_days` (30), `read_data_subset` ("5%" quarterly), `window_hint` (outside the device's backup window) |
| `agent_permissions` | `allow_local_backup_now`, `allow_local_pause`, `allow_local_restore`, per-setting lock modes (should) |
| `notification_overrides` | `outdated_alarm` (false), `failure_threshold`, `digest_include` (inherit chain global > customer > device) |

**Partial tolerance** (matrix row r152): a `partial` backup (restic exit 3) counts as a success when `failed_files <= max_failed_files` **or** `failed_files / (total_files_processed + failed_files) * 100 <= max_failed_pct`, **and** every error class is in the "expected" set (`permission`, locked file); a missing source root, a VSS fallback on a volume that held a configured root, or an unparseable count is always *over* tolerance (fail loud). VSS fallback under tolerance still raises the info flag `vss_fallback` naming the volume.

---

## 4. State engine

The classification logic is the product (ADR D10). `internal/state` is pure: no I/O, no clock reads, no logging of paths, `now` always injected, imports only the standard library. It is ported from the reference classifier (`classify.py`: `BLANK > STUCK > FAILED > UNSEEN > STALE > OUTDATED > OK`, flags computed independently, first match in precedence becomes primary, all applicable flags kept, `online` is never a flag, unparseable timestamps are BLANK not OK) and extended to the matrix. UrBackup field names are not carried over.

```go
func Classify(f DeviceFacts, t Thresholds, now time.Time) DeviceState   // one device
func Rollup(c CustomerFacts, ds []DeviceState, now time.Time) CustomerState
func Evaluate(w World, now time.Time) FleetView                         // devices + customers + banners + AllClear()
```

`DeviceState{Primary State; Flags []Flag; Since time.Time; Reason ReasonCode; Alarm Severity; Ack *AckView; Attrs []Attr}`. A flag is any state that applies; `Primary` is the first applicable state in the precedence table. Nothing persists a state: the UI, `/api/v1/status`, the digest and the alarm evaluator all call these functions. The evaluator calls them on a schedule only to decide what to *notify*, deduplicated through `notification_log`.

### 4.1 Inputs

| input | source (all read at call time; DH7) | notes |
| --- | --- | --- |
| `now` | injected | server time; the only clock the engine trusts |
| enrolled_at, first_backup_due_by, enroll_confirmed_at | `devices` | grace deadline is reset on re-enroll or replacement |
| retired_at; paused_at / paused_until / pause_reason | `devices` | pause is console-owned |
| identity_conflict_at | `devices` | cloned identity (F01 edge) |
| last_seen | `device_presence.received_at` (server clock) | if absent, `enrolled_at` is used as the reference |
| service_state, progress_advanced_at | `device_presence` | `deferred` and live progress |
| last_success | newest finished `runs` row, `kind='backup'`, `outcome='success'` (`idx_runs_last_success`), or `partial` that passes tolerance | server-time `finished_at` (DH5) |
| last_backup | newest finished backup run (`idx_runs_last_backup_done`): outcome, finished_at, failed_files, total_files, error classes, deferred_reason | |
| open_run | newest `runs` with `outcome IS NULL` for backup (`idx_runs_open`): started_at, initial flag | |
| consecutive_failures | count of newest finished backup runs with outcome `failed` or `abandoned` since the last success; `cancelled`, `skipped`, `deferred`, `interrupted` are neutral | |
| agent_version, expected_version | `device_presence.agent_version`; `devices.target_version` else newest non-withdrawn release on the device's ring | version **tuples**, not strings (`2.5.9 < 2.5.10`); garbled version sorts low; no expected version means OUTDATED never fires |
| escrow | active `repo_key_escrow` row: custody, created_at, verified_at, last_verify_error_class | |
| maintenance | newest finished forget/prune/check runs, `maintenance_schedule.deferred_count` | |
| clock_offset | `device_presence.clock_offset_ms` | informational flag only |
| facts_complete | set by the loader | false if any query for this device errored, timed out or returned an inconsistent row |
| thresholds | merged policy `sla` + `schedule` (DH6) | section 3.6 |

### 4.2 Precedence table (first applicable row is `Primary`; every applicable row is kept as a flag)

| # | state | definition | inputs | UI | alarms? | resolves when | since |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | **RETIRED** | `retired_at` set | `devices.retired_at` | grey badge; collapsed under "Retired (n)"; excluded from rings and rollups | never | terminal (replace or re-enroll creates a new device) | retired_at |
| 2 | **PAUSED** | `paused_at` set and (`paused_until` is NULL or `> now`) | `devices.paused_at / paused_until / pause_reason`, `now` | "Parked" badge with reason and expiry countdown; own group; digest lists every paused device with age, indefinite ones flagged | suppresses rows 6-9, 10-11, 14-16 (§4.3); does **not** suppress IDENTITY, ESCROW, MAINT | resume, or expiry (a pure function: an expired pause is simply ignored, so a stale device alarms the instant the pause ends) | paused_at |
| 3 | **UNKNOWN** | `facts_complete = false` (load error, timeout, inconsistent row) for a non-retired, non-paused device | loader status (`facts_complete`, error class) | red-grey "?" with "state unknown: facts unavailable"; never counted as healthy | **critical** | facts load succeeds | time of first failed load |
| 4 | **IDENTITY_CONFLICT** | `identity_conflict_at` set and unresolved: a second machine presented this device id / token (clone, restored image) | `devices.identity_conflict_at` | red badge "identity conflict"; actions: resolve | **critical** | `POST /devices/{id}/identity/resolve` | identity_conflict_at |
| 5 | **PENDING_FIRST** | no success since (re)enrollment (`last_success` absent or older than `enrolled_at`, which adoption by new hardware resets) **and** `now <= first_backup_due_by` (extended by `seed_extension_hours` only while the open run is initial and progress advanced within `stall_after`) **and** no *hard* failure class (`repo_missing`, `wrong_password`, `permission`, `disk`, `parse`, `internal`) | `last_success`, `enrolled_at`, `first_backup_due_by`, open run (initial, progress), last backup error class; `sla.seed_extension_hours`, `stall_after_minutes` | blue "Pending first backup (grace until T)", progress if running, attempts + last error class | none (suppresses UNSEEN/STALE/MISSED/DEFERRED/OUTDATED) | first success, or grace expiry (becomes BLANK), or a hard failure | enrolled_at |
| 6 | **BLANK** | no success since (re)enrollment **and** (grace expired, or a hard failure, or `enroll_confirmed_at IS NULL` past the grace: "enrolled, never reported"); also any `last_success` that is zero, negative, unparseable or a seconds-epoch value | `last_success`, `first_backup_due_by`, `enroll_confirmed_at`, last backup error class; `sla.first_backup_grace_hours` | red "Never backed up" + reason chip (`never_reported`, `no_successful_run`, `only_failures`) + error class | **critical** | first success | first_backup_due_by |
| 7 | **STUCK** | open backup run whose runtime `> max_run_hours`; for an *initial* run `> initial_max_run_hours`, and an initial run whose progress advanced within `stall_after_minutes` is not stuck until the hard cap | open backup run (`started_at`, initial), `progress_advanced_at`; `sla.max_run_hours`, `initial_max_run_hours`, `stall_after_minutes` | red "Running 76 h (limit 12 h)" with progress; "100 % done" is not health | **critical** | run ends (any outcome) or is cancelled | run started_at + limit |
| 8 | **FAILED** | newest finished backup run is `failed` or `abandoned` **and** `consecutive_failures >= consecutive_failures_to_alarm` **and** no later success | last finished backup run outcome, `consecutive_failures`; `sla.consecutive_failures_to_alarm` | red "Failed" + error class + attempt count; immediate notification | **critical** | next success | finished_at of the first failure in the streak |
| 9 | **PARTIAL** | newest finished backup run is `partial` and **over** tolerance (or tolerance cannot be computed: fail loud) | last finished backup run (outcome, failed_files, total_files, error classes); `sla.partial_tolerance` | amber-red "Partial: n files unreadable" + classes (path-free) | **alarm** | next success, or a partial that passes tolerance | finished_at |
| 10 | **UNSEEN** | `now - (last_seen ?? enrolled_at) > unseen_after_hours` (strict `>`; garbled timestamp counts as unseen) | `device_presence.received_at` (else `enrolled_at`); `sla.unseen_after_hours` | red "Not seen for 9 d" + likely-cause hint (certificate / DNS / offline); customer-wide silence collapses to one row (§4.3) | **critical** | next heartbeat | last_seen + unseen_after |
| 11 | **STALE** | has a success and `now - last_success > stale_after_hours` (strict `>`); never applied to BLANK | `last_success`; `sla.stale_after_hours` | red "No successful backup for 4 d" | **critical** | next success | last_success + stale_after |
| 12 | **ESCROW_UNVERIFIED** | custody `escrowed` and (no active escrow row, or `verified_at` NULL after `escrow_verify_grace_hours`, or older than `escrow_recheck_days`, or last verify failed) | active `repo_key_escrow` (custody, created_at, verified_at, last_verify_error_class); `sla.escrow_verify_grace_hours`, `escrow_recheck_days` | badge "Escrow unverified" + error class; "customer-held" is a neutral attribute instead | **alarm** | successful `cat config` verification | created_at + grace |
| 13 | **MAINT_FAILED** | newest finished forget/prune/check is `failed` (including `guard_refused`, check `num_errors > 0`), or `deferred_count >= maintenance_deferral_alarm_after` ("maintenance starved"), or no successful forget within 2x cadence | newest forget/prune/check runs, `maintenance_schedule.deferred_count`, cadence; `sla.maintenance_deferral_alarm_after` | badge "Retention failed" + op + class; storage will grow | **alarm** | next success of that operation | finished_at |
| 14 | **MISSED** | seen recently, past grace, `now - max(last_success, last_attempt_start) > missed_factor x interval + window slack`, not STALE, no open run, not deferred | `last_success`, last attempt start, schedule interval + window; `sla.missed_factor` | amber "Missed run"; a "missed" circle in the ten-day strip | none (early warning; becomes STALE) | any run starts | expected time |
| 15 | **DEFERRED** | newest backup run is `deferred`, or `service_state = deferred`, and not STALE | newest backup run (deferred + reason), `device_presence.service_state` | amber "Deferred: gateway \| lock \| battery \| metered \| window \| conditions since T" | none until STALE; digest groups "deferred > 24 h"; a fleet-wide cluster becomes banner C01 | a run starts | run started_at |
| 16 | **OUTDATED** | `version(agent) < version(expected)` | `device_presence.agent_version`, expected version (target or newest release on the ring) | amber "Outdated agent 1.4.2 -> 1.5.0" | none by default (`notification_overrides.outdated_alarm` makes it an alarm) | agent updates | first seen outdated |
| 17 | **OK** | none of the above | every input above negative | green | no | - | last_success |

Non-primary **attributes** shown as badges but never changing the primary: `offline` (no heartbeat for > 2 min; connectivity is not health), `customer-held key`, `policy drift` (applied version differs), `clock skew` (`|offset| > 2 min`), `partial under tolerance`, `vss fallback (volume)`, `no schedule` (policy has no schedule), `acknowledged` / `snoozed until T` (DH8).

### 4.3 Suppression, acknowledgement and collapse rules

| condition | effect |
| --- | --- |
| RETIRED | suppresses everything for that device. |
| PAUSED | suppresses BLANK, STUCK, FAILED, PARTIAL, UNSEEN, STALE, MISSED, DEFERRED, OUTDATED. Does **not** suppress IDENTITY, ESCROW, MAINT (they are not freshness conditions). A pause cancels a running run (DH11). |
| PENDING_FIRST | suppresses UNSEEN, STALE, MISSED, DEFERRED, OUTDATED while within grace; hard failures break out (BLANK/FAILED shown at once). |
| customer `suspended` | notifications muted; states and alarms still computed and rendered with a "customer suspended" label (DH10). |
| customer `onboarding_until > now` | suppresses only MISSING (§4.5). |
| acknowledge / snooze | mutes notifications for that `(subject, alarm_type)`; the alarm stays visible, labelled; `snooze_until` expiry or `snooze_until_next_success` clearing is evaluated at read time, so an expired snooze re-raises with no job. |
| customer-wide collapse | if >= 5 devices **and** >= 50 % of a customer's active devices are UNSEEN (or DEFERRED with reason gateway) at once, the UI shows one customer-level row ("customer-wide silence") and notifications collapse to one; the per-device rows remain listed underneath. Same rule fleet-wide for DEFERRED(gateway) produces banner C01 instead of N rows; fleet-wide silence produces no banner (ADR 0013: C04 comes from the agent endpoint self-probe). |
| recovery | transition from an alarming state to a non-alarming one sends one recovery notice (if the rule asks for it) and writes a `recovery` row to `notification_log`; the dedupe key encodes the episode (`sha256(subject, alarm_type, since)`), so a flapping device does not re-page for the same episode. |

### 4.4 Run outcome to result class (backup runs)

| outcome | class | counts as success | counts as failure | clears FAILED / BLANK |
| --- | --- | --- | --- | --- |
| success | success | yes | no | yes |
| partial, within tolerance | success (+ info flag) | yes | no | yes |
| partial, over tolerance | partial | no | no | no (becomes PARTIAL) |
| failed | failure | no | yes | no |
| abandoned | failure | no | yes | no |
| cancelled, skipped, interrupted | neutral | no | no | no |
| deferred | neutral (DEFERRED state) | no | no | no |
| any unknown value from a newer agent | failure (restic: an unknown result "MUST be treated as a command failure", engine §1.10) | no | yes | no |

### 4.5 Customer rollup

| state | rule | UI | alarms? |
| --- | --- | --- | --- |
| **MISSING** | `expected = 1`, `state <> 'archived'`, **zero active (non-retired) devices**, and `onboarding_until` absent or past | red group header "MISSING: expected customer has no devices"; sorts first with UNKNOWN-heavy groups | **critical**; digest line; alert (matrix rows r139, r161) |
| ONBOARDING | `expected = 1`, zero devices, `onboarding_until > now` | blue "Onboarding until T" | none |
| ALARM | any active device with a critical or alarm state | red; count by state | per device |
| WARN | no alarms, at least one MISSED / DEFERRED / OUTDATED / PAUSED(indefinite) | amber | none |
| OK | every active device OK or PENDING_FIRST | green | no |
| EMPTY | zero active devices and not expected | neutral grey "No devices" - **never green and never counted in health %** | none |

Health % = `devices with a fresh success (age <= stale_after) and no critical/alarm state / (active devices - paused - retired)`; a zero denominator renders "n/a", never 100 %. Customers sort: MISSING, UNKNOWN-containing, ALARM, WARN, OK, ONBOARDING, EMPTY. A paused-only customer is WARN ("all devices parked").

### 4.6 Console-level conditions (banners; never a quiet green)

`Evaluate` returns `FleetView{Banners []Banner; Devices; Customers; ComputedAt; LoadedAt; Complete bool}`. `AllClear()` is true only if there are **no banners of any severity**, no alarming devices or customers, `Complete` is true and the fleet is non-empty (or the first-run flag is set). Every page and every API response carries `ComputedAt` (the freshness stamp, in the operator's zone) and, when any banner exists, renders it above the content. Any unknown input makes `AllClear()` false.

| id | condition | detection | proposed threshold | banner | sev |
| --- | --- | --- | --- | --- | --- |
| C01 | **Gateway unreachable** | `health-prober` TLS + authenticated `GET /probe/config` through the gateway as the reserved `probe` login (200 or 404 = healthy; 401, TLS error, timeout, 5xx = failed); also derived from many devices reporting DEFERRED(gateway) | 3 consecutive failures (about 3 min) or >= 50 % of reporting devices deferred(gateway) | "Gateway unreachable: no agent can back up. Last OK hh:mm." | critical |
| C02 | **Storage probe failed** | `maintain` RPC: list prefix + `restic cat config` on the probe repo | 2 consecutive failures | "Storage unreachable from the admin path: retention, restore and usage are impaired." | critical |
| C03 | **SMTP failing** | last 3 sends failed, or a test failed, or alerts pending with no successful send in 24 h | as stated | "Alert e-mail cannot be delivered: alarms may go unnoticed." | critical |
| C04 | **Agent API unreachable** (ADR 0013; supersedes the fleet-silence rule: devices being offline never raise a console banner) | the console's self-probe of `public_url`, as an agent would: dns, connect, tls (chain and name against the system roots or `agent_probe.ca_file`, plus `agent_probe.spki_pin`), certificate expiry, and the agent API's own unauthenticated 401 answer (wrong responder) | 2 failed runs spanning 5 min; an expired certificate at once | "Agent API unreachable: {the failing stage}. No agent can check in or report. Failing for N min." (the 18-day-outage class) | critical |
| C04b | **Agent API not checked** (ADR 0013) | the self-probe cannot run (`public_url` unset, `agent_probe.ca_file` or `spki_pin` unusable), runs without TLS (`http://` lab), or has not completed for 3 intervals | as stated | "Agent API not self-checked: {reason}, so a broken agent listener, certificate or DNS name would go unnoticed." | warn |
| C05 | **Certificate expiring** | TLS dial of the agent listener (the ADR 0013 self-probe) and the gateway, `notAfter` | warn < 21 d, critical < 7 d | "{Agent listener or Gateway} certificate expires in N days." | warn / critical |
| C06 | **Evaluator stalled** | `job_state.alarm_evaluator.last_finished_at` older than 3x its period | 3 min | "Alerting is stalled: this view and notifications may be stale." | critical |
| C07 | **Maintenance worker down** | RPC socket unreachable or `maintenance_scheduler` last finished > 30 min | 30 min | "Maintenance worker is not running: retention, restore browsing and usage sampling are off." | critical |
| C08 | **Empty fleet** | `fleet_ever_enrolled = true` and active devices = 0 (first run with the flag false shows a neutral "enroll your first customer" panel instead) | immediate | "No active devices enrolled." | critical |
| C09 | **Empty poll / partial data** | the facts loader returns zero rows where the previous load in this process had > 0, or any per-device load fails or times out | immediate | "Partial data: N devices could not be classified" (those devices are UNKNOWN) | critical |
| C10 | **Stale view** | page rendered from a snapshot older than 2x the refresh interval, or SSE disconnected | 2 intervals | "Live updates disconnected: data as of hh:mm." | warn |
| C11 | **Database degraded** | `integrity_check` failed (startup / weekly), free disk < 10 %, WAL > 1 GiB, last self-backup older than 26 h or its verify failed, or the maintenance worker could not read or write the console database on 2 storage probes in a row (critical, ADR 0017) | as stated | "Console database needs attention." | warn / critical |
| C12 | **Master key unavailable** | key not loaded or generation mismatch with `repo_key_escrow.master_key_id` | immediate | "Master key unavailable: enrollment, escrow reveal and server-side retention are disabled." Enrollment returns 503 rather than store an unwrapped key. | critical |
| C13 | **Clock suspect** | >= 20 % of reporting devices (min 5) report offsets with the same sign beyond 2 min | as stated | "Server clock may be wrong: device offsets agree." | warn |
| C14 | **Digest not sent** | no `digest` row in `notification_log` by `digest_local_time + 30 min` | 30 min | "Daily digest was not delivered." | critical |
| C15 | **Quota** | usage vs `storage_targets.soft_quota_bytes` and growth rate | 80 % warn, 90 % and 100 % critical; growth alarm when the projection hits 100 % in < 14 d | "Storage at N % of quota." | warn / critical |
| C16 | **Release manifest invalid** | signature verification fails in `release-check` | immediate | "Release manifest rejected." | warn |

Probes are executed by the `health-prober` job and stored in `job_state` (`last_status`, `last_error_class`) plus an in-memory ring; `Evaluate` receives them as input so the whole engine, including these rows, is testable with fixtures.

### 4.7 Time

Server time decides every state (DH5). Device-local concerns (backup windows, "run at 20:00") live in the agent. The ten-day strip and "day" buckets use the *customer's* `display_tz` (else the global zone); a day is a calendar day in that zone, so a DST day has 23 or 25 hours and a bucket is `[local midnight, next local midnight)`. Strip cell: **success** (>= 1 success, no failure that day), **mixed** (success and failure), **missed** (a day the schedule expected a run and none happened, device not paused/retired/pending), **none** (before enrollment, paused, or retired). The digest fires at `digest_local_time` in the display zone, once per local date (dedupe key `digest:<local-date>`), including across DST changes.

### 4.8 Test-case list (table-driven; `internal/state` must pass all of these before any UI is built)

Layout: `internal/state/classify_test.go` (A, B), `fleet_test.go` (C), `rollup_test.go` (D), `strip_test.go` (E). Each case is a row in a `[]struct{name; facts; thresholds; now; want}` table; the fixture builder takes the reference suite's frozen-now approach. Cases A are ports of the reference suite's cases; B-E are new. The **unreachable** and **partial-data** cases are group C and are the first tests written.

| id | name | given | expect |
| --- | --- | --- | --- |
| A01 | ok_fresh_success_recent_seen | success 2 h ago, seen 1 min ago | OK, flags {OK} |
| A02 | blank_no_success_after_grace | no success, grace expired | BLANK (reason no_successful_run) |
| A03 | blank_zero_timestamp | last_success = 0 | BLANK, no panic |
| A04 | blank_unparseable_or_negative_timestamp | last_success = -5 / garbage | BLANK (judged on the parsed value, not a sentinel) |
| A05 | blank_seconds_epoch_instead_of_ms | last_success = 1.7e9 (seconds) | BLANK (out of range) |
| A06 | stuck_open_run_over_max_runtime | open run 76 h, limit 12 h, progress 100 % | STUCK |
| A07 | open_run_under_threshold_not_stuck | open run 1 h | OK |
| A08 | failed_last_run_failed | success 5 d ago then failed run, threshold 1 | FAILED (and STALE as flag) |
| A09 | blank_outranks_failed | no success ever, grace expired, failed run | BLANK primary, FAILED kept as flag |
| A10 | unseen_over_threshold | last_seen 200 h ago, limit 168 h | UNSEEN |
| A11 | unseen_never_seen_uses_enrolled_at | no presence row, enrolled 200 h ago, confirmed | UNSEEN |
| A12 | unseen_unparseable_last_seen | last_seen garbled | UNSEEN, no panic |
| A13 | unseen_boundary_is_strict | last_seen exactly limit ago | not UNSEEN |
| A14 | stale_over_threshold | success 80 h ago, limit 72 h | STALE |
| A15 | stale_boundary_is_strict | success exactly limit ago | not STALE |
| A16 | stale_never_applied_to_blank | no success ever | BLANK only |
| A17 | offline_but_recent_is_ok | no heartbeat for 10 min, seen within limit, success 2 h ago | OK, attribute `offline` |
| A18 | outdated_compares_version_tuples | agent 2.5.9, expected 2.5.10 | OUTDATED |
| A19 | outdated_garbled_version_sorts_low | agent "x.y" | OUTDATED, no panic |
| A20 | outdated_no_expected_version_never_fires | expected "" | OK |
| A21 | multiflag_primary_unseen | stale + unseen + outdated | primary UNSEEN, flags {UNSEEN, STALE, OUTDATED} |
| A22 | multiflag_primary_stuck | stuck + failed + stale | primary STUCK |
| A23 | expected_version_derived_by_tuple_max | fleet versions 2.5.9, 2.5.10, 2.5.2 | expected 2.5.10 |
| B01 | pending_first_within_grace_no_alarm | no success, deadline in future | PENDING_FIRST, Alarm none |
| B02 | pending_first_boundary | now == deadline | PENDING_FIRST; +1 ms -> BLANK |
| B03 | pending_first_expired_is_blank | deadline passed | BLANK |
| B04 | enrolled_never_reported_reason | confirmed = false, past grace | BLANK reason never_reported |
| B05 | pending_first_hard_failure_breaks_out | wrong_password failure in grace | BLANK/FAILED immediately |
| B06 | pending_first_transient_failure_stays_pending | network failure in grace | PENDING_FIRST with attempts shown |
| B07 | pending_first_seed_progress_extends_grace | initial run, progress 1 h ago, extension 24 h | PENDING_FIRST past deadline |
| B08 | pending_first_seed_stalled_no_extension | initial run, progress 6 h ago | BLANK |
| B09 | adopted_device_gets_fresh_grace | adoption reset `enrolled_at` and `first_backup_due_by`; an older success from the previous hardware exists | PENDING_FIRST until the first run on the new hardware (not STALE) |
| B10 | paused_timed_suppresses_stale | paused 2 h more, success 10 d ago | PAUSED, Alarm none |
| B11 | paused_expired_pause_ignored | paused_until in the past, success 10 d ago | STALE |
| B12 | paused_indefinite | paused_until NULL | PAUSED, flagged indefinite for the digest |
| B13 | paused_does_not_suppress_escrow | paused + escrow unverified | primary PAUSED, ESCROW flag alarms |
| B14 | retired_never_alarms | retired, success 100 d ago | RETIRED, Alarm none |
| B15 | retired_excluded_from_rollup | 3 devices, 1 retired | counts 2 |
| B16 | paused_and_retired_unrepresentable | store insert both | rejected (DB CHECK; `test_schema.py`) |
| B17 | partial_under_tolerance_is_success | exit-3 run, 3 failed of 90 000 files | OK, attribute partial_under_tolerance |
| B18 | partial_over_tolerance_pct | 1 % failed, limit 0.5 % | PARTIAL |
| B19 | partial_over_tolerance_count | 25 failed, limit 20 | PARTIAL |
| B20 | partial_missing_source_root_always_over | error class missing_source | PARTIAL regardless of counts |
| B21 | partial_without_error_counts_fails_loud | exit 3, counts unparseable | PARTIAL |
| B22 | partial_vss_fallback_flag_only | under tolerance + vss fallback | OK + attribute vss_fallback(volume) |
| B23 | deferred_gateway_within_sla_warning | deferred(gateway) 3 h, success 20 h ago | DEFERRED, Alarm none |
| B24 | deferred_beyond_sla_becomes_stale | deferred 5 d, success 5 d ago | STALE |
| B25 | deferred_lock_exit11_not_failure | outcome deferred(lock) | DEFERRED, consecutive_failures unchanged |
| B26 | neutral_outcomes_do_not_change_result | interrupted / cancelled / skipped after success | OK, last_success unchanged |
| B27 | abandoned_counts_as_failure | abandoned run | FAILED |
| B28 | consecutive_failure_threshold | threshold 2, one failure | not FAILED; two failures -> FAILED |
| B29 | failed_cleared_by_next_success | failed then success | OK |
| B30 | maintenance_failed_alarm | prune failed | MAINT_FAILED, Alarm alarm |
| B31 | maintenance_lock_deferral_not_failure | prune deferred(lock) x3 | not MAINT_FAILED |
| B32 | maintenance_starved_after_n_deferrals | deferred_count = 10 | MAINT_FAILED (starved) |
| B33 | maintenance_cleared_by_success | prune ok after failure | cleared |
| B34 | escrow_unverified_after_grace | escrowed, never verified, 30 h after enroll | ESCROW_UNVERIFIED |
| B35 | escrow_within_grace_ok | 2 h after enroll | OK |
| B36 | escrow_customer_held_no_alarm | custody customer_held | OK + attribute customer-held |
| B37 | escrow_recheck_overdue | verified 40 d ago, recheck 30 d | ESCROW_UNVERIFIED (suspended until the M5 verification job by ADR 0014: OK while `state.EscrowRecheckEnforced` is false) |
| B38 | identity_conflict_outranks_pending | conflict + pending first | IDENTITY_CONFLICT |
| B39 | clock_skew_does_not_change_state | device clock +3 h, runs stored offset-corrected | state unchanged, attribute clock skew |
| B40 | future_dated_success_clamped | finished_at beyond received_at | clamped; STALE computed from received_at |
| B41 | missed_run_warning | interval 24 h, last attempt 70 h ago, seen now, within SLA 72 | MISSED |
| B42 | missed_not_when_running_or_deferred | open run / deferred | not MISSED |
| B43 | ack_does_not_change_state | acked FAILED | primary FAILED, `Ack` set, notification muted |
| B44 | snooze_expiry_reraises | snooze_until in the past | not muted (no job involved) |
| B45 | snooze_until_next_success_clears | success after snooze | cleared |
| B46 | unknown_outcome_value_is_failure | outcome "exploded" | failure class |
| B47 | unknown_device_class_ok | class "tablet" | OK |
| B48 | deterministic_and_clock_free | same inputs twice; static check for `time.Now` in package | identical; analyzer passes |
| C01 | facts_loader_error_all_devices_unknown | loader returns error for everything | all UNKNOWN, banner C09, `AllClear() == false`, **no OK anywhere** |
| C02 | facts_partial_some_devices_unknown | 3 of 200 devices missing facts | those 3 UNKNOWN (critical), banner "Partial data: 3", `AllClear() == false` |
| C03 | empty_poll_after_nonempty | previous load 200 devices, now 0 rows, no DB error | banner C09 + C08, not "No devices" empty state |
| C04 | first_run_empty_fleet_is_info | `fleet_ever_enrolled = false`, 0 devices | neutral onboarding panel, not an alarm |
| C05 | empty_after_ever_enrolled_is_alarm | flag true, 0 active devices | banner C08 critical |
| C06 | gateway_probe_failing_banner | 3 failed probes | banner C01, `AllClear() == false` |
| C07 | gateway_single_probe_failure_no_banner | 1 failed probe | no banner |
| C08 | storage_probe_failed_banner | 2 failed probes | banner C02 |
| C09 | smtp_failing_banner | 3 failed sends | banner C03 |
| C10 | smtp_recovers_clears_banner | success after failures | banner gone |
| C11 | agent_endpoint_probe_failing_banner (ADR 0013, superseding agent_api_silent_banner) | self-probe failing 2 runs over 5 min, at each stage | banner C04 naming the stage |
| C12 | offline_fleet_raises_no_console_banner (ADR 0013, superseding agent_api_silent_not_when_all_paused_or_empty) | every device silent for 20 min to 60 h, probe healthy | no C04, no console banner; devices STALE/UNSEEN only |
| C13 | evaluator_stalled_banner | evaluator finished 10 min ago, period 60 s | banner C06 |
| C14 | maintenance_worker_down_banner | RPC unreachable | banner C07 |
| C15 | cert_expiring_warn_and_critical | 20 d and 6 d | warn then critical |
| C16 | stale_view_freshness_banner | ComputedAt older than 2 intervals | banner C10 |
| C17 | db_degraded_banner | integrity failed / disk low / the worker cannot read or write the database on 2 probes in a row | banner C11 |
| C18 | master_key_unavailable_banner | key not loaded | banner C12; enrollment refused |
| C19 | many_deferred_gateway_collapse_to_one_banner | 150 devices deferred(gateway) | one banner C01, rows not individually notified |
| C20 | customer_wide_silence_collapses | 8 of 10 devices of a customer UNSEEN | one customer row + one notification |
| C21 | all_clear_requires_everything | any banner / alarm / incomplete | false; true only when none |
| C22 | digest_lists_unknown_and_missing_even_when_rest_green | one MISSING customer, one UNKNOWN device | both in the digest |
| C23 | freshness_stamp_in_every_fleetview | any input | `ComputedAt` non-zero |
| C24 | clock_suspect_banner | 30 % devices +5 min | banner C13 |
| C25 | digest_not_sent_banner | no digest 45 min after time | banner C14 |
| C26 | quota_thresholds | usage at 79 %, 80 %, 90 %, 100 % of soft quota | none, warn, critical, critical (banner C15) |
| C27 | quota_growth_projection | growth projects 100 % within 10 days | critical banner C15 |
| C28 | release_manifest_invalid_banner | signature verification fails | banner C16 |
| D01 | expected_zero_devices_missing | expected, 0 devices | MISSING |
| D02 | expected_all_retired_missing | expected, 3 retired | MISSING |
| D03 | expected_with_device_not_missing | expected, 1 active | not MISSING |
| D04 | not_expected_zero_devices_empty | not expected, 0 | EMPTY, not green, excluded from health % |
| D05 | onboarding_grace_suppresses_missing | `onboarding_until` future | ONBOARDING |
| D06 | onboarding_expired_missing | `onboarding_until` past | MISSING |
| D07 | suspended_mutes_notifications_not_state | suspended customer, FAILED device | state ALARM, notifications muted, labelled |
| D08 | archived_excluded | archived | not rendered by default, no MISSING |
| D09 | rollup_worst_state_wins | OK + STALE + MISSED | ALARM |
| D10 | health_percent_excludes_paused_retired | 10 devices: 2 paused, 1 retired, 7 active/6 healthy | 6/7 |
| D11 | health_percent_zero_denominator_na | all paused | "n/a" |
| D12 | sort_order | MISSING, UNKNOWN, ALARM, WARN, OK, ONBOARDING, EMPTY | exact order |
| D13 | paused_only_customer_not_green | all devices paused | WARN |
| E01 | strip_dst_spring_forward_23h_day | 23-hour local day | one bucket |
| E02 | strip_dst_fall_back_25h_day | 25-hour local day | one bucket |
| E03 | strip_uses_customer_tz_not_display_tz | customer tz differs | buckets in customer tz |
| E04 | strip_cell_classification | success / mixed / missed / none days | correct per day, paused and pre-enrollment days are `none` |
| E05 | digest_once_per_local_day_across_dst | trigger twice on a DST day | one send |

Beyond `internal/state`: `store` tests (idempotent run ingest, scope matrix, constraint violations), `policy` tests (merge-patch, window mini-language with DST, retention preview vs `forget --dry-run --json` fixtures, keep-within required), `restic` tests (exit-code mapping, JSON parsing from canned output including `exit_error` on stderr), agent `scheduler` tests (due-ness, catch-up collapse, jitter, backoff), and the fixtures-mode snapshot tests used by `/replica-test`.

---

## 5. API

Only CryoBackup's own endpoints exist: no vendor private endpoint is used or reproduced, and the only third-party calls the product ever makes are to the operator-configured SMTP relay, the operator-configured webhook / RMM destinations (public, documented APIs with the operator's own credentials) and the optional heartbeat ping. Three route sets, served by **two listeners** (ADR D5), with disjoint `http.Handler` trees; the admin tree is never mounted on the agent listener, and a test walks both muxes and fails if a path appears in both or if an admin path is reachable on the agent port.

| listener | bind (default) | auth | contains |
| --- | --- | --- | --- |
| agent | `0.0.0.0:8443`, TLS required | device token, or enrollment token on `/enroll` | §5.1 only |
| admin | `127.0.0.1:8080` (operator binds to a mesh IP; public exposure needs auth in front) | session cookie + CSRF, or API token (`Authorization: Bearer cbk_...`) | §5.2, §5.3, `/healthz`, `/metrics`, `/static` |

Conventions: JSON (`application/json`, strict decoding on write; unknown fields tolerated on the agent side for forward compatibility, size-capped); errors are `application/problem+json` (RFC 9457) with a stable `code`; lists are cursor-paged (`?cursor=&limit=`) and never silently truncated - a response that hit its cap says so (`truncated: true`, shown on the page); admin `POST` accepts `Idempotency-Key`; agent idempotency keys are the run id and command id; every response carries `X-CryoBackup-Server-Time` (the agent computes clock offset from it); admin responses carry `ComputedAt`. The OpenAPI document is generated from `internal/api` and diffed in CI (M7 finalises coverage; each milestone adds its routes).

### 5.1 Agent API - `/api/v1/agent/*` (public listener)

Who = authenticated principal; the device id always comes from the token. Rate limits: see below the tables.

| method path | does | who | input | output | flow | from |
| --- | --- | --- | --- | --- | --- | --- |
| POST `/api/v1/agent/enroll` | One-time enrollment: create device, gateway login, escrowed repo key, init repo; idempotent on enroll_nonce | enrollment token (Bearer) + nonce | nonce, hostname, os, arch, class, hw fingerprint, agent+restic versions | device_id, device token, gateway url+username+password, repo password (once), first policy, server time | F01 F02 F03 F15 F16 F21 | M0 |
| GET `/api/v1/agent/policy` | ETag-pull of the effective policy (default > customer > device override merge); also carries pending gateway-credential rotation, hold-until, pause, release target | device token | If-None-Match | 200 policy+version+etag, or 304 | F04 F10 F13 F16 | M0 |
| POST `/api/v1/agent/heartbeat` | Presence + health facts every 60 s +/- jitter; first call after enroll sets enroll_confirmed_at | device token | version, uptime, power, network, queue/journal depth, device clock, service state, last error class | server time, clock offset, pending-command hint, policy etag, release hint | F05 F13 F14 F15 F18 | M0 |
| POST `/api/v1/agent/runs` | Batch of run results from the journal; idempotent on run id; open runs may be sent at start and closed later | device token | runs[] (id, kind, trigger, times, outcome, exit code, snapshot id, stats, error_counts, attempt, deferred reason) | per-run created \| duplicate \| rejected | F05 F06 F13 F18 F19 | M0 |
| POST `/api/v1/agent/snapshots` | Snapshot-list cache after each successful backup | device token | snapshots[] from restic snapshots --json (roots only) | upserted count | F07 F22 | M1 |
| GET `/api/v1/agent/commands` | Long-poll (<= 30 s) for queued commands; marks them delivered | device token | wait seconds | commands[] (id, type, params, expires_at) | F05 F07 F09 F12 F13 F14 F19 F21 F22 | M2 |
| POST `/api/v1/agent/commands/{id}/result` | Acknowledge / report a command result (idempotent) | device token | status acked\|succeeded\|failed, result{} | 204 | F05 F07 F09 F12 F21 | M2 |
| POST `/api/v1/agent/token/rotate` | Rotate the device token; old token stays valid for the grace window | device token | none | new device token | F09 F21 | M5 |
| GET `/api/v1/agent/restores/{id}` | Fetch the (decrypted, short-lived) selection for a restore queued for this device | device token | none | snapshot id, include list, target, overwrite, verify/simulate | F07 F08 F21 F22 | M6 |
| GET `/api/v1/agent/restores/{id}/artifact` | Relay restore: stream the server-built archive to the replacement device (Range supported) | device token (target device) | Range | zip/tar stream | F08 F22 | M6 |
| POST `/api/v1/agent/progress` | Live run/restore progress counts (no paths), rate-limited to one per 5 s | device token | run id, percent, bytes, files, errors, stalled flag | 204 | F05 F07 F22 | M9 |
| GET `/api/v1/agent/release` | Signed release manifest for the device's ring (self-update) | device token | current version | manifest + signature, or 204 | F12 | M11 |
| GET `/api/v1/agent/release/artifact/{version}/{name}` | Download a release artifact (MSI / agent / restic.exe) with Range | device token | Range | bytes | F12 | M11 |
| POST `/api/v1/agent/enroll/lobby` | Lobby request for an install without a valid token; creates a pending enrollment_request | anonymous (IP rate-limited, tiny body) | nonce, hostname, os, arch, fingerprint | 202 + poll url | F01 | M9 |
| GET `/api/v1/agent/enroll/lobby/{nonce}` | Poll a lobby request; on approval returns the same payload as /enroll once | anonymous (nonce is the secret) | none | pending \| rejected \| approved + enroll payload | F01 | M9 |

### 5.2 Admin API - `/api/v1/*` (private listener)

Covers every UI action (matrix: api, must). Role = minimum role; customer-scoped operators and read-only users are further limited by the store layer (§3.5). Paths are relative to `/api/v1`. Grouped by resource.

#### Auth, sessions, own account (22)

| method path | does | who | input | output | flow | from |
| --- | --- | --- | --- | --- | --- | --- |
| POST `/api/v1/auth/login` | Password step; starts a pending session | ANON (rate-limited) | username, password | mfa_required + pending session cookie | - | M0 |
| POST `/api/v1/auth/mfa/totp` | Complete login with TOTP | ANON (pending session) | code | session cookie | - | M2 |
| POST `/api/v1/auth/mfa/recovery` | Complete login with a recovery code (single use) | ANON (pending session) | code | session cookie | - | M2 |
| POST `/api/v1/auth/webauthn/begin` | Start a WebAuthn assertion | ANON (pending session) | none | challenge | - | M10 |
| POST `/api/v1/auth/webauthn/finish` | Finish a WebAuthn assertion | ANON (pending session) | assertion | session cookie | - | M10 |
| GET `/api/v1/auth/oidc/start` | Begin OIDC login (Entra ID / Google / generic) | ANON | provider | 302 to IdP | - | M10 |
| GET `/api/v1/auth/oidc/callback` | OIDC callback; maps subject to an admin_users row, still requires local MFA policy | ANON | code, state | session cookie | - | M10 |
| POST `/api/v1/auth/reauth` | Fresh-MFA step-up (stamps sessions.mfa_verified_at) for key reveal and destructive actions | SELF | TOTP / WebAuthn / recovery | 204 | F16 | M5 |
| POST `/api/v1/auth/logout` | Destroy session | SELF | none | 204 | - | M0 |
| GET `/api/v1/me` | Current admin profile, role, scope | SELF | none | profile | - | M2 |
| PATCH `/api/v1/me` | Change email / password | SELF | fields | profile | - | M2 |
| POST `/api/v1/me/totp/begin` | Start TOTP enrollment (secret + QR payload) | SELF | none | otpauth uri | - | M2 |
| POST `/api/v1/me/totp/confirm` | Confirm TOTP enrollment; returns recovery codes once | SELF | code | recovery codes | - | M2 |
| POST `/api/v1/me/recovery-codes` | Regenerate recovery codes (needs fresh MFA) | SELF | none | codes once | - | M2 |
| GET `/api/v1/me/webauthn` | List registered security keys | SELF | none | keys[] | - | M10 |
| POST `/api/v1/me/webauthn` | Register a security key (begin/finish via step param) | SELF | step, attestation | key | - | M10 |
| DELETE `/api/v1/me/webauthn/{id}` | Remove a security key | SELF | none | 204 | - | M10 |
| GET `/api/v1/me/sessions` | List own sessions | SELF | none | sessions[] | - | M2 |
| DELETE `/api/v1/me/sessions/{id}` | Revoke a session | SELF | none | 204 | - | M2 |
| GET `/api/v1/me/api-tokens` | List own API tokens (prefix only) | SELF | none | tokens[] | - | M2 |
| POST `/api/v1/me/api-tokens` | Create a scoped API token (shown once) | SELF | name, scopes, expiry | token once | - | M2 |
| DELETE `/api/v1/me/api-tokens/{id}` | Revoke an API token | SELF | none | 204 | - | M2 |

#### Admin accounts (9)

| method path | does | who | input | output | flow | from |
| --- | --- | --- | --- | --- | --- | --- |
| GET `/api/v1/admins` | List admin accounts | AD | filters | admins[] | - | M2 |
| POST `/api/v1/admins` | Create an account (role, customer scope, optional OIDC subject) | OW | fields | admin | - | M2 |
| GET `/api/v1/admins/{id}` | Get an account | AD | none | admin | - | M2 |
| PATCH `/api/v1/admins/{id}` | Change role / scope / allowlist | OW | fields | admin | - | M2 |
| DELETE `/api/v1/admins/{id}` | Delete an account (never the last owner) | OW | none | 204 | - | M2 |
| POST `/api/v1/admins/{id}/disable` | Disable + revoke sessions and tokens | OW | reason | 204 | - | M2 |
| POST `/api/v1/admins/{id}/enable` | Re-enable | OW | none | 204 | - | M2 |
| POST `/api/v1/admins/{id}/reset-mfa` | Clear TOTP/WebAuthn so the user re-enrolls (break-glass for a lost MFA device) | OW | reason | 204 | F16 | M2 |
| POST `/api/v1/admins/{id}/reset-password` | Issue a one-time password-reset link | OW | none | link once | - | M2 |

#### Customers (12)

| method path | does | who | input | output | flow | from |
| --- | --- | --- | --- | --- | --- | --- |
| GET `/api/v1/customers` | List customers with rollups; filter by state, expected, text | RO | filters, paging | customers[] + rollup | F20 | M2 |
| POST `/api/v1/customers` | Create a customer (policy, storage target, expected, onboarding_until, custody, tz, cap) | AD | fields | customer | F03 F16 F20 | M2 |
| GET `/api/v1/customers/{id}` | Get a customer | RO | none | customer | F20 | M2 |
| PATCH `/api/v1/customers/{id}` | Edit; mark expected / not expected; assign policy | AD | fields | customer | F10 F20 | M2 |
| POST `/api/v1/customers/{id}/suspend` | Suspend (mute notifications; backups continue) | AD | reason | customer | F09 | M10 |
| POST `/api/v1/customers/{id}/unsuspend` | Unsuspend | AD | none | customer | F09 | M10 |
| POST `/api/v1/customers/{id}/archive` | Archive (retires all devices; refused while expected) | AD | confirm | customer | F09 F20 | M10 |
| POST `/api/v1/customers/{id}/unarchive` | Unarchive | AD | none | customer | F09 | M10 |
| DELETE `/api/v1/customers/{id}` | Purge a customer after retention expiry; double confirm + fresh MFA; audited | OW | confirm text, reason | 204 | F09 | M10 |
| GET `/api/v1/customers/{id}/devices` | Devices of a customer with computed state | RO | filters | devices[] | F13 F14 | M2 |
| GET `/api/v1/customers/{id}/health` | Customer rollup: counts per state, health %, ten-day strip, MISSING flag | RO | none | rollup | F15 F20 | M3 |
| GET `/api/v1/customers/{id}/usage` | Storage usage, growth rate, quota meter, next prune date | RO | range | usage | F17 | M5 |

#### Devices (22)

| method path | does | who | input | output | flow | from |
| --- | --- | --- | --- | --- | --- | --- |
| GET `/api/v1/devices` | Fleet table: sort, filter (clause tree), columns, paging; includes computed state | RO | query | devices[] + total | F05 F13 F14 F15 | M0 |
| GET `/api/v1/devices/{id}` | Device detail (facts + computed state + applied policy version) | RO | none | device | F06 F13 F14 | M0 |
| PATCH `/api/v1/devices/{id}` | Rename, notes, class, update ring | OP | fields | device | - | M2 |
| GET `/api/v1/devices/{id}/state` | Computed state with reasons and inputs (for the explain tooltip) | RO | none | state | F06 F14 F15 | M3 |
| GET `/api/v1/devices/{id}/runs` | Run history for one device | RO | kind, outcome, range, paging | runs[] | F05 F06 F13 F18 | M2 |
| GET `/api/v1/devices/{id}/heartbeats` | Heartbeat ring (24 h) | RO | range | heartbeats[] | F13 F14 F18 | M3 |
| GET `/api/v1/devices/{id}/ten-day` | Ten-day strip (success / mixed / missed / none) in the customer's timezone | RO | none | 10 buckets | F05 F13 | M3 |
| POST `/api/v1/devices/{id}/move` | Move to another customer (re-tags policy; storage target must match or be migrated) | AD | customer_id | device | F10 | M10 |
| POST `/api/v1/devices/{id}/pause` | Console-owned pause: 1/2/4/8/24 h, until date, or indefinite; reason required | OP | until, reason | device | F14 | M3 |
| POST `/api/v1/devices/{id}/resume` | End a pause early | OP | none | device | F14 | M3 |
| POST `/api/v1/devices/{id}/retire` | Retire: revoke gateway login + token, stop schedules, keep data until retention expires; optional replaced_by links the successor device (F08) | AD | reason, final_backup?, replaced_by? | device | F09 F14 F20 | M5 |
| POST `/api/v1/devices/{id}/replace` | Start replacement (adoption, DH29): mint a single-use adoption token bound to this device identity; on use the same row is re-keyed and the old hardware is locked out | AD | none | enrollment token + script | F08 F21 | M9 |
| POST `/api/v1/devices/{id}/identity/resolve` | Resolve a cloned-identity conflict (re-key one machine / dismiss) | AD | action | device | F01 F15 | M2 |
| DELETE `/api/v1/devices/{id}` | Purge a retired device record and its repository after retention expiry; fresh MFA | OW | confirm | 204 | F09 | M10 |
| POST `/api/v1/devices/bulk` | Bulk action on a selection: assign policy, move, pause, update agent, retire | OP/AD per action | ids or query, action, params | per-item results | F10 F12 | M7 |
| GET `/api/v1/devices/{id}/effective-policy` | Merged policy with provenance (default / customer / override per field) | RO | none | policy + sources | F04 F10 | M2 |
| GET `/api/v1/devices/{id}/override` | Active per-device override | RO | none | override \| 404 | F04 | M2 |
| PUT `/api/v1/devices/{id}/override` | Set / replace the sparse override (reason required, optional expiry) | AD | patch, reason, expires_at | override | F04 | M2 |
| DELETE `/api/v1/devices/{id}/override` | Clear the override | AD | none | 204 | F04 | M2 |
| GET `/api/v1/devices/{id}/usage` | Usage samples for one device | RO | range | samples[] | F17 | M5 |
| POST `/api/v1/devices/{id}/rotate-token` | Force device-token rotation on next contact | AD | none | 204 | F21 | M5 |
| POST `/api/v1/devices/{id}/rotate-gateway-login` | Rotate gateway password (hot-reload htpasswd; delivered via policy pull) | AD | none | 204 | F18 F21 | M5 |

#### Commands (3)

| method path | does | who | input | output | flow | from |
| --- | --- | --- | --- | --- | --- | --- |
| POST `/api/v1/devices/{id}/commands` | Queue a command: backup_now, cancel, refresh_policy, restart_service, update_agent, check_in, uninstall (idempotency key required) | OP | type, params, idempotency_key, ttl | command | F05 F06 F09 F12 F13 | M2 |
| GET `/api/v1/devices/{id}/commands` | Command history / queue | RO | status | commands[] | F05 | M2 |
| DELETE `/api/v1/devices/{id}/commands/{cid}` | Cancel a queued (not yet delivered) command | OP | none | 204 | F05 | M2 |

#### Runs (3)

| method path | does | who | input | output | flow | from |
| --- | --- | --- | --- | --- | --- | --- |
| GET `/api/v1/runs` | Search runs fleet-wide (clause builder: status, customer, device, kind, date) | RO | query | runs[] | F06 | M3 |
| GET `/api/v1/runs/{id}` | Run detail: stats, error classes (path-free), policy version, timings | RO | none | run | F06 F18 | M3 |
| POST `/api/v1/runs/{id}/cancel` | Cancel a running run (queues a cancel command) | OP | none | command | F05 F06 | M2 |

#### Snapshots and restore (11)

| method path | does | who | input | output | flow | from |
| --- | --- | --- | --- | --- | --- | --- |
| GET `/api/v1/devices/{id}/snapshots` | Snapshot list from the cache (latest preselected) | RO | range | snapshots[] | F07 F22 | M6 |
| POST `/api/v1/devices/{id}/snapshots/refresh` | Refresh the cache from the repo via the admin path | OP | none | 202 | F22 | M6 |
| GET `/api/v1/devices/{id}/snapshots/{sid}/ls` | Browse one directory level of a snapshot (admin path `restic ls`), paged; needs the browse permission | OP | path, cursor | entries[] | F07 F22 | M6 |
| GET `/api/v1/devices/{id}/find` | Filename search across snapshots incl. deleted files (admin path `restic find`) | OP | pattern, snapshots | hits[] | F07 F22 | M9 |
| GET `/api/v1/devices/{id}/versions` | Per-file version history | OP | path | versions[] | F07 F22 | M9 |
| POST `/api/v1/devices/{id}/restores` | Create a restore: device_original \| device_path \| device_relay \| download_zip \| download_tar; overwrite, verify, simulate | OP | snapshot, selection, mode, target, overwrite | restore request | F07 F08 F21 F22 | M0 |
| GET `/api/v1/restores` | List restore requests | RO | filters | restores[] | F22 | M6 |
| GET `/api/v1/restores/{id}` | Restore status / progress | RO | none | restore | F07 F22 | M0 |
| POST `/api/v1/restores/{id}/cancel` | Cancel a restore | OP | none | 204 | F07 F22 | M9 |
| GET `/api/v1/restores/{id}/download` | Download the built archive (single-use signed URL, Range) | OP | token | stream | F22 | M0 |
| DELETE `/api/v1/restores/{id}` | Purge artifact + selection now | OP | none | 204 | F22 | M6 |

#### Enrollment (8)

| method path | does | who | input | output | flow | from |
| --- | --- | --- | --- | --- | --- | --- |
| GET `/api/v1/enrollment-tokens` | List tokens (never the secret) | AD | customer, status | tokens[] | F01 F02 | M2 |
| POST `/api/v1/enrollment-tokens` | Create a customer-scoped token (expiry, max uses, policy, class default, custody) | AD | fields | token once + id | F01 F02 F15 F16 | M2 |
| GET `/api/v1/enrollment-tokens/{id}` | Get a token (usage, remaining) | AD | none | token meta | F02 | M2 |
| DELETE `/api/v1/enrollment-tokens/{id}` | Revoke a token | AD | none | 204 | F02 | M2 |
| GET `/api/v1/enrollment-tokens/{id}/script` | Generate RMM script / MSI parameters / GPO-Intune snippet (needs the secret just shown) | AD | format ps1\|msi\|gpo, token secret | script text | F01 F02 | M2 |
| GET `/api/v1/enrollment-requests` | Pending lobby requests | OP | status | requests[] | F01 | M9 |
| POST `/api/v1/enrollment-requests/{id}/approve` | Approve a lobby request and choose customer | OP | customer_id | device | F01 | M9 |
| POST `/api/v1/enrollment-requests/{id}/reject` | Reject | OP | reason | 204 | F01 | M9 |

#### Policies (10)

| method path | does | who | input | output | flow | from |
| --- | --- | --- | --- | --- | --- | --- |
| GET `/api/v1/policies` | List default / template / customer policies | RO | scope, customer | policies[] | F04 F10 | M2 |
| POST `/api/v1/policies` | Create | AD | fields | policy | F04 | M2 |
| GET `/api/v1/policies/{id}` | Get with current version | RO | none | policy | F04 | M2 |
| PATCH `/api/v1/policies/{id}` | Edit (bumps version, writes a revision); requires prior impact preview token | AD | fields, impact_token | policy | F04 F10 | M2 |
| DELETE `/api/v1/policies/{id}` | Delete (RESTRICTed while any customer references it) | AD | none | 204 | F10 | M2 |
| POST `/api/v1/policies/{id}/impact` | Blast-radius preview: devices/customers affected, fields changed, drift introduced | AD | proposed patch | impact + token | F10 | M2 |
| GET `/api/v1/policies/{id}/revisions` | Revision history | RO | none | revisions[] | F10 | M9 |
| POST `/api/v1/policies/{id}/revert` | Revert to a revision (creates a new version) | AD | version | policy | F10 | M9 |
| POST `/api/v1/policies/assign` | Bulk-assign a policy to customers or devices | AD | policy_id, targets | per-item results | F10 | M7 |
| POST `/api/v1/policies/retention-preview` | Retention preview: `forget --dry-run --json` on the device repo via the admin path -> calendar of survivors | AD | device_id, proposed retention | survivors, removed | F17 F19 | M9 |

#### Storage, usage, maintenance (9)

| method path | does | who | input | output | flow | from |
| --- | --- | --- | --- | --- | --- | --- |
| GET `/api/v1/storage-targets` | List storage targets | AD | none | targets[] | F17 F19 | M5 |
| POST `/api/v1/storage-targets` | Create a target (endpoint, bucket, provider facts, governance lock, credential_ref) | OW | fields | target | F19 | M5 |
| GET `/api/v1/storage-targets/{id}` | Get | AD | none | target | F17 | M5 |
| PATCH `/api/v1/storage-targets/{id}` | Edit (quota, prune cadence, lock days) | OW | fields | target | F17 F19 | M5 |
| DELETE `/api/v1/storage-targets/{id}` | Delete (RESTRICTed while customers reference it) | OW | none | 204 | F19 | M5 |
| POST `/api/v1/storage-targets/{id}/probe` | Probe gateway + bucket list + cert expiry | AD | none | probe result | F18 F19 | M3 |
| GET `/api/v1/usage` | Fleet / per-customer / per-device usage and growth, quota thresholds | RO | group_by, range | usage[] | F17 | M5 |
| GET `/api/v1/devices/{id}/maintenance` | Maintenance schedule + last forget/prune/check outcomes | RO | none | schedule + runs | F19 | M5 |
| POST `/api/v1/devices/{id}/maintenance` | Run maintenance now: forget \| prune \| check \| unlock (stale only) \| repair-index; serialized | AD | op, options | run | F17 F19 | M5 |

#### Alarms, acknowledgements (4)

| method path | does | who | input | output | flow | from |
| --- | --- | --- | --- | --- | --- | --- |
| GET `/api/v1/alarms` | Everything currently alarming (overdue + unseen + failed + MISSING + maintenance + escrow), grouped by customer | RO | filters | alarms[] | F06 F14 F20 | M3 |
| GET `/api/v1/acks` | Active acknowledgements / snoozes | RO | subject | acks[] | F14 | M4 |
| POST `/api/v1/acks` | Acknowledge with note; snooze until date or next success | OP | subject, alarm_type, note, snooze | ack | F06 F14 F20 | M4 |
| DELETE `/api/v1/acks/{id}` | Clear an ack (re-raises) | OP | none | 204 | F14 | M4 |

#### Notifications (8)

| method path | does | who | input | output | flow | from |
| --- | --- | --- | --- | --- | --- | --- |
| GET `/api/v1/notification-rules` | List rules (global > customer > device inheritance) | AD | scope | rules[] | F11 | M4 |
| POST `/api/v1/notification-rules` | Create (email \| webhook \| rmm; triggers, severity, digest, nag, threshold) | AD | fields | rule | F11 | M4 |
| PATCH `/api/v1/notification-rules/{id}` | Edit | AD | fields | rule | F11 | M4 |
| DELETE `/api/v1/notification-rules/{id}` | Delete | AD | none | 204 | F11 | M4 |
| POST `/api/v1/notification-rules/{id}/test` | Send a test through the rule | AD | none | delivery result | F11 | M4 |
| GET `/api/v1/notification-log` | Delivery log incl. failures and suppressions | RO | filters | entries[] | F11 F14 | M4 |
| GET `/api/v1/digest/preview` | Render today's digest without sending | RO | none | html + text | F11 F14 F20 | M4 |
| POST `/api/v1/digest/send` | Send the digest now (idempotent per local day unless forced) | AD | force? | result | F11 | M4 |

#### Reports, exports, saved views (11)

| method path | does | who | input | output | flow | from |
| --- | --- | --- | --- | --- | --- | --- |
| GET `/api/v1/reports` | List report definitions | RO | none | reports[] | F11 F17 | M11 |
| POST `/api/v1/reports` | Create (kind, scope, saved view, format, schedule, recipients) | AD | fields | report | F11 F17 | M11 |
| PATCH `/api/v1/reports/{id}` | Edit | AD | fields | report | F11 | M11 |
| DELETE `/api/v1/reports/{id}` | Delete | AD | none | 204 | F11 | M11 |
| POST `/api/v1/reports/{id}/run` | Run now; returns the rendered artifact | OP | none | html \| pdf \| csv \| xlsx | F11 F17 | M11 |
| GET `/api/v1/reports/calendar` | Calendar-matrix data (device x day, Last / Worst, Issue Only) | RO | month, customer, mode | matrix | F05 F06 | M11 |
| GET `/api/v1/exports/{entity}` | CSV / XLSX export of devices, runs, customers, usage (format by suffix) | RO | entity.format, query | file | F06 F17 | M11 |
| GET `/api/v1/views` | List saved views (own + shared) | RO | entity | views[] | F06 | M3 |
| POST `/api/v1/views` | Create a saved view | RO | name, entity, filter, columns, sort, shared | view | F06 | M3 |
| PATCH `/api/v1/views/{id}` | Edit | SELF/AD | fields | view | F06 | M3 |
| DELETE `/api/v1/views/{id}` | Delete | SELF/AD | none | 204 | F06 | M3 |

#### Fleet, search, live (8)

| method path | does | who | input | output | flow | from |
| --- | --- | --- | --- | --- | --- | --- |
| GET `/api/v1/fleet/summary` | Rings (state mix, last-success age, unseen, customers), counts, banners, freshness stamp | RO | customer? | summary | F05 F14 F15 F18 F20 | M3 |
| GET `/api/v1/fleet/banners` | Console-level conditions (gateway, storage, SMTP, silent agents, stalled jobs, partial data) | RO | none | banners[] | F18 F19 | M3 |
| GET `/api/v1/fleet/overdue` | Overdue list with reason chips | RO | customer? | devices[] | F06 F14 | M3 |
| GET `/api/v1/fleet/unseen` | Devices-not-seen; customer-wide silence collapsed to one row | RO | customer? | rows[] | F14 F18 | M3 |
| GET `/api/v1/fleet/ten-day` | Ten-day activity grid for all or one customer | RO | customer? | grid | F05 F13 | M3 |
| GET `/api/v1/search` | Global search: hostname, customer, device id | RO | q | hits[] | F06 | M10 |
| GET `/api/v1/status` | Read-only machine status: state per device and customer for external monitors | RO (token scope status:read) | customer? | json | F14 F20 | M3 |
| GET `/api/v1/events` | Server-sent events: fleet state ticker, run progress, banners | RO | topics | text/event-stream | F05 F18 | M3 |

#### Audit (2)

| method path | does | who | input | output | flow | from |
| --- | --- | --- | --- | --- | --- | --- |
| GET `/api/v1/audit` | Audit events (actor, action, object, customer, method, IP); never file paths | AD | filters | events[] | F16 | M2 |
| GET `/api/v1/audit/export` | Export audit events | AD | range | csv | F16 | M11 |

#### Releases and agent updates (5)

| method path | does | who | input | output | flow | from |
| --- | --- | --- | --- | --- | --- | --- |
| GET `/api/v1/releases` | Registered releases per channel | AD | none | releases[] | F12 | M11 |
| POST `/api/v1/releases` | Register a release manifest from the releases directory (signature verified) | OW | version, channel, file | release | F12 | M11 |
| PATCH `/api/v1/releases/{id}` | Promote pilot -> stable / withdraw | OW | channel \| withdrawn | release | F12 | M11 |
| POST `/api/v1/releases/{id}/rollout` | Set target_version for a ring / filter / selection (campaign) and queue update_agent | OW | target filter | rollout id | F12 | M11 |
| GET `/api/v1/releases/{id}/rollout` | Per-device rollout status (not seen, pending, updated, failed) | AD | none | devices[] | F12 | M11 |

#### Settings, keys, system (19)

| method path | does | who | input | output | flow | from |
| --- | --- | --- | --- | --- | --- | --- |
| GET `/api/v1/settings` | All non-secret settings | AD | none | settings | F11 | M2 |
| PUT `/api/v1/settings/{key}` | Set a setting (validated; audited) | OW | value | setting | F11 | M2 |
| POST `/api/v1/settings/smtp/test` | Send a test mail through the configured SMTP relay | AD | to | result | F11 | M4 |
| POST `/api/v1/settings/integrations/{name}/test` | Test an RMM adapter (Syncro) or webhook | AD | none | result | F11 | M11 |
| GET `/api/v1/settings/master-key` | Master-key status (generation id, loaded, rewrap progress); never the key | OW | none | status | F16 | M5 |
| POST `/api/v1/settings/master-key/rotate` | Rotate the master key and re-wrap every wrapped column (fresh MFA) | OW | new key source | 202 | F16 | M13 |
| GET `/api/v1/devices/{id}/escrow` | Escrow status for a device (custody, verified_at, key_version) | AD | none | escrow meta | F16 | M5 |
| POST `/api/v1/devices/{id}/escrow/reveal` | Reveal the repo password once: fresh MFA + reason; audited; never logged | OW | reason | password once | F16 | M5 |
| POST `/api/v1/devices/{id}/escrow/verify` | Re-run the escrow test decrypt (`restic cat config`) | AD | none | result | F16 | M5 |
| GET `/api/v1/system/health` | Probe results: gateway, storage, SMTP, maintenance worker, DB, jobs, certificate expiry, versions | RO | none | health | F18 F19 | M3 |
| GET `/api/v1/system/jobs` | Job table: last run, status, next due | AD | none | jobs[] | F19 | M3 |
| POST `/api/v1/system/jobs/{name}/run` | Run a job now (lease-guarded) | AD | none | 202 | F19 | M3 |
| GET `/api/v1/system/backups` | Console self-backups with verification status | OW | none | backups[] | F16 | M5 |
| POST `/api/v1/system/backups` | Take a self-backup now (VACUUM INTO + bundle) | OW | none | backup | F16 | M5 |
| POST `/api/v1/system/backups/verify` | Tested restore: open latest backup in a temp dir, integrity_check, row counts | OW | none | result | F16 | M5 |
| POST `/api/v1/import/customers` | CSV import of customers | AD | csv | per-row results | F03 | M11 |
| POST `/api/v1/import/devices` | CSV import of devices (pre-create with expected hostnames) | AD | csv | per-row results | F03 | M11 |
| GET `/api/v1/openapi.json` | OpenAPI 3.1 document (generated from internal/api types; CI diffs it) | RO | none | json | - | M7 |
| GET `/api/v1/version` | Console, schema, restic and rclone versions | RO | none | versions | - | M0 |


### 5.3 UI routes (private listener) mapped to screens

Server-rendered pages and htmx partials. Every page is deep-linkable (matrix row r195); partials are `hx-get` fragments of the same URL space; `POST`s are htmx form posts that call the same store/service functions as the API handlers (one implementation, two transports). Screen IDs are the recon's S/A/B IDs; screens not listed here are dispositioned in §10.4.

| method path | screen | kind | does | who | flow | from |
| --- | --- | --- | --- | --- | --- | --- |
| GET `/login` | S01 | page | Login: password, TOTP / recovery / WebAuthn step, OIDC button | anonymous | - | M0 |
| POST `/login` | S01 | action | Form post to /api/v1/auth/* | anonymous | - | M0 |
| GET `/` | B01 B03 S02 | page | Fleet health board: banners, age rings, exception-first customer groups, MISSING rows, freshness stamp | RO | F05 F14 F15 F18 F20 | M0 |
| GET `/ui/fleet/rings` | B03 | partial | Rings fragment (click a segment -> filter) | RO | F14 F15 | M3 |
| GET `/ui/fleet/banners` | B01 | partial | Banner strip (polled + pushed via SSE) | RO | F18 F19 | M3 |
| GET `/ui/fleet/customers` | B01 B02 | partial | Customer groups with health gauge + ten-day strips | RO | F05 F20 | M3 |
| GET `/fleet/overdue` | B04 | page | Overdue list with reason chips + acknowledge | RO | F06 F14 | M3 |
| GET `/fleet/unseen` | B05 | page | Devices-not-seen alarm page (customer-wide silence collapsed) | RO | F14 F18 | M3 |
| GET `/activity` | S23 B02 | page | Ten-day activity grid | RO | F05 F06 | M3 |
| GET `/reports/calendar` | B06 | page | Calendar-matrix history | RO | F05 F06 | M11 |
| GET `/customers` | S03 | page | Customer list | RO | F20 | M2 |
| GET `/ui/customers/new` | S04 | partial | Add-customer dialog | AD | F03 F20 | M2 |
| POST `/ui/customers` | S04 | action | Create customer | AD | F03 F16 F20 | M2 |
| GET `/customers/{id}` | B08 S05 | page | Customer page: devices, policy, usage, contacts, expected flag, MISSING state | RO | F15 F20 | M2 |
| GET `/customers/{id}/devices` | S06 | partial | Devices tab | RO | F13 F14 | M2 |
| GET `/customers/{id}/runs` | S16 | partial | Job-logs tab for the customer | RO | F06 | M3 |
| GET `/customers/{id}/policy` | S17 | partial | Policy assignment tab | AD | F10 | M2 |
| GET `/customers/{id}/notifications` | S18 | partial | Per-customer reporting / notification overrides tab | AD | F11 | M4 |
| POST `/ui/customers/{id}/archive` | B08 | action | Archive / suspend / unsuspend / purge dialogs | AD | F09 F20 | M10 |
| GET `/devices` | S19 | page | All devices table (filter, columns, export, saved views) | RO | F05 F13 F14 | M2 |
| GET `/ui/devices/table` | S19 | partial | Table body (htmx paging / filtering) | RO | F05 F13 | M2 |
| GET `/devices/{id}` | B07 | page | Device panel (tabs: summary, history, errors, settings, audit, keys, restore) | RO | F05 F06 F13 F14 F15 | M0 |
| GET `/devices/{id}/{tab}` | B07 | partial | Tab fragment (deep-linkable) | RO | F06 F13 F14 | M2 |
| GET `/ui/devices/{id}/actions` | S07 | partial | Device action menu (disabled / queued when offline) | OP | F05 F09 F12 F19 | M2 |
| POST `/ui/devices/{id}/backup-now` | S08 | action | Back up now / cancel (queues a command) | OP | F05 F06 | M2 |
| GET `/ui/devices/{id}/pause` | B15 | partial | Pause dialog (1/2/4/8/24 h, until date, indefinite) | OP | F14 | M3 |
| POST `/ui/devices/{id}/pause` | B15 | action | Pause / resume | OP | F14 | M3 |
| POST `/ui/devices/{id}/ack` | B04 B05 B07 | action | Acknowledge / snooze with note | OP | F06 F14 F20 | M4 |
| GET `/ui/devices/{id}/override` | B07 B12 | partial | Settings tab: inherit / override control with source labels | AD | F04 | M2 |
| POST `/ui/devices/{id}/retire` | B07 | action | Retire dialog (final backup option) | AD | F09 F14 | M5 |
| GET `/devices/{id}/replace` | B07 | page | Replace-device wizard (token + script for the new hardware) | AD | F08 F21 | M9 |
| GET `/devices/{id}/keys` | B10 | partial | Keys tab: escrow health, custody, reveal dialog (fresh MFA + reason) | AD | F16 | M5 |
| GET `/keys` | B10 | page | Fleet key-custody & escrow health | AD | F16 | M5 |
| GET `/devices/{id}/restore` | B17 S09 | page | Technician restore wizard (snapshot, browse/search, select, mode, progress) | OP | F07 F08 F22 | M0 |
| GET `/ui/devices/{id}/restore/tree` | B17 | partial | Lazy directory tree fragment (admin-path ls) | OP | F07 F22 | M6 |
| GET `/ui/devices/{id}/restore/search` | B17 | partial | Filename search results fragment | OP | F07 F22 | M9 |
| POST `/ui/devices/{id}/restore` | B17 | action | Submit restore | OP | F07 F08 F22 | M0 |
| GET `/restores/{id}` | B17 | page | Restore progress + download | OP | F07 F22 | M0 |
| GET `/runs` | S21 | page | Job logs: search with clause builder | RO | F06 | M3 |
| GET `/runs/{id}` | S22 | page | Run detail: properties, error classes, severity filter | RO | F06 F18 | M3 |
| GET `/enroll` | B16 S30 | page | Enrollment wizard + script generator (token expiry/uses, custody, policy, RMM script, MSI parameters) | AD | F01 F02 F15 F16 | M2 |
| POST `/ui/enroll/token` | B16 | action | Create token (secret shown once) | AD | F01 F02 F16 | M2 |
| GET `/downloads` | S30 | page | Agent downloads; per-customer pre-bound page | AD | F03 | M2 |
| GET `/enroll/requests` | B16 | page | Lobby: pending approvals | OP | F01 | M9 |
| GET `/policies` | S24 | page | Policies list (default marked, device counts) | RO | F04 F10 | M2 |
| GET `/policies/{id}` | S25 S11 S12 B12 | page | Policy editor: schedule, window editor, conditions, selection, exclusions, throttle, SLA, maintenance, permissions | AD | F04 F10 | M2 |
| GET `/policies/{id}/retention` | S13 B13 | partial | Retention editor with presets and calendar preview | AD | F04 F17 | M5 |
| POST `/ui/policies/{id}/impact` | S25 | partial | Blast-radius dialog before save | AD | F10 | M2 |
| GET `/storage` | S33 B11 | page | Storage usage & quota: targets, per customer/device, growth, next prune | RO | F17 F19 | M5 |
| GET `/storage/targets/{id}` | S15 S33 | page | Storage target editor + probe | OW | F19 | M5 |
| GET `/audit` | B09 | page | Audit log viewer | AD | F16 | M2 |
| GET `/releases` | S20 | page | Releases, channels, rollout status | OW | F12 | M11 |
| GET `/settings` | S26 | page | Settings index | AD | F11 | M2 |
| GET `/settings/email` | S27 | page | SMTP delivery + test + digest time | AD | F11 | M4 |
| GET `/settings/notifications` | S18 S34 S35 | page | Rules, webhooks, Syncro adapter | AD | F11 | M4 |
| GET `/settings/accounts` | S32 | page | Admin accounts and roles | OW | - | M2 |
| GET `/settings/self-backup` | S35 | page | Console self-backup status + verify | OW | F16 | M5 |
| GET `/account` | S38 | page | My account: password, TOTP, recovery codes, keys, API tokens | SELF | - | M2 |
| GET `/system` | S36 | page | System health: gateway, storage, SMTP, maintenance worker, DB, certificates, jobs, versions | RO | F18 F19 | M3 |
| GET `/search` | - | page | Global search results | RO | - | M10 |
| GET `/ui/events` | B01 B07 | stream | SSE: fleet ticker, run progress, banners | RO | F05 F18 | M3 |
| GET `/news` | S37 | page | Device news / bulletin | AD | - | M14 |
| GET `/portal` | S39 S28 | page | Customer read-only portal and branding (reserved route) | customer viewer | - | M14 |
| GET `/static/{path}` | - | asset | Embedded, vendored CSS / JS / fonts / icons (no third-party request) | anonymous | - | M0 |
| GET `/healthz` | - | page | Liveness + readiness (DB, master key loaded) - admin listener | anonymous (loopback / mesh) | - | M0 |
| GET `/metrics` | - | page | Prometheus metrics (counts and durations only; no paths, no hostnames in labels) | token scope metrics:read | - | M3 |

### 5.4 Flow index (F01-F22 to routes)

| flow | name | agent API | admin API | UI |
| --- | --- | --- | --- | --- |
| F01 | Enroll via RMM silent install + lobby | `POST /enroll`; `POST /enroll/lobby`; `GET /enroll/lobby/{nonce}` (3) | `POST /devices/{id}/identity/resolve`; `GET /enrollment-tokens`; `POST /enrollment-tokens`; `GET /enrollment-tokens/{id}/script`; `GET /enrollment-requests` +2 more (7) | `GET /enroll`; `POST /ui/enroll/token`; `GET /enroll/requests` (3) |
| F02 | Enroll with a pre-seeded token | `POST /enroll` (1) | `GET /enrollment-tokens`; `POST /enrollment-tokens`; `GET /enrollment-tokens/{id}`; `DELETE /enrollment-tokens/{id}`; `GET /enrollment-tokens/{id}/script` (5) | `GET /enroll`; `POST /ui/enroll/token` (2) |
| F03 | Enroll by hand from the console | `POST /enroll` (1) | `POST /customers`; `POST /import/customers`; `POST /import/devices` (3) | `GET /ui/customers/new`; `POST /ui/customers`; `GET /downloads` (3) |
| F04 | Create policy: schedule, selection, retention | `GET /policy` (1) | `GET /devices/{id}/effective-policy`; `GET /devices/{id}/override`; `PUT /devices/{id}/override`; `DELETE /devices/{id}/override`; `GET /policies` +3 more (8) | `GET /ui/devices/{id}/override`; `GET /policies`; `GET /policies/{id}`; `GET /policies/{id}/retention` (4) |
| F05 | A scheduled backup run (missed / retry paths) | `POST /heartbeat`; `POST /runs`; `GET /commands`; `POST /commands/{id}/result`; `POST /progress` (5) | `GET /devices`; `GET /devices/{id}/runs`; `GET /devices/{id}/ten-day`; `POST /devices/{id}/commands`; `GET /devices/{id}/commands` +6 more (11) | `GET /`; `GET /ui/fleet/customers`; `GET /activity`; `GET /reports/calendar`; `GET /devices` +5 more (10) |
| F06 | Find out why a backup failed | `POST /runs` (1) | `GET /devices/{id}`; `GET /devices/{id}/state`; `GET /devices/{id}/runs`; `POST /devices/{id}/commands`; `GET /runs` +12 more (17) | `GET /fleet/overdue`; `GET /activity`; `GET /reports/calendar`; `GET /customers/{id}/runs`; `GET /devices/{id}` +5 more (10) |
| F07 | Restore to original or alternate location | `POST /snapshots`; `GET /commands`; `POST /commands/{id}/result`; `GET /restores/{id}`; `POST /progress` (5) | `GET /devices/{id}/snapshots`; `GET /devices/{id}/snapshots/{sid}/ls`; `GET /devices/{id}/find`; `GET /devices/{id}/versions`; `POST /devices/{id}/restores` +2 more (7) | `GET /devices/{id}/restore`; `GET /ui/devices/{id}/restore/tree`; `GET /ui/devices/{id}/restore/search`; `POST /ui/devices/{id}/restore`; `GET /restores/{id}` (5) |
| F08 | Replace a dead PC (restore to a new device) | `GET /restores/{id}`; `GET /restores/{id}/artifact` (2) | `POST /devices/{id}/replace`; `POST /devices/{id}/restores` (2) | `GET /devices/{id}/replace`; `GET /devices/{id}/restore`; `POST /ui/devices/{id}/restore` (3) |
| F09 | Decommission a device | `GET /commands`; `POST /commands/{id}/result`; `POST /token/rotate` (3) | `POST /customers/{id}/suspend`; `POST /customers/{id}/unsuspend`; `POST /customers/{id}/archive`; `POST /customers/{id}/unarchive`; `DELETE /customers/{id}` +3 more (8) | `POST /ui/customers/{id}/archive`; `GET /ui/devices/{id}/actions`; `POST /ui/devices/{id}/retire` (3) |
| F10 | Bulk policy change | `GET /policy` (1) | `PATCH /customers/{id}`; `POST /devices/{id}/move`; `POST /devices/bulk`; `GET /devices/{id}/effective-policy`; `GET /policies` +6 more (11) | `GET /customers/{id}/policy`; `GET /policies`; `GET /policies/{id}`; `POST /ui/policies/{id}/impact` (4) |
| F11 | Alerting and reports setup | - (0) | `GET /notification-rules`; `POST /notification-rules`; `PATCH /notification-rules/{id}`; `DELETE /notification-rules/{id}`; `POST /notification-rules/{id}/test` +12 more (17) | `GET /customers/{id}/notifications`; `GET /settings`; `GET /settings/email`; `GET /settings/notifications` (4) |
| F12 | Fleet agent upgrade | `GET /commands`; `POST /commands/{id}/result`; `GET /release`; `GET /release/artifact/{version}/{name}` (4) | `POST /devices/bulk`; `POST /devices/{id}/commands`; `GET /releases`; `POST /releases`; `PATCH /releases/{id}` +2 more (7) | `GET /ui/devices/{id}/actions`; `GET /releases` (2) |
| F13 | Laptop catch-up and run-on-wake | `GET /policy`; `POST /heartbeat`; `POST /runs`; `GET /commands` (4) | `GET /customers/{id}/devices`; `GET /devices`; `GET /devices/{id}`; `GET /devices/{id}/runs`; `GET /devices/{id}/heartbeats` +3 more (8) | `GET /customers/{id}/devices`; `GET /devices`; `GET /ui/devices/table`; `GET /devices/{id}`; `GET /devices/{id}/{tab}` (5) |
| F14 | Device unseen -> alarm -> acknowledge | `POST /heartbeat`; `GET /commands` (2) | `GET /customers/{id}/devices`; `GET /devices`; `GET /devices/{id}`; `GET /devices/{id}/state`; `GET /devices/{id}/heartbeats` +13 more (18) | `GET /`; `GET /ui/fleet/rings`; `GET /fleet/overdue`; `GET /fleet/unseen`; `GET /customers/{id}/devices` +7 more (12) |
| F15 | First-backup grace for a new device | `POST /enroll`; `POST /heartbeat` (2) | `GET /customers/{id}/health`; `GET /devices`; `GET /devices/{id}/state`; `POST /devices/{id}/identity/resolve`; `POST /enrollment-tokens` +1 more (6) | `GET /`; `GET /ui/fleet/rings`; `GET /customers/{id}`; `GET /devices/{id}`; `GET /enroll` (5) |
| F16 | Key custody / escrow at deploy | `POST /enroll`; `GET /policy` (2) | `POST /auth/reauth`; `POST /admins/{id}/reset-mfa`; `POST /customers`; `POST /enrollment-tokens`; `GET /audit` +9 more (14) | `POST /ui/customers`; `GET /devices/{id}/keys`; `GET /keys`; `GET /enroll`; `POST /ui/enroll/token` +2 more (7) |
| F17 | Storage quota approaching | - (0) | `GET /customers/{id}/usage`; `GET /devices/{id}/usage`; `POST /policies/retention-preview`; `GET /storage-targets`; `GET /storage-targets/{id}` +7 more (12) | `GET /policies/{id}/retention`; `GET /storage` (2) |
| F18 | Gateway unreachable from the agent | `POST /heartbeat`; `POST /runs` (2) | `GET /devices/{id}/runs`; `GET /devices/{id}/heartbeats`; `POST /devices/{id}/rotate-gateway-login`; `GET /runs/{id}`; `POST /storage-targets/{id}/probe` +5 more (10) | `GET /`; `GET /ui/fleet/banners`; `GET /fleet/unseen`; `GET /runs/{id}`; `GET /system` +1 more (6) |
| F19 | Server-side retention, prune and check | `POST /runs`; `GET /commands` (2) | `POST /policies/retention-preview`; `GET /storage-targets`; `POST /storage-targets`; `PATCH /storage-targets/{id}`; `DELETE /storage-targets/{id}` +7 more (12) | `GET /ui/fleet/banners`; `GET /ui/devices/{id}/actions`; `GET /storage`; `GET /storage/targets/{id}`; `GET /system` (5) |
| F20 | Expected customer missing | - (0) | `GET /customers`; `POST /customers`; `GET /customers/{id}`; `PATCH /customers/{id}`; `POST /customers/{id}/archive` +7 more (12) | `GET /`; `GET /ui/fleet/customers`; `GET /customers`; `GET /ui/customers/new`; `POST /ui/customers` +3 more (8) |
| F21 | Replace a device and adopt its repository | `POST /enroll`; `GET /commands`; `POST /commands/{id}/result`; `POST /token/rotate`; `GET /restores/{id}` (5) | `POST /devices/{id}/replace`; `POST /devices/{id}/rotate-token`; `POST /devices/{id}/rotate-gateway-login`; `POST /devices/{id}/restores` (4) | `GET /devices/{id}/replace` (1) |
| F22 | Technician restore without the device | `POST /snapshots`; `GET /commands`; `GET /restores/{id}`; `GET /restores/{id}/artifact`; `POST /progress` (5) | `GET /devices/{id}/snapshots`; `POST /devices/{id}/snapshots/refresh`; `GET /devices/{id}/snapshots/{sid}/ls`; `GET /devices/{id}/find`; `GET /devices/{id}/versions` +6 more (11) | `GET /devices/{id}/restore`; `GET /ui/devices/{id}/restore/tree`; `GET /ui/devices/{id}/restore/search`; `POST /ui/devices/{id}/restore`; `GET /restores/{id}` (5) |

### 5.5 Rate limits and abuse controls on the public listener

Agents live behind customer NATs, so **per-IP limits would punish a whole office**; they apply only to unauthenticated routes and to *failed* authentications. Authenticated routes are limited per device.

| route class | limit | on exceed |
| --- | --- | --- |
| `/enroll`, `/enroll/lobby*` (unauthenticated or token-gated) | 10 requests/min/IP; one enrollment at a time per token, later ones wait up to 20 s (M2-FIX-B1, F4); body <= 4 KiB | 429 + `Retry-After`; counted into the audit log (`agent.enroll.denied`) |
| failed device-token authentications | 30/min/IP, then exponential block up to 15 min | 429; one audit row per block, not per request |
| `/heartbeat` | 1 per 20 s per device; up to 4 per 20 s while the device has an open identity conflict, so machines sharing its token are not refused (M2 lab F9; ADR 0014, `internal/console/agentapi` doc.go) | 429 + `Retry-After`; agent honours it |
| `/runs` | 30/min per device, body <= 512 KiB | 429 |
| `/snapshots` | 6/min per device, body <= 2 MiB | 429 |
| `/progress` | 1 per 5 s per device | 429 |
| `/policy` | 6/min per device (ETag, usually 304) | 429 |
| `/commands` (long-poll) | 1 concurrent per device, <= 30 s | 409 for the second poller |
| artifact downloads | 2 concurrent per device | 429 |
| listener | <= 1 024 connections, 8 KiB headers, 30 s read timeout (long-poll excepted), TLS >= 1.2 | connection closed |

The gateway (`rclone serve restic`) has no rate limiting of its own (engine §3.1); a hostile endpoint can fill its own repository, so growth is alarmed (C15) and the append-only + soft-quota story in §8 applies.

### 5.6 Webhooks and outbound integrations

**Webhooks in:** none from vendors (no payment processor, no PSA push) and **no generic inbound webhook in v1**. The only inbound HTTP is the two authenticated listeners above, so there is nothing to forge.

**Webhooks out:**

| channel | what | detail |
| --- | --- | --- |
| generic JSON webhook | one POST per event: `{id, type, severity, at, subject{type,id,customer,hostname}, state, since, reason, links{console}}` | `X-CryoBackup-Signature: sha256=<hmac>` over the raw body with a per-rule secret (stored wrapped); headers/secret in `secret_wrapped`; event whitelist and payload format (generic or chat-style) in `config`; 3 retries with backoff via `notification_log.next_retry_at`; **payloads are path-free by construction** (they are built from `DeviceState`, which holds no paths); SSRF guard: destination must resolve to a public address unless the rule is marked `allow_private` by an owner |
| RMM adapter | raise / clear an alert (and optionally create / close a ticket) on state transitions | first adapter: Syncro through its documented public REST API with the operator's API key (M11); exact calls to be taken from the vendor's current public documentation at implementation time - **UNVERIFIED here**; other PSA/RMM adapters are could (M14). Alerts auto-clear on recovery (matrix row r162). |
| e-mail | immediate failure, recovery, nag, daily digest, scheduled reports, test | SMTP relay from settings; HTML + text parts; subject carries state and customer, body carries hostnames and error classes, never paths; a delivery failure writes `notification_log(result='failed')` and raises C03 |
| heartbeat ping | optional `GET` to a Healthchecks-style URL every N minutes while the evaluator is healthy | configuration only; the console stops pinging when its own evaluator is stalled, so an external monitor notices a dead console (closes the "console itself is silent" hole) |

### 5.7 Jobs

All jobs run in `cryo-console serve` unless marked **(maintain)**, are lease-guarded through `job_state` (`lease_holder`, `lease_until`), and are written to be safe to run twice. "Idempotency" is what makes a duplicate or retried execution harmless.

| name | schedule | what it does | idempotency | from |
| --- | --- | --- | --- | --- |
| `alarm-evaluator` | every 60 s | calls `Evaluate`; for each transition computes `dedupe_key = sha256(subject, alarm_type, since)`; sends immediate/recovery/nag notifications per rule; applies ack/snooze/collapse; writes `notification_log` | a notification is sent only if no `sent` row with the same `dedupe_key` exists; retries use `attempt`/`next_retry_at` | M4 (loop from M3) |
| `daily-digest` | daily at `digest_local_time` in the display zone; catch-up at start-up if missed today | renders the exceptions-first digest (MISSING, UNKNOWN, unseen, blank, failed, stale, partial, escrow, maintenance, parked, deferred > 24 h; banner state first) and sends it | dedupe key `digest:<local-date>:<rule>`; "send now" without `force` is a no-op after the first success of the day | M4 |
| `health-prober` | every 60 s | probes gateway (reserved `probe` login), storage (via maintain RPC), SMTP (connect + EHLO, no mail), TLS certificate expiry of both listeners; records results for §4.6 | read-only probes; results overwrite the previous sample | M3 |
| `abandoned-run-sweeper` | every 5 min | marks open runs `abandoned` when `now - started_at > max_runtime + slack` and the device's last heartbeat does not report that run active; writes `finished_at = last known activity` | `UPDATE ... WHERE outcome IS NULL`; a later report of the same run id with a real outcome overwrites `abandoned` | M3 |
| `maintenance-scheduler` **(maintain)** | every 5 min | picks devices from `maintenance_schedule` whose `next_*_after <= now`, leases one (`lease_holder/lease_until`), runs the loop of §7.3 serialised (concurrency default 1), writes `runs` rows (`origin='console'`), updates next due times with jitter | per-device lease; each op records a `runs` row; a crashed worker's lease expires and the op restarts (prune and copy resume, engine §1.11) | M5 |
| `usage-sampler` **(maintain)** | hourly, staggered by `hash(device_id) % 60` minutes; provider-side listing daily | `restic stats --json --mode raw-data` per device through the admin path; upserts `usage_samples` | sample time floored to the hour; `INSERT ... ON CONFLICT DO UPDATE` | M5 |
| `htpasswd-sync` | on change signal + every 5 min reconcile | renders the htpasswd file from active `gateway_logins` (bcrypt) and replaces it atomically (temp file + rename) if its content hash changed; sets `htpasswd_synced_at`; rclone hot-reloads it | content-hash compare; a lease prevents two writers (§9: two consoles) | M0 |
| `escrow-verifier` **(maintain)** | on enroll, then hourly | for escrowed devices with `verified_at` NULL or older than `escrow_recheck_days`: unwrap the key, run `restic cat config` through the admin path, set `verified_at` or `last_verify_error_class` | idempotent read-only test; failure leaves `verified_at` unchanged | M5 |
| `snapshot-cache-refresh` **(maintain)** | nightly, staggered; and on demand | `restic snapshots --json --host <id>`; reconciles `snapshots` (upsert present, delete forgotten) | reconcile by set difference | M6 |
| `command-expiry` | every 60 s | `queued`/`delivered` commands past `expires_at` become `expired` | `UPDATE ... WHERE status IN (...) AND expires_at < now` | M2 |
| `run-history-retention` | nightly | deletes `runs` older than `run_history_retention_days` (default 90) in batches of 5 000 per transaction, never the newest success and newest outcome per device and kind | batch deletes by id range; safe to interrupt | M5 |
| `audit-retention` | nightly | deletes `audit_events` older than `audit_retention_days` (default 400, minimum 90 enforced in settings validation) | by age | M5 |
| `heartbeat-ring-trim` | every 10 min | deletes `heartbeats` older than `heartbeat_ring_hours` (24) in batches | by age | M0 |
| `db-self-backup` | nightly (config); weekly verify | `VACUUM INTO` a temp file, bundle with config and a manifest, encrypt under a key derived from the master key, write to the backup directory (and optionally a second location), prune old copies; weekly **tested restore**: open the newest copy in a temp directory, `integrity_check`, compare table row counts | file name encodes the date; verify is read-only | M5 |
| `release-check` | every 15 min | scans the releases directory, verifies manifest signatures against the embedded release public key, registers new `agent_releases` on their channel (no network, DH14) | `UNIQUE (version, channel)` | M11 |
| `restore-artifact-purge` | every 5 min | deletes artifacts and nulls `selection_wrapped` where `purge_after < now` or status is terminal; marks `purged`; scans the artifact directory for orphans | idempotent file removal | M6 |
| `session-cleanup` | hourly | deletes expired/revoked sessions | by timestamp | M2 |
| `lobby-expiry` | hourly | marks `pending` enrollment requests past `expires_at` as `expired` | by timestamp | M9 |
| `report-scheduler` | every minute | runs reports whose schedule is due and sends them | dedupe key `report:<id>:<period>` | M11 |
| `notification-retry` | every 60 s | retries `failed` deliveries whose `next_retry_at <= now` (max 3) | attempt counter | M4 |
| `healthcheck-ping` | configurable (default 5 min) | outbound ping while `alarm-evaluator` is healthy | stateless | M3 |

---

## 6. Agent internals

`cryo-agent.exe` is a Windows service (`x/sys/windows/svc`, LocalSystem, SCM recovery = restart; engine §5.1-5.3) that owns its schedule - there is no Task Scheduler, no stored next-run (matrix 203-204). Windows 10 / Server 2016 minimum (restic 0.18+, engine §1.2). No kernel driver, no reboot requirement. Everything below is pull-based: the agent opens all connections; endpoints expose no listener.

### 6.1 Components

| component | package | responsibility | rules that matter |
| --- | --- | --- | --- |
| service host | `agent/service` | `svc.Handler.Execute`; accepts `Stop`, `Shutdown`, `PreShutdown`, `PowerEvent`, `SessionChange`; reports `StopPending` with checkpoints | the control handler returns in < 30 s (engine §5.1): every event is posted to a channel and handled on another goroutine; PBT_* / WTS_* constants are defined locally (UNCERTAIN whether x/sys exports them); `svc.IsWindowsService()` selects service vs console mode (`cryo-agent run` for lab work) |
| scheduler | `agent/scheduler` | a 60 s tick plus event triggers (resume, logon/unlock, boot, command, policy change, connectivity back) call the pure `Due(now, lastSuccess, lastAttemptStart, policy)` | due-ness = interval elapsed since the last *success*; N missed intervals collapse into **one** run (a laptop asleep for a week gets one catch-up run, not seven); a stable per-device offset derived from the device id spreads load; jitter up to `jitter_minutes`; windows are evaluated in **device-local time** (Go `time` with the Windows zone, DST-correct) and gate *starts* only, they never stop a running job; monotonic time is used for elapsed measurements, wall time for windows |
| conditions evaluator | `agent/conditions` | battery floor (on AC overrides), metered connection, idle, Wi-Fi SSID block, VPN requirement | conditions **delay** a due run and record the reason (`battery`, `metered`, `conditions`, `window`); `run_anyway_after_minutes` bounds the delay; a console `backup_now` bypasses window and conditions (matrix row r32) |
| restic supervisor | `agent/supervisor` | builds arguments (`internal/restic`), starts `restic.exe` under a **job object** (kill-on-close, process memory cap, `CREATE_NO_WINDOW`), reads stdout (JSON lines) and stderr (JSON `error` / `exit_error`) concurrently, maps exit codes, enforces `max_runtime`, cancels | pinned binary: path under `C:\Program Files\CryoBackup\`, sha256 compared with the value embedded in the agent at every start, `restic version --json` checked; graceful cancel mechanism is UNCERTAIN (see §6.6) |
| journal | `agent/journal` | durable SQLite journal of runs, an outbox of undelivered results, a kv table (policy ETag + last-known-good policy, clock offset, pending credential rotation), command log | a run row is written **before** restic is started and updated as it progresses; at-least-once delivery, idempotent on run id; on start, open rows from a previous process are closed as `interrupted` (service stop / suspend recorded) or `abandoned` (no record of why) |
| uplink | `agent/uplink` | HTTPS client: heartbeat, policy pull, run/snapshot upload, command long-poll, progress, release | bounded exponential backoff with jitter (1 min doubling to 30 min); honours `Retry-After`; HTTP/HTTPS proxy and SOCKS (should); SPKI pin from the enrollment token (trust on first use, could: mTLS); computes `clock_offset` from `X-CryoBackup-Server-Time`; never blocks the scheduler |
| command executor | `agent/commands` | executes `backup_now`, `cancel`, `refresh_policy`, `restart_service`, `update_agent`, `pause`, `resume`, `hold`, `restore`, `check_in`, `uninstall` | idempotent on command id (command log); results posted with `/commands/{id}/result`; `uninstall` runs the scrub described in the RMM uninstall script; `hold` delays new starts until `hold_until` (used by maintenance) |
| restore executor | `agent/restore` | runs `restic restore` for `device_original` / `device_path`; for `device_relay` downloads the archive and extracts it | selection fetched from `GET /restores/{id}` (never from the command queue), written to a SYSTEM-only temp file, deleted after; progress via `/progress`; path-free logging |
| updater | `agent/updater` | verifies the signed manifest, stages the new MSI/agent, swaps by rename, restarts via SCM recovery, rolls back on a failed start (should, M11) | until M11 upgrades come from MSI upgrades pushed by an RMM (engine §5.6) |
| secrets | `agent/secrets` | DPAPI (machine scope) store for device token, gateway password, repo password; ACL SYSTEM + Administrators on `secrets\` | secrets are decrypted into memory only, passed to the child by environment (DH3) |
| tray pipe server | `agent/tray` | named pipe `\\.\pipe\CryoBackup.Tray` serving status, "check in", "back up now" to `cryo-tray.exe` (should, M8) | ACL: SYSTEM and Administrators full; interactive Users get a read-only status verb and, if `agent_permissions` allows, "back up now"; one JSON line per request |
| diagnostics | `agent/diag` | `cryo-agent status|check-in|diagnose` | `diagnose` emits a path-free bundle (error classes, counts, versions, service state); the path-bearing local logs are deliberately excluded (DH15) |

Boot sequence: validate ACLs on `C:\ProgramData\CryoBackup` (repair or refuse), verify restic hash and version, load secrets, open journal and close stale open runs, load last-known-good policy (or refuse to schedule until the first policy pull succeeds), start uplink and scheduler, write an Event Log line. If the console is unreachable at boot the agent runs from the last-known-good policy.

### 6.2 Run state machine

States are stored in the journal (`runs.state`); the console only ever sees the outcome vocabulary.

| from | event | to | journal / report |
| --- | --- | --- | --- |
| (none) | scheduler says due, or command `backup_now` | **GATED** | no row yet; reason is tracked in memory and reported in the next heartbeat (`service_state = deferred`, `last_error_class`) |
| GATED | window closed / battery / metered / idle / VPN | GATED | after `run_anyway_after_minutes` the gate opens; for each long wait one `deferred` run is journaled with `deferred_reason` (`window`, `battery`, `metered`, `conditions`), at most one per hour |
| GATED | gate open and no other run active | **STARTING** | run row created (`state = starting`, id = new ULID, trigger) and queued for upload as an *open* run |
| GATED / STARTING | another run active (`skip_if_running`) | **DONE(skipped)** | outcome `skipped` |
| STARTING | restic spawned, job-object assigned | **RUNNING** | `state = running`; progress sampled from `status` JSON lines every 5 s into memory; `/progress` posts at most every 5 s |
| RUNNING | exit 0 and a `summary` line parsed | **DONE(success)** | stats, `snapshot_id`, `data_added`; `POST /runs` outbox entry |
| RUNNING | exit 3 | **DONE(partial)** | `error_counts` by class (path-free); tolerance is decided by the console state engine, not the agent |
| RUNNING | exit 11 (lock conflict) | **DEFERRED(lock)** | outcome `deferred`, reason `lock`; retry with the lock-aware backoff (below) |
| RUNNING | exit 1/2/other with a network, TLS, 401/403 or 5xx classification | **DEFERRED(gateway)** | outcome `deferred`, reason `gateway`, `error_class` network / tls / auth / storage (DH26) |
| RUNNING | exit 10, 12, disk, parse, unknown code | **DONE(failed)** | outcome `failed` with `error_class`; exit 10/12 are *hard* failures |
| RUNNING | `cancel` command or pause | **DONE(cancelled)** | restic receives the graceful cancel (exit 130) |
| RUNNING | service stop, pre-shutdown, suspend | **INTERRUPTED** | outcome `interrupted`; restic cancelled; the next start (boot / wake) re-enters GATED with trigger `retry`/`boot`/`wake`; uploaded packs are reused via restic's index checkpoints (engine §1.11) |
| RUNNING | `max_runtime` exceeded (policy `schedule.max_runtime_hours`; initial runs get their own larger cap) | **DONE(cancelled)** | outcome `cancelled` with `stats.long_run = true`; the console raises STUCK from the still-open run *before* this point (its limit is the same policy value), so the alarm does not depend on the cancel succeeding; the next attempt resumes from restic's index checkpoints |
| journal open on start | previous process died without a record | **DONE(abandoned)** | outcome `abandoned` with `finished_at` = last journal activity; a new run is scheduled |
| DEFERRED | backoff elapsed | GATED | next attempt increments `attempt`; backoff 5 min x 2^n, cap 60 min, +-20 % jitter, reset on success; lock deferrals use 2 min to 15 min |
| DONE(any) | outbox delivery | (delivered) | row marked `delivered_at` only after a 2xx `created` or `duplicate` |

A *due check* never creates a run row while GATED for a short time; a `deferred` row is journaled only when a start was actually attempted or a gate held a due run for more than the hourly limit, so the console's run list shows real events, not scheduler noise.

### 6.3 restic invocations (flags per the engine sheet)

Common environment (DH3): `RESTIC_REPOSITORY=rest:https://<device_id>:<gateway-pw>@<gateway>/<device_id>/` (trailing slash mandatory, engine §3.1), `RESTIC_PASSWORD=<repo password>`, `RESTIC_CACHE_DIR=C:\ProgramData\CryoBackup\cache\restic` (never the SYSTEM profile default, engine §1.16/§5.3), `TEMP=C:\ProgramData\CryoBackup\tmp` (pack temp space = pack size x (connections + 1), engine §1.6), `GOMAXPROCS` per resource caps. `--json` on every command that supports it. Flags **never** emitted on Windows: `--one-file-system` (fails immediately, engine §1.5), `--no-extra-verify`, `--ignore-inode` / `--ignore-ctime` (ignored by restic, engine §1.4), `--unsafe-allow-remove-all`.

| operation | command (agent side) | notes |
| --- | --- | --- |
| backup | `restic backup --json --host <device_id> --tag cb.customer=<customer_id> --tag cb.policy=<policy_id>.<version> --tag cb.run=<run_id> --use-fs-snapshot -o vss.timeout=<policy.vss.timeout_s>s [-o vss.exclude-all-mount-points] [-o vss.exclude-volumes="<list>"] --exclude-cloud-files --iexclude-file <ProgramData>\CryoBackup\policy\excludes.txt [--iexclude <p>]... [--exclude-caches] [--exclude-larger-than <size>] [--exclude-if-present <name>] --limit-upload <KiB/s> --limit-download <KiB/s> [--read-concurrency <n>] [--pack-size <MiB>] --retry-lock <lock_wait>s [--skip-if-unchanged --no-scan] <root>...` | `--host` is the stable device id so renames and re-images do not break parent selection or `forget` grouping (engine §1.7); customer / policy / run ride in `--tag`; excludes are generated from baseline + presets + mandatory + custom and are case-insensitive (`--iexclude*`, engine §1.5); `--use-fs-snapshot` falls back per volume on VSS failure and restic then exits 3 (engine §1.1) - the agent reports the volume as a warning class; throttle windows (should) restart the child at boundaries (engine §6); `--skip-if-unchanged --no-scan` only for sub-hourly schedules (could); `--force` only for an operator "full rescan" command |
| restore (files) | `restic restore <snapshot_id> --json --target <dir> --overwrite <always\|if-changed\|if-newer\|never> [--include <path>]... [--include-file <f>] [--verify] --retry-lock <n>s` | `--verify` and `--dry-run` are mutually exclusive (CHECK in `restore_requests`); `--delete` is never emitted; **simulate** = `--dry-run -vv --json`; original-location restore uses `<snapshot>:<root>` with `--target <root>` (engine §1.12 documents the syntax; **UNKNOWN** how drive-letter roots map on Windows - lab test L-RESTORE-1); privileges for full ACL / symlink restore come from LocalSystem (engine §1.3) |
| snapshots | `restic snapshots --json --host <device_id>` | after each successful backup, uploaded as the cache (roots only) |
| ls | admin path: `restic ls --json <snapshot_id> <dir>` | one level per request; **UNKNOWN** whether 0.19.1 lists non-recursively by default - the browse code paginates and caps results server-side and a lab test (L-LS-1) decides whether an explicit flag is needed |
| find | admin path: `restic find --json --host <device_id> [--ignore-case] [--snapshot <id>]... <pattern>` | JSON shape per engine §1.9; the flag names are not in the engine sheet - verify against `restic find --help` of the pinned version |
| dump | admin path: `restic dump -a zip --target <artifact> <snapshot_id> <path>` (folder contents at archive root: `<snapshot>:<subfolder> /`) | one path per invocation; multi-path selections use restore-to-temp then archive (DH27) |
| unlock | `restic unlock` (stale locks only); `restic unlock --remove-all` only from the console, only when the device is quiesced | removing a live lock under a running backup would corrupt the exclusivity assumption (engine §3.2) |
| cat config | `restic cat config` | verifies the key and repo; exit 10 = repository missing, 12 = wrong password |
| stats | admin path: `restic stats --json --mode raw-data` | usage sampling; `--mode debug` for prune sizing (UNCERTAIN cost) |
| version | `restic version --json` | start-up check against the embedded pin |

### 6.4 Exit code to outcome mapping (`internal/restic`, shared and tested once)

| restic exit | meaning (engine §1.10) | backup outcome | restore | forget / prune / check |
| --- | --- | --- | --- | --- |
| 0 | success | `success` **only if a `summary` line was parsed**; exit 0 without one is `failed` (`parse`) | `success` | `success` (check: `num_errors > 0` means `failed`) |
| 3 | backup: unreadable source data (snapshot still created); forget: a removal failed; any missing source path (0.19.0) | `partial` | `partial` | `partial` |
| 10 | repository does not exist | `failed` (`repo_missing`, hard) | `failed` | `failed` |
| 11 | lock conflict | `deferred` (`lock`) | `deferred` (`lock`) | `deferred` (`lock`) |
| 12 | wrong password | `failed` (`wrong_password`, hard) | `failed` | `failed` |
| 130 | cancelled (SIGINT/SIGTERM, 0.19.0) | `cancelled` if the agent asked, `interrupted` if the service is stopping / suspending | same | same |
| 1, 2 | command failed / Go runtime error | classify stderr: connect/TLS/401/403/5xx -> `deferred` (`gateway`); disk full -> `failed` (`disk`); else `failed` (`unknown`) | same | same |
| any other | unknown | `failed` (`unknown`) - "MUST be treated as a command failure" | `failed` | `failed` |

stderr is parsed as JSON `error` / `exit_error` messages (engine §1.9) into **classes and counts**; the message text and the `item` field (a path) are discarded at the parsing boundary.

### 6.5 On-disk layout

Binaries live in `C:\Program Files\CryoBackup\` (admin-writable only, MSI-managed): `cryo-agent.exe`, `restic.exe` (separate MSI component, pinned, signed), `cryo-tray.exe`. State lives in `C:\ProgramData\CryoBackup\` (ACL: SYSTEM and Administrators full control, no inheritance from `ProgramData`):

| path | contents |
| --- | --- |
| `config\agent.json` | server URL, device id, SPKI pin, update ring, proxy; no secrets |
| `secrets\` | `device.token`, `gateway.cred`, `repo.key` - each DPAPI-protected (machine scope); the directory ACL is SYSTEM + Administrators only |
| `policy\` | `policy.json` (last known good), `etag`, `excludes.txt` (generated), `roots.txt` |
| `journal\journal.db` (+ `-wal`) | runs, outbox, kv, command log |
| `cache\restic\` | `RESTIC_CACHE_DIR` (grows with repo metadata; pruned with `restic cache --cleanup`, engine §1.16) |
| `tmp\` | `TEMP` for restic packs, restore selection files; emptied at start and after each run |
| `logs\agent.log` (rotating 10 MiB x 5), `logs\runs\<run_id>.jsonl` (kept 30 days) | path-bearing, local-only, ACL SYSTEM + Administrators (DH15); the uploaded run record is the path-free projection `{during, class, ext, count}`; Windows Event Log source `CryoBackup` mirrors Info / Warning / Error so an RMM can watch it |

### 6.6 Power, session and lifecycle behaviour

| event | handling |
| --- | --- |
| **boot** (delayed auto-start) | start-up sequence above; after `startup_delay_s` run a due check with trigger `boot` |
| **suspend** (`PBT_APMSUSPEND` 0x4) | return immediately; cancel any running restic (it will not survive the network going away), journal `interrupted` (best effort - the machine may sleep before the write); mark `resume_pending` |
| **resume** (`PBT_APMRESUMEAUTOMATIC` 0x12 / `PBT_APMRESUMESUSPEND` 0x7) | return immediately; a goroutine waits for the network (up to 2 min) then runs a due check with trigger `wake`, plus an immediate heartbeat; Modern Standby can delay the event by minutes and the run-on-wake guarantee is therefore **best effort** (recon "hard parts") |
| **logon / unlock** (`WTS_SESSION_LOGON` 5, `WTS_SESSION_UNLOCK` 8) | notify the tray; run a due check (a returning user is the best moment for a missed backup, bounded by conditions) |
| **logoff / lock** | tray only |
| **pre-shutdown** (`SERVICE_CONTROL_PRESHUTDOWN`) | ask restic to stop gracefully so it releases its lock, wait up to the configured pre-shutdown timeout while reporting `StopPending` checkpoints, then terminate the job object; journal `interrupted`; flush the journal |
| **stop** | same as pre-shutdown without urgency |
| **crash / power loss** | next start closes open journal rows as `abandoned`; restic's leftover lock goes stale after 30 min (engine §2.1) and does not block the next run |
| **clock jump** | scheduler uses monotonic time for elapsed intervals; a jump forward looks like "due" and yields one run; a jump backward cannot create a future-dated success because the console clamps (DH5) |
| **graceful cancel** | the intended mechanism is `CTRL_BREAK_EVENT` to a child started with `CREATE_NEW_PROCESS_GROUP` so restic exits 130 and removes its lock (engine §5.4). **UNCERTAIN** whether this works for a console-less child; fallback after 10 s is `TerminateJobObject` (lock goes stale, harmless). Lab test L-CANCEL-1 decides before M1 exits. |

### 6.7 Offline behaviour

| situation | behaviour |
| --- | --- |
| console unreachable, gateway reachable | backups continue on the last-known-good policy; results queue in the journal (cap 50 000 undelivered rows; beyond that oldest *delivered-equivalent* detail is summarised and a `journal_overflow` class is reported later); heartbeats keep retrying with backoff; the console will correctly show UNSEEN after `unseen_after_hours` - the alarm never depends on the agent being able to reach the console |
| gateway unreachable, console reachable | runs become `deferred(gateway)` with an error class (network / tls / auth), reported by heartbeat and run upload; backoff 5 min to 60 min; no false `failed`; if the outage exceeds the SLA the console correctly shows STALE |
| both unreachable | journal only; scheduler keeps trying; no data loss because the journal is local and restic resumes from index checkpoints |
| credential rejected (401 after a rotation) | agent calls `/policy`, which carries the pending new gateway credential (DH13); acknowledges delivery; retries |
| certificate expiry / DNS failure on either endpoint | identical to "offline" from the agent's side by design; the console's C04/C05 banners and the UNSEEN state are what make it visible - the monitoring must not rely on the agent |
| captive portal / proxy | classified `network`; deferred; the proxy configuration can be set in `config\agent.json` or by policy |
| clone / restored image | the heartbeat carries the hardware fingerprint; a mismatch or two simultaneous sessions sets `identity_conflict_at` (state IDENTITY_CONFLICT) instead of two machines writing into one repo silently |

---

## 7. Gateway and maintenance

### 7.1 The compose stack (DH18)

Three services from one `docker compose up` (the "maintenance timer" of ADR D1/D7 is the `maintenance` service's internal scheduler; `docker compose run --rm maintenance maintain --once` or a systemd timer works too); the reference file is `deploy/compose/docker-compose.yml` (a fragment is meant to drop into an existing stack). Image tags are pinned (digest in production); the rclone and restic binaries are pinned and hash-checked at image build.

| service | image / command | holds | network | volumes |
| --- | --- | --- | --- | --- |
| `console` | `cryo-console serve` | DB (rw), master key (read via mounted secret), **no S3 credential** | `8443` agent API (public), `127.0.0.1:8080` admin (operator rebinds to a mesh IP) | `db`, `gateway-auth` (rw), `artifacts`, `adminpath` socket, config + TLS (ro) |
| `gateway` | pinned rclone: `serve restic ... store:<bucket>/<prefix>` | the S3 **gateway** credential in a mounted `rclone.conf` (remote `store:` with `no_check_bucket = true`, engine §3.1/§4.4) | `9443` (public, TLS >= 1.2) | `gateway-auth` (**ro**), `rclone.conf` (ro), TLS (ro) |
| `maintenance` | `cryo-console maintain --loop` | S3 **admin** credential, master key, DB (rw) | none inbound; outbound to storage only | `db`, `adminpath` (socket server), `artifacts` |

Gateway command (CLI flags, never environment variables - the env form of `--append-only` has failed to enforce before, engine §3.2):

```
rclone serve restic --addr :9443 --cert /tls/gateway.crt --key /tls/gateway.key --min-tls-version tls1.2 \
       --htpasswd /auth/htpasswd --append-only --private-repos store:<bucket>/<prefix>
```

`--private-repos` confines the login `<device_id>` to `/<device_id>/`; `--htpasswd` is bcrypt and hot-reloaded; `--min-tls-version tls1.2` because the default is 1.0. `--server-read-timeout/--server-write-timeout` stay at 1 h (a pack upload on a slow link). rclone offers no quota, metrics or upload verification (engine §3.1): quota is soft and computed by the console, integrity is restic's own end-to-end verification plus scheduled `check`. A release gate (**L-APPEND-1**) runs `restic forget` / `forget --prune` / `rewrite` / `key remove` with a device login against the deployed gateway and requires 403/failure.

Bare-metal alternative: `deploy/systemd/` units for the same three processes; the same flags.

### 7.2 htpasswd and gateway-login lifecycle (D7, D9, DH13)

| step | what happens |
| --- | --- |
| **create** (during `/enroll`) | generate a 32-byte hex password (no URL escaping needed), bcrypt it at cost 10 (`$2y$`, htpasswd-compatible), insert `gateway_logins(username = device_id)`, signal `htpasswd-sync`, then confirm with one authenticated request as that login (the console knows the plaintext only at this instant); a 401 after 10 s means hot-reload did not happen and enrollment returns 503 for retry |
| **sync** | render all active logins plus the reserved `probe` login into a temp file in `/auth`, `fsync`, `rename` over `htpasswd`, mode 0640; the first line is a comment `# cryobackup generation <n> sha256=<hash>`; set `htpasswd_synced_at`. rclone re-reads the file on change (engine §3.1). |
| **rotate** | one transaction: set `revoked_at` + `rotated_at` on the active row and insert a new row with the new bcrypt hash and `pending_secret_wrapped`; sync. A rotation is a **swap, not an overlap** (one htpasswd line per user): the agent treats a 401 as "pull policy now", receives the new password in the policy response, acknowledges (`delivered_at` set, `pending_secret_wrapped` nulled) and resumes - the window is one failed request. |
| **revoke** (retire, customer archive, compromised device) | set `revoked_at`, sync; the entry disappears and the next request on any open connection fails authentication; data stays until retention expires |
| **reconcile** | every 5 min compare the file's content hash with the rendering of the DB; a mismatch rewrites the file and writes an audit event (`gateway.htpasswd.drift`) |
| **two consoles** | unsupported topology (single node, ADR D3). Detected, not prevented: the sync job takes the `htpasswd-sync` lease and refuses to overwrite a file whose generation header is newer than its own DB view, raising an alarm instead. |
| **probe login** | reserved username `probe` (not a ULID, so it cannot collide with a device and is not stored in `gateway_logins`); its password is derived from the master key (HKDF context `cryobackup/probe/v1`), never stored; the sync job bcrypts it into the htpasswd file; `health-prober` authenticates as `probe` and requests `/probe/config` - a 404 (no repository) or 200 proves the listener, TLS, authentication **and** the gateway-to-S3 hop; a 401, a TLS error, a timeout or a 5xx is a failed probe (C01) |

### 7.3 Maintenance loop, per device (D8)

`maintenance-scheduler` (in `maintain`) selects due devices from `maintenance_schedule`, takes the per-device lease, and runs the steps below **serialised** (default concurrency 1, because prune RAM scales with blob count, engine §2.3-2.4). Every operation addresses the repository through restic's `rclone:` backend on the admin path - `-r rclone:admin:<bucket>/<prefix>/<device_id>` with `-o rclone.program=rclone` and `-o rclone.args="serve restic --stdio [--s3-bypass-governance-retention]"` (the `rclone.args` option name is from restic's repository documentation, not the engine sheet: verify against the pinned restic). The repo password is unwrapped from `repo_key_escrow` in memory for the duration of the call.

| # | step | detail |
| --- | --- | --- |
| 0 | **applicability** | skip devices with `maintenance_schedule.enabled = 0` (customer-held key: no server-side retention, growth reported but not controlled, UI says so) and retired devices past retention (those go to the purge path) |
| 1 | **pre-flight** | skip and reschedule (+15 to 60 min, jittered) if the latest heartbeat reports `service_state = running` or an open backup run exists; queue a `hold` command (delay new starts for the expected duration) so the exclusive lock window is not raced; prefer the quiet hours implied by the policy window |
| 2 | **lock handling** | no separate lock probe: the exclusive operation itself is the test; exit **11** is a **deferral** (`runs.outcome = deferred`, `deferred_reason = lock`, `maintenance_schedule.deferred_count + 1`), never a failure. After 3 consecutive lock deferrals run `restic unlock` (stale locks only, 30-minute rule, engine §2.1). `unlock --remove-all` is never automatic. |
| 3 | **forget (dry run first)** | `restic forget --json --dry-run --host <device_id> --group-by host,paths <policy keep-* flags> --keep-within <D> [--keep-tag <t>]...` - `--keep-within` is **always present** (policy validation rejects a retention section without it) so a snapshot flood cannot push real history out of the keep windows (engine §1.14, §3.2). The JSON (`remove[]`, `keep[]`) feeds the **safety guard**: abort, write `runs(failed, error_class = guard_refused)` and alarm if the removal would exceed `X %` of snapshots (proposed 50 %), would remove the newest snapshot, or if the newest snapshot is future-dated or implausibly old (clock skew / flood). |
| 4 | **forget (real)** | the same command without `--dry-run` and without `--prune` (two separate invocations because `forget --prune` mixes JSON and text, engine §1.9). Exit 3 = a removal failed -> `partial`. |
| 5 | **prune** (per cadence) | `restic prune --max-unused <p> [--repack-cacheable-only] [--max-repack-size <s>]`; **no JSON** (engine §1.9): success is exit 0, the summary lines are parsed best-effort into `stats` (UNKNOWN stable format: keep the raw tail out of storage, store only parsed numbers); resumes after interruption (engine §1.11); exit 11 = deferral |
| 6 | **check** (per cadence) | `restic check --json [--read-data-subset <x%>]` (structure monthly; read-data subset on a longer cadence); `summary.num_errors > 0` or `broken_packs` > 0 -> `failed` with `error_counts`; `suggest_repair_index` / `suggest_prune` are shown to the operator, **not auto-run** |
| 7 | **stale locks** | `restic unlock` after the operations to clear locks this worker left behind |
| 8 | **usage refresh** | `restic stats --json --mode raw-data` -> `usage_samples`; triggers C15 evaluation |
| 9 | **record** | one `runs` row per operation (`origin = console`, `kind` forget / prune / check / unlock, trigger `schedule` or `manual`), `maintenance_schedule.last_*_at` and jittered `next_*_after`; failures raise MAINT_FAILED (§4.2); a failure increments `consecutive_failures`, a success resets it; audit event for manual runs |
| 10 | **governance bypass** | `--s3-bypass-governance-retention` is passed **only** here, only when `storage_targets.object_lock_mode = 'governance'`, never on the client-facing gateway; **L-GOV-1** (lab test with a 1-day retention: lock files, undeletable stale locks, exclusive-lock acquisition, `unlock` with bypass) must pass before M9 exposes the setting (engine §4.5) |

### 7.4 Prune cadence by provider (defaults; DH12, UNKNOWN real cost until the pilot)

| storage target characteristic | default prune cadence | prune options | why (engine §) |
| --- | --- | --- | --- |
| minimum storage duration (Wasabi: 90 days pay-as-you-go, 30 days reserved) | quarterly (`default_prune_cadence_days = 90`) | `--max-unused 20%..30%`, `--repack-cacheable-only` for routine runs | every object deleted before the minimum is billed for the remainder, and repack writes new packs that start a fresh clock (§4.2) |
| no minimum duration (Backblaze B2, IDrive e2) | monthly | `--max-unused 5%` | no early-delete fee; B2 needs the "keep only the last version" lifecycle rule so hidden versions are purged (§4.3); `check --read-data*` egress is free only up to 3x stored data (§4.1) |
| generic S3 / MinIO | monthly | defaults | measure first |
| any target with Object Lock governance | as above, plus bypass flag on the admin path only | | §4.5 |

`min_storage_days` and `default_prune_cadence_days` are columns of `storage_targets`; `policy.maintenance.prune_every_days` overrides per policy. Pricing and policies change: the UI shows the stored numbers, not a quoted price, and the docs say "re-check before quoting customers" (engine §4.1).

### 7.5 What the console records

| thing | where |
| --- | --- |
| every forget / prune / check / unlock outcome, exit code, parsed stats, error class | `runs` (`origin = 'console'`), visible in the device history (matrix: monitoring, run history) |
| next due times, deferral and failure counters, lease | `maintenance_schedule` |
| repository size, snapshot and blob counts over time | `usage_samples` (feeds quota and the "next prune" date) |
| manual maintenance actions, escrow reveals, key rotations | `audit_events` (path-free) |
| maintenance health | `job_state` rows -> C07; per-device MAINT_FAILED state -> digest and alerts |
| raw restic output | **not stored**; classes and counts only |

---

## 8. Security model

### 8.1 Trust boundaries

```
 customer network / internet                          operator host (Linux, one compose project)
+-------------------------------+   HTTPS :8443        +------------------------------------------------+
| Windows endpoint              |  device token ------>| cryo-console serve                              |
|  cryo-agent (SYSTEM)          |                      |   agent listener (public)   admin listener     |
|  restic.exe (job object)      |                      |   (no admin routes)          127.0.0.1 / mesh  |<--- operator browser
|  DPAPI secrets: device token, |   HTTPS :9443        |   holds: DB, master key      session + MFA     |     (mesh / VPN)
|  gateway pw, repo password    |  gateway login ----->|   NO S3 credential                             |
+-------------------------------+        |             +----+----------------------------+---------------+
   endpoint never holds the S3 key         v                  | htpasswd (rw)              | unix socket RPC
                                +----------------------+      v                            v
                                | rclone serve restic  |   +--------------+      +----------------------+
                                | --append-only        |   | gateway-auth |      | cryo-console maintain |
                                | --private-repos      |   | volume       |      | master key + S3 admin |
                                | S3 gateway credential|   +--------------+      | credential            |
                                +----------+-----------+                          +----------+-----------+
                                           |  S3 API                                         | restic rclone: backend
                                           v                                                 v (governance bypass here only)
                              +-------------------------------------------------------------------+
                              |  S3-compatible object storage (BAA-covered when PHI is in scope)  |
                              +-------------------------------------------------------------------+
```

Boundaries: (1) endpoint to internet: only outbound HTTPS to two ports, no inbound listener; (2) public listeners to console: authenticated per device, rate-limited, JSON-only, no admin handler present; (3) console to storage: the console process never holds the S3 credential (DH1); (4) gateway: append-only, per-device confinement, holds the gateway credential; (5) maintain: the only place with delete rights and the escrowed keys in use.

### 8.2 Secrets inventory

| secret | lives | protected by | rotation |
| --- | --- | --- | --- |
| console master key (32 bytes) | mounted file or environment of `console` and `maintenance`; **never** in the DB | file mode 0400, operator offline backup (printed/escrowed copy is the operator's duty); losing it loses every escrowed repo password | `master-key/rotate` (re-wraps every `*_wrapped` column; M13) |
| S3 gateway credential | `rclone.conf` in the `gateway` container | mounted secret, mode 0400 | provider rotation + gateway restart |
| S3 admin credential | `maintenance` container only | mounted secret | provider rotation + restart |
| device token | agent DPAPI store; sha256 in `device_tokens` | machine-scope DPAPI + ACL | `POST /token/rotate`, forced from the console |
| gateway password | agent DPAPI store; bcrypt in `gateway_logins`; plaintext wrapped only while `pending_secret_wrapped` | as above | rotate/revoke (7.2) |
| repo password (per device) | agent DPAPI store; wrapped copy in `repo_key_escrow` (AES-256-GCM, HKDF context `cryobackup/escrow/v1`) | master key | key rotation is could (M13): `restic key add/passwd/remove` from the admin path |
| enrollment token secret | the installer / RMM script (shown once); sha256 in `enrollment_tokens` | expiry + use cap + revocation | create a new token |
| admin password | argon2id hash | per-account lockout | self-service |
| TOTP secret | wrapped (context `cryobackup/totp/v1`) | master key | re-enroll |
| recovery codes, sessions, API tokens | sha256 / hash only | | regenerate / revoke |
| webhook / RMM secrets | `notification_rules.secret_wrapped` (context `cryobackup/notify/v1`) | master key | edit rule |
| restore selection | `restore_requests.selection_wrapped` (context `cryobackup/restore/v1`), purged | master key | purge job |
| SMTP password, OIDC client secret, Healthchecks URL | environment or mounted file named by configuration (`credential_ref`), `.example` templates committed, `.gitignore`d | | operator |
| code-signing key; release-manifest signing key | CI secret / HSM / offline; the *public* release key is embedded in the agent | | per signing policy |
| TLS private keys (agent listener, gateway) | mounted files | mode 0400 | cert renewal; expiry is monitored (C05) |

### 8.3 Threat table

| threat | impact | mitigation |
| --- | --- | --- |
| **Compromised endpoint** (admin on the box, malware as SYSTEM) | attacker holds that device's token, gateway login and repo password: can read that device's backups, push junk snapshots and fill its repo | per-device repo and key bound the blast radius to one device (engine §2.5, §2.7); endpoints never hold the S3 credential; append-only means history cannot be deleted or rewritten from the endpoint; retire/revoke is one action; `--keep-within` and the prune safety guard stop junk snapshots from evicting real history; growth-rate alarm (C15) |
| **Stolen device token** | attacker can call the agent API as that device (policy, heartbeat, runs, restores for that device) but cannot read repository data (no gateway credential) | tokens hashed at rest, rotation with grace, per-device rate limits, anomaly: a second fingerprint or session raises IDENTITY_CONFLICT; `/restores/{id}` only returns selections queued for that device |
| **Stolen gateway login** | attacker can read that device's repo ciphertext and append | restic encryption (no repo password = no plaintext); append-only; rotate the login; repo password is a separate secret |
| **Snapshot flood / lock deletion by a hostile endpoint** | fake snapshots push real ones out of `keep-*` windows; the endpoint can delete any lock in its own repo, including an admin's exclusive lock mid-prune (engine §3.2) | `--keep-within` always present; guard refuses mass removal and newest-snapshot removal; maintenance runs when the device is quiesced (`hold`), accepts the narrow residual race, and `check` runs after prune; provider versioning + lifecycle window as a backstop (matrix row r84) |
| **Compromised console host** | attacker obtains DB, master key (if on the same host) -> can unwrap every repo password; with the maintenance container also the S3 admin credential | accepted by design for an escrow product; mitigations: serve holds no S3 credential (DH1), master key not in the DB, host hardening is the deployment guide's first chapter, fresh-MFA + audit on reveal, immutable audit, optional customer-held custody (M13) for customers who cannot accept it, provider-side versioning/lock as the last line |
| **Lost master key** | every escrowed repo password is unrecoverable; devices keep working (they hold their own copy) but server-side retention, technician restore and verification stop | operator-held offline copy documented in the `init` wizard (it refuses to finish until the operator confirms a copy); the self-backup bundle excludes the key by design; `doctor` warns when no copy was confirmed; per-device passwords can be re-escrowed from a surviving endpoint (`cryo-agent` break-glass export, audited) |
| **Lost SQLite file** | registry, policies, history, acks, tokens gone; escrow gone | nightly self-backup with weekly tested restore (C11); restore from the bundle; agents re-appear through `enroll` with their existing device tokens revoked - documented disaster-recovery runbook; repositories are untouched |
| **Operator account compromise** | attacker acts as an admin: can queue commands, reveal keys, change policy, create tokens | argon2id + mandatory TOTP, WebAuthn (should), lockout, IP allowlist, session idle/absolute timeouts, fresh-MFA step-up for reveal/purge/master-key operations, audit with source IP, scoped API tokens, read-only role, customer-scoped roles, notification on critical configuration changes (M11) |
| **Rogue server at enroll (MITM)** | agent enrolls to an attacker and hands it repo material | SPKI fingerprint carried in the enrollment token (trust on first use), TLS >= 1.2, the installer is signed |
| **Malicious release / supply chain** | arbitrary code as SYSTEM on every endpoint | signed MSI and binaries, signed release manifests verified against an embedded public key, pinned and hash-checked restic and rclone, staged rings, SBOM, `govulncheck`; self-update off by default until M11 |
| **Webhook SSRF / header injection** | console makes requests into the operator's private network | destinations must resolve to public addresses unless an owner marks the rule `allow_private`; header values wrapped and validated |
| **Spreadsheet injection in exports** | formula execution when an operator opens a CSV/XLSX | cells starting with `= + - @` are prefixed with `'` on export |
| **Replay of agent requests** | duplicated runs/commands | idempotency keys (run id, command id), TLS, per-device nonce-free design relies on idempotency rather than replay windows |
| **DoS on the public listener** | fleet cannot check in | per-device limits, small bodies, connection caps; a listener that stops answering is detected by the agent endpoint self-probe (C04, ADR 0013); devices that cannot check in turn STALE/UNSEEN |

### 8.4 HIPAA path-free logging rule (design constraint)

CryoBackup carries HIPAA and NDA obligations: **no file contents and no path (or path fragment, directory name, user-profile name, file name) is ever logged, cached, metered, mailed, sent to a webhook, written to an audit row or included in a crash report.** Enforcement is structural, in layers:

1. **Parse boundary.** The raw restic JSON/stderr stream is first written verbatim to the local run log (path-bearing, local-only, DH15); then `internal/restic` converts restic's JSON and stderr into types that have no path field: `ErrorClass`, a count, and optionally `ext` (file extension, lower-cased, allow-listed) and `hmac8` (first 8 hex of `HMAC-SHA256(per-device salt, path)`). The `item` and free-text `message` fields are dropped at this boundary; they cannot reach any other package.
2. **`logx`.** All logging goes through a `slog.Handler` that (a) accepts only typed attributes (`logx.Count`, `logx.Class`, `logx.Hash`, `logx.ID`), (b) redacts any string value that looks like a path (drive letter, UNC, leading `/` with two or more segments, `%VAR%` forms) to `<path:hmac8>` as a safety net, and (c) unwraps `*fs.PathError`/`*os.LinkError` so `err.Error()` never prints the path. A fuzz test feeds Windows, UNC and POSIX paths through every attribute type and fails if any path text survives.
3. **Static analysis.** Analyzer `nopaths` forbids `fmt.Sprintf("%s", err)`-style logging of errors from the `os`/`io/fs`/`filepath` packages outside `logx`, and forbids direct `slog.*` calls outside `logx`.
4. **Wire types.** Agent-to-console types (`internal/api`) contain no path fields; restore selections are the single exception, travel only on the device-authenticated `GET /restores/{id}` response, are stored encrypted with a purge date, and are never logged.
5. **Metrics.** Label values are limited to ids, states and classes; no hostname, customer or path in a Prometheus label.
6. **Emails, digests, webhooks, reports** are rendered from `DeviceState`/`CustomerState`, which hold hostnames (permitted: the console's job) and classes only.
7. **Tests.** The e2e suite seeds file names containing patient-like strings, runs backups with induced errors and greps every log file, metric dump, e-mail body, webhook payload, audit row and journal export for them.
8. **Per-run detail on the device** (DH15) contains paths and never leaves it: not in `diagnose` bundles, not in uploads, not in the console. The logging boundary in `internal/agent/uplink` projects every error to `{during, class, ext, count}` and a test asserts no path separator survives the projection.

---

## 9. The parts that bite

- **time zones and DST:** backup windows and "run at 20:00" are device-local (Go `time` with the Windows zone); the SLA, grace and unseen clocks are server time (DH5); ten-day strips use the customer's zone and a DST day has 23 or 25 hours; the digest fires once per local date.
- **idempotency:** run ids, command ids, `enroll_nonce` and `Idempotency-Key` make every retry harmless; the journal is at-least-once by design, so the console must answer `duplicate` rather than error, and must reject a duplicate id that belongs to a *different* device (409).
- **races:** (a) a cloned VM or restored image presents the same device id and token - detected by hardware fingerprint / concurrent sessions (IDENTITY_CONFLICT); (b) two consoles writing `htpasswd` - single-node topology, generation header + lease detect it; (c) a backup arriving mid-prune - exit 11 is a deferral, `hold` reduces the window; (d) an endpoint deleting the admin's lock mid-prune - residual, guarded by `check` after prune.
- **clock skew:** device clocks are wrong more often than servers; run times are corrected by the heartbeat offset and clamped, skew over 2 min is an attribute, a majority-same-sign skew is a server-clock banner (C13); TLS fails if the device clock is far off (classified `tls`, deferred).
- **rate limits on the public listener:** per-IP limits would lock out a whole office behind one NAT, so they apply only to unauthenticated routes and failed authentications; authenticated limits are per device (§5.5).
- **large restores and artifact lifetime:** a multi-GB zip needs temp space on the admin host, a purge date and a single-use signed URL; the UI shows the size estimate before starting, one restore per device at a time, cancel supported; relay restores stream through the console (bounded concurrency).
- **SMTP deliverability:** SPF/DKIM/DMARC for the sender domain is the operator's job; the product detects only what it can (relay rejections, failed sends -> C03) - a "sent" that lands in spam is invisible, so the digest also exists on the board and via the status API.
- **SQLite single-writer discipline:** one write connection per process, `BEGIN IMMEDIATE`, transactions measured in milliseconds, no long reads inside write transactions, batches of 5 000 for deletes; two processes (serve, maintain) share the file under WAL with `busy_timeout`; `VACUUM INTO` for backups, never a file copy of a live WAL database.
- **Modern Standby:** the resume event can arrive minutes late or the machine may never fully wake; run-on-wake is best effort, the SLA clock keeps running, and a lid-closed backup is not promised (engine §5.1).
- **VSS fallback classification:** a per-volume VSS failure makes restic read the live filesystem and exit 3 (engine §1.1); the console distinguishes "locked files under tolerance" from "a configured root's volume had no snapshot" and never lets the latter count as success.
- **prune economics on minimum-storage providers:** every early delete is billed (Wasabi 90 days), repack rewrites new packs with new clocks; hence quarterly prune, high `--max-unused`, `--repack-cacheable-only`; lock files churn is a rounding error but not free (engine §4.2).
- **lock litter:** crashed clients leave locks that go stale after 30 minutes; governance-mode Object Lock makes locks undeletable for the client gateway, so stale locks accumulate until the admin path bypasses them (lab test L-GOV-1).
- **retire / delete semantics:** retire revokes credentials and stops schedules but **keeps data until retention expires**; the repository is purged only by an owner action with fresh MFA after the retention window (the GDPR-style purge path: `DELETE /devices/{id}` and `DELETE /customers/{id}`), which also removes escrow, tokens and cached snapshots; backups in provider-side versioning/lock windows outlive the purge until their own expiry, and the UI says so.
- **expected-customer alarm vs reality:** "zero active devices" is judged on non-retired devices; a customer that legitimately retired everything must be marked not expected first (the archive constraint forces the decision).
- **journal growth on a long outage:** bounded (50 000 undelivered rows) with an overflow class reported on return; a month-long offline laptop does not fill the disk.
- **hostname vs identity:** hostnames are labels only; identity is the device id (renames, re-images and duplicate names are normal).

---

## 10. Build order

Full-featured scope means the plan runs through *could*; it does not stop at must. Milestones are **vertical slices** ordered must, then should, then could (a few rows are pulled forward because a must depends on them, §10.3). Each milestone ends with a demonstrable build and an acceptance test; the state engine and the schema constraints are tested before any UI depends on them; UI work starts in fixtures mode; anything the engine sheet marks UNCERTAIN is a named lab test (§12.2) that gates the milestone that relies on it. **All durations are estimates** in engineer-weeks for one full-time engineer who knows Go, Windows services and restic; the recon sized the whole as XL (agent L, gateway M, console L) and these numbers are consistent with that, not a commitment. "Delivered" means the milestone *completes* the feature row (earlier milestones may have started it).

### 10.1 Milestone summary

| milestone | name | rows delivered (must / should / could) | routes (agent / admin / UI) | estimate (weeks) | marker |
| --- | --- | --- | --- | --- | --- |
| **M0** | Vertical slice | 14 (14 / 0 / 0) | 4 / 8 / 9 | 3 |  |
| **M1** | Windows agent hardening and MSI v1 | 29 (29 / 0 / 0) | 1 / 0 / 0 | 6 |  |
| **M2** | Console core: customers, devices, policy, enrollment, accounts, audit | 26 (25 / 1 / 0) | 2 / 51 / 22 | 5 |  |
| **M3** | State engine, monitoring and the fleet board | 20 (16 / 3 / 1) | 0 / 24 / 14 | 4 |  |
| **M4** | Alerting | 9 (9 / 0 / 0) | 0 / 12 / 4 | 3 |  |
| **M5** | Gateway, server-side maintenance and keys | 20 (17 / 3 / 0) | 1 / 21 / 7 | 5 |  |
| **M6** | Restore | 7 (7 / 0 / 0) | 2 / 5 / 1 | 3 | **PILOT-ABLE** |
| **M7** | Must-have close-out | 4 (4 / 0 / 0) | 0 / 3 / 0 | 3 |  |
| **M8** | Should: scheduling, conditions, selection and agent extras | 18 (0 / 18 / 0) | 0 / 0 / 0 | 4 |  |
| **M9** | Should: policy, retention, storage and restore extras | 16 (0 / 16 / 0) | 3 / 10 / 3 | 5 |  |
| **M10** | Should: security, console and tenancy extras | 13 (0 / 13 / 0) | 0 / 15 / 2 | 4 |  |
| **M11** | Should: alerting, reporting and deployment | 18 (0 / 18 / 0) | 2 / 16 / 2 | 5 |  |
| **M12** | Could: scheduling, selection and agent | 7 (0 / 0 / 7) | 0 / 0 / 0 | 4 |  |
| **M13** | Could: storage, retention, restore and security | 15 (0 / 0 / 15) | 0 / 1 / 0 | 5 |  |
| **M14** | Could: console, deployment, tenancy, API and reporting | 16 (0 / 0 / 16) | 0 / 0 / 2 | 5 |  |
| **M15** | Could: non-Windows and non-device sources (gated by a new ADR) | 5 (0 / 0 / 5) | 0 / 0 / 0 | 2 |  |
| | **total** | 237 (121 / 72 / 44) | 15 / 166 / 66 | 66 | |

Cumulative estimate: **29 weeks to PILOT-ABLE (M0-M6)**, 32 to every must row (M0-M7), 50 to every should row (M0-M11), 66 through the could rows and the M15 spike. Routes are counted from the three tables in §5 (agent / admin / UI) in the milestone where each route **first ships** (the `from` column), even if it ships rough: M0 ships rough versions of `GET /devices`, `GET /devices/{id}`, technician restore and a password-only login, which M2, M3 and M6 complete. Feature rows and screens are counted where they are **completed**.

**PILOT-ABLE = M6.** After M6 a 30-day pilot on one real customer can start: devices enroll with no per-device click, laptops catch up and run on wake, silence is an alarm (banners, UNSEEN/STALE/MISSING, digest, e-mail), retention and integrity run server-side with deferral, keys are escrowed and verified, the console can be restored, and a file can be restored with the device on, off or dead. What is **not** there yet and the pilot must tolerate (all in M7, running in parallel): bulk actions, the uninstall-and-scrub script, bulk policy assignment, complete OpenAPI. The pilot's exit criteria are written into the M6 acceptance test so the pilot cannot be declared a success by default.

### 10.2 Milestone details

#### M0 - Vertical slice

**Goal.** One device end to end, ugly, proving every tier: enroll, policy, backup, heartbeat, green, restore.

**Scope.**
- repository skeleton, CI (Linux + Windows jobs), `cryo-console init|serve --fixtures`, migration 0001 = `schema.sql`, two listeners with disjoint muxes
- agent: console-mode run plus a minimal service (no power events yet), fixed-interval scheduler, `backup --json` supervisor, simple journal, heartbeat, run upload, default-policy pull
- gateway: compose stack with htpasswd sync and the reserved probe login; `maintain` v0 over the unix socket: `init`, `cat config`, single-level `ls`, `dump -a zip`
- console: enroll (token minted by `cryo-console token`), device list/detail, plain fleet table, technician restore of one file as a zip; state engine v0 = ported reference cases A01-A23 + PENDING_FIRST
- security spine: path-free `logx`, vendored assets only, loopback-bound admin listener, escrow wrap/unwrap (verify and reveal come in M5)

- **Screens completed:** rough versions of S01 (password only), B01 (flat table), B07 (summary), B17 (single file) - completed in M2, M3, M2 and M6; A01 is a manual zip install, completed in M1
- **Tables first written / fully used:** schema_migrations, settings, storage_targets, policies, customers, admin_users, sessions, enrollment_tokens, devices, device_tokens, device_presence, heartbeats, gateway_logins, repo_key_escrow, runs, audit_events, job_state (all 34 tables exist from M0, DH20)
- **Routes:** 4 agent, 8 admin, 9 UI (rows with `from` = M0 in §5.1-5.3)
- **Jobs:** htpasswd-sync, heartbeat-ring-trim
- **Feature rows delivered:** 14 (14 must, 0 should, 0 could) - by area: enrollment 1, policy 1, storage 6, security 5, m365-gws 1; the rows are listed in §11
- **Acceptance test:**
  - scripted lab e2e: `docker compose up`; `cryo-console init`; create a customer and a token; install the agent on a Windows VM with the token - the device appears as PENDING_FIRST with no further click
  - the agent backs up a 100 MB test tree through the gateway to MinIO; the board turns green without operator action
  - stopping the agent for longer than the unseen threshold turns the row red; `restic forget` with the device credentials against the gateway fails (**L-APPEND-1** first pass)
  - technician restore of one file as a zip with the device powered off: sha256 equals the original
  - `go test ./internal/state` passes the ported reference cases A01-A23; fixtures mode renders the board without a network
- **Estimate:** 3 engineer-weeks *(estimate)*

#### M1 - Windows agent hardening and MSI v1

**Goal.** The agent survives real laptops: sleep, reboot, flaky networks, VSS, upgrades.

**Scope.**
- service host with Stop/Shutdown/PreShutdown/PowerEvent/SessionChange; run-on-wake, run-on-start, catch-up collapse, windows in device-local time, jitter, backoff, skip-if-running
- supervisor: job object (kill-on-close, memory cap), cache/temp under ProgramData, VSS options, `--exclude-cloud-files`, exit-code mapping + stderr classifier + path-free parse boundary
- journal v1: at-least-once, abandoned/interrupted recovery, offline resilience; DPAPI secrets; log rotation; pinned and hash-checked restic
- WiX MSI (fixed UpgradeCode, silent `/qn TOKEN= SERVER=`, ServiceInstall/ServiceControl, in-place upgrade) and the Authenticode signing step

- **Screens completed:** A01
- **Tables first written / fully used:** runs (full ingest + idempotency), snapshots (cache upsert) (all 34 tables exist from M0, DH20)
- **Routes:** 1 agent, 0 admin, 0 UI (rows with `from` = M1 in §5.1-5.3)
- **Jobs:** -
- **Feature rows delivered:** 29 (29 must, 0 should, 0 could) - by area: enrollment 1, scheduling 10, selection 2, monitoring 1, agent 12, deployment 3; the rows are listed in §11
- **Acceptance test:**
  - laptop matrix on one physical laptop and one Modern Standby device: suspend/resume triggers a due check (**L-WAKE-1** result recorded, best effort); gateway down mid-run gives `deferred`, never `failed`, and resumes via index checkpoints
  - reboot mid-run: next start records `interrupted`/`abandoned` and completes; a 7-day offline simulation yields one catch-up run
  - MSI silent install, upgrade and uninstall run twice idempotently; binaries and MSI verify as signed; **L-VSS-1** (LocalSystem VSS), **L-CANCEL-1** (graceful cancel), **L-JUNCTION-1** results recorded
  - path-free fuzz test passes; duplicate-delivery test (same run id twice) yields one row
- **Estimate:** 6 engineer-weeks *(estimate)*

#### M2 - Console core: customers, devices, policy, enrollment, accounts, audit

**Goal.** An operator can run the fleet: customers, devices, policies with blast radius, zero-click enrollment, commands, MFA, roles and an audit trail.

**Scope.**
- store layer with `Scope`; auth (argon2id, TOTP, recovery codes, sessions, CSRF, API tokens) and roles; customers CRUD (expected / onboarding stored)
- device table + panel (summary, history, settings, audit tabs), action menu, command queue + back up now / cancel (long-poll), per-device override, effective-policy view, identity-conflict detection and resolve
- policy editor (schedule + window editor, conditions defaults, selection tree + presets + published exclusion baseline, SLA fields), blast-radius preview
- enrollment wizard + token CRUD + RMM script / MSI-parameter generator; audit viewer; settings; display timezone; component library with empty/loading/error states; deep-linkable URLs

- **Screens completed:** B07, B08, B09, B16, S01, S03, S04, S05, S06, S07, S08, S11, S12, S17, S19, S24, S25, S26, S30, S32, S38
- **Tables first written / fully used:** customers, policies, device_policy_overrides, enrollment_tokens, commands, api_tokens, admin_recovery_codes, audit_events (full use) (all 34 tables exist from M0, DH20)
- **Routes:** 2 agent, 51 admin, 22 UI (rows with `from` = M2 in §5.1-5.3)
- **Jobs:** command-expiry, session-cleanup
- **Feature rows delivered:** 26 (25 must, 1 should, 0 could) - by area: enrollment 3, policy 3, scheduling 1, selection 4, security 4, monitoring 2, console 7, deployment 1, tenancy 1; the rows are listed in §11
- **Acceptance test:**
  - the generated scope-matrix test passes (no store method leaks another customer's rows)
  - login cannot complete without MFA; every mutation produces an audit row
  - three VMs enroll from one generated script with zero clicks per device; `backup now` reaches an online device within 30 s
  - saving a policy shows the device count and changed fields first; an override shows per-field source labels; OpenAPI diff clean for all M0-M2 routes; screenshot set renders in fixtures mode
- **Estimate:** 5 engineer-weeks *(estimate)*

#### M3 - State engine, monitoring and the fleet board

**Goal.** Silence becomes visible: the full precedence table, banners, MISSING, rings, ten-day strips and the system-health page.

**Scope.**
- `internal/state` complete (all of §4.2-4.6) with the full test list §4.8; `health-prober`, `abandoned-run-sweeper`, evaluator loop (no notifications yet)
- B01 board (exception-first, customer groups, MISSING, freshness stamp), B03 rings, B02/S23 strips, B04/B05, S21/S22 run pages, saved views, device state explain tooltip
- pause/resume (B15), clock offset + skew, partial tolerance, first-backup grace, server-judged missed run, system-health page, `/status` and `/metrics`, SSE ticker

- **Screens completed:** B01, B02, B03, B04, B05, B15, S02, S16, S21, S22, S23, S36
- **Tables first written / fully used:** alarm-related reads on runs/device_presence; saved_views; job_state (probe rows) (all 34 tables exist from M0, DH20)
- **Routes:** 0 agent, 24 admin, 14 UI (rows with `from` = M3 in §5.1-5.3)
- **Jobs:** health-prober, abandoned-run-sweeper, healthcheck-ping (+ evaluator loop)
- **Feature rows delivered:** 20 (16 must, 3 should, 1 could) - by area: scheduling 1, monitoring 15, console 2, api 2; the rows are listed in §11
- **Acceptance test:**
  - all of §4.8 (groups A-E) passes before the UI is wired; fixtures mode renders every state, every banner and a MISSING customer
  - kill the gateway container: banner C01 within 3 minutes and `AllClear()` stays false; stop all heartbeats: devices STALE/UNSEEN and no console banner, then stop the agent listener or break its certificate: C04 within about 5 to 8 minutes (ADR 0013, superseding "stop all heartbeats: C04"); inject a facts-load failure: devices UNKNOWN + C09 (never green)
  - lab dogfood on the operator's own test machines (not a customer pilot) for 7 days with induced outages: every one produces a visible alarm
- **Estimate:** 4 engineer-weeks *(estimate)*

#### M4 - Alerting

**Goal.** Alarms reach a human and resolve themselves: digest, immediate failure, silence, unseen and MISSING alerts, ack/snooze, SMTP health.

**Scope.**
- SMTP (STARTTLS/TLS) + test button; notification rules with global > customer > device inheritance; `alarm-evaluator` notifications with episode-keyed dedupe
- daily digest (exceptions first, banner state first), immediate failure, silence, unseen, expected-customer-missing alerts, auto-resolve with recovery notice, acknowledge/snooze, delivery log, `notification-retry`

- **Screens completed:** S18, S27
- **Tables first written / fully used:** notification_rules, notification_log, alarm_acks (full use) (all 34 tables exist from M0, DH20)
- **Routes:** 0 agent, 12 admin, 4 UI (rows with `from` = M4 in §5.1-5.3)
- **Jobs:** alarm-evaluator (notifying), daily-digest, notification-retry
- **Feature rows delivered:** 9 (9 must, 0 should, 0 could) - by area: alerting 9; the rows are listed in §11
- **Acceptance test:**
  - digest sent once per local day, lists MISSING, UNKNOWN, unseen, blank, failed, stale, parked; resend without `force` is a no-op
  - induced failure -> one e-mail; recovery -> one recovery notice; ack mutes but the row stays red; snooze expiry re-raises
  - break SMTP: C03 within 3 failed sends; stop the evaluator: C06 and the outbound ping stops
- **Estimate:** 3 engineer-weeks *(estimate)*

#### M5 - Gateway, server-side maintenance and keys

**Goal.** Retention and integrity run unattended through the admin path; keys are escrowed, verifiable and revealable only with MFA; the console can be restored.

**Scope.**
- full htpasswd lifecycle (rotate, revoke, reconcile), retire device, device-token rotation, storage-target pages and usage views
- `maintain`: forget (dry-run guard, `--keep-within`) / prune / check / unlock loop with deferral on exit 11, usage sampler, escrow verifier
- escrow health (B10), reveal with fresh MFA, master-key status, console self-backup with tested restore, run/audit retention jobs, compose reference + gateway deployment recipe

- **Screens completed:** B10, B11, S13, S15, S33, S35
- **Tables first written / fully used:** gateway_logins (rotation), repo_key_escrow (verify), maintenance_schedule, usage_samples, storage_targets (full use) (all 34 tables exist from M0, DH20)
- **Routes:** 1 agent, 21 admin, 7 UI (rows with `from` = M5 in §5.1-5.3)
- **Jobs:** maintenance-scheduler, usage-sampler, escrow-verifier, db-self-backup, run-history-retention, audit-retention
- **Feature rows delivered:** 20 (17 must, 3 should, 0 could) - by area: enrollment 1, retention 8, storage 3, security 2, monitoring 3, console 2, deployment 1; the rows are listed in §11
- **Acceptance test:**
  - **L-APPEND-1** passes against the deployed stack; 14 simulated days of forget/prune/check on 5 test repos with injected exit 11 (deferral, not failure) and a snapshot flood (guard refuses, alarm raised)
  - rotate and revoke a gateway login during a live backup: one failed request, then recovery; retire keeps data
  - reveal needs fresh MFA, is audited and shows the key once; escrow verify turns ESCROW_UNVERIFIED green; the self-backup is restored into a clean directory by the weekly verify
  - a stranger-following-the-README timing test brings the compose stack up from nothing in under 30 minutes (target, measured)
- **Estimate:** 5 engineer-weeks *(estimate)*

#### M6 - Restore - PILOT-ABLE

**Goal.** Restore works with the device on, off or dead; the first build a real customer can be piloted on.

**Scope.**
- snapshot cache, tree browse, point-in-time selection, restore to original or alternate path, overwrite policy, restore on the device from the console, technician download (zip/tar), relay restore to a replacement device, dead-device restore
- artifact lifetime (single-use signed URL, purge), per-customer browse permission audited, restore progress

- **Screens completed:** B17, S09
- **Tables first written / fully used:** restore_requests, snapshots (full use) (all 34 tables exist from M0, DH20)
- **Routes:** 2 agent, 5 admin, 1 UI (rows with `from` = M6 in §5.1-5.3)
- **Jobs:** restore-artifact-purge, snapshot-cache-refresh
- **Feature rows delivered:** 7 (7 must, 0 should, 0 could) - by area: restore 7; the rows are listed in §11
- **Acceptance test:**
  - five clicks or fewer to a downloaded file with the device powered off (F22); restore on the device with each overwrite mode; relay restore onto a replacement (F08); sha256 comparison of restored data on three sample sets including ACLs and long paths; artifact purge verified
  - **pilot exit criteria are written down before the pilot starts:** every induced outage produced an alarm (zero silent misses); at least 95 % of scheduled runs within SLA excluding device-off time; restore drill passed; console self-backup restore passed; no path found in any log, metric, e-mail or audit row
- **Estimate:** 3 engineer-weeks *(estimate)* - **PILOT-ABLE: a 30-day pilot on one real customer can start after this milestone.**

#### M7 - Must-have close-out

**Goal.** Every remaining must row, run in parallel with the pilot.

**Scope.**
- bulk actions on a selection (policy assign, move, pause, update agent) and bulk policy assignment; uninstall-and-scrub script that also revokes server-side
- OpenAPI complete and diffed in CI: a generated check proves every UI action has an API route

- **Screens completed:** none new
- **Tables first written / fully used:** - (all 34 tables exist from M0, DH20)
- **Routes:** 0 agent, 3 admin, 0 UI (rows with `from` = M7 in §5.1-5.3)
- **Jobs:** -
- **Feature rows delivered:** 4 (4 must, 0 should, 0 could) - by area: policy 1, console 1, deployment 1, api 1; the rows are listed in §11
- **Acceptance test:**
  - generated UI-action-to-route coverage check passes; bulk action results are per item; the uninstall script is dry-run by default, idempotent and revokes the device (verified on a VM)
- **Estimate:** 3 engineer-weeks *(estimate)*

#### M8 - Should: scheduling, conditions, selection and agent extras

**Goal.** Laptop-correct behaviour and the agent conveniences.

**Scope.**
- fixed-time schedules, throttle windows (child restarted at boundaries), start-condition framework with run-anyway-after-N, battery floor, metered deferral, run-time limit + long-run alarm, prevent sleep (best effort), resumable large first backup window
- include filters, exclusion presets, per-path flags, Outlook OST/PST handling; agent-local permissions
- tray (A14/B14), local CLI, resource caps, Event Log source, proxy support

- **Screens completed:** A02, A08, A09, A14, A15, B12, B14
- **Tables first written / fully used:** - (all 34 tables exist from M0, DH20)
- **Routes:** 0 agent, 0 admin, 0 UI (rows with `from` = M8 in §5.1-5.3)
- **Jobs:** -
- **Feature rows delivered:** 18 (0 must, 18 should, 0 could) - by area: enrollment 1, policy 1, scheduling 7, selection 4, agent 5; the rows are listed in §11
- **Acceptance test:**
  - laptop matrix 2: battery floor delays but run-anyway fires; metered link defers without a failed run; a throttle window boundary restarts the child without data loss; tray colour follows state; Event Log lines visible to an RMM monitor
- **Estimate:** 4 engineer-weeks *(estimate)*

#### M9 - Should: policy, retention, storage and restore extras

**Goal.** Policy at scale and safer, cheaper retention.

**Scope.**
- policy templates by device class, versioning + drift, mandatory exclusions; retention editor with calendar preview (B13); soft quota with threshold alarms; bucket versioning/lifecycle guidance; stale-lock cleanup and repair UI
- Object Lock governance (only after **L-GOV-1**); lobby enrollment; replace device (F21); restore search, version history, verify/simulate, live progress and cancel; live run progress via SSE

- **Screens completed:** B13
- **Tables first written / fully used:** policy_revisions, enrollment_requests (full use) (all 34 tables exist from M0, DH20)
- **Routes:** 3 agent, 10 admin, 3 UI (rows with `from` = M9 in §5.1-5.3)
- **Jobs:** lobby-expiry
- **Feature rows delivered:** 16 (0 must, 16 should, 0 could) - by area: enrollment 2, policy 3, retention 1, storage 4, security 1, restore 4, monitoring 1; the rows are listed in §11
- **Acceptance test:**
  - policy preview matches `forget --dry-run --json` on fixtures; replace-device keeps the incremental history (first run after replacement is incremental); lobby approval enrolls without a token; L-GOV-1 report attached before the governance toggle is enabled
- **Estimate:** 5 engineer-weeks *(estimate)*

#### M10 - Should: security, console and tenancy extras

**Goal.** Enterprise-grade access control and lifecycle.

**Scope.**
- WebAuthn, OIDC (LDAP optional), granular permission flags, session/lockout/IP-allowlist hardening, per-customer browse-names toggle
- move device between customers, global search, query builder, console upgrade path with pre-upgrade backup + rollback, customer-scoped roles, per-customer timezone/locale, suspend, archive, delete with retention-respecting purge

- **Screens completed:** none new
- **Tables first written / fully used:** admin_webauthn_credentials (full use) (all 34 tables exist from M0, DH20)
- **Routes:** 0 agent, 15 admin, 2 UI (rows with `from` = M10 in §5.1-5.3)
- **Jobs:** -
- **Feature rows delivered:** 13 (0 must, 13 should, 0 could) - by area: security 5, console 4, tenancy 4; the rows are listed in §11
- **Acceptance test:**
  - scope matrix extended to scoped roles passes; archive refuses an expected customer; purge requires fresh MFA + double confirm and respects retention; upgrade rolls back from a failed migration
- **Estimate:** 4 engineer-weeks *(estimate)*

#### M11 - Should: alerting, reporting and deployment

**Goal.** Integrations, reports and managed updates.

**Scope.**
- consecutive-failure threshold, nag/recovery/transition dedupe, webhooks, Syncro adapter, critical-configuration-change notification, operational threshold alerts
- per-customer status report (HTML/PDF), CSV/XLSX export, scheduled reports, storage and compliance reports, calendar matrix (B06)
- self-update with signature check and rollback, release channels + force-update + update window (S20), pre-bound installer page, GPO/Intune guide, CSV import, policy notification overrides

- **Screens completed:** B06, S20, S34
- **Tables first written / fully used:** reports, agent_releases (full use) (all 34 tables exist from M0, DH20)
- **Routes:** 2 agent, 16 admin, 2 UI (rows with `from` = M11 in §5.1-5.3)
- **Jobs:** release-check, report-scheduler
- **Feature rows delivered:** 18 (0 must, 18 should, 0 could) - by area: policy 1, monitoring 1, alerting 6, reporting 5, agent 1, deployment 3, api 1; the rows are listed in §11
- **Acceptance test:**
  - adapter raises and clears an alert on a test instance; webhook signature verifies; staged ring rollout updates the pilot ring first and rolls back a deliberately broken build; reports render identical numbers to the board
- **Estimate:** 5 engineer-weeks *(estimate)*

#### M12 - Could: scheduling, selection and agent

**Goal.** Edge scheduling and selection features.

**Scope.**
- idle vs in-use throttle split, wake timers, pre/post run hooks, event triggers + post-run power action, sub-hourly interval with `--skip-if-unchanged --no-scan`, network-share backup with stored credentials, EFS-encrypted files

- **Screens completed:** none new
- **Tables first written / fully used:** - (all 34 tables exist from M0, DH20)
- **Routes:** 0 agent, 0 admin, 0 UI (rows with `from` = M12 in §5.1-5.3)
- **Jobs:** -
- **Feature rows delivered:** 7 (0 must, 0 should, 7 could) - by area: scheduling 5, selection 2; the rows are listed in §11
- **Acceptance test:**
  - hooks cannot run a user-supplied command unless the policy permits it and the action is audited; sub-hourly schedule shows scan cost on a 500 k-file profile (measured, recorded)
- **Estimate:** 4 engineer-weeks *(estimate)*

#### M13 - Could: storage, retention, restore and security

**Goal.** Advanced retention, copies and key custody.

**Scope.**
- pre-seed first backup, delete single snapshot (audited), keep-tag exemption, min/max counts, cold-tier archive via `restic copy`, hard quota, other targets, secondary copies / backup cache
- customer-held key custody + two-person reveal gate (enables the DH22 path), repo key rotation, master-key rotation, SPKI pinning / mTLS, agent tamper resistance
- NTFS-permission restore toggle, restore to cloud/remote location, automated restore test

- **Screens completed:** none new
- **Tables first written / fully used:** - (all 34 tables exist from M0, DH20)
- **Routes:** 0 agent, 1 admin, 0 UI (rows with `from` = M13 in §5.1-5.3)
- **Jobs:** -
- **Feature rows delivered:** 15 (0 must, 0 should, 15 could) - by area: enrollment 1, retention 4, storage 3, security 4, restore 3; the rows are listed in §11
- **Acceptance test:**
  - customer-held enrollment shows the consequence text and disables server-side retention for that device; master-key rotation re-wraps every wrapped column and verification still passes; automated restore test fails loudly when a sample cannot be restored
- **Estimate:** 5 engineer-weeks *(estimate)*

#### M14 - Could: console, deployment, tenancy, API and reporting

**Goal.** Self-service and polish.

**Scope.**
- self-service restore, customer read-only portal and view-as-customer, white-label, localization, device news, local end-user UI (A03/A10)
- bulk upgrade campaigns with device filters, import from other products, customer > site and reseller hierarchy, tags, per-customer BYO storage UI, other PSA/RMM adapters, report variants + inventory + external recipients, CLI / PowerShell module / read-only MCP server

- **Screens completed:** A03, A10, S28, S37, S39
- **Tables first written / fully used:** - (all 34 tables exist from M0, DH20)
- **Routes:** 0 agent, 0 admin, 2 UI (rows with `from` = M14 in §5.1-5.3)
- **Jobs:** -
- **Feature rows delivered:** 16 (0 must, 0 should, 16 could) - by area: restore 1, alerting 1, reporting 3, console 4, agent 1, deployment 2, tenancy 3, api 1; the rows are listed in §11
- **Acceptance test:**
  - portal user sees only their customer (scope matrix); campaign shows per-device status; PowerShell module covers the status API
- **Estimate:** 5 engineer-weeks *(estimate)*

#### M15 - Could: non-Windows and non-device sources (gated by a new ADR)

**Goal.** Spike only: decide the engines for Microsoft 365 / Google Workspace and Linux/macOS agents.

**Scope.**
- ADR for the SaaS-backup engine (restic cannot back up tenants); data-model check (`source_kind`, `source_id` already present); Linux/macOS agent feasibility; module rows are **planned, not scheduled**

- **Screens completed:** none new
- **Tables first written / fully used:** - (all 34 tables exist from M0, DH20)
- **Routes:** 0 agent, 0 admin, 0 UI (rows with `from` = M15 in §5.1-5.3)
- **Jobs:** -
- **Feature rows delivered:** 5 (0 must, 0 should, 5 could) - by area: agent 1, m365-gws 4; the rows are listed in §11
- **Acceptance test:**
  - an accepted ADR per module, or an explicit decision to drop the row
- **Estimate:** 2 engineer-weeks *(estimate)*


### 10.3 Rows delivered ahead of their priority tier

| feature row | priority | delivered in | reason |
| --- | --- | --- | --- |
| r40 Console-owned time-boxed pause (1/2/4/8/24 h or indefinite) with reason, visible as a state | should | M3 | pause is a state-engine input (PAUSED) and a DB constraint |
| r69 Prune safety guard: refuse when forget would remove more than X percent or the last snapshot | should | M5 | the destructive forget path must ship guarded |
| r85 Repo integrity check on schedule (restic check, optional --read-data-subset) | should | M5 | check is part of the D8 maintenance loop |
| r152 Partial-failure tolerance (failed files percent or count before a run counts as failed) | should | M3 | partial tolerance is part of the state table |
| r153 Clock-skew detection between device and server | should | M3 | clock offset is needed for server-time run ingest |
| r156 Repo integrity status from the last scheduled check | should | M5 | check result feeds the MAINT_FAILED state |
| r195 Deep-linkable URLs for devices, customers and runs | should | M2 | server-rendered pages are deep-linkable by construction |
| r251 Prometheus /metrics endpoint | could | M3 | /metrics is part of ADR D13 observability |

### 10.4 Screen map (all 71 recon screens)

Disposition of every recon screen. "Built" screens are listed with the milestone that completes them; "not built" screens are Comet concepts with no CryoBackup counterpart (per-device protected items, endpoint-held storage vaults, tenants, desktop main window) and the reason is given.

| ID | screen | disposition | milestone |
| --- | --- | --- | --- |
| S01 | Console login | built (basic in M0) | M2 |
| S02 | Home / dashboard | merged into B01 | M3 |
| S03 | Users list | built as Customers list | M2 |
| S04 | Add User dialog | built as Add customer | M2 |
| S05 | User profile: Profile | merged into B08 customer page | M2 |
| S06 | User profile: Devices tab | built (customer devices tab) | M2 |
| S07 | Device action dialog | built | M2 |
| S08 | Run Backup dialog | built (back up now / cancel) | M2 |
| S09 | Remote Restore dialog | merged into B17 | M6 |
| S10 | Protected Items tab | not built: no per-device protected items; selection lives in policy + override | - |
| S11 | Protected Item editor | merged into S25 (selection section) | M2 |
| S12 | Schedule editor | merged into S25 (schedule section + window editor) | M2 |
| S13 | Retention editor | built inside S25; preview in B13 | M5 |
| S14 | Storage Vaults tab | not built: one repo per device (see B07 keys/usage) | - |
| S15 | Storage Vault editor | built as storage-target editor | M5 |
| S16 | Job Logs tab | built (customer runs tab) | M3 |
| S17 | Policy tab | built (customer policy tab) | M2 |
| S18 | Reporting tab | built (customer notification overrides) | M4 |
| S19 | Devices page (all) | built | M2 |
| S20 | Bulk Device Upgrade | built as Releases + rollout (campaign filters in M14) | M11 |
| S21 | Job logs (history) | built as Runs | M3 |
| S22 | Job detail / log viewer | built (path-free) | M3 |
| S23 | Recent activity | built as ten-day activity grid | M3 |
| S24 | User Policies list | built | M2 |
| S25 | Policy editor | built | M2 |
| S26 | Settings index | built | M2 |
| S27 | Settings: Email Reporting | built | M4 |
| S28 | Settings: System Branding | deferred (white-label is could) | M14 |
| S29 | Agent branding + codesigning | not built in the console: signing is a CI step; branding is could | - |
| S30 | Download backup agent | built (pre-bound page in M11) | M2 |
| S31 | Settings: Tenants | not built: single operator; hierarchy is could | - |
| S32 | Settings: Admin accounts | built | M2 |
| S33 | Storage Templates / Gateway / Buckets | built as Storage and usage + targets | M5 |
| S34 | Settings: Integrations | built (Syncro adapter, webhooks) | M11 |
| S35 | Webhooks / Audit / Access / Self-backup | split: audit M2, self-backup M5, access M10, webhooks M11 | M5 |
| S36 | System Logs / About / Replication | built as System health (no raw logs; replication not built) | M3 |
| S37 | Device News / bulletin | deferred (could) | M14 |
| S38 | My Account (admin) | built | M2 |
| S39 | Customer Web Portal | deferred (could) | M14 |
| A01 | Installer | built (MSI) | M1 |
| A02 | Login / registration | built as CLI enroll + lobby poll (silent enroll in M0/M1) | M8 |
| A03 | Main window: Backup tab | not built in v1: agent is policy-driven; local UI is could | M14 |
| A04 | Add/Edit Protected Item wizard | not built: policy-driven | - |
| A05 | Choose files dialog | not built in the agent: tree picker lives in the console (S25) | - |
| A06 | Schedule editor (agent) | not built: console policy | - |
| A07 | Retention editor (agent) | not built: console policy | - |
| A08 | Backup Now dialog | built as tray action | M8 |
| A09 | Job logs / progress view | built as tray status + CLI; full local UI in M14 | M8 |
| A10 | Restore wizard (agent) | local restore UI is could; console restore is B17 | M14 |
| A11 | Account tab | not built: no vaults or accounts on the endpoint | - |
| A12 | Add Storage Vault dialog | not built: endpoints hold no storage credentials | - |
| A13 | Account dialog | not built | - |
| A14 | Tray icon + exit menu | built | M8 |
| A15 | Throttle / runtime settings | built as policy section (S25) | M8 |
| B01 | Fleet health board | built (flat table in M0) | M3 |
| B02 | Ten-day strip view | built | M3 |
| B03 | Backup-age ring dashboard | built | M3 |
| B04 | Overdue list | built | M3 |
| B05 | Devices-not-seen alarm page | built | M3 |
| B06 | Calendar-matrix history | built | M11 |
| B07 | Device detail panel | built (tabs arrive M2, M3, M5, M6) | M2 |
| B08 | Customer page | built | M2 |
| B09 | Audit log viewer | built | M2 |
| B10 | Key custody and recovery | built | M5 |
| B11 | Storage usage and quota | built (usage M5, quota M9) | M5 |
| B12 | Laptop start-conditions panel | built inside S25 (defaults M2, full framework M8) | M8 |
| B13 | Retention editor with calendar preview | built | M9 |
| B14 | Agent tray status | built | M8 |
| B15 | Pause / maintenance dialog | built | M3 |
| B16 | Enrollment wizard and script generator | built | M2 |
| B17 | Technician restore | built (ugly single-file version in M0) | M6 |

60 of 71 screens are built (some merged into another screen), 11 are not built because they have no CryoBackup counterpart.
## 11. Feature coverage appendix

Generated programmatically from `replica/features.csv` (258 rows: 121 must, 72 should, 44 could, 21 skip). Every non-skip row (237) appears exactly once below with the milestone that completes it (§10); `rNNN` is the row's line number in the CSV (header = line 1). The skip rows are listed after the tables as not built. Rows delivered ahead of their tier are explained in §10.3.

#### enrollment (10 mapped of 10 rows)

| feature | milestone |
| --- | --- |
| r2 Customer-scoped enrollment token (expiring, multi-use) passed to the installer | M2 |
| r3 Silent install with token and server URL (MSI /qn and EXE /S) | M1 |
| r4 Console enrollment wizard that generates the RMM deployment script | M2 |
| r5 Server-side provisioning at enroll: repo init, gateway login, escrowed repo key | M0 |
| r6 Stable device ID used as restic --host (independent of hostname), duplicate/cloned identity detection, editable friendly name | M2 |
| r7 Retire device: revoke gateway login, stop schedules, keep data until retention expires; archive state stays restorable | M5 |
| r8 Pending-approval enrollment (lobby) for installs without a valid token | M9 |
| r9 Replace device: new hardware adopts the existing repo and history | M9 |
| r10 Large first backup resumable inside an initial-backup window | M8 |
| r11 Pre-seed first backup from a local repository or shipped drive | M13 |

#### policy (10 mapped of 10 rows)

| feature | milestone |
| --- | --- |
| r12 Policy object per customer: schedule, selection, retention, throttle, thresholds | M2 |
| r13 Per-device override: sparse partial override with reason, shown as overridden | M2 |
| r14 Default policy applied to new customers and devices | M0 |
| r15 Policy edit states its blast radius: live reference to existing devices, device count shown before save | M2 |
| r16 Bulk-assign policy to selected devices or customers | M7 |
| r17 Policy templates by device class (laptop vs desktop) | M9 |
| r18 Policy versioning: device reports applied version, drift shown | M9 |
| r19 Mandatory exclusions merged into every device selection | M9 |
| r20 Per-customer notification overrides with inherit (global > customer > device) | M11 |
| r21 Agent-local permissions: lock local edits, per-customer feature toggles, per-setting lock mode | M8 |

#### scheduling (24 mapped of 25 rows)

| feature | milestone |
| --- | --- |
| r22 Interval-in-window schedule with due-ness recomputed from last success (no stored next-run) | M1 |
| r23 Missed-run catch-up on reconnect or boot, default ON, with catch-up/skip selector | M1 |
| r24 Run-on-wake: resume from sleep triggers a due check | M1 |
| r25 Run-on-start with startup delay | M1 |
| r26 Backup-window mini-language (Mon-Fri/8-18;Sat,Sun/0-24) with visual editor; windows gate starts and never stop a running backup | M1 |
| r27 Static upload/download limit (--limit-upload / --limit-download) | M1 |
| r28 Skip if already running (one restic job per device) | M1 |
| r29 Retry failed runs with exponential backoff | M1 |
| r30 Start jitter to spread fleet load, plus fleet concurrency cap | M1 |
| r31 Resume after reboot or crash mid-run | M1 |
| r32 Console Back-up-now (bypasses window and conditions) and cancel running backup | M2 |
| r33 Fixed time-of-day schedules and hourly/monthly/yearly/once frequencies | M8 |
| r34 Scheduled throttle windows (limit varies by day/time) | M8 |
| r35 Start-condition framework: conditions delay a start, never cancel it, with run-anyway-after-N fallback; Wi-Fi/VPN gating | M8 |
| r36 Battery floor (no start below X percent unless on AC) | M8 |
| r37 Metered-connection deferral (defer, never fail) | M8 |
| r38 Run-time limit (cancel if exceeding N hours) and long-run alarm | M8 |
| r39 Prevent sleep during a run (best effort) | M8 |
| r40 Console-owned time-boxed pause (1/2/4/8/24 h or indefinite) with reason, visible as a state | M3 |
| r41 Idle vs in-use throttle split (keyboard/mouse activity) | M12 |
| r42 Wake from sleep to start a scheduled backup (wake timers) | M12 |
| r43 Pre/post run hooks | M12 |
| r44 Event triggers (lock, log-off, rate limit no-more-often-than-N) and post-run power action | M12 |
| r45 Sub-hourly interval (15-60 min) with --skip-if-unchanged and --no-scan | M12 |

#### selection (12 mapped of 14 rows)

| feature | milestone |
| --- | --- |
| r47 Path-based selection with tree picker and volume selection (fixed NTFS only) | M2 |
| r48 Selection presets: all user profiles, Documents/Desktop, fixed drives; auto-selection by extension tier | M2 |
| r49 Exclude patterns (glob; --iexclude for case-insensitive NTFS) | M2 |
| r50 Published default exclusion baseline (temp, Recycle Bin, pagefile, hiberfil, swapfile, System Volume Information, FilesNotToBackup key) | M2 |
| r51 Skip online-only cloud placeholders (--exclude-cloud-files), default ON | M1 |
| r52 NTFS security descriptors, attributes, creation time and EAs preserved | M1 |
| r53 Include filters | M8 |
| r54 Exclusion presets and rule helpers (browser caches, VM images, installers; size limit; cache-dir tags; marker files; system/hidden toggle) | M8 |
| r55 Per-path flags: required vs optional, require snapshot, follow symlinks | M8 |
| r56 Outlook OST/PST handling | M8 |
| r57 Network share backup with stored credentials | M12 |
| r58 EFS-encrypted files | M12 |

#### retention (13 mapped of 13 rows)

| feature | milestone |
| --- | --- |
| r61 Retention rules from restic forget keep-* and keep-within, OR-combined | M5 |
| r62 Named retention presets plus custom rule editor (e.g. 30 days, 1 year GFS, 7 years) | M5 |
| r63 Retention counts days a backup actually ran, not calendar days | M5 |
| r64 Last successful snapshot is never pruned | M5 |
| r65 Retention executed server-side only (forget then prune as two invocations) | M5 |
| r66 Prune cadence per storage target, provider-aware (default quarterly where minimum-storage billing applies) | M5 |
| r67 Retention or prune failure raised as a device alarm | M5 |
| r68 Retention preview (calendar of what survives) from forget --dry-run --json | M9 |
| r69 Prune safety guard: refuse when forget would remove more than X percent or the last snapshot | M5 |
| r70 Delete a single snapshot (audited admin action) | M13 |
| r71 Keep-tag exemption (legal hold / archive a snapshot) | M13 |
| r72 Min/max counts with emergency floor | M13 |
| r73 Cold-tier GFS archive via restic copy into a second repo | M13 |

#### storage (16 mapped of 20 rows)

| feature | milestone |
| --- | --- |
| r74 S3-compatible object storage behind the gateway | M0 |
| r75 One restic repo and one gateway login per device (--private-repos) | M0 |
| r76 Append-only gateway (rclone serve restic --append-only) | M0 |
| r77 Single storage credential on gateway/admin hosts only; separate admin path with delete rights | M5 |
| r78 TLS 1.2+ at the gateway, bcrypt htpasswd, hot reload | M0 |
| r79 Client-side compression and chunk-level dedup inside the device repo | M0 |
| r80 Usage tracking per device and customer (restic stats raw-data, provider metrics) | M5 |
| r81 Incremental-forever: no periodic fulls, every snapshot self-contained, no chain to break | M0 |
| r82 Customer-scoped bucket or prefix for cost attribution | M9 |
| r83 Soft quota per customer/device with threshold alarms | M9 |
| r84 Bucket versioning plus lifecycle window as ransomware backstop | M9 |
| r85 Repo integrity check on schedule (restic check, optional --read-data-subset) | M5 |
| r86 Stale-lock cleanup and repo repair from the console | M9 |
| r87 Hard quota enforcement at the gateway | M13 |
| r88 Other targets: local disk / NAS via rest-server, SFTP, Azure, GCS via rclone | M13 |
| r89 Secondary copies: LAN speed cache, backup cache while gateway is down, geo copy (restic copy) | M13 |

#### security (21 mapped of 22 rows)

| feature | milestone |
| --- | --- |
| r94 Client-side encryption (restic AES-256 + Poly1305) | M0 |
| r95 Unique repo password per device, held by the endpoint only in an admin-only file | M0 |
| r96 Key escrow on the console host (wrapped under a server master key) with MFA-gated, audited reveal | M5 |
| r97 Gateway login rotation and revoke (hot-reload htpasswd) | M5 |
| r98 Admin MFA with TOTP and recovery codes | M2 |
| r99 Roles: owner, admin, operator, read-only | M2 |
| r100 Audit log (actor, action, object, method web/API, source IP) with fixed retention | M2 |
| r101 Scoped, revocable API tokens | M2 |
| r102 Console bound to loopback or mesh interface by default; public exposure needs auth in front | M0 |
| r103 No telemetry, no phone-home, no CDN-hosted assets | M0 |
| r104 Path-free central logs, metrics, alerts, emails and crash reports | M0 |
| r105 WebAuthn / FIDO2 admin MFA | M10 |
| r106 SSO via OIDC (Entra ID, Google) and optional LDAP | M10 |
| r107 Granular permission flags | M10 |
| r108 Console hardening: session timeout, login rate limit and lockout, IP allowlist | M10 |
| r109 Per-customer toggle: operator may browse file names, audited | M10 |
| r110 Customer-held key option (no escrow) with consequence warning; two-person reveal gate | M13 |
| r111 Repo key rotation (key add / passwd / remove from admin host) | M13 |
| r112 Server identity pinned at enroll (fingerprint carried in the token); optional mTLS or signed agent requests | M13 |
| r113 Agent tamper resistance (stop/uninstall gated) | M13 |
| r114 Immutability via S3 Object Lock, governance mode | M9 |

#### restore (15 mapped of 18 rows)

| feature | milestone |
| --- | --- |
| r116 Snapshot list and tree browse per device (restic snapshots / ls --json) | M6 |
| r117 Point-in-time restore (show the machine as of a date) | M6 |
| r118 Restore to original or alternate path | M6 |
| r119 Overwrite policy (always / if-changed / if-newer / never) | M6 |
| r120 Restore on the device, started from the console (agent runs restic restore) | M6 |
| r121 Technician restore without the device: server-side browse and zip/tar download via admin host (restic dump -a zip) | M6 |
| r122 Restore a dead device's data onto a new device | M6 |
| r123 Filename search across snapshots incl. deleted files (restic find --json) | M9 |
| r124 Per-file version history | M9 |
| r125 Restore safety options: --verify and dry-run simulate | M9 |
| r126 Live restore progress and cancel | M9 |
| r127 NTFS permission restore toggle | M13 |
| r128 Self-service end-user restore (web link or local app) | M14 |
| r129 Restore to cloud or remote location | M13 |
| r130 Automated file-restore test on a schedule (sample restore with --verify) | M13 |

#### monitoring (23 mapped of 23 rows)

| feature | milestone |
| --- | --- |
| r134 Device state computed at read time from timestamps + policy (ok / stale / failed / blank / offline, plus pending-first-backup and paused), with explicit grace rules | M3 |
| r135 SLA threshold per customer (days since last success) with per-device override | M3 |
| r136 Device heartbeat with online/offline presence and outdated-agent flag | M3 |
| r137 Device unseen for N days state | M3 |
| r138 First-backup grace deadline for a newly enrolled device | M3 |
| r139 Expected-customers alarm: a configured customer with zero active devices renders as MISSING | M3 |
| r140 Console/gateway/storage unreachable raises a prominent banner, never quiet green; freshness stamp on every page | M3 |
| r141 No-data paths are alarms: zero devices, empty poll, partial response | M3 |
| r142 Server-judged Missed Run when the expected interval passes without a run | M3 |
| r143 Run outcome vocabulary: success, partial (exit 3), failed, cancelled, skipped, deferred, interrupted, abandoned | M1 |
| r144 Abandoned / unexpectedly-closed run detection | M3 |
| r145 Lock contention (restic exit 11 during server-side maintenance) is deferral, not failure | M5 |
| r146 Operator-configured display timezone, UTC storage | M2 |
| r147 Exception-first default view grouped by customer, with saved views and filters | M3 |
| r148 Backup-age ring dashboard (under 24 h / 24-48 h / over 48 h, click to filter) and per-customer health gauge | M3 |
| r149 Ten-day activity grid and per-device last-ten-days strip | M3 |
| r150 Per-device run history and audit trail with error classes (path-free) | M2 |
| r151 Key-escrow health: every active device has a verified escrowed key | M5 |
| r152 Partial-failure tolerance (failed files percent or count before a run counts as failed) | M3 |
| r153 Clock-skew detection between device and server | M3 |
| r154 Calendar-matrix history (device by day, Last/Worst toggle, Issue Only) | M11 |
| r155 Live run progress from restic JSON status | M9 |
| r156 Repo integrity status from the last scheduled check | M5 |

#### alerting (16 mapped of 17 rows)

| feature | milestone |
| --- | --- |
| r157 Daily digest email grouped by customer, exceptions first, including unseen / blank / MISSING customers | M4 |
| r158 Immediate failure notification | M4 |
| r159 Silence alert: no successful backup within SLA | M4 |
| r160 Unseen-device alert | M4 |
| r161 Expected-customer-missing alert | M4 |
| r162 Auto-resolve on next success | M4 |
| r163 Acknowledge with note and snooze (until date or until next success) | M4 |
| r164 SMTP delivery with test button (relay-agnostic) | M4 |
| r165 Alert-delivery failure is itself an alarm | M4 |
| r166 Consecutive-failure threshold before alerting | M11 |
| r167 Nag interval, recovery notice and transition-based dedupe | M11 |
| r168 Webhooks (JSON, custom headers, event whitelist, chat payload formats; push via relay) | M11 |
| r169 Syncro alert raise/clear adapter | M11 |
| r170 Critical configuration-change notification | M11 |
| r171 Operational threshold alerts: storage quota, outdated agent, max run duration, maintenance (forget/prune/check) failure | M11 |
| r172 Other PSA/RMM adapters and ticket create/close (NinjaOne, ConnectWise, Autotask) | M14 |

#### reporting (8 mapped of 8 rows)

| feature | milestone |
| --- | --- |
| r174 Per-customer status report (HTML and PDF) with no extra licence | M11 |
| r175 CSV/XLSX export of tables and job logs | M11 |
| r176 Scheduled reports from a saved view, uncapped | M11 |
| r177 Storage usage report per customer and device | M11 |
| r178 Compliance report: devices within SLA over a period | M11 |
| r179 Device and agent inventory report | M14 |
| r180 External recipients with opt-in | M14 |
| r181 Grouped-status and recent-activity email variants | M14 |

#### console (20 mapped of 23 rows)

| feature | milestone |
| --- | --- |
| r182 Device table (sort, filter, columns, pager, export) | M2 |
| r183 Customer grouping with collapse and per-customer rollups | M3 |
| r184 Customer CRUD with contacts, notes, optional device cap | M2 |
| r185 Device detail panel (summary, history, errors, settings, audit) | M2 |
| r186 Device action menu (back up now, cancel, refresh policy, pause, unlock, retire, restore) | M2 |
| r187 Bulk actions on a selection (assign policy, move customer, pause, update agent) | M7 |
| r188 Designed empty, loading and error states on every screen | M2 |
| r189 System health page: gateway reachability, storage probe, SMTP test, maintenance worker status, versions | M3 |
| r190 Console self-backup with tested restore (database + escrow vault); replication/HA later | M5 |
| r191 Single-binary or container deploy with a compose reference stack (console, gateway, maintenance worker) | M5 |
| r192 Pull-based idempotent expiring command queue; push channel later | M2 |
| r193 Move device between customers | M10 |
| r194 Global search (hostname, customer, user) | M10 |
| r195 Deep-linkable URLs for devices, customers and runs | M2 |
| r196 Query builder for run and device search, reused by saved views, reports and campaigns | M10 |
| r197 Console upgrade path with automatic pre-upgrade backup, migrations and rollback | M10 |
| r198 Customer-facing read-only portal and view-as-customer | M14 |
| r199 White-label and branding (console, reports, per-customer sender) | M14 |
| r200 Localization | M14 |
| r201 Device news / bulletin to agents | M14 |

#### agent (20 mapped of 22 rows)

| feature | milestone |
| --- | --- |
| r205 Windows service as LocalSystem with SCM auto-restart; Windows 10 / Server 2016 minimum | M1 |
| r206 Agent-owned scheduler (no Task Scheduler or cron) | M1 |
| r207 Power and session events handled; clean cancel on pre-shutdown | M1 |
| r208 restic.exe bundled (not embedded), pinned per agent release, hash-checked at start | M1 |
| r209 restic child process: job object kill-on-close, no console window, memory cap, cache dir under ProgramData | M1 |
| r210 Parse restic --json (stdout and stderr) and map exit codes | M1 |
| r211 VSS snapshot by default; per-volume fallback to live read reported as warning; timeout and mount-point options | M1 |
| r212 Policy pull with last-known-good retention | M1 |
| r213 Durable local result journal with at-least-once delivery to the console | M1 |
| r214 Offline resilience: gateway or console unreachable defers and retries with backoff, never a false failure | M1 |
| r215 Local logs rotated; full detail stays on the device | M1 |
| r216 No kernel driver, no reboot requirement | M1 |
| r217 Tray icon: status colour, check in now, back up now, no-recent-backup nag | M8 |
| r218 Local CLI (status, enroll, check-in, diagnose) | M8 |
| r219 Resource caps: process priority, GOMAXPROCS, read concurrency, pack size | M8 |
| r220 Windows Event Log source (Info / Warning / Error) | M8 |
| r221 Proxy support (HTTP/HTTPS, SOCKS) | M8 |
| r222 Self-update with signature check, staged channel and rollback | M11 |
| r223 Local end-user UI for browse and restore | M14 |
| r224 Linux and macOS agents | M15 |

#### deployment (11 mapped of 12 rows)

| feature | milestone |
| --- | --- |
| r227 WiX MSI installer with fixed UpgradeCode, rollback, silent install | M1 |
| r228 Authenticode signing of agent, installer and bundled restic.exe | M1 |
| r229 RMM deployment script templates (idempotent, dry-run default) | M2 |
| r230 Uninstall and scrub script that also revokes the device server-side | M7 |
| r231 In-place upgrade preserving config and state | M1 |
| r232 Gateway deployment recipe: systemd unit, TLS, htpasswd, admin-host maintenance loop | M5 |
| r233 GPO / Intune deployment guide | M11 |
| r234 Release channels (pilot, stable), force-update selected devices, update window | M11 |
| r235 Pre-bound installer download page per customer | M11 |
| r236 Bulk upgrade campaigns with device filters and per-device status | M14 |
| r237 Coexist with and import from other backup products | M14 |

#### tenancy (8 mapped of 9 rows)

| feature | milestone |
| --- | --- |
| r239 Customer as the grouping, policy and cost-attribution unit (one level) | M2 |
| r240 Customer-scoped roles (operator or read-only for one customer) | M10 |
| r241 Per-customer timezone and locale | M10 |
| r242 Customer suspend and archive | M10 |
| r243 Customer deletion with retention-respecting purge and double confirm | M10 |
| r244 Customer > site second level; reseller/distributor hierarchy | M14 |
| r245 Tags and groups across customers | M14 |
| r246 Per-customer bring-your-own storage target | M14 |

#### api (5 mapped of 6 rows)

| feature | milestone |
| --- | --- |
| r248 REST API covering every UI action, OpenAPI described | M7 |
| r249 Read-only status endpoint (state per device and customer) for external monitors | M3 |
| r250 Bulk import (CSV) of customers and devices; customer sync from PSA or documentation platforms | M11 |
| r251 Prometheus /metrics endpoint | M3 |
| r252 Client tooling: CLI, PowerShell module, MCP server (read-only) | M14 |

#### m365-gws (5 mapped of 6 rows)

| feature | milestone |
| --- | --- |
| r254 Data model does not preclude non-device sources (protected-source abstraction) | M0 |
| r255 Microsoft 365 backup module (Exchange, OneDrive, SharePoint, Teams) | M15 |
| r256 Google Workspace backup module (Gmail, Drive, Calendar, Contacts) | M15 |
| r257 M365/GWS sources shown in the same health grid and alerting | M15 |
| r258 Per-service scope selection and auto-add new users | M15 |

#### Skip rows (not built; 21)

- r46 (scheduling) Continuous / real-time data protection (CDP)
- r59 (selection) Alternate Data Streams
- r60 (selection) Junction / reparse-point contents (symlinks are recorded, not followed)
- r90 (storage) Cross-device deduplication
- r91 (storage) Cold storage classes
- r92 (storage) Vendor-hosted storage pools and partner storage resale
- r93 (storage) Storage gateway product, cluster manager and replication
- r115 (security) Immutability via S3 Object Lock, compliance mode
- r131 (restore) Mount snapshots as a Windows drive
- r132 (restore) Image, volume and bare-metal restore with boot media
- r133 (restore) Restore to VM, virtual-disk export, cloud failover, standby image, boot-screenshot verification
- r173 (alerting) Third-party monitoring partner integrations
- r202 (console) Licensing, prepaid credit, usage billing, subscription price sheets
- r203 (console) Vendor-hosted console
- r204 (console) Bundled endpoint security and RMM suites (anti-malware, patching, remote desktop, DLP)
- r225 (agent) Non-file engines and application-aware backup (Hyper-V, VMware, SQL, Exchange, system state, NAS agents)
- r226 (agent) Block-level change-tracking driver (Backup Accelerator / CBT)
- r238 (deployment) Hardware appliances and BDR boxes
- r247 (tenancy) Hostname-bound tenants with own admins and branding
- r253 (api) Vendor SDKs
- r259 (m365-gws) Per-seat licensing and metering

---

## 12. Summary

**Stack in one line:** one Go module and three binaries - a Windows-service agent wrapping pinned `restic.exe`, an `rclone serve restic --append-only` gateway in front of S3-compatible storage, and a Go console (SQLite/WAL, `html/template` + htmx + SSE, read-time state engine, separate maintenance worker) - packaged as a signed WiX MSI and a compose stack.

| item | value |
| --- | --- |
| tables | **34** (24 recon entities mapped to 25 tables, plus 9 added tables) in `schema.sql`; 103 explicit indexes (14 unique, 28 partial); 252 DDL assertions in `schema_test.py` |
| routes | agent **15**, admin API **166**, UI **66** = **247** |
| state engine | 17 states in one precedence table, 16 console-level conditions, 117 named test cases |
| jobs | 21 (console and maintenance worker) |
| milestones | 16 (M0-M15); **PILOT-ABLE = M6** |
| feature coverage | 237 of 237 non-skip rows mapped (21 skip rows listed as not built) |

**The three riskiest parts**

1. **The Windows agent's real-world behaviour** (M1): run-on-wake on Modern Standby laptops, the graceful-cancel mechanism for a console-less `restic.exe`, VSS per-volume fallback classification, LocalSystem privileges. Several mechanisms are UNCERTAIN in the engine sheet and can only be settled on hardware (L-WAKE-1, L-CANCEL-1, L-VSS-1, L-JUNCTION-1).
2. **Append-only plus server-side maintenance** (M5): forget/prune/check choreography from an admin path that holds delete rights, exit-11 deferral, hostile lock deletion, the prune safety guard, minimum-storage-duration economics, and governance-mode Object Lock. The gateway's append-only behaviour must be proven on every release (L-APPEND-1); prune memory and gateway throughput at 200 clients are unmeasured (L-PRUNE-1, L-GW-LOAD-1).
3. **Key escrow and the master key** (M5, M13): a single 32-byte secret outside the database wraps every repo password; losing it loses server-side retention and technician restore for every device, losing the console database loses the registry. The mitigations (tested self-backup restore, operator-held key copy confirmed in `init`, MFA-gated audited reveal) must be rehearsed in the pilot, not assumed.

The state engine is the *product* but is deliberately not on this list: it is pure, small and tested first (§4.8).

**Next step:** `/replica-design` - design tokens and component specs for the 71 screens (the screen map in §10.4 says which are built), with original assets and no CDN dependency.

### 12.1 Operator decisions this document needs (none blocks M0)

| # | question | where | proposed default |
| --- | --- | --- | --- |
| 1 | SLA and grace defaults (stale 72 h, unseen 168 h, first-backup grace 72 h, partial tolerance 20 files / 0.5 %, max run 12 h) are proposals, not measurements | §3.6 | confirm in the `init` wizard; tune in the pilot |
| 2 | First-backup *seed extension* (a multi-day first upload that keeps advancing counts as healthy) is a policy decision in the recon | §4.2 row 5 | off (`seed_extension_hours = 0`) |
| 3 | Per-run device logs: salted path hashes with a local resolver versus a local-only path-bearing log with SYSTEM/Administrators ACL | §6.5, DH15 | **Decided 2026-10-20: local path-bearing log, ACL'd, never uploaded; central is path-free (ADR D11 amended)** |
| 4 | Object Lock governance is `should` in `features.csv` but `could` in the recon engine gate | DH23 | M9, gated on L-GOV-1 |
| 5 | Customer-held custody is `could` in the matrix but described in ADR D9 | DH22 | schema ready; UI/enforcement M13 |
| 6 | M365 / Google Workspace and Linux/macOS agents need a different engine | DH24 | M15 spike behind a new ADR |
| 7 | Suspended = muted, not paused | DH10 | as stated |
| 8 | Indefinite pause allowed (listed in every digest) | DH11 | allowed |
| 9 | `features.csv` has 72 should / 44 could, while `recon.md` text says 71 / 45: row r114 (Object Lock governance) is `should` in the CSV and `could` in the engine gate | DH23 | follow the CSV for counts, the engine gate for risk |
| 10 | ADR D3 "one write connection" versus a second process (`maintain`) that also writes | §1.3 | **Decided 2026-10-20: ADR D3 amended to one write connection per process; `maintain` stays a separate process so the master key and S3 credential never pass through the web-facing process** |
| 11 | Per-run detail on devices and how a technician gets back to a path | DH15 | **Decided 2026-10-20: read the local log on the machine; no resolver** |

### 12.2 Lab tests that gate milestones (engine-sheet UNCERTAIN items made executable)

| id | question | gates | engine § |
| --- | --- | --- | --- |
| L-APPEND-1 | Device credentials against the deployed gateway: `forget`, `forget --prune`, `rewrite`, `key remove`, `key passwd` must fail; lock create/delete and backup must succeed. Run on every release; the flag is passed on the CLI, never via environment | M0 first pass, M5 gate | 3.2 |
| L-VSS-1 | Backup as LocalSystem with `--use-fs-snapshot`: snapshot creation, locked Outlook files, per-volume fallback and the exit-3 classification | M1 | 1.1, 5.3 |
| L-CANCEL-1 | Does `CTRL_BREAK_EVENT` to a `CREATE_NEW_PROCESS_GROUP`, `CREATE_NO_WINDOW` child make restic exit 130 and remove its lock? Else the fallback is `TerminateJobObject` | M1 | 5.4 |
| L-WAKE-1 | Latency and reliability of resume events on a Modern Standby laptop; lid-closed behaviour | M1 | 5.1 |
| L-JUNCTION-1 | How 0.19.1 reports junctions (`%LOCALAPPDATA%\Microsoft\WindowsApps`, profile junctions): warnings, exit code | M1 | 1.3 |
| L-SIGN-1 | SmartScreen / Smart App Control behaviour for the signed MSI and agent; restic.exe is not Authenticode-signed upstream | M1 | 5.8 |
| L-PRUNE-1 | prune memory and time at 1-10 TB; stable shape of prune's text output | M5 | 2.3, 2.4, 1.9 |
| L-GW-LOAD-1 | 200 concurrent clients through `rclone serve restic` to S3 | M5 | 3.3 |
| L-CERT-1 | Does rclone reload a renewed TLS certificate without a restart? | M5 | 3.1 |
| L-RCLONEARGS-1 | restic's `rclone.args` option carries `--s3-bypass-governance-retention` on the admin path | M5 / M9 | 3.4, 4.5 |
| L-LS-1 | Default recursion and flags of `restic ls` in the pinned version (one-level browse) | M0 / M6 | 1.9 |
| L-RESTORE-1 | Original-location restore with drive-letter roots (`<snapshot>:<root>` + `--target`), ACL and long-path fidelity | M6 | 1.2, 1.3, 1.12 |
| L-FIND-1 | Flag names of `restic find` in the pinned version | M9 | 1.9 |
| L-GOV-1 | Object Lock governance with a 1-day retention: lock files, undeletable stale locks, exclusive-lock acquisition, `unlock` with bypass | M9 | 4.5 |

### 12.3 Unknowns that are not lab-testable yet

Gateway cost at fleet scale; real prune cadence economics per provider (prices change - re-check before quoting customers, engine §4.1); Syncro's current public API calls for alert raise/clear (to be read from the vendor's current documentation at M11); whether `restic dump` accepts more than one path (DH27 avoids relying on it); how large the agent footprint is on 8 GB laptops under the memory cap.

## 13. Addenda

### 13.1 UI live-update channel (decided in W2, 2026-10-21)

- SSE on the admin listener at `GET /ui/events` (same session auth as pages), events `fleet.state`, `run.progress`, `banner.update`, `freshness`, `ping` (every 15 s).
- Fallback polling when the stream is closed: board partials every 30 s, run-progress partials every 5 s.
- A small first-party `fresh.js` (vendored, embedded) owns the freshness stamp: if neither an event nor a poll has arrived within 2 x the expected interval, every live element shows "stale since T" and the page banner C10 appears. A page never freezes looking fresh.
- `POST /ui/policy/window/validate` validates the window / throttle mini-language live (returns the parsed 7x24 grid or the first error position).
- Partials: `GET /ui/partials/run-progress/{run_id}`, `GET /ui/partials/log-tail/{run_id}` (path-free).

### 13.2 Rulings from the component specification (replica/design/components.md)

- `DEFERRED` is a warning-class chip (amber), as §4.2 says; the v0.3 concept has been corrected.
- `RUNNING` is an overlay chip on top of the primary state, never a primary state.
- A ten-day cell with failures and no success renders as a failed cell; a day with both renders mixed.
- `NO SCHEDULE` is critical (a device with no schedule will never turn green).
- The selection editor ships in preset + path-list mode in v1; the live tree picker needs a device-side directory-listing command (add to the command catalogue at M8).
- The age ring's middle bucket spans 24 h to the customer's `stale_after_hours`; the recon's "24-48 h" was Cove's default, not ours.
- Small buttons are 26 px tall with a 32 px padded hit area; segmented controls are 32 px.
- Big-number sizes are 18-28 px (ring centre 18, digest summary 19, KPI 23).
- Email templates use Unicode state glyphs and font fallbacks; Orbitron reaches only the digest summary counts. Whether the PDF report (M11) takes a PDF dependency is decided at M11 under the dependency budget.

### 13.3 Rulings from the Phase 1 M0 build (2026-10-22, tickets P1-T1 to P1-T3)

Each line records a point where the implementation, verified against restic 0.19.1 or the §4.8 cases, refined the text above. The text above is not edited; where a ruling changes a stated rule, this addendum wins.

**State engine (`internal/state`, §4)**

- §4.3: PENDING_FIRST also suppresses a non-hard FAILED (a transient failure inside the first-backup grace shows as attempts and an error class, case B06); a hard failure class still breaks out to BLANK with FAILED kept as a flag (A09, B05).
- §3.4 / §4.2 row 9: a VSS fallback is over tolerance only when it hit a volume that held a configured root. The facts carry `VSSFallbackOnRoot`, set by the facts loader by intersecting the run's fallback volumes with the effective policy's roots; a fallback elsewhere is the `vss_fallback` attribute only (B22).
- §4.4: an unknown or empty outcome string on the newest run classifies the device FAILED (fail loud), not merely as a failure class.
- §4.2 row 3: a paused device whose facts are incomplete stays PAUSED as primary but carries an UNKNOWN flag and a critical alarm, because rows 4, 12 and 13 cannot be evaluated; a retired device with incomplete facts is RETIRED (row 1 suppresses everything). Every incomplete device, whatever its primary, counts in banner C09.
- §4.6 C08: "no active devices" is raised only when the load completed; an incomplete or empty-after-non-empty load raises C09 (and C08 only when the empty poll is otherwise complete). An incomplete load must never claim "no devices".
- §4.2 row 15: DEFERRED additionally requires no open run (a running run is not deferred).
- §4.2 row 8 "since": FAILED's `Since` is the newest failed run's finish until the loader supplies the streak start (M3).
- `runs.error_class` (schema) gained `locked` and `missing_source`, which §3.4 (locked file as an expected class) and case B20 already assumed.

**Policy (`internal/policy`, §3.6, §6.1)**

- Exclude file: restic 0.19.1 expands `$VAR` inside `--iexclude-file`, so every `$` is written doubled (`$$Recycle.Bin`); a bare `$Recycle.Bin` excludes nothing (verified live). Operators must not pre-escape `$` in custom patterns.
- Exclude file: patterns are matched against the ancestors of a backup root too, so baseline entries with more than one component are anchored with `%PROFILE%` (expanded once per profile directory) or `%SYSTEMDRIVE%`; only single-name entries stay unanchored. Both separators are honoured; the file is written with backslashes.
- Exclude file: last match wins, so the mandatory block (CryoBackup's own state, restic cache, pagefile/hiberfil/swapfile, Recycle Bin, System Volume Information) is re-emitted after any custom `!` re-include and cannot be defeated.
- Exclusion presets (r54) are selected as `@name` entries inside `selection.excludes`; no field is added to §3.6. Revisit when the M8 editor needs a picker.
- `retention.keep_within` is section-level and required in both the preset and the rules form; tolerant decoding fills it from a named preset, the write path rejects its absence.
- `Due` adds no jitter: the agent applies `Jitter(deviceID, SlotStart, jitter_minutes)` once per slot. `Decision.Reason` may be `clock_skew` (last success in the future). `slot_attempted.NotBefore` is the next slot start plus the device's stable offset. An unparsable window never blocks backups in `Due`; `Validate` rejects it upstream.
- Proposed defaults for the init wizard (unvalidated): retention preset `1_year_gfs` with `keep_within` 14d; `prune_every_days` 30; `allow_local_backup_now` true, local pause and restore false; default selection `all_user_profiles`.

**restic (`internal/restic`, §6.3, §6.4)**

- `-o vss.exclude-all-mount-points` must be written with `=true` (restic parses the value).
- `ls --json <id> <dir>` prints the directory node plus its direct children; `ls --json <id>` with no directory walks the whole snapshot. One level per request means always passing a directory; `/` lists the roots (L-LS-1 resolved).
- A missing backup root produces a plain-text stderr line (`<path> does not exist, skipping`) and the `exit_error`, not a JSON `error` line. Plain-text lines (also `cat config` and `prune` errors) decode as `Unparsed` carrying only a classification, never text.
- `restore` prints a summary even at exit 1; `check` errors carry no `during`/`item`; `broken_packs` is an array; `cat config` has no `message_type`; `find` matches have no `name`; `ls` node `mode` is a number.
- Snapshot references are validated (`latest` or 8–64 hex, optional `:subfolder` on restore and dump) so a reference can never become a flag. Numeric HTTP codes classify only as whole tokens.
- §6.4 check column: `num_errors > 0` or broken packs mean `failed` (class `storage`) at any exit code, not only at exit 0 (fix batch F1).
- `stats --mode debug` is not advertised by restic 0.19.1 and is not exposed.
- `--retry-lock` takes a Go duration (`30s`, `5m`); `find` has `-i`, `--snapshot`, `-H` (L-FIND-1 resolved).

**Environment**

- The race detector needs cgo; the operator's Windows PC has no C compiler, so `go test -race` runs only in CI (Ubuntu and Windows runners). Local gates are `go test -count=1`; a package with real concurrency is not accepted until CI's race run is green.

**Wire protocol (`internal/api`, §5, §8.4)**

- §8.4 item 4: a second and last path-bearing wire exemption exists besides the restore selection: `restic.Snapshot.Paths` on the snapshot cache upload carries the *configured backup roots* (which §5.1 already said travel); file names never do, and `Snapshot.Username` must be empty on the wire. `api.AssertPathFree` skips exactly those two.
- §5: error bodies are RFC 9457 `application/problem+json`, one flat object `{type: urn:cryobackup:error:<code>, title, status, code, retry_after_s, request_id}`; titles are fixed sentences from a table, an unknown wire code renders as `unknown`.
- §5.1 `GET /enroll/lobby/{nonce}` (M9): the automated security review flags the poll secret in the URL path; the access log records the route pattern, never the path. Design note for the M9 ticket: carry the poll secret in a header or POST body and bind approval to a client key.
- §5.1 `GET /release` (M11): the response carries only the signed manifest and its signature; artifact names and hashes live *inside* the signed manifest and the agent takes them from nowhere else.

**Gateway, maintenance and agent (tickets P1-T4d, P1-T6, P1-T7)**

- Engine §1.13 / DH27: `restic dump -a zip --target <file> <snapshot> <path>` writes the RAW BYTES when `<path>` is a single file; only a directory produces a zip (verified live on 0.19.1). The maintenance worker runs `ls` first and wraps a single file into a one-entry zip itself, so a technician download is always a zip.
- §7.1 L-APPEND-1: under `--append-only`, `rewrite --exclude x` WITHOUT `--forget` succeeds (it only adds a tagged snapshot); the gate therefore tests `rewrite --forget --exclude` (403). `forget` and `forget --prune` refusals exit 3, not 1, so the gate checks the 403 text rather than an exit code.
- §7.3: restic 0.19.1 splits `-o rclone.program=` like a shell (backslashes escape, spaces split); the worker normalises the path to forward slashes and rejects whitespace and quotes. `-o rclone.args="serve restic --stdio [--s3-bypass-governance-retention]"` is accepted by restic 0.19.1 and rclone 1.75.1 (L-RCLONEARGS-1 syntax half answered; the governance effect is M9's lab test).
- DH18 refined: two rclone configuration files, not one. `rclone-gateway.conf` (remote `store`, the gateway credential) is mounted only into the gateway; `rclone-admin.conf` (remote `admin`, the delete-capable credential) only into maintenance. The console image also carries the pinned rclone because `maintain` drives restic's `rclone:` backend. The master key is mounted as a directory so a missing file cannot become a Docker-created directory.
- §7.1 lab MinIO policy: the gateway user may delete only `<prefix>/*/locks/*` (restic removes its own lock files); everything else is get/put/list under the prefix.
- §7.2: bcrypt is salted, so the `htpasswd-sync` job parses the deployed file and reuses the existing `probe` line while it still verifies against `ProbePassword(master)`; it hashes a new one only when absent or stale, otherwise the five-minute reconcile would see drift forever. rclone 1.75.1 accepts both `$2y$` and `$2a$` prefixes; the console emits `$2y$`. On Windows, rclone keeps the htpasswd file open so rename-over fails; production is Linux (lab test T8 proves the rename-then-reload path).
- §6.1 boot: a bundled restic that fails the hash or version pin does not stop the agent; it reports `last_error_class = guard_refused`, keeps the uplink up and refuses to schedule, so the failure is visible on the board instead of a silent exit. `Retries.Max` is not enforced in M0 (backoff to the cap, never stop). A gateway-password rotation is applied even when the accompanying policy fails validation. The SYSTEM + Administrators ACL is skipped when the process is unelevated (warning in lab mode, refusal in service mode); secrets are not written to an unprotected directory unless the explicit lab flag is set (FIX5).
- §6.3 tags: the enroll and policy responses carry `customer_id` and `policy_id` so `cb.customer` and `cb.policy=<id>.<version>` are real ids (FIX4/FIX5).
- §6.6 cancel: in service mode a console-less child cannot receive CTRL_BREAK, so the supervisor goes straight to `TerminateJobObject`; restic's lock goes stale and the next run resumes from index checkpoints (L-CANCEL-1 stays a T8 lab item). A child exit 1/2 that merely races a pending cancel keeps its real mapping.
- Secrets never appear on a command line in `deploy/` or `test/lab/` (the probe password reaches curl through `--config -`; the master key is read on stdin by `test/lab/probe-password.py`); the single documented exception is `mc admin user add` for the two throw-away lab users inside the lab-only `minio-init` container.

**Console (tickets P1-T5, FIX7, FIX8)**

- §1.1 / §7.1: the console configuration file is JSON (`config.json`, `CRYO_*` environment overrides, authoritative example in `internal/console/config/config.example.json`); the `.toml` mention is superseded because the standard library has no TOML parser (dependency budget).
- §5: both listeners answer errors as RFC 9457 `application/problem+json` with a stable `code`, including the admin listener's unmatched `/api/*` paths; UI pages keep HTML errors. Login returns the same invalid-credentials result for an unknown user, a wrong password and a locked account (the lock is enforced; `locked` appears only in the audit row) so accounts cannot be enumerated.
- §5.2 restore download: the single-use signed URL is a session-bound, 2-minute, server-side-hashed token minted by a CSRF-protected `POST` that 303-redirects to the `GET`; the download needs the session cookie and the token, marks the restore `delivered` on the first completed response (Range honoured within the window), and is never logged (route pattern only). Tokens live in memory: a console restart invalidates them.
- §4.6 C07 job name is `maintenance-scheduler` (hyphen) everywhere; the `maintenance_scheduler` spelling in the C07 row is a typo. Only the lease holder writes `job_state`.
- DH17 / §7.3: the admin-path `Init` against an existing repository with the wrong key returns a result (`Outcome failed`, `wrong_password`, exit 12) rather than an RPC error; the console treats any non-success `Init` outcome as a 503 and leaves the device in place for the retry. The device ULID is generated before `AgentEnroll` so the repo key is wrapped under the real id from the start.
- §6.1 / §6.5 (agent, FIX5/FIX9): the state directory is created with its restrictive security descriptor already set; every layout directory is verified at start, after repair and before every heartbeat; `--repair-acl` never takes over a foreign-owned tree in place — it quarantines it (`CryoBackup.quarantine-<utc stamp>`), creates a fresh restricted root and requires an elevated re-enrolment; a failed pre-check refuses before any descriptor is modified.

**Agent state trust (FIX10) and CI facts**

- §6.5: the 'untrusted - repair required' verdict is a per-state-directory value `UntrustedRepairRequired-<first 16 hex of sha256(lower-cased cleaned absolute path)>` under `HKLM\SOFTWARE\CryoBackup\Agent`, written and cleared only by elevated starts; a verdict that cannot be read refuses; no file marker exists (a planted one is inert). Quarantines are named `<state>.quarantine-<utc stamp>-<8 random hex>` with bounded retries so a predictable name cannot be squatted. Every security read is handle-based and no-follow; `SetSecurityInfo` handles carry READ_CONTROL (it reads the current descriptor first).
- CI: the GitHub windows-latest runner is elevated, so the agent's elevated paths are exercised on every push; the race detector runs only in CI (no C compiler on the operator's PC), and a package with real concurrency is accepted only when the CI race run is green. Run 39863810066 on dee5834 was the first green run of the M0 build.

**Lab prep rulings (2026-10-22 evening)**

- Toolchain: Go 1.26 minimum and current dependency tags (ADR 0004 supersedes the Go-minimum bullet of ADR 0003).
- rclone's environment namespace: no build argument, compose variable or process environment that an rclone process inherits may be named `RCLONE_*` unless it is meant as an rclone flag; rclone parses every such variable as the flag of that name (a build-time `RCLONE_VERSION` was read as `--version` and aborted the image self-check). The pinned-version arguments keep their names only where no rclone process sees them.
- The lab S3 target: MinIO's community edition is source-only and archived upstream, and its images were withdrawn; the lab profile builds the last pinned release from the Go module proxy (immutable pseudo-versions, checksum-database verified). MinIO is a lab fixture, never a production target; the production rule stays 'a real provider with bucket versioning and an admin-only lifecycle rule'. A maintained lab target is an M5 decision.
- `cryo-console init` writes `config.json` and derives every path from `--data-dir`; a container deployment that mounts the key, database and sockets at fixed paths must post-edit the file (L-INIT-1). Before the M1 documentation is called done, `init` gains explicit path flags or a config-driven mode so the compose README's one-command flow is true.

**Lab bring-up rulings (2026-10-22 night, real stack on the lab host)**

- §7.1 / §8.1 gateway credential, for every provider (template `deploy/compose/minio-init.sh`): `s3:GetObject` and `s3:PutObject` on `<prefix>/*` AND on the bare key `<prefix>` (FIX16: rclone's `NewFs` checks whether its root is a file with a HEAD on that key; a 403 makes it treat the prefix as a file and serve the bucket root, after which every object access is outside the prefix and the gateway answers 500; the key never holds an object, so the grant is inert); `s3:ListBucket` on the bucket with the `s3:prefix` condition; `s3:GetBucketLocation` in a statement of its own (FIX15: the `s3:prefix` condition key is valid for ListBucket only and MinIO rejects the policy otherwise); `s3:DeleteObject` on `<prefix>/*/locks/*` only. The smoke script's probe login (404) is the acceptance check for the whole chain.
- The first-start sequence of the compose README was wrong in three ways (L-INIT-1): `cryo-console init` writes `config.json` and derives every path from `--data-dir`; the service's bind mount of a not-yet-existing `config.json` makes Docker create a directory of that name; and the container's uid 10001 cannot write into a root-owned directory. Until `init` grows explicit path flags (FIX ticket, before the M1 docs), the documented flow is `docker run` with explicit mounts, a uid-10001 scratch directory for the config, moving `master.key` into the read-only key directory and rewriting the container paths; the lab script `lab-init.sh` is the reference.
- Proven on Linux tonight (P1-T8 step 7): `TestLiveRclone` (htpasswd rename-over + hot reload) and the maintenance live tests including `TestLiveAppendOnlyGateway` pass on the lab host with the pinned rclone 1.75.1 and restic 0.19.1.

**M0 lab acceptance rulings (2026-10-23, real machines)**

- §8.3 / lab flag: `--insecure-tls-for-lab` governs the agent's own uplink only. restic verifies the gateway certificate against the operating system's trust store; a gateway certificate that is not publicly trusted must be installed on the endpoint (lab) or distributed by the console (M5, L-TLS-1). Production: a publicly trusted gateway certificate (DNS-01) remains the default.
- §3.4 partial tolerance: every Windows profile carries app-execution aliases under `AppData\Local\Microsoft\WindowsApps` that restic 0.19.1 cannot archive (`unsupported file type "irregular"`, exit 3); they are in the baseline exclude (FIX18) and L-JUNCTION-1 is answered. A dedicated tolerable class `unsupported_file` for other reparse points is an M1 decision because `runs.error_class` is a schema CHECK (a migration once released).
- §1.1 session cookie: `Secure` follows the connection (TLS, or `X-Forwarded-Proto: https` from a loopback/private peer, last hop), because the admin listener is plain HTTP on loopback or a private address by design (FIX17/FIX17b). M1: `IsLoopbackOrPrivate` gains the CGNAT range 100.64.0.0/10.
- §6.5 L-CANCEL-1 answered: in service mode a stop terminates restic (TerminateProcess, exit 130, run `interrupted`, `cancel_mode terminated`); a lock file stays behind and the retry proceeds without an `already locked` error (restic treats the same-host stale lock as removable). Graceful CTRL_BREAK remains console-mode only.
- Repair path proven on a real VM: squat and tamper (root and `journal\`) refuse with a per-directory HKLM verdict, quarantine to a random-suffix name, fresh root, verdict cleared, fresh identity. Open: `install` must be idempotent when the Event Log source exists (FIX19) and the repair must write Event Log 1007 from every entry point (FIX20).
- `init` creates the owner before writing `config.json`, so a failed config write loses the one-time password (L-INIT-1 facet 4); the init FIX ticket orders config write first, or prints the password only after every step succeeded.

**M1 wave-1 rulings (2026-10-24)**

- §6.2 run-on-wake: a resume power event re-plans at once; a run it makes due is journaled with trigger `wake` ("resume" is only the scheduler loop's internal reason). Gated on `run_on_wake`, 30 s debounce. Suspend and session events are logged and ignored. Not built yet: the wait for the network after a resume (up to 2 min, §6 resume row); until then a run that starts before the network is back defers and retries.
- §3.6 throttle gains `memory_limit_mib` (0 = auto = half of physical RAM, at least 1024 MiB; -1 = no cap; explicit 512 MiB..1 TiB), applied through the restic child's job object. A job-object memory kill is `failed` / `memory`.
- §6.3 and §9 correction for restic 0.19.1: a VSS per-volume fallback on its own exits **0**, not 3; restic reports it as one stderr error line `failed to create snapshot for [<vol>\]` through the progress reporter, not the archiver. The agent detects that line, reports the volume letter only (`stats.vss_fallback_volumes`, e.g. `C`) and turns the run into `partial` / `vss`, which is always over tolerance: restic only snapshots volumes that hold a root, so every fallback is "a configured root's volume had no snapshot". Fallbacks restic does not report (a volume in `vss.exclude-volumes`, a UNC root, an uncovered mount point) cannot be detected.
- §3.4 partial tolerance: `unsupported_file` (restic `<path>: unsupported file type "irregular"` or `""`) is a tolerable per-file class alongside `permission` and `locked`; migration 0002 widens `runs.error_class`. `replica/schema.sql` is now the current schema (DH20): tests compare it with the fully migrated database by normalised `sqlite_master`, and 0001's checksum is pinned.
- §1.1 / FIX17: `IsLoopbackOrPrivate` includes the shared address space 100.64.0.0/10 (RFC 6598, mesh VPNs). The admin listener takes optional TLS (`admin.tls.cert`/`key`); `doctor` warns when the admin bind is not loopback and TLS is off.
- FIX19 and FIX20 closed: an existing Event Log source is success (registry key check), and a repair from `enroll` or `install` writes Event Log 1007 (or prints the logging failure to stderr).

**M1 fleet concurrency cap (2026-10-24, ADR 0005, M1-T6)**

- §5.1 gains a sixteenth agent route, `POST /api/v1/agent/start-lease` (device token; body `{run_id}`, 1 KiB cap, 6/min per device; RFC 9457 errors like every agent route). Both answers are 200: `{granted: true, lease_until}` (a lease is held), `{granted: true}` without `lease_until` (the target has no cap; nothing is stored), or `{granted: false, retry_after_s}` with the time until the earliest live lease expires, clamped to 60..900 s and never random (the agent adds 0 to 20 % jitter). A run id that holds another device's lease is 409. `api.Patterns()` lists it after the fifteen rows of the table.
- The cap is `storage_targets.max_concurrent_backups` (0 = unlimited, the default; migration 0003), set with `cryo-console target set --max-concurrent-backups N [--name <target>]` or `init --max-concurrent-backups N`, shown by `cryo-console target list`. Leases live in `start_leases` (run_id PK); the console decides in one `BEGIN IMMEDIATE` transaction: a live lease of the same run is returned unchanged (idempotent), the device's other leases are released (an agent runs one backup at a time), expired rows of the target are deleted, and the lease is stored while fewer than the cap are live.
- A lease lives 15 minutes. Every heartbeat that names the run renews it to now + 15 min: `HeartbeatRequest.active_run_id` (new, optional; sent while a backup is active), else `progress.run_id` from an older agent. An expired lease is not revived. The upload of the CLOSED run releases it (the open record sent at start does not); the heartbeat ring trim job also deletes expired rows.
- Agent: only scheduled starts ask (schedule, catchup, boot, wake, retry), after the command is assembled and before the run row is written. A refusal is journaled `deferred` / `fleet_cap` without starting restic; it is not an attempt (no retry-ladder step, the slot is not marked attempted, it is not the "last run"), the boot label is kept for the next start, and heartbeats report `deferred` / `fleet_cap` while the agent waits. Fail open: unreachable, a 10 s timeout, 5xx, 429, or 404/405 (a console without the route) start anyway and record `stats.lease = bypassed` with a warn line; any other 4xx does not start, journals nothing and waits the uplink backoff. `backup_now` never asks (`not_required`). The per-run fact is `runs.stats.lease` (`granted`, `bypassed`, `not_required`; empty for runs that never asked), kept in the stats JSON because the console only shows and counts it.
- State engine: `fleet_cap` is a DEFERRED reason of its own (`ReasonDeferredFleetCap`), no alarm by itself; the SLA clock keeps running, so a cap set too low surfaces as STALE.
- M1-FIX4 gateway watchdog (§6.2 "gateway down mid-run is deferred, never failed"): restic 0.19.1 retries each backend operation for up to 15 minutes and keeps refreshing its lock, so it does not exit when the gateway goes away. While a backup runs, the agent probes the repository URL's host:port with a plain TCP connect every 30 s (no credentials, no restic call) and follows bytes_done/files_done; no progress AND no gateway for `throttle.gateway_down_grace_minutes` (new §3.6 field in the throttle section, like `memory_limit_mib`; default 15, 0 = default for policies stored before it, otherwise 5..120) stops restic and journals the run `deferred` / `gateway` / class `network`, retried on the normal backoff. Progress alone resets the clock; a reachable gateway with no progress is the unchanged stall path. While the condition lasts the heartbeat says `running` with `progress.stalled = true` and `last_error_class = network` (no wire change). A restic that reaches the gateway only through an HTTP proxy is probed directly, so there the watchdog rests on progress alone.

**M1 no-data-at-rest rulings (2026-10-28, ADR 0008, M1-HIPAA1)**

- DH27 / §5.2: technician restores stream. Creating a restore validates and records it and it is `ready` at once; `GET .../download?token=` (single-use, session-bound, no Range, no resume) has the worker run `restic dump` to stdout - a directory as restic's zip, a single file wrapped on the fly into a one-entry zip - and the bytes travel worker -> admin-path socket -> console -> browser without touching disk. The admin-path `dump` method answers with the zip as the body and its result (size, SHA-256) in the `Cryo-Dump-Result` trailer; size and SHA-256 are recorded on the restore only when a download completes. A failed or dropped download records its class (`disconnected` for the browser going away), leaves the restore downloadable until `expires_at`, and a failure after the first byte aborts the HTTP response so a cut zip is never saved as complete.
- No artifacts for restores: the restore path writes nothing to the `artifacts` volume (kept, unused in M1, for agent releases). `restore-purge` now expires spent restores (clears the encrypted selection at `expires_at`) and deletes, with a warning, any `restore-<ULID>.zip` an earlier version staged; `artifact_ref` and `purge_after` are no longer set for technician downloads.
- Maintenance: every restic call (the version check included) runs with `--no-cache` (`restic.Env.NoCache`, no `RESTIC_CACHE_DIR`); restic's and rclone's temp and cache directories (`TMPDIR`, `RCLONE_TEMP_DIR`, `RCLONE_CACHE_DIR`) live under `CRYO_MAINTAIN_SCRATCH_DIR` (default `/tmp/cryo-maintain`, a 512m tmpfs in compose, created 0700, emptied at start; a scratch directory not on tmpfs is logged as `scratch_not_tmpfs`). The worker has no persistent state directory: `CRYO_MAINTAIN_STATE_DIR` is ignored (and said so) and the `maintain-state` volume is gone.
- Compose: `console`, `gateway` and `maintenance` run with `ulimits: core: 0` (soft and hard): no crash dumps.
- `cryo-console doctor` gains a `swap` line: OK with no active swap or only dm-crypt/zram swap, WARN for any other active swap or an unreadable `/proc/swaps`; never FAIL.
- Operator ruling 2026-10-29 (M1-HIPAA2): the re-download window stays. A delivered restore may be downloaded again until `expires_at` (24 h), each time with a new single-use, session-bound token and a fresh `restic dump` stream; every completed download increments `download_count` and writes its `restore.download` audit row; past `expires_at` every token and every mint is refused (410 Gone, code `not_found`), and `restore-purge` clears the encrypted selection, leaving only `selection_hash` and `selection_count`.
