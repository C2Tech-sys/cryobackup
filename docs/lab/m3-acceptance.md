# M3 lab acceptance procedure (state engine, monitoring, fleet board)

For the operator. Run it against a real console, a real gateway and real Windows machines, not fixtures: CLAUDE.md
requires a phase to be exercised on real machines before it is called done. Hosts are described generically; keep
concrete addresses outside the repository. Record results in `docs/lab/m3-run.md` in the format of `m1-run.md` (one
table row per step: result PASS/FAIL, evidence).

The point of M3 is that silence becomes visible, so every drill below breaks something and then looks for the alarm.
A drill also fails if the board stays green while the thing is broken, whatever the banner text says.

Decisions this procedure checks: ADR 0013 (self-probe), ADR 0015 (banners in the page flow), ADR 0016 (device retire).

Each step is marked **operator** (a person does it on real machines) or **automated** (a test covers it; the step
says which, and running it again here is optional). **sudo** marks a step that needs root on the console host. The
commands assume the systemd units in `deploy/systemd/`; under compose, use the matching `docker compose` commands.

## Setup

- A console built from the M3 merge commit, its gateway (`rclone-gateway.service`), its maintenance worker
  (`cryo-maintain.service`), and a storage target.
- `public_url` set to the address agents use, and a real certificate on the agent listener, or the self-probe answers
  C04b ("not self-checked") and step 5 cannot run.
- Two or more enrolled Windows machines that have finished a backup, in one customer, plus one machine you can retire.
- One enrollment token past its use: revoked or expired.
- A terminal on the console host: `journalctl -fu cryo-console`.
- The default intervals: health-prober 60 s, agent self-probe 180 s, alarm-evaluator 60 s, htpasswd-sync 5 min, page
  refresh 30 s. The timings below are for these; a changed interval changes them.
- The drills that edit the database are for a **lab console only**. Take a copy of the database first.

## Step 1. Upgrade from M2 (operator, sudo)

| | |
| --- | --- |
| Do | Note `cryo-console doctor` output. Stop the console and the worker. Replace the binary. Start both. |
| Expect | The console starts, applies its migrations, and `doctor` shows the new schema version, `quick_check` ok and the master key loaded. Devices enrolled under M2 stay on the board with their history; agents need no change. |
| Look | **System health** (every row has a status and says why). Audit log. Existing API tokens keep working. |
| Fail if | A device or customer disappears; any row reads `ok` with its check not having run; the board shows green before the first evaluation has finished (C06b "Alerting has not run yet" is the expected reading for the first minute). |

## Step 2. Kill the gateway: C01 (operator, sudo)

| | |
| --- | --- |
| Do | `sudo systemctl stop rclone-gateway`. Note the time. |
| Expect | Within **about 3 minutes** (3 failed probes, 60 s apart), a critical banner on every page: **"Gateway unreachable: no agent can back up."** The all-clear state is gone from the board. The **gateway** row on System health reads failed. |
| Also | Backups that start meanwhile fail and the devices go to FAILED, with the gateway as the cause, not a generic error. |
| Undo | `sudo systemctl start rclone-gateway`. The banner clears at the next probe (within 60 s). |
| Fail if | No banner after 5 minutes; the board reads all clear with the banner up. |

## Step 3. Stop all heartbeats: STALE, then UNSEEN (operator)

| | |
| --- | --- |
| Do | Stop the agent service on every machine of one customer (or power them off). Leave them stopped. |
| Expect | **No console banner**: the console itself is healthy, and a banner would be wrong. With the default thresholds the devices turn **STALE** when their last success is older than **72 hours** and **UNSEEN** when nothing has been heard for **168 hours (7 days)**. When a whole customer goes silent the Devices not seen page collapses it to one line for the customer. |
| Note | These thresholds are long on purpose, so this step finishes inside the 7-day run in step 13. Keep a row in its log. |
| Fail if | A device with no heartbeat for 72 hours and no recent success still reads OK. |

## Step 4. Facts-load failure: UNKNOWN and C09, never green (operator, sudo, lab console only)

| | |
| --- | --- |
| Do | On the lab database, with the console running: `sudo -u cryobackup sqlite3 /var/lib/cryobackup/console.db 'ALTER TABLE device_presence RENAME TO device_presence_drill;'` (use your `db_path`). |
| Expect | Within **one refresh** (30 s): every device reads **UNKNOWN**, and a critical banner reads **"Partial data: N devices could not be classified (N load errors)"**. The board is not green and shows no all-clear. |
| Undo | `sudo -u cryobackup sqlite3 <db> 'ALTER TABLE device_presence_drill RENAME TO device_presence;'`. The states and the banner recover by themselves within one refresh. |
| Fail if | Any device reads OK while the table is gone. |

