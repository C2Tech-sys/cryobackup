# The gateway command line

The gateway is `rclone serve restic` with exactly these flags (architecture §7.1).
They are the same in `../compose/docker-compose.yml` and in
`../systemd/rclone-gateway.service`. "engine §" cites
`docs/recon-notes/engine_capabilities.md`.

```
rclone serve restic \
    --addr :9443 \
    --cert /tls/gateway.crt --key /tls/gateway.key \
    --min-tls-version tls1.2 \
    --htpasswd /auth/htpasswd \
    --append-only \
    --private-repos \
    store:<bucket>/<prefix>
```

| flag | why | source |
| --- | --- | --- |
| `--addr :9443` | The public listener. The default is `127.0.0.1:8080`; a LAN or public address requires authentication, which `--htpasswd` provides. The port is a compose variable (`GATEWAY_PORT`, default 9443, architecture DH18). | engine §3.1 |
| `--cert`, `--key` | TLS. Plain HTTP would put the gateway password and the (encrypted) backup stream on the wire. | engine §3.1 |
| `--min-tls-version tls1.2` | The default is **tls1.0**. Always set it. | engine §3.1 |
| `--htpasswd /auth/htpasswd` | One bcrypt line per device login plus the reserved `probe` login. rclone hot-reloads the file when it changes. `$2y$` hashes are accepted (verified with 1.75.1, `internal/console/gateway` live test). Do not use `--user`/`--pass`: one shared credential would defeat per-device confinement. With auth on, every request, including OPTIONS, must authenticate. | engine §3.1; architecture §7.2 |
| `--append-only` | Application-layer guard in rclone (not a storage-layer one; see the versioning section below): refuses deletion of repository data (`DELETE` is allowed only on `/locks/<name>`) and refuses to overwrite an existing object through the gateway. `forget`, `prune`, `rewrite --forget`, `key remove` and `key passwd` fail with 403. **Always a command-line flag, never `RCLONE_APPEND_ONLY`**: the environment form has failed to enforce before. Run L-APPEND-1 after every change. | engine §3.2; architecture §12.2 |
| `--private-repos` | Confines the login `<device_id>` to `/<device_id>/`; a request for any other repository is 403. The repository URL is `rest:https://<device_id>:<password>@<gateway>/<device_id>/` (the trailing slash is mandatory). | engine §3.1 |
| `store:<bucket>/<prefix>` | The `[store]` remote of the gateway's rclone file, with `no_check_bucket = true`: the gateway key has no `CreateBucket` permission and repository creation calls `Mkdir` (engine §4.4). | engine §3.1, §4.4 |

## Flags deliberately left at their defaults

| flag | default | note |
| --- | --- | --- |
| `--server-read-timeout`, `--server-write-timeout` | 1h each | The whole transfer must fit: a 16 to 128 MiB pack on a slow uplink. Keep at 1h. |
| `--max-header-bytes` | 4096 | Fine for restic. |
| `--cache-objects` | true | Caches List results so the following HEAD and GET need no backend call. Leave on. |
| `--allow-origin` | off | The gateway has no browser clients. Never `*`. |
| `--realm`, `--salt` | rclone's | Irrelevant to restic. |

## Storage-layer protection: bucket versioning is REQUIRED

`--append-only` is enforced by rclone, in the gateway process. It is not a storage-layer
control: if the gateway host is compromised, the gateway credential's `s3:PutObject` can
overwrite an existing object directly at the provider. History is therefore **not** safe
from the gateway credential by the flag alone. What makes it recoverable is bucket
versioning (an overwrite or delete marker adds a version) together with a gateway key
that cannot remove versions. Enable versioning on the production bucket before the first
backup:

* AWS S3: `aws s3api put-bucket-versioning --bucket <bucket> --versioning-configuration Status=Enabled`
* Wasabi: console, bucket Settings, Versioning, Enable (or `put-bucket-versioning` with the Wasabi `--endpoint-url`).
* Backblaze B2: versions are always kept; lifecycle "Keep all versions", or remove old versions only after N days.
* IDrive e2: console, bucket Settings, Versioning, Enable.
* MinIO: `mc version enable <alias>/<bucket>`.

The gateway credential holds get, put and list under the prefix and delete on
`<prefix>/*/locks/*` only: never `s3:DeleteObjectVersion`, `s3:PutLifecycleConfiguration`,
`s3:PutBucketVersioning` or `s3:BypassGovernanceRetention`. Only the admin credential
(the maintenance path) may hold lifecycle and version-delete rights, and it expires
non-current versions only after N days. This is recovery, not immutability: Object Lock
governance is M9 (L-GOV-1). Details and the policy template are in
[`../compose/README.md`](../compose/README.md) and
[`../compose/minio-init.sh`](../compose/minio-init.sh).

## What the gateway does not do

rclone offers no quota, no metrics and no upload verification (engine §3.1): quota is a
soft number computed by the console, integrity is restic's own end-to-end verification
plus scheduled `check`. A compromised device can still fill the bucket, create arbitrary
new snapshots (which is why retention always carries `--keep-within`), and delete any
lock file in its own repository, including an admin's exclusive lock mid-prune (engine
§3.2); maintenance runs when the device is quiesced for that reason.

## Never

* `--append-only` or any security flag through the environment.
* `--user-from-header` (it trusts a proxy header; the gateway has no proxy).
* The admin credential in the gateway's rclone file.
* `--s3-bypass-governance-retention` on the gateway (Object Lock bypass belongs to the
  maintenance path only, architecture §7.3 step 10).

## The admin path is not this

Server-side `forget`, `prune`, `check` and `unlock` do not go through the gateway. They
run in `cryo-console maintain` through restic's `rclone:` backend
(`-o rclone.program=rclone -o rclone.args="serve restic --stdio"`) with the `[admin]`
remote: a second, non-append-only path that is never exposed to clients (engine §3.4).
`rclone.args` is the option name from restic's repository documentation, not from the
engine sheet; it was exercised with restic 0.19.1 and rclone 1.75.1 against a local
remote (L-RCLONEARGS-1 is partly answered: the option is accepted and carries
`--s3-bypass-governance-retention` syntactically; the effect against a governance-mode
bucket is the M9 lab test). restic splits `rclone.program` like a shell command line:
the path must contain no whitespace or backslash (write forward slashes on Windows).
