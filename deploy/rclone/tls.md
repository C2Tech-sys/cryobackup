# TLS for the gateway (and the agent listener)

Agents and endpoints talk to the gateway over HTTPS only; the gateway password travels
in the request. `rclone serve restic` terminates TLS itself (`--cert`, `--key`,
`--min-tls-version tls1.2`; see [`gateway-flags.md`](gateway-flags.md)). The console's
agent listener uses its own certificate. Both are plain PEM files, mounted read-only.

## File layout

```
tls/
  gateway.crt      full chain, leaf first
  gateway.key      private key, mode 0400
  agent.crt        console agent listener, full chain
  agent.key        mode 0400
```

Compose mounts `TLS_DIR` at `/tls` (read-only); the systemd layout uses
`/etc/cryobackup/tls/`. The files are git-ignored (`../.gitignore`). Owner: uid 10001
(compose) or the `cryobackup` user (systemd).

## Choose a certificate

restic verifies the gateway certificate against the endpoint's trust store (or the file
named by `RESTIC_CACERT`); the agent trusts the console by the SPKI fingerprint carried in
the enrollment token (trust on first use, architecture §8.3). A publicly trusted gateway
certificate therefore saves distributing a CA to every endpoint.

* **Let's Encrypt with DNS-01.** Works for a gateway that is not reachable on port 80 and
  gives a wildcard if you want one. Use any ACME client with your DNS provider's plugin,
  for example:

  ```sh
  certbot certonly --manual --preferred-challenges dns -d gateway.example.net
  # or: lego --dns <provider> --domains gateway.example.net --email <you> run
  ```

  Copy `fullchain.pem` to `tls/gateway.crt` and `privkey.pem` to `tls/gateway.key`.
  Renewal is every 60 to 90 days; certificate expiry is monitored by the console (C05).
* **Your own CA.** Issue a server certificate with the gateway's DNS name as a SAN. The
  endpoints must trust the CA (install it with the agent, or rely on the SPKI pin).
* **Lab only: self-signed.**

  ```sh
  openssl req -x509 -newkey rsa:3072 -nodes -days 30 \
      -keyout tls/gateway.key -out tls/gateway.crt \
      -subj "/CN=gateway.example.net" -addext "subjectAltName=DNS:gateway.example.net"
  chmod 0400 tls/gateway.key
  ```

  `restic` then needs `--cacert tls/gateway.crt` (or `RESTIC_CACERT`), and
  `../../test/lab/smoke-gateway.sh` takes `CACERT=tls/gateway.crt`.

## Rotation

1. Write the new `gateway.crt` and `gateway.key` next to the old ones, then move both into
   place (do not leave a new certificate beside an old key).
2. Restart the gateway: `docker compose restart gateway` or
   `systemctl restart rclone-gateway`.
3. Check: `../../test/lab/smoke-gateway.sh https://<gateway>:<port>`.

**UNCERTAIN (L-CERT-1, engine §3.1):** the engine sheet does not say whether
`rclone serve restic` reloads a renewed certificate without a restart. The htpasswd file is
hot-reloaded; nothing is documented for `--cert`/`--key`. Until the lab test (L-CERT-1,
milestone M5) answers it, treat a certificate renewal as a restart. A restart interrupts
in-flight uploads; restic retries and the agent records a deferral (`gateway`), never a
failure (architecture DH26).

## Minimum versions

`--min-tls-version tls1.2` on the gateway (rclone's default is tls1.0). The smoke script
checks that a TLS 1.1 handshake is refused. The agent listener enforces TLS 1.2 or later
in the console.
