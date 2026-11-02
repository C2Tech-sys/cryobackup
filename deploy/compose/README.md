# Compose reference stack

One `docker compose up` gives a working server side: the console, the rclone
gateway and the maintenance worker, plus an optional throw-away MinIO for a lab.
The file is `docker-compose.yml`; it is meant to be read and, if you already run a
stack, to be dropped into it as a fragment. Everything configurable is a variable
in `.env` (start from `.env.example`). Nothing in it is specific to an installation.

Requires Docker Engine with Compose v2.20 or later (`depends_on.required`).

## What each service holds

| service | runs | holds | does not hold |
| --- | --- | --- | --- |
| `console` | `cryo-console serve` | the database (rw), the master key (ro), the htpasswd (rw), TLS for the agent listener | **no storage credential**, no `rclone.conf` |
| `gateway` | `rclone serve restic --append-only --private-repos` | the **gateway** storage credential (`[store]` only), the htpasswd (**ro**), TLS | the admin credential, the master key, the database |
| `maintenance` | `cryo-console maintain --loop` | the **admin** storage credential (`[admin]` only), the master key (ro), the database (rw), the admin-path socket | any inbound port |
| `minio`, `minio-init` | lab only (`--profile lab`) | a throw-away S3 store with a gateway user and an admin user; one locally built image (`minio.Dockerfile`) | n/a |

The trust boundary is architecture §8.1. Two consequences are enforced by this file:

* The gateway and the maintenance worker mount **different** rclone files
  (`RCLONE_GATEWAY_CONF`, `RCLONE_ADMIN_CONF`). `rclone.conf.example` shows both
  remotes in one place and tells you where to cut it. A file that contains `[admin]`
  must never be mounted into the gateway, or a compromised gateway could delete
  history and append-only would mean nothing.
* The gateway command line carries `--append-only` and `--private-repos` as
  **flags**. Never move `--append-only` to an environment variable (engine §3.2: the
  environment form has failed to enforce before). Run
  [`../rclone/append-only-test.sh`](../rclone/append-only-test.sh) (L-APPEND-1)
  against the deployed gateway on every release.

The gateway, console and maintenance containers run as uid/gid 10001 with a read-only
root file system, all capabilities dropped and `no-new-privileges`. The htpasswd the
console writes (mode 0640) is readable by the gateway because both run as gid 10001.

## Bucket versioning is REQUIRED

`--append-only` is an application-layer guard inside rclone. At the storage layer the
gateway key's `s3:PutObject` can overwrite an existing object if the gateway host is
compromised, and no flag changes that. The control that survives such a host is
**bucket versioning**: an overwrite or delete marker only adds a version, and the gateway
key cannot remove one. Enable it on the production bucket before the first backup:

| provider | how |
| --- | --- |
| AWS S3 | `aws s3api put-bucket-versioning --bucket <bucket> --versioning-configuration Status=Enabled` |
| Wasabi | Console, bucket Settings, Versioning, Enable; or the same `put-bucket-versioning` call with `--endpoint-url https://s3.<region>.wasabisys.com` |
| Backblaze B2 | Versions are always kept; set the bucket lifecycle to "Keep all versions", or to remove old versions only after N days, never "keep only the last version". **UNCERTAIN:** a B2 application key cannot allow delete on `locks/` alone; verify in the lab what the gateway key needs |
| IDrive e2 | e2 console, bucket Settings, Versioning, Enable (or `put-bucket-versioning` against your e2 endpoint) |
| MinIO | `mc version enable <alias>/<bucket>` (the lab profile does this in `minio-init.sh`) |

Rules that go with it:

* The **gateway** credential holds get and put on `<prefix>/*` **and on the bare key `<prefix>`**
  (rclone probes whether its root is a file with a HEAD on that key; a 403 there makes it serve
  the bucket root and every request then fails with 500; the key never holds an object), list on
  the bucket limited to the prefix, and delete on
  `<prefix>/*/locks/*`. It must not hold `s3:DeleteObjectVersion`,
  `s3:PutLifecycleConfiguration`, `s3:PutBucketVersioning` or
  `s3:BypassGovernanceRetention`. `minio-init.sh` is the policy template, and a test
  (`TestGatewayPolicyHasNoVersionOrLifecycleRights`) pins it.
