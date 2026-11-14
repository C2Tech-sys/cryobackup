# M3 lab run - evidence log

Run started 2026-11-09 with the operator, against the production console host (no PHI on it yet), its gateway
and maintenance worker, and the operator's own Windows machines. Hosts are described generically on purpose;
the operator's concrete addresses live outside the repository. Times are UTC unless a column says otherwise.

## Versions

| component | version | where |
| --- | --- | --- |
| console | 0.3.0-rc1 (16a6331), upgraded from 0.2.1 (7a2e3bc) with `deploy/systemd/cryo-upgrade.sh`; then 0.3.0-rc2 (e008f3b, the day-2 fixes) on day 2 at 21:46Z the same way; then 0.3.0-rc3 (7c3d34e, the login wordmark fix and self-service username/display name) on day 3 at 15:07Z the same way | console host |
| database | schema 4 -> 6 at the upgrade; `quick_check` and `integrity_check` ok before and after | console host |
| agent MSI | 0.2.1 (tag v0.2.1), signed | lab VMs, operator's lab desktop |
| restic | 0.19.1, unsigned, byte-identical to upstream | bundled in the MSI |

## Results

| # | step | result | evidence |
| --- | --- | --- | --- |
| 1 | upgrade from M2 | PASS | `cryo-upgrade --apply` at 21:54:06Z: stop, database copy, install and start inside one second; copy kept root-only. Doctor after: schema 6, integrity ok, master key loaded. All 3 customers and 10 devices (9 active, 1 retired) still on the board with history. System health: 16 components, each with a status and a reason; Overall NOT CHECKED because SMTP is unconfigured (C03b), which is the intended reading. 9 background jobs OK. Not checked: an existing API token (none at hand); the board during the first minute after start. |
| 2 | gateway down: C01 about 3 min | PASS | `systemctl stop rclone-gateway` at 17:16:12Z. Console journal 17:16:43Z: `health probe failing, probe=gateway, class=refused`. C01 raised 17:19 (System page: Gateway FAILED, raised Tue 10 Nov 11:19 AM local; Overall FAILED), banner "Gateway unreachable: no agent can back up. Last OK 11:15 AM" on every page. Time to alarm: 3 min. Certificates went NOT CHECKED ("No certificate was read from the gateway") rather than OK, which is right. Fleet itself still 100 % within SLA, as the per-device data is unaffected. Gateway started 17:21:19Z; journal 17:21:41Z `health probe passing`; by the 17:24 evaluation C01 was gone, Gateway and Certificates OK, Overall back to NOT CHECKED (SMTP only). Clear in about 3 min. |
| 3 | heartbeats stopped: STALE, UNSEEN, no banner | RUNNING | Agent service stopped and disabled on both lab VMs (the whole lab customer) at 19:53Z on day 2, each with one successful backup behind it (A at 15:33Z, B at 19:12Z). Due: STALE for A after 2026-11-13 15:33Z and for B after 19:12Z; UNSEEN for both after 2026-11-17 19:53Z. Expected meanwhile: OFFLINE on the board, no console banner. Read at 02:03Z on day 6: both devices `STALE 3d` and OFFLINE on the fleet page, no banner (the time each turned STALE was not captured). UNSEEN still to come. |
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
| 15 | retired agent: REFUSED after three 401s, one line, one 1008, hourly probe, backups defer (agent 0.3.0) | PASS, with finding L10 | A lab desktop with 8 runs behind it was upgraded to the signed agent 0.3.0 with the RMM wrapper and retired from its page at 23:41:36Z on day 3 (`retired_ok`, login gone from the gateway file in the same second; its customer read MISSING). Console journal: the command long poll in flight and one re-poll answered 401 at 23:41:36Z, then heartbeats answered 401 at 23:41:45Z, 23:42:45Z and 23:44:52Z (the 1 min and 2 min backoff steps), one policy pull (23:43:21Z) and one command poll (23:43:41Z) answered 401 before the third. At 23:44:52Z the agent logged one ERROR `the console refuses the device token`, `class=token_refused`, `since=2026-11-11T23:41:45Z`, wrote one Event Log entry 1008 with the same sentence, and `cryo-agent status` showed the TOKEN REFUSED block with the time and what to do. Sixteen hours later: still exactly one 1008 entry and one ERROR line; 28 heartbeats answered 401 in the journal over those 16 h (about 1.75 an hour: the hourly probe plus the heartbeat each finished run kicks, as ADR 0018 says). Every scheduled run logged `start lease skipped: the console refuses the device token` and ended `outcome=deferred class=auth exit=1` within a second (the gateway turned it away), first on the 5 to 60 min ladder (23:54Z, 23:59Z, 00:10Z, 00:30Z, 01:11Z), then about hourly. The service kept running; nothing was deleted. The agent's own log has no failed policy pull, command poll or run upload after 23:44:52Z, so those loops went silent. (The console journal does show 340 policy requests answered 401 over the same hours, one every 3 min. Those are the console's own self-probe, which asks the agent API for the policy without a token every 3 min and takes the 401 as proof the API answers; see finding L13. No other agent was involved.) |
| 16 | console stopped 10 min: fleet ONLINE within 5 min of the start (agent 0.3.0) | PASS on the return time, with finding L12 | One device (the lab desktop, enrolled again as a new device on agent 0.3.0; the two lab VMs are held for step 3). `systemctl stop cryo-console` at 16:58:44Z, started 600 s later (about 17:08:44Z; the start time itself was not captured). Agent log: heartbeat, command poll and policy pull failed with `network error` from 16:59:49Z, their waits climbing 116 s, 206 s, 246 s to the 300 s cap and no further. The heartbeat due at 17:09:13Z was answered: `console reachable again from=heartbeat` at 17:09:14Z, about 30 s after the start. Under the old 30 min cap the same heartbeat would have been waiting 8 min or more. The wake-all path was not exercised (the heartbeat itself was first). Finding L12: two false `console reachable again from=outbox` lines during the outage (17:02:13Z, 17:03:43Z). |

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
- **M3-L4, as built** (agent 0.3.0, ADR 0018): see step 15. Three heartbeats answered 401 put the agent in REFUSED;
  it says so once and asks once an hour.
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
  Done in rc2 and seen live when rc3 replaced it: "the console has stopped", took_ms 5.
