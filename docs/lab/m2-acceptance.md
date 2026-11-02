# M2 lab acceptance procedure (console core)

For the operator. Run it against a real console and real Windows machines, not fixtures: CLAUDE.md requires a phase
to be exercised on real machines before it is called done. Hosts are described generically; keep concrete addresses
outside the repository. Record results in `docs/lab/m2-run.md` in the format of `m1-run.md` (one table row per step:
result PASS/FAIL, evidence).

Decisions this procedure checks: ADR 0009 (policy previews), ADR 0010 (device commands), ADR 0011 (identity-conflict
detection, token policy), ADR 0012 (operator settings in the database).

## Setup

- A console built from the M2 merge commit, with a real database (`cryo-console init` done), a gateway, and a
  storage target. Note `cryo-console doctor` output (schema version, `quick_check`, master key loaded).
- Three Windows VMs (Windows 10/11 or Server), snapshotted clean, on a hypervisor you can clone and migrate on.
  Call them VM-A, VM-B, VM-C. Time-synchronised with the console host (note the clock offset the device page shows).
- One customer with a storage target. Two accounts: your owner, and a second admin for the two-admin steps.
- A stopwatch, or a terminal on the console host to compare timestamps (the Commands tab shows delivered and
  answered times in the display zone).
- Screens referenced below are in `docs/screens/m2/README.md`.

## Step 1. First login with MFA

| | |
| --- | --- |
| Do | Sign out. Sign in as a new account with only a password (create it on **Accounts and roles**, give it the one-time link). |
| Expect | After the password you land on **Authenticator setup** (QR and a manual key), not on the board. A wrong code is refused with the generic message and the page stays. The right code shows **10 recovery codes once**; **Continue** goes to the board. |
| Expect | A later login asks for the authenticator code after the password. The same code cannot be used twice (wait for the next one). A recovery code works once. |
| Look | Audit log: `admin.mfa_enroll`, `auth.login` rows, none containing a code or key. **My account** shows MFA enrolled, recovery codes left (10, then 9 after one is used). |
| Fail if | Any page except login and the MFA pages renders before the code step; a key or code appears in the console log or an audit row. |

## Step 2. Three VMs enrolled from one RMM script, zero per-device clicks

| | |
| --- | --- |
| Do | **Enroll**: pick the customer, expiry 24 hours, "Up to 5 devices", create the token. Copy the **RMM script variables** from the created page into one RMM script (`deploy/windows/rmm/install-cryobackup.ps1`, token as a secret variable). Put the MSI on **Downloads**' artifacts path, check the SHA-256 shown there matches `MsiSha256`. |
| Do | Run that single script on VM-A, VM-B and VM-C from the RMM. Do not touch the VMs otherwise. |
| Expect | Each VM exits 0 (`installed, enrolled and running`), appears on **Devices** within a minute, and on the board as pending (first backup due) then OK after its first run. Zero prompts, zero per-device edits. |
| Expect | The token detail page shows used 3 of 5. Each enrollment is one `device.enroll` audit row; the token is never shown again after the created page. |
| Look | **Enroll > token > Details**, **Devices**, Audit log filter `device.enroll`. |
| Fail if | Any VM needs a manual step; the token appears on a command line, in the RMM log or in the audit log. |

## Step 3. Back up now reaches an online device within 30 s