## Step 5. Break or stop the agent listener: C04 (operator, sudo)

| | |
| --- | --- |
| Do (certificate) | Point `public_url`'s DNS or the proxy in front of it at a host that does not answer, or set `agent_probe.ca_file` to a CA that did not sign the listener's certificate, and restart the console. |
| Do (stop) | Stop the proxy or firewall the port. The console's own process keeps running, so nothing but the self-probe can see this. |
| Expect | After **about 5 to 8 minutes** (at least 2 failed runs, 180 s apart, spanning at least 5 minutes) a critical banner: **"Agent API unreachable: <stage>. No agent can check in or report. Failing for N min."** naming the failing stage. An expired certificate skips the wait. |
| Undo | Restore it. The banner clears at the next self-probe (within 3 minutes). |
| Fail if | No banner after 10 minutes; a stage that does not match what you broke. |

## Step 6. Stop maintenance: C07 (operator, sudo)

| | |
| --- | --- |
| Do | `sudo systemctl stop cryo-maintain`. Note the time. |
| Expect | Within **about 60 s** (the next RPC probe) a critical banner: **"Maintenance worker is not running: retention, restore browsing and usage sampling are off."** The **maintenance** row reads failed. |
| Also (scheduler only) | If the worker answers its socket but its scheduler has stalled, the same banner comes when the scheduler has not finished for **30 minutes**. The System health job table marks `maintenance-scheduler` overdue and the jobs row names it at the same 30 minutes, not sooner. |
| Undo | `sudo systemctl start cryo-maintain`. The banner clears at the next probe. |
| Fail if | No banner after 3 minutes; the table calls the scheduler overdue at 2 minutes while no banner is up. |

## Step 7. Stall the evaluator: C06 (operator, sudo, lab console only)

| | |
| --- | --- |
| Do | Hold the job's lease so no run can take it: `sqlite3 <db> "UPDATE job_state SET lease_holder='lab-drill', lease_until=(strftime('%s','now')+3600)*1000 WHERE name='alarm-evaluator';"` |
| Expect | After **about 3 minutes** (3 periods of 60 s): a critical banner: **"Alerting is stalled: this view and notifications may be stale."** with the time of the last evaluation. |
| Undo | `UPDATE job_state SET lease_holder=NULL, lease_until=NULL WHERE name='alarm-evaluator';`. The banner clears after the next evaluation (within 60 s). |
| Fail if | No banner after 5 minutes. |

## Step 8. Unwritable gateway auth dir, then retire: pending and C17 (operator, sudo)

| | |
| --- | --- |
| Do | `sudo chmod 0555 <gateway_auth_dir>` (the sync cannot replace the file; the file stays readable). Retire a device from its page: **Retire**, a reason, type the hostname. |
| Expect | The answer comes back within **about 5 s** (up to 7 s, longer only if the disk hangs in a commit; ADR 0016). The page says the device is retired and the gateway login is **not removed**, with the reason class. The device is RETIRED. |
| Expect | The critical banner within **60 s** (the next login-file read): **"The gateway login of a retired device is still in the gateway file: the gateway may still accept it until the sync succeeds."** |
| Look | Audit log: one `device.retire` and one `device.retire_sync` with result pending. |
| Undo | `sudo chmod 0750 <gateway_auth_dir>` (the mode the unit's StateDirectoryMode sets; use your original if it differs). Within about **5 minutes** (the next htpasswd-sync, 4.5 to 5.5 min with the runner's jitter) the login leaves the file, the banner clears at the next read, and the device page says no login remains. |
| Fail if | The answer claims the login was removed; no banner; the retire was refused because the file could not be written. |

## Step 9. The old file put back: C17 (operator, sudo)

| | |
| --- | --- |
| Do | Before retiring a device, `sudo cp -p <gateway_auth_dir>/<file> /root/htpasswd.old`. Retire it and wait until the page says no login remains. Put the old copy back: `sudo cp -p /root/htpasswd.old <gateway_auth_dir>/<file>`. Delete the copy afterwards: it holds password hashes. |
| Expect | Within **60 s** the same critical C17 banner: the deployed file, not the database, is the proof of a removed login. The next htpasswd-sync (about 5 minutes, at most 5.5) repairs the file, and the banner clears. |
| Fail if | No banner while a retired login is in the file. |

## Step 10. Real signals under systemd (operator, sudo; also automated)