- **M3-L9** (design note): after a 20-minute console outage the agents came back one by one over the following
  half hour, as their uplink backoff (1 min doubling to 30 min) ran out. The board read OFFLINE for them meanwhile,
  which is true, but an operator who has just fixed the console sees the fleet "come back" slowly. A shorter cap, or
  a reset when the console answers again, is worth weighing against the thundering-herd protection the cap gives.
- Doctor warned that the master key file was readable by group or others; the operator set owner and mode
  (`chmod 0400`) on day 3 and doctor reads the key as OK since.
- `config.json` names a display zone that differs from the one saved in the console; the saved one wins (ADR 0012),
  so the display is right and the config line is stale.
  The operator corrected the config line on day 3; the startup WARN was gone at the rc3 restart.
- **M3-L10** (design question, from step 15): a retired device's agent starts restic about once an hour for
  good (the run ladder caps at 60 min), each run turned away by the gateway in under a second and each one
  kicking a heartbeat that the console answers 401. Bounded and cheap, and what ADR 0018 chose, but a device that
  is retired for months writes an hourly deferred run. Decide whether class `auth` deferrals while REFUSED should
  stretch further (for example to the policy interval).
- **M3-L11** (polish, from step 15): the retired-device page says the agent "keeps trying until you do"
  (uninstall), which is true of 0.2.1 and no longer of 0.3.0; and neither that page nor the agent's status block
  says how to re-enroll (uninstall, move the state directory aside, install with a new token).
- **M3-L12** (bug, from step 16): while the console was down the agent twice logged `console reachable again
  from=outbox` and woke every loop, which then failed again. The run-upload loop counts a pass with nothing due
  (its one record had just been put back with a later retry time) as a success after failures. No request had
  been answered. The line must only follow a request the console answered.