| | |
| --- | --- |
| Do | On VM-A (online, idle): device page > **Back up now**. Note the wall-clock time you click. |
| How to time | Open the **Commands** tab. Each row shows when the command was queued to the second (for example `Wed 21 Oct, 1:12:05 PM`) and, once delivered, `delivered 1:12:07 PM (after 2 s)`. That "after N s" is delivered minus queued, and is the number to record. Cross-check once with a stopwatch from your click to the board flipping to running. Repeat 5 times, 1 minute apart, and record the maximum. |
| Expect | Delivered within 30 s every time (ADR 0010 expects milliseconds: queueing wakes the agent's open long-poll). Status goes queued, delivered, then succeeded ("The backup started."). |
| Also | Click **Back up now** twice quickly: one command, the second click shows the existing one ("coalesced", no second row). Click **Cancel run** while it runs and confirm: the Commands tab shows "The run was stopped"; if a newer run had started you see "A newer run had started; it was left running." |
| Offline case | Stop the agent service on VM-B, queue **Back up now**: row stays "Waiting for the device to check in", device shows offline. Start the service: the command is delivered at the next poll and runs (it is valid 4 h). Queue **Check in** and let it expire (15 min): it shows expired, "The device did not receive it in time." |
| Fail if | Any delivery over 30 s on an online device; a second row for a repeated click. |

## Step 4. Identity conflict: clone a VM disk and boot both

| | |
| --- | --- |
| Do | Power off VM-C. Clone its disk to a new VM (VM-C2) on the same network with a new hardware identity (a normal hypervisor clone gets a new SMBIOS UUID; if yours keeps the UUID, regenerate it, otherwise there is nothing for the console to see). Boot both. |
| Expect | Within about one heartbeat (default 20 s) the clone's different `hw_fingerprint` sets the flag **once**. Board: the device shows IDENTITY_CONFLICT. Device page: the "Resolve this identity conflict" banner with the time. The Audit log has one critical `device.identity_conflict` row (no fingerprint in it); further heartbeats do not add rows. |
| Expect | **Backups continue on both machines.** Detection never locks anyone out (ADR 0011). Check that VM-C's next scheduled run still succeeds. |
| Resolve, keep | **Resolve conflict > Keep this identity**. The flag clears (`device.identity_resolve`, keep). The recorded fingerprint is cleared, so the next machine to heartbeat becomes the identity; because both are alive the flag returns on the other machine's next heartbeat. That is expected: the real fix is retire. |
| Resolve, retire | **Resolve conflict > Retire**, type the hostname. The device is retired, its token and gateway login revoked, the gateway htpasswd sync runs at once. Both machines stop backing up and report an auth failure; enroll the real machine again with a fresh token (token rotation is M5). |
| Look | Board, device page, Audit log (`device.identity_conflict`, `device.identity_resolve`). |
| Fail if | The flag raises twice for one event; a heartbeat is refused; the fingerprint is visible anywhere in the console. |

## Step 5. Fingerprint stability: BIOS/firmware update and migration must NOT raise a conflict (ADR 0011 risk)

The fingerprint is a hash of the SMBIOS system UUID, system serial/product and baseboard serial/product (falling
back to the machine GUID), so firmware version changes should not move it. ADR 0011 names this as a risk because it
is untested on real hardware. Test it, do not assume it.

| | |
| --- | --- |
| Do (physical or VM firmware) | On a machine with a vendor BIOS/UEFI update available, or a VM where you change the firmware/hardware version (for example bump the virtual hardware version, or switch BIOS to UEFI only if the UUID is preserved), apply it and reboot. |
| Do (migration) | Live-migrate VM-A to another host (vMotion, Proxmox live migration or Hyper-V live migration) and cold-migrate VM-B. |
| Expect | No `device.identity_conflict` row, no banner, device keeps backing up. If the update changed the SMBIOS serial or product string, the first heartbeat will mismatch: record exactly which and treat it as a finding. |
| Record | For each machine: before and after `wmic csproduct get uuid` (or `Get-CimInstance Win32_ComputerSystemProduct`) and `Get-CimInstance Win32_BaseBoard` serial/product. A change in any of these is the cause of a flag. |
| Fail if | A legitimate firmware update or migration raises a conflict. Resolve with Keep (the fingerprint is re-adopted), then file the finding: the fingerprint inputs need narrowing before M2 closes. |

## Step 6. Policy blast-radius preview

| | |
| --- | --- |
| Do | **Policies**, open the customer's policy (or the default), change one value (for example the interval), **Review changes**. |
| Expect | The review page lists the number of affected devices (the three VMs for the customer policy; every customer's devices for the default), the changed fields as old and new, and the customers affected. Nothing is saved yet. **Cancel** saves nothing. |
| Do | **Save** from the review. Expect one `policy.update` audit row naming field names and counts, never values; the revision appears under **Revisions**; devices pick the change up (device Policy tab shows the new version applied after the next policy fetch, or press **Refresh policy**). |
| Negative | Open the review in two tabs, save in the first, save in the second: the second is refused ("Preview again"). Wait over 15 minutes after a review and save: refused as expired. Restart the console between review and save: refused (the token key is in memory only). |
| Negative | Assign a different policy to the customer: the same preview-then-confirm flow, with the affected device count. |
| Fail if | A save succeeds without a fresh preview; a count or field list differs from what the saved change affected. |

## Step 7. Token policy applied (ADR 0011)

| | |
| --- | --- |
| Do | Create a template policy that differs from the customer's in one field (for example `schedule.interval_minutes`). On **Enroll**, choose that template under **Policy**, create a token for one device, enroll a fourth VM (or retire and re-enroll VM-C) with it. |
| Expect | The new device's **Policy** tab shows that field with source "this device's override (from the enrollment token)" and the reason "Enrollment token policy: <name>"; every other field follows the customer's policy. The `device.enroll` audit row has `token_policy` = `applied`. |
| Expect | Enroll with a token whose policy equals the customer's: `token_policy` = `same`, no override row. Enroll with a template that does not compose: enrollment still succeeds, `token_policy` = `skipped`. |
| Expect | Edit the template afterwards: the already-enrolled device does not change (it is a snapshot), and the policy review for the template does not list it. |
| Fail if | Enrollment is refused because of the policy; the device ignores the token's field. |

## Step 8. Display zone and refresh interval (ADR 0012)

| | |
| --- | --- |
| Do | **Settings** as owner: change the display time zone, save. Reload any page. |
| Expect | Times everywhere use the new zone with no restart, 12-hour clock; the zone name next to the stamp changes; the board follows a new refresh interval within about 15 s. One `settings.update` audit row per changed key. |
| Expect | Edit `config.json`'s `display_timezone` to a third value and restart the service: the saved value still wins, and **Settings** says "config.json says ...; the value saved here (...) wins." |

## Step 9. Sweep

- Audit log: every action above has exactly one row (actor, action, object, result); filter by the object to check.
- Console log: no hostnames of clients' files, no paths, no tokens or codes (`journalctl -u cryo-console` or `docker compose logs console`).
- Board: no "no data" condition was shown as green at any point; stop the console for a minute and confirm the board
  fallback reads "No data" rather than an empty or green fleet when it returns.
- Clean up: retire the lab devices, revoke the lab tokens, delete the lab accounts, restore the VM snapshots.

## Results table (copy into m2-run.md)

| # | step | result | evidence |
| --- | --- | --- | --- |
| 1 | MFA first login | | |
| 2 | one script, three VMs, zero clicks | | |
| 3 | Back up now <= 30 s (max of 5) | | |
| 4 | clone: one flag, backups continue, keep and retire | | |
| 5 | firmware update and migration: no flag | | |
| 6 | policy preview, token binding, stale refusal | | |
| 7 | token policy applied, snapshot semantics | | |
| 8 | settings win over config.json | | |
| 9 | sweep: audit, logs, no-data alarm | | |
