# ADR 0013 - The agent-endpoint banner comes from a self-probe, not from fleet silence

**Date:** 2026-11-02 - **Status:** Accepted - **Decider:** foreman (M2 lab fix batch, ticket M2-FIX-B2), recorded for the operator

**Supersedes:** the C04 row of architecture §4.6 ("Agent API silent": fleet has ever enrolled devices and
`max(device_presence.received_at)` is older than 15 min while at least one non-paused, non-retired device exists,
critical), the matching C04 row of `replica/design/components.md` §9.2 (chip AGENT API SILENT), and architecture
§4.8 test cases C11 and C12 (`agent_api_silent_banner`, `agent_api_silent_not_when_all_paused_or_empty`). The
"same rule fleet-wide produces banner C01 / C04" sentence of the §4.3 customer-wide-collapse row no longer applies
to C04. C05 (§4.6) is kept and now has a real source for the agent listener half.

## Context

C04 exists for the 18-day silent outage class: an agent listener, certificate or DNS name that breaks while the
board stays quietly green. It was specified as fleet silence. In the M2 lab run (docs/lab/m2-run.md, F5) it fired
for four hours because the only enrolled PC was asleep, and in a fleet of laptops it would fire every night and
every weekend. A critical banner that is always on teaches the operator to ignore it, which is the outage class
again by another route. Silence does not tell a broken listener from a sleeping fleet; probing the listener does.

## Decision

**The probe.** `cryo-console serve` probes its own public agent endpoint (`public_url`) on a timer, the way an agent
does before its first request, and stops at the first stage that fails:

| stage | check | failure class examples |
| --- | --- | --- |
| `dns` | resolve the host (skipped for an IP literal) | `nxdomain`, `timeout`, `lookup_failed` |
| `connect` | TCP to a resolved address (or `agent_probe.connect_address`) | `refused`, `timeout`, `unreachable` |
| `tls` | TLS 1.2+ handshake; chain and host name verified against the system roots, or `agent_probe.ca_file` | `untrusted`, `hostname`, `handshake` |
| `certificate_expired` | the leaf's notAfter against the console clock | `expired` |
| `responder` | an unauthenticated `GET /api/v1/agent/policy` must be answered by this console's agent API: status 401 (or 429), `application/problem+json`, code `unauthorized` (or `rate_limited`) with the matching type and status, and an `X-CryoBackup-Server-Time` header | `status_502`, `content_type`, `server_time`, `body`, `not_http` |

The request carries no credential. Every stage has a 10 s bound and a run 30 s, so a hung network never stalls the
loop. The probe starts after the listeners are bound and never blocks startup; its first run is immediate, then
every `agent_probe.interval_s` (default 180 s). A run cut short by shutdown is not recorded. Logs are path-free and
token-free: the stage and an error class, written only when the result changes (first result, healthy to failing,
another stage or class, failing to healthy), so a steady state writes nothing.

**The banners** (computed in `internal/console/health` from the probe status, never stored):

- **C04 AGENT API UNREACHABLE, critical:** the probe has failed at least 2 runs in a row spanning at least 5 min
  (the grace), naming the stage: "Agent API unreachable: {host} does not resolve (DNS). No agent can check in or
  report. Failing for {N} min." (and the connect, TLS, expired-certificate and wrong-responder variants). An expired
  certificate skips the grace, and is reported as soon as notAfter passes even before the next run. A passing run
  clears it.
- **C05 CERTIFICATE EXPIRING:** "Agent listener certificate expires in {N} days." Warning under 21 days, critical
  under 7 days, the §4.6 thresholds unchanged. The gateway half of C05 arrives with the gateway prober.
- **C17 AGENT API NOT CHECKED, warning:** the probe cannot run (`public_url` unset, or `agent_probe.ca_file`
  unreadable), runs without TLS (`public_url` is `http://`, a lab), or has not completed a run for 3 intervals.
  Serve also logs once that the probe is not running. This is a warning, not critical: a mesh-only lab is a
  legitimate deployment, but the console must say that the protection is off rather than skip it silently.
- **C18 AGENTS STOPPED TOGETHER, warning (the weak signal, kept):** at least 5 active devices were heard within
  the hour before the newest check-in, every one of them stopped within 10 min of the others, nothing has been
  heard for 15 min, and C04 is not raised. Kept because a probe from the console host cannot see everything an
  agent's path crosses (an outside firewall, public DNS answering differently, NAT, a proxy in front of the
  sites): agents cut off at the same moment is evidence of that, and is cheap to detect. A sleeping fleet trickles
  off over hours and does not match; a single sleeping PC never matches. It is never critical, and it clears as
  soon as any device checks in.
- **Fleet silence alone raises no console banner.** An offline device is only its own UNSEEN or STALE row on the
  board (and the customer-wide-silence entity banner of §4.3 still applies to five or more devices of one
  customer).

**Configuration** (`agent_probe` in config.json, each with a `CRYO_AGENT_PROBE_*` override): `interval_s` (180,
30 to 3600), `ca_file` (optional PEM roots for a private-CA deployment), `connect_address` (optional host:port
dialled instead of the resolved addresses, for a console behind NAT without hairpin; DNS, TLS and the host name
are still checked against `public_url`). Grace, failure count, certificate thresholds and the C18 numbers are
constants, documented in `internal/console/health/endpoint.go`.

**Fixtures mode** reports a canned healthy probe whose certificate expires in 12 days, so the board shows the C05
warning through the same code path. `health.EndpointExamples` returns one banner of every kind above for the
/design page.

## Consequences

- The lab case (one PC asleep for hours) raises nothing at console level; a broken listener, certificate, DNS name
  or proxy raises a critical banner within about 5 to 8 minutes of breaking (the grace plus up to one interval)
  whether or not any agent is awake, which is earlier than the old 15-minute silence rule and also works for a
  fleet that is entirely asleep.
- The probe sees the endpoint from the console host. Split-horizon DNS or a firewall that treats the console host
  differently can hide a failure that agents see; C18 is the backstop for that, and the residual risk is accepted.
- Each probe run costs one access-log line and one failed-auth count against the console host's own address on
  the agent listener (one per interval, far below the 30 per minute block threshold).
- A deployment whose agent certificate is from a private CA must set `agent_probe.ca_file`, or C04 (TLS,
  untrusted) is raised: the probe checks exactly what an agent with only the system roots would.
- Follow-ups outside this ticket's write set: the /design page's canned C04 text
  (`internal/console/fixtures` `Banners`) still carries the superseded "No agent has checked in" copy and should
  use `health.EndpointExamples`; architecture §4.6/§4.8 and components.md §9.2 should point at this ADR.
