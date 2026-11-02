# M1 lab run (M1-T8) - evidence log

Run 2026-10-28 with the operator. Hosts are described generically on purpose; the operator's concrete
addresses live outside the repository. Times are UTC.

## Versions

| component | version | where |
| --- | --- | --- |
| CryoBackup | `main` 4c55ec3 (console, gateway, maintenance images rebuilt on the lab host) | lab host |
| agent MSI | signed release builds of ff0d36d / 4c55ec3: 0.0.0.214, 0.0.0.217, 0.0.1, 0.0.2 (`CRYO_MSI_VERSION`) | lab VM, laptop |
| restic | 0.19.1, unsigned, byte-identical to upstream | bundled in the MSI |
| lab VM | Windows Server 2025 Standard, reverted to snapshot `clean` | Proxmox |
| laptop | the operator's Windows 11 laptop, Modern Standby (S0 low power idle, network disconnected); no S3 | operator's desk |

## Stack

- Re-extracted from 4c55ec3, images rebuilt, `up -d`. Migrations 0002 and 0003 applied to the M0 database in place:
  `doctor` reports schema version 3, `quick_check` and `integrity_check` ok, maintenance worker reachable, master key loaded.
- The M0 devices left in the database alarm correctly: one STALE critical, two BLANK critical (fail loudly).

## Results

| # | test | result | evidence |
| --- | --- | --- | --- |
| 1a | MSI install through the RMM wrapper (VM; token on stdin) | PASS | `package 0.0.0.214; installed none` → `installed, enrolled and running`, exit 0; first run (trigger boot) success 18 s later; board OK |
| 1b | MSI install through the RMM wrapper (laptop, operator-run helper; token pasted, not echoed) | PASS | enrolled, heartbeating, first run started |
| 1c | upgrade 0.0.0.214 → 0.0.1 → 0.0.2 (wrapper) | PASS | same device id; delayed auto start and restart 60 s / 300 s / 900 s kept; Apps and Features shows the new version each time |
| 1d | upgrade 0.0.0.214 → 0.0.0.217 (wrapper) | **FAIL (L-UPG-1)** | wrapper: `already installed and current; nothing to do` |
| 1e | service settings self-repair | PASS | start type set to demand and recovery actions cleared by hand, service restarted → AUTO_START (DELAYED) and 3 restart actions back; Event 1009 written (its text is invisible, L-EVT-1) |
| 1f | uninstall, reinstall | PASS | `msiexec /x` exit 0, ProgramData kept; reinstall → same device id, enrolled. (The reinstall ran through plain `msiexec /i`, not the wrapper.) |
| 2a | L-VSS-1: VSS with a file held open with no sharing | PASS | run success; the file restored through the admin API has the same SHA-256 as the original |
| 2b | L-VSS-1: VSS service disabled | PASS | run partial, exit 3, class `vss`, `vss_fallback_volumes: ["C"]`, error counts locked 1 / vss 1; board PARTIAL alarm. Agent-side retry: see L-VSS-2 |
| 3a | reboot mid-run (laptop) | PASS | run journaled `interrupted` (exit 130), delivered after boot; attempt 2 (trigger retry) started 2 min after boot. Stats of the interrupted run read 0 although ~3.4 GB had been uploaded (L-STATS-1) |
| 3b | Modern Standby 5 min mid-run (laptop) | PASS | restic kept running; run not failed, continued as attempt 2; storage grew afterwards. Heartbeat resumed ~2.5 min after wake while the command poll resumed at once (observation) |
| 4 | gateway down mid-run (laptop, grace 15 min default) | PASS | gateway stopped 18:05:39; run journaled `deferred` / `gateway` / class `network` at 18:23:48; retry started at 18:34:52 after the gateway was back |
| 5 | seven days offline (VM; journal moved back 7 days with the service stopped) | PASS | `next due: now (catchup)`; exactly one run (18:20:21, labelled `boot` because the service was restarted), which rode out a 4-minute gateway outage and finished success; next due +24 h |
| 6 | fleet cap 1 (laptop holding the lease, VM made due) | PASS | VM run journaled `deferred` / `fleet_cap` (18:48:37), no restic start; cap set back to unlimited; the VM asked again after the console's retry time (lease TTL 15 min + jitter) and ran at 19:04:22 |

## Findings

- **L-UPG-1** (wrapper vs MSI): untagged builds are `0.0.0.N`; the RMM wrapper compares only X.Y.Z and skips
  0.0.0.214 → 0.0.0.217, while the MSI itself allows same-X.Y.Z upgrades (`AllowSameVersionUpgrades`). Tagged
  releases are unaffected. The wrapper should apply a package whose four-part version is higher.
- **L-EVT-1** (fail loudly): the Event Log source is registered with `EventCreate.exe` as its message file, which
  only carries texts for IDs 1-1000. Events 1003, 1009, 1010 show "description cannot be found" in Event Viewer;
  the 1009 self-repair alarm is therefore unreadable there. Ship a message resource or keep IDs within 1-1000.
- **L-VSS-2** (agent and console disagree): `scheduler.Completed` counts a partial run under the failed-file
  tolerance as complete even when `vss_fallback_volumes` is set; the console rules it over tolerance (PARTIAL
  alarm). The agent then waits a full interval instead of retrying with backoff.
- **L-STATS-1**: a run that ends without a restic summary (interrupted, deferred by the watchdog) reports zero
  files and bytes. Report the last progress values instead.
- **L-STATUS-1** (cosmetic): `cryo-agent status` prints the registry location of the untrusted-state verdict as
  "state verdict" even when no verdict exists; "next due: now" ignores the start jitter, so a new device looks due
  for up to 30 minutes.
- **L-LOG-1**: `C:\ProgramData\CryoBackup\logs\agent.log` stayed at 0 bytes on the VM while the service ran.
- Plan correction: Modern Standby was testable after all (the laptop has no S3); covered by 3b.
- HIPAA (from the hosting BAA review, not this run): the maintenance worker keeps a persistent restic cache and
  temp directory, and restores are staged as plaintext archives on the artifacts volume. Ruling: restores stream
  and are never staged; ticketed separately.

## Clean-up

- Laptop: remove the lab gateway certificate from Trusted Root (thumbprint printed by the install helper) and
  uninstall the agent when the operator no longer wants it.
- Lab policy: `gateway_down_grace_minutes` 5 left on the default policy (version 2); fleet cap back to unlimited.
