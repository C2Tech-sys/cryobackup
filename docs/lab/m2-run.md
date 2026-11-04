# M2 lab run - evidence log

Run 2026-11-02 with the operator, against the production console (M2 build, cryo-console 29f8419) and its gateway
and object storage, following `docs/lab/m2-acceptance.md`. Hosts are described generically on purpose; the
operator's concrete addresses stay outside the repository.

| | | |
| --- | --- | --- |
| console | M2 build 29f8419, systemd units, real database, Let's Encrypt certificates | production console host |
| agent MSI | 0.2.0, signed (SHA-256 d77bf825...13c5) | lab VMs |
| lab VMs | three Windows Server 2025 VMs (VM-A, VM-B, VM-C), each an independent copy of the M0 lab image with its own name, IP, SMBIOS UUID and machine GUID, snapshotted clean | single-node Proxmox host |
| RMM | the RMM wrapper `deploy/windows/rmm/install-cryobackup.ps1` run as SYSTEM through the hypervisor guest agent, with the variables block copied from the console's created-token page; the token travelled in a root-only file and was shredded after use | |

## Results

| # | step | result | evidence |
| --- | --- | --- | --- |
| 1 | MFA first login | PASS | second admin forced to authenticator setup after the password; wrong code refused; 10 recovery codes shown once; code reuse refused; one recovery code accepted once (F1: the setup page's Copy button does nothing) |
| 2 | one script, three VMs, zero clicks | **FAIL (F4)** | the script run on all three VMs at once: VM-A enrolled (exit 0, 8 s); VM-B and VM-C failed with MSI 1603 because the console answered 429 to the second and third concurrent enrollment on the same token (one in flight per token) and `msi-configure` does not retry. Run again one at a time, both enrolled (exit 0). Token used 3 of 5; three `device.enroll` rows; the token appears in no console, maintenance or gateway log line and is masked in the MSI log. Rerun with all three at once after the fix |
| 3 | Back up now <= 30 s (max of 5) | PASS | five clicks one minute apart: delivered 0-2 ms after queueing (the open long-poll returned at once), answered 26-29 ms later. Repeated click: "Not started: a backup was already running." Cancel: "The run was stopped. Files already uploaded are kept." Offline: queued with the service stopped, delivered at the first poll 2 s after the service started, the run finished 19 s later. Check in queued while offline expired after 15 min ("The device did not receive it in time.") |
| 4 | clone: one flag, backups continue, keep and retire | PASS with a finding | VM-C's disk copied to a new VM with a new SMBIOS UUID, both booted: `heartbeat: identity conflict raised` once; one critical audit row, no fingerprint shown; board and device page show the conflict. Keep: cleared, raised again 32 s later by the other machine (expected). Retire: both machines refused with 401 within 4 s. F9: about one heartbeat in four from the pair was refused with 429 while the conflict stood (shared per-device bucket). No backup fell due during the conflict, so "backups continue" was not exercised |
| 5 | firmware update and migration: no flag | PASS (limited) | VM-A: disk moved live between storage pools while running. VM-B: virtual hardware version changed (q35 11.0 to 10.0), disk moved cold, rebooted. System UUID, product, serial, baseboard and BIOS identical before and after; no conflict, heartbeats normal. Limits: single-node host, so no migration to another host; a vendor BIOS update on physical hardware is still to be tested |
| 6 | policy preview, token binding, stale refusal | PASS | review lists affected devices, old and new values and customers; Cancel saves nothing; Save writes one `policy.update` row and a revision; the second of two tabs is refused |
| 7 | token policy applied, snapshot semantics | PASS | template policy differing in one field, token with that policy, VM-C (reset to clean) enrolled with it: the field shows the enrollment-token override and reason; `token_policy` = `applied`; editing the template afterwards leaves the device unchanged and its review does not list it |
| 8 | settings win over config.json | PASS | zone changed on Settings, applied everywhere without restart, 12-hour clock; config.json edited and the service restarted: the saved value still wins and Settings says so (F12: the zone should be a dropdown) |
| 9 | sweep: audit, logs, no-data alarm | PASS | audit: one row per action, no codes, keys, tokens or fingerprints. 1,518 console, maintenance and gateway log lines: no paths, bearer tokens, passwords, secrets, lab hostnames or otpauth URIs. Console stopped 60 s with the board open: NO DATA pill, "Live updates disconnected" with the data time, values marked "as of"; the tab reconnected by itself after the restart |

## Findings

| id | finding | severity |
| --- | --- | --- |
| F1 | the Copy button on the authenticator setup page does nothing | low |
| F2 | Add a customer: fields in one row are vertically misaligned and help text wraps unevenly | low |
| F3 | the acceptance procedure asks for a customer "with a storage target"; M2 has one console-wide target | doc |
| F4 | concurrent enrollments on one token are refused with 429 and the MSI custom action does not retry: an RMM push to a site fails most machines | high |
| F5 | the AGENT API SILENT banner fires whenever no agent has checked in, which is normal for a small or sleeping fleet; it should come from the console probing its own agent endpoint (DNS, TLS, certificate), and an offline device should change only its own state | high |
| F6 | the agent reports `v0.1.0-83-g...` inside the 0.2.0 MSI: the build does not stamp the release version | low |
| F7 | every escrowed device turns ESCROW_UNVERIFIED 24 h after enrollment: the enrollment's successful `cat config` is not recorded as a verification, and the 30-day recheck job does not exist before M5 | high |
| F8 | the banner strip overlays page content when scrolling | medium |
| F9 | heartbeats from a cloned pair are refused with 429 by the per-device bucket while the conflict stands | medium |
| F10 | the state tooltip opens inside the needs-attention table's scroll box and is clipped | medium |
| F11 | the conflict tooltip says "Resolve it to continue." although backups continue (ADR 0011) | low |
| F12 | the display time zone is a free-text field; it should be a list of zone names | low |
| F13 | "Live updates disconnected" appears on any EventSource error, including a page navigation or a blip that reconnects within seconds; show it only after the stream has been down for about two ping intervals | medium |

M2 is accepted except step 2, which is rerun with all three VMs at once on the MSI that fixes F4.

## Step 2 rerun on 0.2.1 (2026-11-03)

Console and agent 0.2.1 (7a2e3bc), with all of F1-F13 fixed. The three VMs were rolled back to their clean
snapshot (no agent), and the RMM script was started on all three in the same second with one fresh 3-use token.

| VM | script exit | agent | enrolled |
| --- | --- | --- | --- |
| A | 0 | 0.2.1 | yes, key escrowed |
| B | 0 | 0.2.1 | yes, key escrowed |
| C | 0 | 0.2.1 | yes, key escrowed |

The console answered all three `POST /api/v1/agent/enroll` with 200, after 4.4 s, 8.3 s and 13.7 s: the per-token
queue (F4) serialised them and refused none. No MSI retried and none failed. Step 2 passes, and M2 is accepted.