- **M3-L10, L11, L12, as built** (agent 0.3.1): the run-upload loop reports a
  recovery and resets its backoff only after a request the console answered (L12); a refused agent attempts a
  backup once per policy interval instead of about hourly (L10, operator ruling, ADR 0018 amended); the status
  block, the retired-device page and the retire dialog say what a 0.3.0 or later agent does and how to enroll
  the machine again (L11).
  Drill started on day 4: the lab desktop, upgraded in place to agent 0.3.1 (console 0.3.0-rc4), was retired again;
  last accepted heartbeat 23:26:04Z, `TOKEN REFUSED since 2026-11-12 23:27 UTC` in `cryo-agent status` with the new
  three-line `To fix` text, one new Event Log 1008 (two in the log, the other from step 15). Its next run is due
  2026-11-13 17:56Z; the L10 result (one deferred run, then a day's wait) is read after that.
  L10 result, read at 01:53Z on day 6 (PASS): one run in the 26 h since the retire. It started at 18:00:38Z on
  day 5, logged `start lease skipped`, and ended `outcome=deferred class=auth exit=1` in under a second. No
  second attempt in the 7 h 52 min after it (agent 0.3.0 made five in its first 77 min, step 15). Status:
  `last run: deferred (auth), attempt 1`, `next due: 2026-11-14T18:52:58Z (not_yet)`, 24 h 52 min after the
  deferral; one run record queued; still one ERROR line. The service kept running. The run is marked
  `trigger boot` although the machine did not restart: the planner labels the first run after a service start
  that way, and the service last started with the 0.3.1 upgrade on day 4. The 4 min between the planned 17:56Z
  and the start is not explained. Not checked: the Event Log 1008 count after the wait, and the end of REFUSED (the ladder resuming
  from the last deferral), which a retired device cannot show.
- **M3-L13** (polish, from step 15): the console's self-probe shows in its own request log as
  `GET /api/v1/agent/policy` answered 401 every 3 min, with nothing to tell it from an agent holding a dead
  token. On day 4 that cost an hour looking for a machine that did not exist (the cadence re-anchoring to the
  console restart gave it away). The request line should say it is the probe, or the probe should not be logged
  as an agent request at all. The same pattern was half of what finding L4 first reported.

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
| 2 | 2026-11-10 | console upgraded rc1 -> rc2 with the fixed cryo-upgrade (units stopped 21:46:12, started 21:46:22) | 21:46 | none; self-probe passing 1 s after start | - | - | 21:46 | the L1 fix held live: "doctor after" ran 2 s after start and read both listeners as busy and the worker as reachable at the first try. Stopping rc1 took 10 s (its drain grace; rc2 will log what it waited for). |
| 3 | 2026-11-11 | console upgraded rc2 -> rc3 with cryo-upgrade (units stopped 15:07:11, started 15:07:21); master key file mode and the config display zone corrected by the operator before it | 15:07 | none; self-probe passing at start | - | - | 15:07 | rc2 logged its stop as asked under L8 ("the console has stopped", took_ms 5). The stale-zone startup WARN is gone. One `tls_handshake_failed` (not_tls, count 1) on 8443 at 15:07:54: something spoke plain HTTP to the agent port once; not an agent. The operator confirmed at 16:55Z that the login wordmark no longer flashes on reload and changed his own username and display name from the account page. |
| 3 | 2026-11-11 | lab desktop upgraded to agent 0.3.0, then retired (step 15) | 23:41 | device RETIRED; its customer MISSING (critical): "marked expected but has no active devices" | 23:41 | seconds | - | the agent went REFUSED at 23:44:52Z; true condition, not an outage |
| 4 | 2026-11-12 | nothing broken; the retired lab desktop left running overnight | - | MISSING customer still shown | - | - | - | one hourly 401 probe plus one per deferred run in the journal; no other alarm |
| 4 | 2026-11-12 | lab desktop enrolled again (new device row, agent 0.3.0); console stopped for 10 min (step 16) | 16:58 | none expected for a 10 min stop; the device read OFFLINE | - | - | 17:09 | agent back 30 s after the start, at its 5 min cap |
| 5 | 2026-11-13 | nothing broken; step 3 devices still stopped; the retired lab desktop on agent 0.3.1 (drill) | - | both step 3 devices STALE 3d, OFFLINE (read 02:03Z on day 6); MISSING customer still shown | not captured | - | - | the retired desktop made one deferred run all day (L10) |
| 4 | | | | | | | | |
| 5 | | | | | | | | |
| 6 | | | | | | | | |
| 7 | | | | | | | | |
| 9 | 2026-11-17 | | | | | | | UNSEEN due for the devices of step 3 (19:53Z) |
