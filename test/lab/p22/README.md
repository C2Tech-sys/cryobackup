# P22-S1 spike rig

Throwaway rig that answers the restic / rclone questions of the P22 pilot slice. Results and the
quoted command lines: [`docs/lab/p22-spike.md`](../../../docs/lab/p22-spike.md).

Windows PowerShell 5.1, no elevation, nothing downloaded. Needs restic 0.19.1 and rclone 1.75.1
(on `PATH`, or `CRYO_RESTIC` / `CRYO_RCLONE`), Go with `golang.org/x/crypto` already in the module
cache (the htpasswd helper runs with `GOPROXY=off`), Git for Windows (openssl, bash) and Python 3.

```powershell
$env:P22_RIG_ROOT = 'D:\scratch\p22'     # optional; default %TEMP%\cryo-p22-rig. Not in a synced folder.
powershell -NoProfile -ExecutionPolicy Bypass -File test\lab\p22\00-setup.ps1      # rig + first backup
powershell -NoProfile -ExecutionPolicy Bypass -File test\lab\p22\01-gateway.ps1    # item 1
# ... 02-original-location, 02b-parent-acl, 03-include-patterns, 04-overwrite,
#     05-acl-ads-long, 06-dump, 07-browse, 09-selfbackup
powershell -NoProfile -ExecutionPolicy Bypass -File test\lab\p22\99-teardown.ps1   # stop, unsubst, delete
```

| file | what |
| --- | --- |
| `rig.ps1` | shared variables and helpers (dot-sourced); every call is logged to `<rig>\log\calls.md` with rig paths, user and host names replaced |
| `00-setup.ps1` | random passwords (files under `<rig>\secrets`, never printed), TLS pair, htpasswd, rclone config, `subst` drive, sample data, gateway with the `deploy/rclone/gateway-flags.md` flags on 127.0.0.1, repository, first backup |
| `htpasswd.go` | one htpasswd line via `internal/console/gateway.HashPassword`; password on stdin; build tag `ignore` keeps it out of `./...` |
| `archive_list.py` | reads a tar / zip from a pipe and lists entry names, types, PAX keys |
| `99-teardown.ps1` | stops every rclone serving the rig, removes the `subst` drive and the rig directory (`P22_KEEP_LOGS=1` keeps `calls.md`) |

Scripts 02, 02b and 05 change the sample data; for a clean second run, tear down and set up again.
Item-specific switches: `P22_ITEM6_SKIP_ARCHIVES=1` (run only the non-regular and outage parts of
06), `P22_ITEM7_SKIP_ROUND1=1` (07 without the first measurement round).
