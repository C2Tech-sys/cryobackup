# M0 lab procedure

The M0 acceptance run (architecture §10.2): one device end to end, on a lab host you
control, against a throw-away MinIO. This directory holds the scripts and notes for the
server side; the agent steps are in the agent's own documentation (ticket P1-T7) and the
scripted end-to-end suite (`../e2e/`, ticket P1-T8) automates the whole list.

Placeholders you replace: `<lab-host>` (the Linux host that runs the stack),
`<console-name>` and `<gateway-name>` (DNS names or addresses the endpoints use for the
agent API and the gateway), `<gateway-port>` (default 9443; use another free port if the host
has it taken), `<windows-vm>` (the agent lab box).

| file | what it is |
| --- | --- |
| [`smoke-gateway.sh`](smoke-gateway.sh) | TLS, minimum TLS version, the `probe` login and authentication |
| [`probe-password.py`](probe-password.py) | derives the probe password from the master key (key on stdin, never on a command line; python3) |
| [`minio-lab.md`](minio-lab.md) | the bucket, users and policies `minio-init` creates, and how to inspect them |
| [`append-only.md`](append-only.md) | running L-APPEND-1 (`../../deploy/rclone/append-only-test.sh`) |

## 1. Prepare the host

Docker Engine with Compose v2.20 or later; nothing else needs installing (restic and rclone
are inside the images). Free ports for the agent API, the admin UI, the gateway and MinIO
(`ss -ltn` first); nothing here uses 80 or 443.

```sh
git clone <repository> cryobackup && cd cryobackup/deploy/compose
cp .env.example .env
```

Edit `.env`: `BUCKET`, `PREFIX`, the ports (`GATEWAY_PORT=<gateway-port>`), and the lab
credentials. The pinned restic and rclone checksums are already filled in; compare them
with the vendors' `SHA256SUMS` if you change a version. Bind the admin UI to loopback or to
your management network (`ADMIN_BIND`), never to the internet.

## 2. Credentials and certificates

```sh
# the two rclone files, cut from the template (see ../../deploy/compose/README.md)
awk '/^# =+ rclone\.gateway\.conf/{f=1;next} /^# =+ rclone\.admin\.conf/{f=0} f' rclone.conf.example > rclone.gateway.conf
awk '/^# =+ rclone\.admin\.conf/{f=1;next} /^# -+$/{f=0} f' rclone.conf.example > rclone.admin.conf
```

In both files set `provider = Minio`, `endpoint = http://minio:9000`, delete the `region`
line, and use the lab keys: `LAB_GATEWAY_ACCESS_KEY` / `LAB_GATEWAY_SECRET_KEY` in
`[store]`, `LAB_ADMIN_ACCESS_KEY` / `LAB_ADMIN_SECRET_KEY` in `[admin]`. `chmod 0400` both and
make uid 10001 the owner (`chown 10001:10001`).

Certificates (lab: self-signed; recipe and the production options in
[`../../deploy/rclone/tls.md`](../../deploy/rclone/tls.md)):

```sh
mkdir -p tls && cd tls
for n in agent gateway; do
  openssl req -x509 -newkey rsa:3072 -nodes -days 30 -keyout $n.key -out $n.crt \
    -subj "/CN=<console-name>" -addext "subjectAltName=DNS:<console-name>"
done
chmod 0400 *.key && chown 10001:10001 *.key *.crt && cd ..
```

(Use `<gateway-name>` for the gateway certificate if it differs.)

## 3. Initialise and start

```sh
cp config.example.json config.json       # edit public_url and gateway_url
mkdir -p secrets && chown 10001:10001 secrets
docker compose --profile lab build       # the lab MinIO is compiled from source: see minio-lab.md
# one-off: database, master key, first owner account (secrets mounted writable for this run only;
# exact flags: `cryo-console init --help`, ticket P1-T5)
docker compose run --rm --no-deps -v "$PWD/secrets:/keys-rw" -e CRYO_MASTER_KEY_PATH=/keys-rw/master.key console init --config /config/config.json
docker compose --profile lab up -d
docker compose ps                        # console, gateway, maintenance, minio healthy; minio-init exited 0
docker compose logs minio-init           # "done (bucket ..., users ...)"
```

Keep the offline copy of `secrets/master.key` that `init` asks for: losing it loses every
escrowed repository password.

## 4. Smoke checks

```sh
CACERT=tls/gateway.crt MASTER_KEY_FILE=secrets/master.key \
  ../../test/lab/smoke-gateway.sh https://<gateway-name>:<gateway-port>
```

All four checks must pass. `401` on the probe login means the htpasswd has not been loaded
(the console writes it at start; check `docker compose logs console`); a `5xx` means the
gateway cannot reach MinIO (check `rclone.gateway.conf`, `no_check_bucket = true`, the
policy in [`minio-lab.md`](minio-lab.md)).

Admin path liveness (from the host):

```sh
docker compose exec maintenance curl -fsS --unix-socket /run/cryo/adminpath.sock http://worker/v1/ping
```

## 5. Customer, token, agent

Create a customer and an enrollment token in the admin UI (`http://127.0.0.1:<ADMIN_PORT>`
through an SSH tunnel or your management network) or with `cryo-console token`. On
`<windows-vm>` install the agent with the token and `<console-name>:<AGENT_PORT>` (agent
documentation, ticket P1-T7). Without any further click the device appears as
PENDING_FIRST (the enrollment ran `init` through the admin path, which created its
repository in the bucket: list it with `mc` inside the `minio` container, after
`export MC_HOST_lab="http://$MINIO_ROOT_USER:$MINIO_ROOT_PASSWORD@127.0.0.1:9000"`, with
`docker compose --profile lab exec -e MC_HOST_lab minio mc ls --recursive lab/<BUCKET>/<PREFIX>`;
see [`minio-lab.md`](minio-lab.md)).

## 6. The M0 acceptance list (§10.2)

1. The device appears as PENDING_FIRST after enrollment, with no further click.
2. A 100 MB test tree backs up through the gateway to MinIO and the board turns green
   without operator action.
3. Stopping the agent for longer than the unseen threshold turns the row red.
4. **L-APPEND-1**: `restic forget` with the device credentials against the gateway fails
   (see [`append-only.md`](append-only.md): run the script, it covers `forget`,
   `forget --prune`, `rewrite --forget`, `key remove` and `key passwd`, and the successful
   operations).
5. Technician restore of one file as a zip with the device powered off: the zip contains
   the file and its sha256 equals the original's.
6. `go test ./internal/state` passes the reference cases A01 to A23 (CI), and
   `cryo-console serve --fixtures` renders the board without a network.

Record the result of each line, with the date and the versions of restic, rclone and the
image tag, in the milestone's ledger entry.

## Tear down

```sh
docker compose --profile lab down -v     # removes every volume, including the lab data
```
