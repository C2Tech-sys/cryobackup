# deploy

How CryoBackup's server side is put on a host, plus the pieces that every installation
shares. Tickets P1-T6 (this directory's server side) and M1 (installer, RMM scripts).

| path | what it is |
| --- | --- |
| [`compose/`](compose/) | The reference stack: console, rclone gateway, maintenance worker, and an optional MinIO for a lab (`--profile lab`). Dockerfiles with pinned, hash-verified restic and rclone; `.env.example`, `rclone.conf.example`, `config.example.json`. Start here. |
| [`systemd/`](systemd/) | The same three processes as units, for a host without Docker. |
| [`rclone/`](rclone/) | The gateway recipe: [`gateway-flags.md`](rclone/gateway-flags.md) (every flag and why), [`tls.md`](rclone/tls.md) (certificates, rotation), and [`append-only-test.sh`](rclone/append-only-test.sh), the L-APPEND-1 release gate. |
| `../test/lab/` | The M0 lab procedure, the gateway smoke check and the MinIO notes. |
| (later) | WiX installer sources and RMM deployment scripts arrive with M1. |

## The shape of a deployment

```
 endpoints ──HTTPS 8443──▶ console (serve) ──unix socket──▶ maintenance (maintain)
     │                         │ htpasswd (rw)                  │ S3 ADMIN credential, master key
     └──HTTPS 9443──▶ gateway (rclone serve restic --append-only --private-repos)
                              │ htpasswd (ro), S3 GATEWAY credential
                              ▼
                        S3-compatible storage (BAA-covered when PHI is in scope)
```

* Endpoints never hold a storage credential.
* The console holds none either: every repository operation it needs (init, browse, dump,
  stats, unlock, and from M5 forget, prune, check) runs in the maintenance worker over a
  unix socket.
* The gateway can only append. The maintenance worker is the one place that can delete.
* Two rclone files, one per credential. Never one file with both.

## Rules for everything in this directory

* **Secrets never in the repository.** Real `.env`, `rclone*.conf`, `config.json`, keys and
  certificates are git-ignored (`.gitignore` here); only `.example` templates are
  committed. The Docker build context excludes them too.
* **Nothing installation-specific.** Bucket, prefix, hostnames, ports, TLS paths, image
  tags and the lab credentials are variables or files you provide. The repository builds
  and runs for someone who has never heard of the people who wrote it.
* **Pinned and verified.** restic and rclone are pinned (ADR 0003: restic 0.19.1, rclone
  1.75.1) and checked against the vendors' SHA-256 at image build; the worker checks the
  restic binary's hash and version again at start.
* **PHI only on BAA-covered storage.** The software logs no paths and phones nowhere; the
  bucket is yours to choose and cover with an agreement.
* **Run L-APPEND-1 on every release** against the deployed gateway:
  [`rclone/append-only-test.sh`](rclone/append-only-test.sh).

## Admin UI behind a reverse proxy on the same host

A reverse proxy (any that can set the headers below) may sit in front of the admin listener on the
same host, to terminate TLS for the admin UI. The examples use documentation names and addresses:
`console.example.net` for the name operators type, `10.0.0.5` for the host's own private address,
`192.0.2.10` for an operator's workstation.

What the proxy must do:

* **Keep the `Host` header** the browser sent (do not rewrite it to the backend address).
* **Append the client to `X-Forwarded-For`** as a bare IP address (no port, no brackets), and set
  **`X-Forwarded-Proto: https`**. RFC 7239 `Forwarded` is not read.
* Connect to the admin listener from one fixed source address, the one you list below.

What to put in `config.json` (the proxy connects from the host's private address here):

```json
"admin_listen": "10.0.0.5:8080",
"admin_url": "https://console.example.net",
"trusted_proxies": ["10.0.0.5"]
```

* `admin_url` is the address operators type; it is the link in e-mails, and its host is accepted in
  the `Origin` and `Referer` of a form post even when the proxy does not pass `Host` through.
* `trusted_proxies` lists the proxy's source address (IP addresses or CIDR prefixes; environment
  `CRYO_TRUSTED_PROXIES`, comma-separated). Name only the proxy: anything that can connect to the
  admin listener from a listed address can name any client address. If the proxy connects over
  loopback instead (`admin_listen` `127.0.0.1:8080`, `trusted_proxies` `["127.0.0.1"]`), the admin
  listener is not reachable from the network, but every process on the host can then speak for a
  client. `0.0.0.0/0` and `::/0` are refused.
* `cryo-console doctor` prints a `trusted proxies` line naming the list. Its `admin tls` warning for a
  non-loopback `admin_listen` still shows; with the proxy on the same host that hop never leaves it.

What the audit trail then shows: a sign-in through the proxy is recorded with the workstation's
address (`192.0.2.10`) in the audit row, the session list and the account's last sign-in address,
and the per-address login limit counts each workstation on its own. A request that reaches the
admin listener from any address not in the list, with or without forwarded headers, is recorded
with its own address; its headers are ignored, and only a listed proxy may say the connection was
https. Without `trusted_proxies`, every row shows the proxy's address (`10.0.0.5`) and everyone
behind the proxy shares one login limit. The agent listener never reads forwarded headers. The rule
is recorded in [`../docs/decisions/`](../docs/decisions/) ("trusted proxies").
