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
