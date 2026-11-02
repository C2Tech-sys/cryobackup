# systemd units (bare-metal alternative to compose)

The same three processes as [`../compose/`](../compose/), with the same flags and the
same trust boundary (architecture §7.1, §8.1):

| unit | runs | credential it can reach |
| --- | --- | --- |
| `cryo-console.service` | `cryo-console serve` | none (database and master key only) |
| `cryo-maintain.service` | `cryo-console maintain --loop` | **admin**: `/etc/cryobackup/rclone-admin.conf`, the `[admin]` remote only |
| `rclone-gateway.service` | `rclone serve restic --append-only --private-repos` | **gateway**: `/etc/cryobackup/rclone-gateway.conf`, the `[store]` remote only |

Use two rclone files, never one: a gateway that can read `[admin]` can delete history.
Cut `../compose/rclone.conf.example` at its marker lines.

All three units run as one unprivileged user with `ProtectSystem=strict`,
`NoNewPrivileges`, `PrivateTmp`, an empty capability set and `LimitCORE=0`.

## Credential isolation (`InaccessiblePaths=`)

One user runs all three processes, so file modes alone would let a compromised gateway read
the admin credential. Each unit therefore hides, in its own mount namespace, what it must
never open (a leading `-` ignores a path that does not exist, so a host without one of these
files still starts):

| unit | `InaccessiblePaths=` |
| --- | --- |
| `rclone-gateway.service` | `-/etc/cryobackup/rclone-admin.conf -/etc/cryobackup/master.key -/etc/cryobackup/config.json -/var/lib/cryobackup` |
| `cryo-console.service` | `-/etc/cryobackup/rclone-admin.conf -/etc/cryobackup/rclone-gateway.conf` |
| `cryo-maintain.service` | `-/etc/cryobackup/rclone-gateway.conf` |

The gateway reads its htpasswd from `/var/lib/cryobackup-auth`, a different directory from
the hidden `/var/lib/cryobackup`. If you move a file named above, move the matching line too.

`cryo-console.service` and `cryo-maintain.service` set `ConfigurationDirectoryMode=0750`, so
the `/etc/cryobackup` they create matches the `install -d -m 0750` below (systemd's default
is 0755 and warns about the difference).

## No data at rest

Same rules as compose ([ADR 0008](../../docs/decisions/0008-no-restored-data-at-rest-on-the-console-host.md),
`../compose/README.md`, "No data at rest"):

* **No crash dumps.** Every unit sets `LimitCORE=0`.
* **Maintenance scratch is in memory.** `cryo-maintain.service` sets
  `CRYO_MAINTAIN_SCRATCH_DIR=/run/cryo-maintain`, a `RuntimeDirectory` on the `/run` tmpfs
  (the worker narrows it to 0700 and empties it at start; it refuses a root, a top-level
  directory, one it does not own, and any path with a symlink in it - `/var/run/...` is
  refused because `/var/run` links to `/run`: use `/run/...`). The worker has no persistent state directory: an old
  `/var/lib/cryobackup-maintain` from an earlier version holds nothing it reads and can be
  removed.
