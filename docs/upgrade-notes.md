# Upgrade notes

What an operator must check before replacing the console binary with a newer release. Each section
names the release line it applies to; read every section between the version you run and the one
you install. Run `cryo-console doctor` with the new binary and your current config.json before you
apply the upgrade: it reports every configuration problem the new console would refuse to start
with, without starting it.

## M4: alert e-mail, the daily digest and the heartbeat ping (ADR 0020)

### Schema v8

The console database moves to schema version 8 (migration `0008`, the `alarm_episodes` table). The
migration runs by itself at the first start of the new binary. Take the usual backup of the
database file first.

### `smtp.security: "plain"` needs `smtp.allow_plaintext: true`

A relay on port 25 without TLS is accepted only when you opt in explicitly:

```json
"smtp": { "security": "plain", "allow_plaintext": true }
```

(`CRYO_SMTP_ALLOW_PLAINTEXT=true` in the environment does the same.) Without it the console refuses
to start. A plain connection never carries credentials: `smtp.username` together with
`smtp.security: "plain"` is refused as well. Use `starttls` (587) or `tls` (465) for a relay that
needs a login.

### `healthcheck_ping.url` must be https

The optional outbound heartbeat ping (`healthcheck_ping`) now refuses a URL that is not https; plain
http is accepted only for a loopback host (`localhost`, `127.0.0.1`, `::1`). It also refuses a URL
with a user name or password in it, with white space before or after it, or with a port outside 1
to 65535. A refused URL stops the console from starting, with a message that names the key and
never repeats the URL (it usually carries the monitor's token). `cryo-console doctor` shows the
problem before the upgrade is applied. The ping stays off while the URL is empty, the default.

Two things to know about the ping:

- The proxy environment variables (`HTTPS_PROXY`, `NO_PROXY`) are honoured. An https ping goes
  through the proxy as a tunnel, so the proxy sees the monitor's host name, never the path, the
  query or the token in them.
- The console warns (banner C19, "The external heartbeat ping is failing") after three failed pings
  in a row. At the default interval of 300 seconds that is about fifteen minutes; at a long interval
  it is three intervals, up to three days at 86400.