| | |
| --- | --- |
| Do (clean stop) | With agents connected and a board open, `sudo systemctl stop cryo-console`, then `systemctl show -p ExecMainStatus cryo-console`. |
| Expect | `ExecMainStatus=0`. It stops within **65 s** (`shutdownWorstCase`), usually a few seconds. |
| Do (listener failure) | `sudo ss -K state listening sport = :<agent port>` kills the listening socket (needs a kernel with `CONFIG_INET_DIAG_DESTROY`; if it reports nothing killed, record the step as not run and rely on the automated test). |
| Expect | Journal: `listener failed` with `"listener":"agent"` and class `listener_failed`. The process exits within 65 s. (If the listener that dies is the one whose every connection slot is held, the death is noticed only when a slot frees.) `ExecMainStatus=1`, and `Restart=on-failure` starts it again 5 s later. |
| Automated | `TestServeEndsOnCancelWithEverySlotHeld`, `TestServeEndsOnListenerFailureWithTheOtherFull`, `TestServeListenerFailureEndsRunWithAnError`, `TestServeExitCode`. |
| Fail if | Exit 0 after a listener failure; a console left running with a dead listener and no log line. |

## Step 11. An idle board signs out and says why (operator)

Checks the M3-HARDEN change: neither an event stream nor a page's background refresh counts as activity.

| | |
| --- | --- |
| Do | On a lab console set `session.idle_h` to 1 and restart. Sign in and open the board. Touch nothing for 65 minutes, with the tab visible. |
| Expect | Within **15 s** of the hour (the next ping) the page moves by itself to the sign-in page, which reads **"You were signed out."** No customer or device name stays on the screen. Signing in shows the current state. |
| Also | Sign out in a second tab: the first follows within 15 s. Click around during the hour instead: the session stays alive. |
| Automated | `TestUIEventStreamDoesNotKeepAnIdleSessionAlive`, `TestABackgroundRefreshDoesNotKeepAnIdleSessionAlive`, `TestM3EventStreamDoesNotKeepAnIdleSessionAlive`. |
| Fail if | The board is still updating after the timeout; the sign-in form appears inside the board frame; the page sits stale with no word. |

## Step 12. Retire a dead device and delete an old token (operator)

| | |
| --- | --- |
| Do | Retire a device that is really gone (reason, type the hostname). On **Enroll**, filter to revoked or expired tokens and delete one. |
| Expect | The device leaves every alarm and health count and collapses under **Retired (n)** on each list. Its data stays until retention. If it was an expected customer's last active device the customer reads MISSING, or ONBOARDING inside its window. The token leaves the list, its audit trail and each device's enrolled-via link remain. An open board in another tab updates within seconds. |
| Look | Audit log: `device.retire`, `device.retire_sync` with result removed, and the token delete: one row each. |
| Fail if | A retired device still raises an alarm; a live token can be deleted. |

## Step 13. Seven days of dogfood (operator)

Run on the operator's own machines for 7 days with induced outages. Every outage must produce a visible alarm. Copy
this log into `docs/lab/m3-run.md`, one row per event, including the days nothing happened.

| day | date (UTC) | what was done or broke | when | alarm seen (id and text) | when seen | time to alarm | cleared at | notes |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | | | | | | | | |
| 2 | | | | | | | | |
| 3 | | | | | | | | STALE due for the devices of step 3 |
| 4 | | | | | | | | |
| 5 | | | | | | | | |
| 6 | | | | | | | | |
| 7 | | | | | | | | UNSEEN due for the devices of step 3 |

At the end: any outage with no alarm fails the milestone. So does any alarm with no outage behind it that recurs. Note
each machine's backup count and last restore check.

## Step 14. Sweep

- Audit log: every action above has exactly one row (actor, action, object, result); the retire has its two.
- Console log: no client hostnames in free text, no paths, no tokens or hashes (`journalctl -u cryo-console`).
- The drill leftovers are gone: the database copy, the `.old` login file, the renamed table, the held lease.
- Restore `session.idle_h` and any changed `agent_probe` or `public_url` setting.

## Results table (copy into m3-run.md)

| # | step | result | evidence |
| --- | --- | --- | --- |
| 1 | upgrade from M2 | | |
| 2 | gateway down: C01 about 3 min | | |
| 3 | heartbeats stopped: STALE, UNSEEN, no banner | | |
| 4 | facts-load failure: UNKNOWN + C09 | | |
| 5 | agent listener broken: C04 in 5 to 8 min | | |
| 6 | maintenance stopped: C07 | | |
| 7 | evaluator stalled: C06 about 3 min | | |
| 8 | unwritable auth dir, retire: pending and C17 | | |
| 9 | old file back: C17, then repaired | | |
| 10 | SIGTERM exit 0, listener failure exit 1 | | |
| 11 | idle board signs out and says why | | |
| 12 | retire a dead device, delete an old token | | |
| 13 | 7-day dogfood: every outage alarmed | | |
| 14 | sweep | | |