* **Restores stream.** Nothing restored is written to disk. At most
  `CRYO_MAINTAIN_MAX_STREAMS` downloads (default 2, below the worker's 4 restic slots)
  stream at once; `restore_stream.min_bytes` and `restore_stream.window_s` in `config.json`
  (default 65536 bytes in 120 s) cut a download that is too slow, and
  `restore_stream.first_byte_s` (default 300 s) cancels one whose first byte never comes.
* **Swap off or encrypted.** `cryo-console doctor` warns about swap it cannot tell is
  encrypted.

## Install

Binaries: `cryo-console` from your build (`make build`), `restic` 0.19.1 and `rclone`
1.75.1 downloaded from the vendors and checked against their `SHA256SUMS` (the values are
in `../compose/.env.example`), all in `/usr/local/bin`.

```sh
useradd --system --home /var/lib/cryobackup --shell /usr/sbin/nologin cryobackup
install -d -o root -g cryobackup -m 0750 /etc/cryobackup /etc/cryobackup/tls
cp cryo-console.service cryo-maintain.service rclone-gateway.service /etc/systemd/system/
```

Files in `/etc/cryobackup` (all mode 0640 or 0400, group `cryobackup`; none is committed):

| file | content |
| --- | --- |
| `config.json` | the console config (schema owned by the console config package); see the path table below |
| `master.key` | from `cryo-console init`; mode 0400; back it up offline |
| `rclone-gateway.conf`, `rclone-admin.conf` | the two rclone files; **both remotes need `no_check_bucket`**, see "The two rclone files" below |
| `tls/agent.crt`, `tls/agent.key`, `tls/gateway.crt`, `tls/gateway.key` | see [`../rclone/tls.md`](../rclone/tls.md) |
| `gateway.env` | `GATEWAY_PORT=9443`, `BUCKET=<bucket>`, `PREFIX=<prefix>` |
| `maintain.env` | `CRYO_MAINTAIN_BUCKET=<bucket>`, `CRYO_MAINTAIN_PREFIX=<prefix>`, optionally `CRYO_MAINTAIN_RESTIC_SHA256=<sha256 of the restic binary>` and `CRYO_MAINTAIN_MAX_STREAMS=<1..3>` |

### The two rclone files

`../compose/rclone.conf.example` has both sections ready to cut (provider, endpoint and
region lines are yours). Whatever the provider, **both remotes carry `no_check_bucket = true`**:
Wasabi refuses rclone's CreateBucket probe on a regional endpoint
(`IllegalLocationConstraintException`, so every enrollment fails with `restic could not reach
the storage`), the gateway key has no CreateBucket permission anyway, and the maintenance
worker creates each device repository through rclone's `Mkdir`.

```ini
# /etc/cryobackup/rclone-gateway.conf (the [store] remote only)
[store]
type = s3
# provider, access_key_id, secret_access_key, endpoint, region ...
no_check_bucket = true
```

```ini
# /etc/cryobackup/rclone-admin.conf (the [admin] remote only)
[admin]
type = s3
# provider, access_key_id, secret_access_key, endpoint, region ...
no_check_bucket = true
```

`config.json` paths for this layout (the compose example uses container paths):

| key | value |
| --- | --- |
| `data_dir` | `/var/lib/cryobackup` |
| `db_path` | `/var/lib/cryobackup/cryobackup.db` |
| `artifacts_dir` | `/var/lib/cryobackup/artifacts` |
| `gateway_auth_dir` | `/var/lib/cryobackup-auth` |
| `adminpath_socket` | `/run/cryobackup/adminpath.sock` |
| `master_key_path` | `/etc/cryobackup/master.key` |
| `tls.cert`, `tls.key` | `/etc/cryobackup/tls/agent.crt`, `/etc/cryobackup/tls/agent.key` |

Start order does not matter much (`Restart=` covers it), but the gateway needs the
htpasswd file that `cryo-console` writes into `/var/lib/cryobackup-auth`:

```sh
systemctl daemon-reload
systemctl enable --now cryo-console cryo-maintain rclone-gateway
systemctl status cryo-console cryo-maintain rclone-gateway
```

## Display time zone and refresh interval: the console's saved value wins (ADR 0012)

`display_timezone` and `refresh_interval_s` in `config.json` are only the starting values.

- An owner changes either one on **Settings** in the console (or with `PUT /api/v1/settings/{key}`). No root, no
  restart: the next page uses the new zone, and the live board follows a new refresh interval within about 15 s.
- Once a value has been saved there, **the saved value wins** over `config.json` on every later start. Until a key
  has been saved, `config.json` supplies it. `cryo-console init` saves the display zone at first run.
- Editing `display_timezone` or `refresh_interval_s` in `config.json` after that therefore changes nothing. The
  Settings page says so: when the saved value differs from the file it shows "config.json says ...; the value saved
  here (...) wins."
- If the saved zone is not a usable IANA name (for example the host's time-zone data changed), the console logs a
  warning at start and falls back to the `config.json` value, then UTC. It does not refuse to start.
- Storage is UTC everywhere; only display changes.

## After every change to a unit

Run L-APPEND-1 against the gateway ([`../rclone/append-only-test.sh`](../rclone/append-only-test.sh)),
and `../../test/lab/smoke-gateway.sh` for TLS and the probe login. Never put
`--append-only` in an `Environment=` line: the environment form has failed to enforce
before (engine §3.2).

## Certificate renewal

Replace `tls/gateway.*`, then `systemctl restart rclone-gateway`. Whether rclone can
reload a renewed certificate without a restart is **UNCERTAIN** (L-CERT-1); restart until
the lab test says otherwise. The agent listener reads its certificate at start:
`systemctl restart cryo-console` after replacing `tls/agent.*`.

### Optional: `cryobackup-certsync` (an ACME client on the same host)

If an ACME client (certbot, acme.sh, lego) issues the certificates on this host, the
[`certsync/`](certsync/) directory does the copy and the restart for you. It is optional:
nothing else depends on it.

* `cryobackup-certsync` (a POSIX `sh` script) copies the certificate and key for **two host
  names you configure**, the agent listener's and the gateway's, to
  `/etc/cryobackup/tls/agent.{crt,key}` and `gateway.{crt,key}`. Nothing is hard-coded: the
  host names and the ACME client's file layout come from `/etc/cryobackup/certsync.env`
  (start from [`certsync/certsync.env.example`](certsync/certsync.env.example)).
* A pair is installed only when the certificate parses and the key matches it. Files are
  written to a private directory in `tls/` and renamed into place (atomic), mode **0440,
  owner `root:cryobackup`**.
* The matching unit (`cryo-console` for the agent pair, `rclone-gateway` for the gateway
  pair) is restarted **only when its pair changed**; an unchanged run touches nothing.
* It fails loudly: a missing source, a mismatched key, a failed restart or a certificate
  that expires within `CERTSYNC_WARN_DAYS` (default 14, the ACME client has stopped renewing)
  exits 1 and shows in `systemctl --failed`; the other pair is still processed. Exit 2 is a
  configuration error. `CERTSYNC_DRY_RUN=1` reports and changes nothing.

```sh
install -m 0755 certsync/cryobackup-certsync /usr/local/sbin/cryobackup-certsync
install -m 0640 certsync/certsync.env.example /etc/cryobackup/certsync.env   # then edit it
cp certsync/cryobackup-certsync.service certsync/cryobackup-certsync.timer /etc/systemd/system/
systemctl daemon-reload
CERTSYNC_DRY_RUN=1 sh -c 'set -a; . /etc/cryobackup/certsync.env; /usr/local/sbin/cryobackup-certsync'
systemctl enable --now cryobackup-certsync.timer
```

The service runs as root (it reads the ACME client's keys and sets the group) with the
filesystem read-only except `tls/`, and the timer runs it daily with a random delay,
`Persistent=true`. If your ACME client can run a deploy hook, calling
`systemctl start cryobackup-certsync.service` from it makes the copy immediate.

## What these units were not tested on

They were written and reviewed against the architecture, not run: the build host is a
Windows PC without systemd. Run `systemd-analyze verify /etc/systemd/system/*.service`
and `systemd-analyze security <unit>` on the target host first.