* Only the **admin** credential (the maintenance worker) may hold lifecycle and
  version-delete rights. Expire non-current versions only after N days, chosen longer
  than the time in which you would notice tampering and than the provider's minimum
  storage duration. The lab script carries the example rule.
* This is recovery, not immutability. A compromised gateway can still write bad objects
  and hide current ones with delete markers; the prior versions remain and are restored
  with the admin credential. Immutability (Object Lock governance, with the bypass right
  held by the maintenance path only) is M9, gated on L-GOV-1, and is not enabled here.

## PHI and BAA

CryoBackup backs up endpoints that may hold protected health information. Point the
gateway and the maintenance worker only at storage that is covered by a Business
Associate Agreement, and keep the console and the gateway on hosts you control.
The software logs no file names or paths and sends nothing to third parties, but the
bucket you configure is where the (encrypted) data lands: the agreement with the
storage provider is yours to hold.

## No data at rest

The console host keeps no backup payload on persistent disk
([ADR 0008](../../docs/decisions/0008-no-restored-data-at-rest-on-the-console-host.md)).
What lives where:

| where | what | never |
| --- | --- | --- |
| `db` volume | the database: devices, policies, runs, restore requests (a restore's path only encrypted under the master key, cleared when it expires; size and SHA-256 of each finished download) | file contents |
| `artifacts` volume | nothing in M1; reserved for agent releases. The console deletes any restore archive an earlier version staged there and logs a warning | restored files |
| maintenance `/tmp` (tmpfs, 512m) | restic's and rclone's temp files while a run is in flight (`CRYO_MAINTAIN_SCRATCH_DIR=/tmp/cryo-maintain`; the worker refuses a root or top-level directory, one it does not own, and any path with a symlink in it, such as `/var/run/...`: use `/run/...`) | anything after the run; anything on disk |
| process memory | restic's repository index (every run uses `--no-cache`), the restore stream on its way to the browser | a cache directory |

* **Restores stream.** A technician download runs `restic dump` when the download is
  requested and streams the zip through the maintenance worker and the console straight
  to the browser. Nothing restored is written to disk on the way, not even briefly. A
  dropped download starts again from the beginning (no resume); the link works once and a
  new one can be made until the restore expires (24 hours); after that every link is
  refused (410 Gone).
* **Downloads have a budget.** A download holds one of the worker's four restic slots for
  as long as the browser takes, so at most `CRYO_MAINTAIN_MAX_STREAMS` (default 2, must
  stay below 4) stream at once, fleet-wide; one more is refused at once with a 503 "try
  again" (Retry-After), never queued, and maintenance always keeps slots. The refusal does
  not spend the download link (the same URL works once a slot frees), and the restore page
  says "Other restores are already downloading. Try again in a moment." A download that
  delivers fewer than `restore_stream.min_bytes` (default 65536) in any
  `restore_stream.window_s` seconds (default 120) once its first byte went out is cut and
  recorded with class `too_slow`; one with no byte at all within
  `restore_stream.first_byte_s` seconds (default 300) is cancelled - restic with it - and
  recorded with class `no_data` (config.json, or `CRYO_RESTORE_STREAM_MIN_BYTES`,
  `CRYO_RESTORE_STREAM_WINDOW_S` and `CRYO_RESTORE_STREAM_FIRST_BYTE_S`).
* **Maintenance has no cache.** Every restic call runs with `--no-cache`, so prune and
  check download the index on every run (slower, by design). There is no `maintain-state`
  volume any more; an old one can be removed (`docker volume rm <project>_maintain-state`).
* **No crash dumps.** `console`, `gateway` and `maintenance` run with `ulimits: core: 0`
  (soft and hard).
* **Swap must be off or encrypted on the host.** tmpfs pages and process memory can be
  swapped out, and swapped memory can hold restore data and keys. Turn swap off
  (`swapoff -a` and remove it from `/etc/fstab`) or put it on dm-crypt.
  `cryo-console doctor` reports active swap that is not known to be encrypted as a WARN.

## Files you create

| file | from | notes |
| --- | --- | --- |
| `.env` | `.env.example` | git-ignored. Bucket, prefix, ports, pinned checksums. |
| `config.json` | `config.example.json` | The console config. **Its schema is owned by the console config package (P1-T5)**; this file is a copy of its example, with the container paths of this compose file. |
| `rclone.gateway.conf` | the `[store]` section of `rclone.conf.example` | gateway credential, `no_check_bucket = true` |
| `rclone.admin.conf` | the `[admin]` section of `rclone.conf.example` | admin (delete-capable) credential, `no_check_bucket = true` (Wasabi refuses rclone's CreateBucket probe on a regional endpoint) |
| `secrets/master.key` | `cryo-console init` | 32 bytes, base64, mode 0400, readable by uid 10001. The directory `MASTER_KEY_DIR` is mounted at `/run/keys`. Back it up offline. |
| `tls/agent.crt`, `tls/agent.key` | your CA or ACME | agent listener (8443) |
| `tls/gateway.crt`, `tls/gateway.key` | your CA or ACME | gateway (9443) |
| `tls/admin.crt`, `tls/admin.key` | your CA or ACME | optional: admin UI over TLS, see [Admin UI over TLS on a mesh address](#admin-ui-over-tls-on-a-mesh-address) |

Inside the container the admin UI listens on `0.0.0.0:8080` (`admin_listen`, with
`admin_allow_public: true`) because Docker's port mapping needs it; what the host exposes is
decided by `ADMIN_BIND` alone, so keep that on loopback or a management address.

All of these are git-ignored (`../.gitignore`). TLS recipes and rotation are in
[`../rclone/tls.md`](../rclone/tls.md).

Splitting the rclone file, from `rclone.conf.example` (the markers are the cut lines):

```sh
awk '/^# =+ rclone\.gateway\.conf/{f=1;next} /^# =+ rclone\.admin\.conf/{f=0} f' rclone.conf.example > rclone.gateway.conf
awk '/^# =+ rclone\.admin\.conf/{f=1;next} /^# -+$/{f=0} f' rclone.conf.example > rclone.admin.conf
```

Then fill in the `<placeholders>` and `chmod 0400` both files (owner uid 10001).

## First start

```sh
cp .env.example .env            # set BUCKET, PREFIX, ports; the checksums are filled for restic 0.19.1 / rclone 1.75.1
cp config.example.json config.json       # edit public_url and gateway_url
mkdir -p secrets tls && chown 10001:10001 secrets
docker compose build            # downloads restic and rclone, verifies SHA-256, fails on mismatch
# one-off: create the database, the master key and the first owner account. The secrets
# directory is mounted writable for this run only (it is read-only in the service).
docker compose run --rm --no-deps -v "$PWD/secrets:/keys-rw" -e CRYO_MASTER_KEY_PATH=/keys-rw/master.key console init --config /config/config.json
# place the TLS files in ./tls and the two rclone files next to this file
docker compose up -d
docker compose ps               # every service should become healthy
```

`cryo-console init` belongs to the console config package (ticket P1-T5): its exact flags
come from `cryo-console init --help`; the command above assumes it reads the mounted config
and writes `master.key` to the `master_key_path` of that config.

The gateway waits for the console to be healthy because the console writes the
htpasswd file the gateway reads (`gateway-auth` volume); if the file is missing the
gateway restarts until it appears.

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

## Fleet concurrency cap

`max_concurrent_backups` on a storage target caps how many scheduled backups of its devices
run at once (ADR 0005). It is 0 (unlimited) until you set it:

```sh
docker compose exec console cryo-console target list --config /config/config.json
docker compose exec console cryo-console target set --config /config/config.json --max-concurrent-backups 10
```

A device that finds the cap full records a `deferred` run with reason `fleet_cap` and asks again
after the console's `retry_after_s`; operator `backup_now` runs are never capped. If the console
cannot be reached the agents start anyway and the runs show `lease: bypassed`. A cap set too low
shows up as STALE devices, never as quiet green.

## Admin UI over TLS on a mesh address

By default the admin UI is plain HTTP on the host's loopback (`ADMIN_BIND=127.0.0.1`). To reach
it from your workstation across a Tailscale or Headscale mesh, and to have the browser's
session cookie marked `Secure`, serve it over TLS on the host's mesh address. On the compose
path the console never sees that address: `ADMIN_BIND` is the address Docker publishes the port
on, and inside the container `admin_listen` is always `0.0.0.0:8080` (with `admin_allow_public:
true`, because Docker's port mapping needs the wildcard bind). Which host addresses can reach the
admin UI is decided by `ADMIN_BIND` alone; nothing in `config.json` changes when you move it.

`ADMIN_BIND` takes any host address, and a Tailscale or Headscale address is in `100.64.0.0/10`
(RFC 6598, shared address space). Internet providers use the same range for carrier-grade NAT, so
a host whose WAN interface has a `100.64.x.x` address from its ISP is not on a mesh: there, set
admin TLS and set `ADMIN_BIND` explicitly to the mesh or management address, never to the WAN
address or `0.0.0.0`. (The rule matters to the console itself only outside compose, where it
reads `admin_listen` directly and treats `100.64.0.0/10` as a private bind.)

1. Get a certificate for the host's mesh name. With Tailscale, `tailscale cert <host>.<tailnet>.ts.net`
   writes a CA-signed pair; Headscale and private CAs work the same way. Any certificate whose name
   your browser will accept is fine.
2. Put the pair in `TLS_DIR` as `admin.crt` and `admin.key` (mode 0400, owner uid 10001). The
   directory is already mounted read-only at `/tls`.
3. In `config.json` set the two keys (their `CRYO_ADMIN_TLS_CERT` and `CRYO_ADMIN_TLS_KEY`
   environment forms work too). Both or neither: one alone stops the console at start.

   ```json
   "admin": { "tls": { "cert": "/tls/admin.crt", "key": "/tls/admin.key" } }
   ```
4. In `.env` bind the published port to the mesh address, then restart the console:

   ```sh
   ADMIN_BIND=100.100.10.20       # example: this host's own mesh IP; never 0.0.0.0 on a host with a public interface
   docker compose up -d console
   ```
5. Open `https://<mesh name>:8080/`. The listener speaks TLS 1.2 or newer and rereads `admin.crt`
   when it changes (checked at most once a minute), so a renewal needs no restart. A certificate or
   key that does not load stops the console at start with a message that names the admin listener
   and no path.

The compose healthcheck tries plain HTTP and then HTTPS (without name verification, on the
container's own loopback), so it passes in both modes. `docker compose exec console cryo-console
doctor --config /config/config.json` prints an `admin tls` line: `OK` when TLS is on or
`admin_listen` is loopback, `WARN` when TLS is off and `admin_listen` is anything else (a private
LAN is not an encrypted path; a mesh is, but doctor cannot tell which one you have). Doctor reads
`admin_listen`, not `ADMIN_BIND`, and in compose `admin_listen` is always `0.0.0.0:8080`, so with
admin TLS off doctor shows `WARN` even when `ADMIN_BIND` is the default `127.0.0.1`. That is
expected and acceptable while `ADMIN_BIND` is loopback: the published port then answers only on
the host itself. Check `ADMIN_BIND` in `.env` (or `docker compose port console 8080`) to confirm;
to clear the warning, turn admin TLS on as above.

Plain HTTP on a mesh address is acceptable only while the mesh encrypts every hop. TLS adds the
`Secure` cookie flag and takes that dependency away.

## Lab profile (MinIO)

MinIO's community edition is source only and its upstream repository
(<https://github.com/minio/minio>) is archived: no pre-compiled binaries are published and the
project's container images can no longer be pulled. The lab therefore builds the last pinned
releases from the Go module proxy (`minio.Dockerfile`, the pseudo-versions in `.env.example`,
verified against sum.golang.org) into one local image, `MINIO_LAB_IMAGE`, used by both `minio`
and `minio-init`. This is a lab fixture, never a production target (production uses a real
provider with bucket versioning, above). A maintained lab target is an M5 decision.

```sh
docker compose --profile lab build     # compiles minio and mc from source; a few minutes the first time
docker compose --profile lab up -d
```

`minio` is healthy when `http://127.0.0.1:9000/minio/health/live` answers (checked inside the
container). The `server` command still accepts `--console-address` at the pinned release (it is
in `cmd/server-main.go` of the module), so the embedded console stays on `MINIO_CONSOLE_PORT`;
nothing in the lab depends on it. To inspect the store use `mc`, which is in the image (see
[`../../test/lab/minio-lab.md`](../../test/lab/minio-lab.md)).

`minio-init` creates the bucket with versioning enabled and two users (policies in
[`minio-init.sh`](minio-init.sh)): the **gateway** user may get, put and list under
the prefix and delete only lock files (it can overwrite, which versioning makes
recoverable; it cannot delete a version or change lifecycle or versioning); the
**admin** user may do everything in the bucket. Put the lab keys from `.env` into the two rclone files with
`provider = Minio` and `endpoint = http://minio:9000`. A lab host that already uses
9443 sets `GATEWAY_PORT=8000` (any free port works; the value is used on both sides of
the mapping). The MinIO ports bind to `127.0.0.1` by default. See
[`../../test/lab/`](../../test/lab/) for the M0 lab procedure and the smoke checks.
Production composes simply omit the profile.

## Rotating the gateway certificate

1. Replace `tls/gateway.crt` and `tls/gateway.key` (write both, then move into place;
   mode 0400, owner uid 10001).
2. `docker compose restart gateway`.
3. `../../test/lab/smoke-gateway.sh https://<gateway host>:<port>` to confirm TLS and the
   probe login.

**UNCERTAIN (L-CERT-1):** whether rclone reloads a renewed certificate without a restart
is not established for 1.75.1. Until the lab test says otherwise, restart. The gateway
restarts in seconds and restic retries; agents see a short deferral, never a failure.
The agent listener (`console`) reads its certificate at start: restart `console` after
replacing `tls/agent.*`.

## Ports

| variable | default | meaning |
| --- | --- | --- |
| `AGENT_PORT` | 8443 | agent API, public |
| `ADMIN_BIND`, `ADMIN_PORT` | 127.0.0.1, 8080 | admin UI; rebind `ADMIN_BIND` to a mesh or VPN address (serve it over TLS: [above](#admin-ui-over-tls-on-a-mesh-address)); never the internet |
| `GATEWAY_BIND`, `GATEWAY_PORT` | 0.0.0.0, 9443 | gateway, public (architecture DH18); lab hosts that have 9443 taken use 8000 |
| `MINIO_BIND`, `MINIO_API_PORT`, `MINIO_CONSOLE_PORT` | 127.0.0.1, 9000, 9001 | lab only |

## Upgrading

`docker compose build --pull && docker compose up -d`. The lab MinIO is not upgraded: its two
module versions are frozen at the last community release. Bump `RESTIC_VERSION` /
`RESTIC_SHA256` and `RCLONE_VERSION` / `RCLONE_SHA256` together (the checksums come
from the vendors' `SHA256SUMS`, linked in `.env.example`), and re-run L-APPEND-1.
Pin base images by digest in production.

## Validating the file

`docker compose config` resolves variables and fails on a missing required one
(`BUCKET` and the two checksums). The lab credentials default to empty so that a
production `.env` without them still resolves; `minio-init` refuses to run with any of
them empty. Without
Docker, `go test ./internal/console/maintain/ -run Compose` checks the structure of
`docker-compose.yml` and that every variable it references has an entry in `.env.example`.
