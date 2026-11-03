# ADR 0013 - The agent-endpoint banner comes from a self-probe, never from devices being offline

**Date:** 2026-11-02 - **Status:** Accepted - **Decider:** operator (no banner for machines being offline), recorded
by the foreman (M2 lab fix batch, ticket M2-FIX-B2)

**Supersedes:**

- the C04 row of architecture §4.6 ("Agent API silent": fleet has ever enrolled devices and
  `max(device_presence.received_at)` is older than 15 min while at least one non-paused, non-retired device exists,
  critical) and the matching C04 row of `replica/design/components.md` §9.2 (chip AGENT API SILENT);
- architecture §4.8 test cases C11 and C12 (`agent_api_silent_banner`,
  `agent_api_silent_not_when_all_paused_or_empty`);
- the "same rule fleet-wide produces banner C01 / C04" sentence of the §4.3 customer-wide-collapse row, for C04;
- the "DoS on the public listener" row of the §8.3 threat table ("the loss of check-ins is itself detected (C04)");
- the acceptance step "stop all heartbeats: C04" (§10.2, M3 acceptance).

Architecture §4.3, §4.6, §4.8, §8.3 and §10.2 and components.md §9.2 now point here. C05 (§4.6) is kept; the agent
listener half now has a real source.

## Context

C04 exists for the 18-day silent outage class: an agent listener, certificate or DNS name that breaks while the
board stays quietly green. It was specified as fleet silence. In the M2 lab run (docs/lab/m2-run.md, F5) it fired
for four hours because the only enrolled PC was asleep. In a fleet of laptops it would fire every night and every
weekend, and in a fleet with a scheduled shutdown every evening. A critical banner that is always on teaches the
operator to ignore it, which is the outage class again by another route. Silence cannot tell a broken listener
from a sleeping fleet; probing the listener can.

## Decision

**Devices being offline never raise a console banner.** However many devices are offline, for however long, and
however suddenly, each is only its own UNSEEN or STALE row on the board. (The customer-wide-silence entity banner
of §4.3, five or more UNSEEN devices of one customer, is unchanged.) Listener breakage is the probe's job.

**The probe.** `cryo-console serve` probes its own public agent endpoint (`public_url`) on a timer, the way an agent
does before its first request, and stops at the first stage that fails:

| stage | check | failure class examples |
| --- | --- | --- |
| `dns` | resolve the host (skipped for an IP literal) | `nxdomain`, `timeout`, `lookup_failed` |
| `connect` | TCP to a resolved address (or `agent_probe.connect_address`) | `refused`, `timeout`, `unreachable` |
| `tls` | TLS 1.2+ handshake; chain and host name verified against the system roots, or `agent_probe.ca_file`; when `agent_probe.spki_pin` is set, the leaf's SubjectPublicKeyInfo must also match it, exactly as an agent with `spki_pin` checks it (internal/agent/uplink) | `untrusted`, `hostname`, `pin_mismatch`, `handshake` |
| `certificate_expired` | the leaf's notAfter against the console clock | `expired` |
| `responder` | an unauthenticated `GET /api/v1/agent/policy` must be answered by this console's agent API: status 401 (or 429), `application/problem+json`, code `unauthorized` (or `rate_limited`) with the matching type and status, and an `X-CryoBackup-Server-Time` header | `status_502`, `content_type`, `server_time`, `body`, `not_http`, `timeout` |

The request carries no credential. Each network stage, the responder included, has its own 10 s bound (the
responder sets a fresh connection deadline and context for its request and answer), and a whole run is bounded at
30 s, so a hung network never stalls the loop. The probe starts after the listeners are bound and never blocks
startup; its first run is immediate, then every `agent_probe.interval_s` (default 180 s). A run cut short by
shutdown is not recorded. Logs are path-free and token-free: the stage and an error class, written only when the
result changes (first result, healthy to failing, another stage or class, failing to healthy), so a steady state
writes nothing.

**The banners** (computed in `internal/console/health` from the probe status, never stored):

- **C04 AGENT API UNREACHABLE, critical:** the probe has failed at least 2 runs in a row spanning at least 5 min
  (the grace), naming the stage, for example "Agent API unreachable: {host} does not resolve (DNS). No agent can
  check in or report. Failing for {N} min." An expired certificate skips the grace and is reported as soon as
  notAfter passes, even before the next run. A passing run clears it.
