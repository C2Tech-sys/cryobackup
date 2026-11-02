# M0 lab run (P1-T8) - evidence log

Started 2026-10-22 (evening prep); the device-side steps follow on the next lab day. Hosts are
described generically on purpose; the operator's concrete addresses live outside the repository.

## Versions

| component | version | where |
| --- | --- | --- |
| CryoBackup | `main` 2f2cf22 (FIX16) | console/gateway/minio-lab images built from this tree on the lab host |
| Go | 1.26.10 (ADR 0004); CI resolves it from go.mod | lab host, lab VM, console image |
| restic | 0.19.1 (`restic version`: compiled with go1.26.4) | console image, lab host, lab VM |
| rclone | v1.75.1 | gateway image, lab host |
| MinIO / mc | RELEASE.2025-09-07T16-13-09Z / RELEASE.2025-08-13T08-35-41Z, built from the Go module proxy (P1-T6b) | lab only |
| Docker / Compose | 29.6.2 / v5.3.1 | lab host (Ubuntu 24.04.4) |
| lab VM | Windows Server 2025 Standard (Desktop Experience), unattended install | Proxmox 9.2 |

## Section A - CI

- run 39863810066 (dee5834): green on ubuntu + windows (elevated) under `-race`, cross-build green.
- run 39885635091 (02003ee, ADR 0004): green. run 39888026936 (2ce184d, P1-T6b + lab docs): green.
- First local race run (gcc via winget): 37 packages ok, 0 data races.

## Section B - stack on the lab host (2026-10-22 night)

| step | result | evidence |
| --- | --- | --- |
| 3 staging | done | `.env`, two rclone files (0400, uid 10001), self-signed TLS (SAN = mesh IP + MagicDNS name), `secrets/` |
| 4 build + up | done | `docker compose --profile lab build`: console 317 MB, gateway 250 MB, minio-lab 329 MB; `ps`: console, gateway, maintenance, minio all `healthy`; `minio-init: done (bucket cryolab, prefix 'fleet', users lab-gateway and lab-admin)`; `mc version info`: versioning enabled; lifecycle rule not yet applied |
| 5 init | done | `Master key created`; owner account created (password captured once, outside the repo); config rewritten for container paths (L-INIT-1) |
| 6 smoke | PASS | `smoke-gateway.sh`: 401 without login; TLS 1.1 refused; **probe login 404**; wrong password 401. Admin `/healthz` 200; adminpath `/v1/ping` `{"version":"dev","master_key_loaded":true}` |
| 7 Linux-only tests | PASS | `TestLiveRclone` ok 5.9 s; maintain `TestLive*` ok 133 s incl. `TestLiveAppendOnlyGateway` (script PASS against append-only, FAIL against a plain gateway) |
| 8 token | done | customer `lab-customer`, 1 use, expires 2026-10-25 |

Findings fixed on the way (each reproduced on the host, fixed, pushed, re-run):

- FIX14 `ARG RCLONE_VERSION` was read by rclone as `--version` in the image self-check (both Dockerfiles).
- FIX15 MinIO rejects `s3:prefix` as a condition on `s3:GetBucketLocation`; own statement.
- FIX16 the gateway key needs `s3:GetObject` on the bare prefix key: rclone's root probe got 403, treated the prefix as a file and served the bucket root (probe login 500 until fixed).
- L-INIT-1 `cryo-console init` vs the compose first-start flow (three facets, see the architecture addendum); bridged by a lab script, FIX ticket pending.
- MinIO community images withdrawn; lab builds the pinned release from source (P1-T6b).
- Windows Server 2025 setup has a mandatory licensing page; the unattend carries the published KMS client key.

## Sections C-F - the lab day (2026-10-23, operator present)

The VM joined the operator's mesh by registering the node in Headscale (no pre-auth key). Agent build:
`main` f78342f (FIX18) with the restic 0.19.1 hash pinned, Authenticode-signed; restic.exe beside it.

