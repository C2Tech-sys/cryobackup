# M3 lab run - evidence log

Run started 2026-11-09 with the operator, against the production console host (no PHI on it yet), its gateway
and maintenance worker, and the operator's own Windows machines. Hosts are described generically on purpose;
the operator's concrete addresses live outside the repository. Times are UTC unless a column says otherwise.

## Versions

| component | version | where |
| --- | --- | --- |
| console | 0.3.0-rc1 (16a6331), upgraded from 0.2.1 (7a2e3bc) with `deploy/systemd/cryo-upgrade.sh` | console host |
| database | schema 4 -> 6 at the upgrade; `quick_check` and `integrity_check` ok before and after | console host |
| agent MSI | 0.2.1 (tag v0.2.1), signed | lab VMs, operator's lab desktop |
| restic | 0.19.1, unsigned, byte-identical to upstream | bundled in the MSI |

## Results

| # | step | result | evidence |
| --- | --- | --- | --- |
| 1 | upgrade from M2 | PASS | `cryo-upgrade --apply` at 21:54:06Z: stop, database copy, install and start inside one second; copy kept root-only. Doctor after: schema 6, integrity ok, master key loaded. All 3 customers and 10 devices (9 active, 1 retired) still on the board with history. System health: 16 components, each with a status and a reason; Overall NOT CHECKED because SMTP is unconfigured (C03b), which is the intended reading. 9 background jobs OK. Not checked: an existing API token (none at hand); the board during the first minute after start. |
| 2 | gateway down: C01 about 3 min | PASS | `systemctl stop rclone-gateway` at 17:16:12Z. Console journal 17:16:43Z: `health probe failing, probe=gateway, class=refused`. C01 raised 17:19 (System page: Gateway FAILED, raised Tue 10 Nov 11:19 AM local; Overall FAILED), banner "Gateway unreachable: no agent can back up. Last OK 11:15 AM" on every page. Time to alarm: 3 min. Certificates went NOT CHECKED ("No certificate was read from the gateway") rather than OK, which is right. Fleet itself still 100 % within SLA, as the per-device data is unaffected. Gateway started 17:21:19Z; journal 17:21:41Z `health probe passing`; by the 17:24 evaluation C01 was gone, Gateway and Certificates OK, Overall back to NOT CHECKED (SMTP only). Clear in about 3 min. |
| 3 | heartbeats stopped: STALE, UNSEEN, no banner | RUNNING | Agent service stopped and disabled on both lab VMs (the whole lab customer) at 19:53Z on day 2, each with one successful backup behind it (A at 15:33Z, B at 19:12Z). Due: STALE for A after 2026-11-13 15:33Z and for B after 19:12Z; UNSEEN for both after 2026-11-17 19:53Z. Expected meanwhile: OFFLINE on the board, no console banner. |
| 4 | facts-load failure: UNKNOWN + C09 | NOT RUN | Edits the live database; the sheet marks it lab-console only and the operator chose not to run it on the production host. Covered by the fixture mode and unit tests. |
| 5 | agent listener broken: C04 in 5 to 8 min | PASS | Port 8443 rejected with an iptables rule at about 19:17:30Z (ufw alone would not do: it passes loopback, and the self-probe reaches the host's own public name). Journal 19:19:44Z: `agent endpoint probe failing, stage=connect, class=refused` (logged once, on the change). C04 raised 19:25 local: banner "Agent API unreachable: nothing accepts connections on <public host>:8443 (connect). No agent can check in or report. Failing for 7 min." The stage named (connect) matches what was broken. Time to alarm: about 8 min from the block, inside the 5 to 8 min window. Rule removed at about 19:38Z; journal 19:40:44Z `agent endpoint probe passing`; C04 gone by the 19:43 evaluation (under 3 min). While the port was shut every device went OFFLINE on the board (honest: no heartbeats were arriving) and the first agent came back at 19:47, nine minutes after the port opened, because the agent's uplink backoff had climbed (1 min doubling, capped at 30 min); see M3-L9. |
| 6 | maintenance stopped: C07 | PASS | `systemctl stop cryo-maintain` at 17:29:05Z. Journal 17:29:46Z: `health probe failing, probe=maintain-rpc, class=unreachable` and `health probe not checked, probe=storage, class=worker_unreachable` (41 s). By 17:31 the board had C07 "Maintenance worker is not running: retention, restore browsing and usage sampling are off" (critical), Maintenance worker FAILED "did not answer a ping", Overall FAILED, versions "not reachable", and the maintenance-scheduler job reading "due 2 min ago". Storage stayed OK on its last real result (by design: the result ages into C02b; C07 names the worker). Started 17:32:52Z; `health probe passing` 17:33:48Z; by the 17:35 evaluation C07 was gone and the worker read reachable, master key loaded. See M3-L6 and M3-L7. |
| 7 | evaluator stalled: C06 about 3 min | NOT RUN | Same reason as step 4. |
| 8 | unwritable auth dir, retire: pending and C17 | PASS | `chmod 0555` on the gateway auth dir at 17:41Z. Lab VM B retired from its page at 17:59:44Z (reason logged): the page came back as `retired_pending`, `why=io`, reading "Device retired, but its gateway login is not removed. The device token is revoked, but the gateway may still accept this device's login until the sync succeeds. Reason: io." Journal, same second: `job failed, job=htpasswd-sync, class=io`. System page at 18:00: C17 "The gateway login of a retired device is still in the gateway file" (critical) raised 11:59 local, Gateway FAILED, Background jobs WARNING "latest run of job htpasswd-sync failed (io)", htpasswd-sync ERROR io with the next try due in 4 min. Time to alarm: under 20 s. The sync kept retrying (failures logged 18:02, 18:07, 18:13) with the condition held. Directory put back to 0750 between 18:13 and 18:16; the next sync succeeded (job OK, last run 18:27 shown), C17 gone, Gateway OK, Background jobs OK, and the device page now reads "a read of the gateway's file finds no login for it, so no backups can reach the gateway". Note: a successful sync writes no journal line, so the repair is visible only on the board. |
| 9 | old file back: C17, then repaired | PASS | Lab VM B was enrolled again, the live `htpasswd` copied aside (root), and B retired cleanly at 19:05:18Z (`retired_ok`, audit `device.retire` + `device.retire_sync` OK + `gateway.htpasswd.sync`). The saved file, still holding the retired login, was copied back at about 19:05:40Z. Journal 19:06:16Z: `health probe failing, probe=login-file, class=retired_login_in_file` (within 40 s). The next sync run at 19:07 rewrote the file and audited it: `gateway.htpasswd.drift` "Gateway login file drifted" then `gateway.htpasswd.sync` OK; journal 19:07:11Z `health probe passing, probe=login-file`. Board clean again by 19:08 (Gateway OK, one condition left: C03b). The C17 banner itself was not caught on screen: the window between the failing read and the repair was under a minute, so the evidence is the journal and the two audit rows. |
| 10 | SIGTERM exit 0, listener failure exit 1 | PASS (SIGTERM half) | `systemctl stop cryo-console` at 19:11:07Z: `Result=success`, `ExecMainCode=1` (exited), `ExecMainStatus=0`, systemd "Deactivated successfully"; stop took under a second. Listener-failure half (exit 1 with the agent port taken): not run on the production host by the operator's decision; covered by the unit tests. |
| 11 | idle board signs out and says why | PASS (observed) | Not forced with a short `idle_h`: the real limit did it. The operator signed in at 23:01Z on day 1; the next request at 15:34Z on day 2 (over 12 h idle) was sent to the sign-in page, which says "Sessions end after 12 hours idle and 7 days in total." A configured 1 h limit on a lab console was not separately tried. |
| 12 | retire a dead device, delete an old token | PASS | 8 dead lab devices retired from their pages (22:42Z to 22:52Z), each answering within seconds: "token revoked, gateway login gone from the gateway's file". Audit: one `device.retire` and one `device.retire_sync` (result OK) per device, plus the htpasswd sync it triggered; 88 -> 112 rows. 7 tokens (5 expired, 2 revoked) deleted; one `enrollment_token.delete` row each, 112 -> 119. Board after: the retired devices are under "Retired (n)" and raise no alarm; the customer whose last active device went reads MISSING (critical), as §4 says. Journal: no hostnames, no errors. |
| 13 | 7-day dogfood: every outage alarmed | | |
| 14 | sweep | | |

## Lab fleet

Two fresh Windows Server 2025 VMs (cloned from one base image, then given their own hostname, machine GUID
and static address) were enrolled on 2026-11-10 with `deploy/windows/rmm/install-cryobackup.ps1`, the MSI
staged by file copy and the token given through the environment on stdin, never on a command line. Each
install reported "MSI SHA-256 matches", "package 0.2.1; installed none; service absent", "MSI applied",
"installed, enrolled and running", exit 0. Console journal: two `POST /api/v1/agent/enroll` 200 at 15:29:39Z
and 15:29:53Z, then policy fetches and heartbeats from both; no hostnames or paths in the journal. One
earlier `tls_handshake_failed` / `eof` warning on the agent listener was a plain TCP reachability probe from
the lab, not an agent. VM B was retired and enrolled again three times for steps 8 and 9 (each time: stop the
service, remove the state directory, run the wrapper again; a retired agent cannot re-enroll in place, by
design), which used all five enrollments of the lab token. The seven-day fleet is VM A, the final VM B and the
operator's lab desktop.

## Findings

Fixed on day 2, after the drills, each blind-verified: L1 and L5 (deploy), L2 (audit "via web"), L6 and L7 (one rule
for the CHECKED column: the newest attempt that produced evidence), L8 (shutdown log lines). Open: L3 (operator
procedure; release channel is M5 work). L4 and L9 were decided by the operator on day 2: yes to both. A retired
agent must stop polling once the console has answered 401 to its device token repeatedly, and the uplink backoff
must not keep a fleet away for half an hour after the console is back. Both are agent changes for the v0.3.0 agent.

- **M3-L1** (polish): `cryo-upgrade` runs its "doctor after" in the same second the units start, so a clean upgrade
  reports the two listeners as free and the maintenance worker as unreachable. Wait for the units to be ready, or
  for the worker socket, before the after-check.
- **M3-L2** (polish): the audit rows for a retire and a token delete done from the web UI read `via api`, while a
  token create or revoke from the same UI reads `via web`.
- **M3-L3** (operator): the console's artifacts directory still held the 0.2.0 MSI, so the enrollment wizard
  printed the 0.2.0 file name and hash while the fleet runs 0.2.1. The console picks the highest version it finds;
  the fix is to place the current MSI there. A release-channel step for this is M5 work.
- **M3-L4** (design question): an agent on a retired device keeps polling `GET /api/v1/agent/policy` every 3 min
  with its revoked token and gets 401 each time (seen from one of the devices retired in step 12, from before the
  lab VMs enrolled). The console side is right; decide whether a retired agent should back off or stop.
- **M3-L5** (polish): `systemctl stop rclone-gateway` leaves the unit `failed` (rclone exits 143 on SIGTERM) rather
  than `inactive`. Add `SuccessExitStatus=143` to `rclone-gateway.service`.
- **M3-L6** (polish, corrected): first read as a wrong RAISED time. The RAISED column holds only the condition id;
  the time beside it is the CHECKED column. For the failed Maintenance worker row CHECKED read 11:28 (the last pass)
  while failing checks had run at 11:29:46 and 11:30:46. So CHECKED does not mean "newest check": together with
  M3-L7 the column needs one rule, "the newest attempt that produced evidence, pass or fail; a skipped attempt never
  moves it".
- **M3-L7** (polish): while the worker was down, the Storage row's CHECKED time kept advancing (11:31) although no
  storage probe had run since before the stop: a skipped probe should not refresh "checked". The status itself (OK on
  the last real result, ageing into C02b) is as designed.
- **M3-L8** (polish): a clean stop writes no console log line of its own; only systemd's "Deactivated
  successfully" says it stopped. One line at shutdown (what was drained, how long it took) would help.
- **M3-L9** (design note): after a 20-minute console outage the agents came back one by one over the following
  half hour, as their uplink backoff (1 min doubling to 30 min) ran out. The board read OFFLINE for them meanwhile,
  which is true, but an operator who has just fixed the console sees the fleet "come back" slowly. A shorter cap, or
  a reset when the console answers again, is worth weighing against the thundering-herd protection the cap gives.
- Doctor warns that the master key file is readable by group or others (`chmod 0400`); unchanged by this run.
- `config.json` names a display zone that differs from the one saved in the console; the saved one wins (ADR 0012),
  so the display is right and the config line is stale.

## Seven-day log (step 13)

| day | date (UTC) | what was done or broke | when | alarm seen (id and text) | when seen | time to alarm | cleared at | notes |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 2026-11-09 | upgrade to 0.3.0-rc1; lab cleanup (8 retires, 7 token deletes) | 21:54 | C03b "Alert e-mail not checked" (warning, SMTP unconfigured); MISSING customer (critical) after the last self-test device was retired | 22:29 / 22:58 | - | - | both are true conditions, not outages |
| 2 | 2026-11-10 | two lab VMs enrolled (15:29Z); gateway stopped (step 2) | 17:16 | C01 "Gateway unreachable: no agent can back up" (critical) | 17:19 | 3 min | 17:24 | admin session had expired overnight (12 h idle), sign-in page says so |
| 2 | 2026-11-10 | maintenance worker stopped (step 6) | 17:29 | C07 "Maintenance worker is not running" (critical) | by 17:31 | under 2 min | 17:35 | storage stayed OK on its last real result (by design) |
| 2 | 2026-11-10 | gateway auth dir read-only, device retired (step 8) | 17:59 | C17 "Gateway login not removed" (critical); job htpasswd-sync ERROR io | 18:00 | under 20 s | 18:27 | cleared on the first sync after the directory was repaired |
| 2 | 2026-11-10 | stale htpasswd copied back (step 9) | 19:05 | login-file probe `retired_login_in_file`; audit `gateway.htpasswd.drift` | 19:06 | 40 s | 19:07 | repaired by the next sync run; banner window under a minute |
| 2 | 2026-11-10 | console stopped and started (step 10) | 19:11 | none expected; agents reconnected | - | - | 19:13 | exit 0 |
| 2 | 2026-11-10 | agent port 8443 rejected (step 5) | 19:17 | C04 "Agent API unreachable: nothing accepts connections on <host>:8443 (connect)" (critical); all devices OFFLINE | 19:25 | 8 min | 19:43 | agents back between 19:47 and 19:50 (uplink backoff) |
| 2 | 2026-11-10 | agents stopped on both lab VMs (step 3 begins) | 19:53 | none expected (devices OFFLINE) | | | | STALE due day 5, UNSEEN due day 9 |
| 3 | | | | | | | | |
| 5 | 2026-11-13 | | | | | | | STALE due for the devices of step 3 (15:33Z and 19:12Z) |
| 4 | | | | | | | | |
| 5 | | | | | | | | |
| 6 | | | | | | | | |
| 7 | | | | | | | | |
| 9 | 2026-11-17 | | | | | | | UNSEEN due for the devices of step 3 (19:53Z) |
