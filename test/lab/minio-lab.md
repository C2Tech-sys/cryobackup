# The lab MinIO

`docker compose --profile lab up` starts a throw-away MinIO (`minio`) and a one-shot
initialiser (`minio-init`, script [`deploy/compose/minio-init.sh`](../../deploy/compose/minio-init.sh)).
It gives the lab the same split of powers as production: a **gateway** identity that can
only append, and an **admin** identity that can delete. Nothing here protects anything of
value; do not reuse the credentials.

**Where this MinIO comes from.** MinIO's community edition is source only and its upstream
repository (<https://github.com/minio/minio>) is archived; no binaries or container images are
published any more. The lab image is built by [`deploy/compose/minio.Dockerfile`](../../deploy/compose/minio.Dockerfile)
from the Go module proxy, at the last pinned releases (`MINIO_MODULE_VERSION` =
`RELEASE.2025-09-07T16-13-09Z`, `MC_MODULE_VERSION` = `RELEASE.2025-08-13T08-35-41Z`, as
immutable pseudo-versions checked against sum.golang.org). It is a lab fixture, never a
production target; a maintained lab target is an M5 decision.

## What `minio-init` creates

| object | name | power |
| --- | --- | --- |
| bucket | `$BUCKET` | created if missing |
| policy | `cryo-gateway` | `s3:GetObject`, `s3:PutObject` on `$BUCKET/$PREFIX/*`; `s3:ListBucket` and `s3:GetBucketLocation` on the bucket for prefix `$PREFIX` and below; `s3:DeleteObject` **only** on `$BUCKET/$PREFIX/*/locks/*` |
| policy | `cryo-admin` | `s3:*` on the bucket and its objects |
| user | `$LAB_GATEWAY_ACCESS_KEY` | policy `cryo-gateway`; used by the `[store]` remote of the gateway's rclone file |
| user | `$LAB_ADMIN_ACCESS_KEY` | policy `cryo-admin`; used by the `[admin]` remote of the maintenance worker's file |

Why delete on `locks/` only: restic creates and removes its own lock files on every run, so
a gateway user that could not delete them would leave stale locks behind (engine §3.4). Every
other object (snapshots, packs, indexes, keys, config) is undeletable by the gateway identity
at the storage layer, in addition to rclone's `--append-only`. With an empty `PREFIX` the
resources are `$BUCKET/*` and `$BUCKET/*/locks/*`. The script is idempotent: run it again
(`docker compose --profile lab run --rm minio-init`) and it converges.

**UNCERTAIN:** the policy has been checked for valid JSON in both prefix forms but not run
against a live MinIO from the development PC (no Docker there). Whether rclone's
`ListObjectsV2` calls always carry a prefix that matches the `s3:prefix` condition is
established by the lab run (ticket P1-T8); if listing fails with `AccessDenied`, widen the
list statement first.

## Inspect it

```sh
# mc takes an alias from the environment, so no secret appears on a command line.
export MC_HOST_lab="http://$MINIO_ROOT_USER:$MINIO_ROOT_PASSWORD@127.0.0.1:9000"
docker compose --profile lab exec -e MC_HOST_lab minio mc ls --recursive lab/$BUCKET | head
docker compose --profile lab exec -e MC_HOST_lab minio mc admin policy info lab cryo-gateway
docker compose --profile lab exec -e MC_HOST_lab minio mc admin user list lab
```

(The lab image carries `mc` next to `minio`; `minio-init` is the same image, so
`docker compose --profile lab run --rm --entrypoint /bin/sh minio-init` gives a shell with the
same commands.) The embedded console is still published on `127.0.0.1:$MINIO_CONSOLE_PORT` at
this release (`--console-address` is accepted by `minio server`), but nothing in the lab relies
on it: use `mc`.

## Prove the split

With the stack up and a device repository present, the gateway identity must fail to delete
anything outside `locks/`, and the admin identity must succeed. From a scratch container with
`mc`:

```sh
export MC_HOST_gw="http://$LAB_GATEWAY_ACCESS_KEY:$LAB_GATEWAY_SECRET_KEY@<lab-host>:<MINIO_API_PORT>"
export MC_HOST_adm="http://$LAB_ADMIN_ACCESS_KEY:$LAB_ADMIN_SECRET_KEY@<lab-host>:<MINIO_API_PORT>"
mc rm gw/$BUCKET/$PREFIX/<device id>/config          # must fail: AccessDenied
mc stat adm/$BUCKET/$PREFIX/<device id>/config       # must succeed
```

Do not run `mc rm` as the admin identity against a repository you want to keep.

## Reset

`docker compose --profile lab down -v` deletes the MinIO volume and with it the lab bucket.