| step | result | evidence |
| --- | --- | --- |
| 9 enrol + install | PASS, with one finding | first `enroll` against the self-signed console cert: `uplink: enroll: tls error`; with `--insecure-tls-for-lab`: `enrolled: device …, key custody escrowed, policy version 1`. `install`: LocalSystem, delayed automatic start, recovery restart 60 s / 300 s. Board: **PENDING_FIRST** with no click (admin API). First heartbeat 13 s after start; Event Log 1000 |
| 10 first backup | PASS after FIX18 | run 1 (trigger boot) was PARTIAL over tolerance: restic refused the self-signed **gateway** cert (L-TLS-1, see findings) and 14 `unsupported file type "irregular"` errors from every profile's `AppData\Local\Microsoft\WindowsApps` (FIX18). After the fixes: `backup_now` through the command table → `outcome: success, exit 0, files_failed 0`, 630 MB added in 49 s; board **OK, severity none** without operator action. Snapshot tags `cb.customer`, `cb.policy`, `cb.run` present. The file held open exclusively was backed up (VSS) |
| 11 path-free check | PASS | `PHI-sentinel` and `C:\` absent from the console, gateway and maintenance logs; the console DB holds only the snapshot root path (`restic.Snapshot.Paths`, by ruling) and never the sentinel name; the access log shows route patterns only |
| 12 unseen → red | PASS | with the presence aged past 168 h: `state UNSEEN, severity critical`, fleet banner **AGENT API SILENT** (C04). Inside the first-backup grace the row stays PENDING_FIRST by design (verified before the first success) |
| 13 L-APPEND-1 | PASS | `append-only-test.sh` against the deployed gateway with the probe login: forget, forget --prune, rewrite --forget, key remove, key passwd all 403; backups, snapshots, unlock, key add allowed. (Device-credential reveal is M5) |
| 14 storage guard | PASS | gateway key overwrote an object twice → both versions retained; deleting a version or the object outside `locks/` → Access Denied; lifecycle rule (non-current expiry 30 d) applied with the admin credential |
| 15 restore | PASS | VM powered off; restore of one file via the admin API: queued → ready in 4 s, zip 1 049 063 bytes, **sha256 equal** to the seed |
| 16 ACLs | PASS | all 8 layout directories: owner SYSTEM, protected DACL, exactly SYSTEM + Administrators Full Control; `status`: `state acl: verified` |
| 17 squat | PASS | clean VM, root pre-owned by a standard user plus a squatted `CryoBackup.quarantine-<stamp>`: `install` refused (exit 4), HKLM verdict written, `status` UNTRUSTED; planted marker file inert; verdict key ACL = default HKLM rights (Users ReadKey); `install --repair-acl` quarantined to a random-suffix name (the squatted name untouched), fresh root owner SYSTEM protected, verdict cleared; the service refuses to start until an elevated enrol |
| 18 tamper | PASS, with two findings | `Users:(OI)(CI)M` on the root and, separately, on `journal\` while running → next heartbeat `last error class: permission`, `state acl: NOT VERIFIED`, Event Log **1006**; restart → refusal **1004**, HKLM value set, `status` UNTRUSTED; `--repair-acl` quarantined (`CryoBackup.quarantine-<UTC>-<8 hex>`, owner Administrators), fresh root, verdict cleared, fresh enrol → new device id. Elevated tests on the VM: TestVerdictHKLMElevated, TestEnsureElevatedAppliesOwnerAndDACLEverywhere, TestQuarantineRootElevatedSecuresTheQuarantine, TestACLNeedsElevation all PASS |
| 19 DPAPI | PASS | `repo.key` copied to another machine: `CryptUnprotectData` fails ("Key not valid for use in specified state") in both scopes; the SYSTEM service reads it (every run) |
| 20 L-CANCEL-1 | ANSWERED | service stop mid-run: `Stop-Service` returned in 0.3 s, restic terminated (`cancel_mode: terminated`, exit 130, outcome `interrupted`); one lock file stayed in `locks/`; the retry (attempt 2, 4 min 24 s later) completed with `success` and no "already locked" message |
| 21 Event Log / reboot | PASS | source `CryoBackup`: 1000 started, 1003 stopped, 1010 backup finished, 1004 refused, 1006 tampered; the service came back after a VM reboot (delayed auto start) |

### Findings of the lab day

- **L-TLS-1.** `--insecure-tls-for-lab` covers the agent's uplink only; restic verifies the gateway certificate against the OS trust store. A self-signed gateway needs its certificate in the endpoint's trusted roots (done on the VM); production uses a publicly trusted gateway certificate or the console distributes a CA (M5).
- **FIX18** (4bb7f18): baseline exclude for `AppData\Local\Microsoft\WindowsApps`; a dedicated `unsupported_file` error class is an M1 schema decision (CHECK change = migration).
- **FIX17 / FIX17b** (f78342f, 138fe5b): session cookie was always Secure, so the plain-HTTP admin UI could not keep a session; now Secure follows the connection (TLS, or X-Forwarded-Proto from a loopback/private peer, last hop). Open: `IsLoopbackOrPrivate` does not include the CGNAT range 100.64.0.0/10 used by Tailscale/Headscale meshes.
- **L-INIT-1 facet 4.** `init` creates the owner account before writing config.json; when the config write fails, the one-time password has already been printed and is lost on a rerun. Reset path: `cryo-console user passwd`.
- **FIX19 (open).** `cryo-agent install` is not idempotent on a machine that already has the Event Log source (`registry key already exists`, exit 4); `install --repair-acl` therefore fails after a repair on an installed machine (enrol + start still work).
- **FIX20 (open).** The repair run from `enroll`/`install` writes the audit line to agent.log but no Event Log 1007.
- A heartbeat right after a quick service restart was answered 429 (rate limited) once and retried after 13 s.
- Operational: three device rows exist on the board after the two repairs (each repair is a fresh identity by design).

## Section G - close-out

M0 acceptance (architecture §10.2) is met on real machines: enrol with no click, the 100 MB tree backed up through the
gateway to the lab store with the board turning green on its own, UNSEEN red when silent, L-APPEND-1 refused, a
technician restore with the device off and an equal hash, the state engine's reference cases and the fixtures
board (CI). Open items carry into M1: FIX19, FIX20, L-INIT-1 (init flow and first-start docs), the
`unsupported_file` class, the CGNAT private range, the device-credential variant of L-APPEND-1 (M5), VSS explicit
evidence (L-VSS-1, M1).