- **C05 CERTIFICATE EXPIRING:** "Agent listener certificate expires in {N} days." Warning under 21 days, critical
  under 7 days, the §4.6 thresholds unchanged. The critical tier stays deliberately: the ticket that started this
  ADR asked for a warning, but under 7 days the certificate is one missed renewal from becoming C04 for every agent
  at once (the outage class itself), and a renewal that has not happened with a week left usually means the
  automation is broken and needs a person; a warning is too easy to leave for later. The gateway half of C05
  arrives with the gateway prober.
- **C04b AGENT API NOT CHECKED, warning:** the probe cannot run (`public_url` unset, `agent_probe.ca_file`
  unreadable, `agent_probe.spki_pin` malformed), runs without TLS (`public_url` is `http://`, a lab), or has not
  completed a run for 3 intervals. Serve also logs once that the probe is not running. It is a warning, not
  critical: a mesh-only lab is a legitimate deployment, but the console must say that the protection is off rather
  than skip it silently. The id is a variant of C04 so that it cannot collide with the §4.8 test-case numbers.
- **No "agents stopped together" signal.** A first version of this ADR kept a weak warning for five or more
  devices that stopped within ten minutes of each other while the probe passed, as a backstop for paths the
  console host cannot see. The operator removed it: it fires nightly for fleets with a scheduled shutdown and after
  every console restart, and the operator does not want a banner for machines being offline in any form. The
  residual risk (below) is accepted.

**Configuration** (`agent_probe` in config.json, each key with a `CRYO_AGENT_PROBE_*` override):

- `interval_s` (180, 30 to 3600);
- `ca_file`: optional PEM roots for a private-CA deployment;
- `connect_address`: optional host:port dialled instead of the resolved addresses, for a console behind NAT
  without hairpin; DNS, TLS and the host name are still checked against `public_url`;
- `spki_pin`: optional, the base64 SHA-256 of the agent listener's SubjectPublicKeyInfo, the same value and format
  as the agents' `spki_pin`. Set it whenever the agents are pinned, so that a certificate rotated to a new key
  fails the probe the way it would fail every pinned agent.

Grace, failure count and the certificate thresholds are constants in `internal/console/health/endpoint.go`.

**The probe can never get the console's own address blocked.** Each run is one unauthenticated request that the
agent listener counts as one failed authentication from the console's address (`failed_auth_per_min_per_ip`
within one minute, then a block). Exempting the probe's request inside the agent API would be a bypass an
attacker could try to imitate, so the boring rule is in configuration instead: `Validate` refuses an
`interval_s` whose worst-case requests per minute, ceil(60 / interval_s) + 1 (a request lands anywhere inside its
run's own timeout), exceed half of `failed_auth_per_min_per_ip`. With the defaults that is 2 of 30. A test drives
the agent listener's real `ratelimit.Blocker` through a day of failing probes at the tightest spacing for every
accepted combination and sees no block. A 429 from the agent API still counts as this console answering, so a
block caused by something else on the same address (for example agents with bad tokens behind the same NAT) does
not turn into a false C04.

**Fixtures mode** reports a canned healthy probe whose certificate expires in 12 days, so the board shows the C05
warning through the same code path. The /design page renders `health.EndpointExamples`: C04 at every failing
stage (including a pin mismatch), C05 warn and critical, and C04b.

## Consequences

- The lab case (one PC asleep for hours), a fleet that shuts down every evening, and a console restart raise
  nothing at console level. A broken listener, certificate, pin, DNS name or proxy raises a critical banner within
  about 5 to 8 minutes of breaking (the grace plus up to one interval), whether or not any agent is awake, which
  is earlier than the old 15-minute silence rule and also works for a fleet that is entirely asleep.
- The probe sees the endpoint from the console host. Split-horizon DNS, or a firewall or proxy that treats the
  console host differently from the customer sites, can hide a failure that agents see. That residual risk is
  accepted by the operator; it shows up as devices going UNSEEN/STALE on the board and as the customer-wide-silence
  entity banner.
- Each probe run costs one access-log line and one failed-auth count against the console's own address, inside
  the bound above.
- A deployment whose agent certificate is from a private CA must set `agent_probe.ca_file`, and one whose agents
  are pinned should set `agent_probe.spki_pin`, or the probe checks less (or other) than the agents do.
