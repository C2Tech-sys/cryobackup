# Upgrade notes

What an operator must check before replacing the console binary with a newer release. Each section
names the release line it applies to; read every section between the version you run and the one
you install. Run `cryo-console doctor` with the new binary and your current config.json before you
apply the upgrade: it reports every configuration problem the new console would refuse to start
with, without starting it. Doctor is safe to run before the upgrade, and beside a running console:
it opens the database read-only, changes no data and applies no migration, and runs no checkpoint,
so the database file and its WAL stay as they are; it may create the -shm and an empty -wal file.
Run it as the service account the console runs as: to read the database SQLite must be able to
create the -shm file next to it, and doctor says so when it cannot. On a database older than the binary it prints the recorded
schema version, the version the first start will migrate it to and the pending migrations, and
reports the checks that need the new schema as NOT CHECKED.

## The "Open the console" link in e-mails is `admin_url`

**Set `admin_url` to get the console link in e-mails.** New optional config key (environment
override `CRYO_ADMIN_URL`): the address you type to open the console, for example
`"admin_url": "https://console.example.net:8080"`. Until now every message linked to `public_url`,
which is the agent API address and serves no console page, so the link never opened the console.
Messages now link to `admin_url` and to nothing else: with `admin_url` unset there is no link and no
button (the plain-text part still says to open the console, without an address), and `public_url`
is never used as a fallback. `public_url` itself is unchanged.

`admin_url` has the shape of `public_url`: `http://` or `https://`, a host name or IP address, an
optional port, an optional trailing slash, no path, user name, query or escapes. A value of any
other shape stops the console from starting, with a message that names the key and does not repeat
the value; `cryo-console doctor` shows the problem before the upgrade is applied.

With mail configured and `admin_url` unset, `cryo-console doctor` prints a warning (not a failure),
`WARN admin_url: admin_url is not set: e-mails carry no console link`, and `serve` logs one
warning at start. With it set, doctor prints `OK admin_url` with the address the links use. System
health shows the same on its own row, "Console link in e-mails", beside "Alert e-mail": a warning
while mail is configured and `admin_url` is not (it raises the overall state to warn, never to
failed), OK with the address when it is set, not applicable when mail is not configured.

## M4: alert e-mail, the daily digest and the heartbeat ping (ADR 0020)

### Schema v8

The console database moves to schema version 8. From v0.3.0 (schema version 6) the first start of
the new binary applies two migrations: `0007_adr19_purge` (device purge and customer delete) and
`0008_m4_alerts` (the `alarm_episodes` table and the digest switch). They run by themselves.

A database that has been migrated cannot be opened by the old binary: it refuses to start with
"database is newer than this binary". Take the backup before the first start of the new binary.
On a systemd host, `cryo-upgrade.sh` does it (deploy/systemd/README.md, "Upgrades"): it copies the
database with its `-wal` and `-shm` files to a new folder under `/var/backups/cryobackup/` before
it installs the new binary, and a rollback restores that copy.

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

### `cryo-console doctor` checks the mail setup

Doctor now ends with a mail section and a heartbeat line. Without an smtp block it prints one WARN,
"not configured: alert e-mail and the daily digest are off", and nothing in the section can fail
doctor. With one, it checks the block as `serve` and every send do (`mail config`), checks the
password file (`mail password file`: it exists, this account can read it, it is not empty, it holds
one line and, on Unix, it is not open to group or others, which the sender refuses; Windows has no
mode to check, so protect the file with its ACL), then probes the relay live (`mail relay`): it
connects, waits for the greeting, says EHLO, starts TLS as configured, signs in when a user name is
set and quits without sending a message. A failure names the stage and the class word (`dns`,
`refused`, `timeout`, `cert_invalid`, `tls_handshake`, `starttls_missing`, `auth_rejected`,
`protocol` and the rest of the sender's list) with one fixed line of advice; the relay's host and
port are printed, never the user name, the password or what the relay answered. Any FAIL here fails
doctor when the smtp block is present. On a database at the new schema it counts the recipients with
each switch (`mail recipients`) and warns when mail is configured and nobody has the alerts switch,
and when the digest is on and nobody has the digest switch; on an older database these lines are
NOT CHECKED until the upgrade. The `heartbeat ping` line says whether `healthcheck_ping.url` is set
(scheme and host only, never the path or the token), with its method and interval; doctor never
sends the ping, because a ping tells the outside monitor that the console is healthy. Doctor has no
flag that skips network checks: the relay probe always runs.

### The daily digest is on by default, and banner C14 is live

The `daily-digest` job now runs: every day at `digest_local_time` (07:00 by default) in the display
zone it mails the digest to each recipient that has the digest switch, also on days when everything
is green. A console that was stopped at that time sends the day's digest when it starts again the
same day; it never sends a missed day's digest late. Turn it off with the "Daily digest" setting
(`digest_enabled`) if you do not want it.

The digest banners are now judged on every installation:

- No smtp block: the warning "Daily digest not sent: e-mail is not set up. Configure the smtp block
  of config.json, or turn the digest off." It stays until mail is configured or the digest is off.
  (Beside it, the existing "Alert e-mail not checked: SMTP is not configured" warning.)
- Mail configured but no recipient receives the digest: the warning "The daily digest has no
  recipient: it is not mailed to anyone." (C20).
- Mail configured and a recipient set, but the day's digest was not delivered within 30 minutes of
  its time: the critical banner C14, "Daily digest was not delivered."

Right after the upgrade, before the day's digest time has come, C14 can name the previous day's
digest, which the old console never sent. It clears when the day's first digest goes out.

### The messages take the console's dark look (ADR 0022)

The HTML part of the alert, the recovery notice, the daily digest and the test message is now in
the console's dark theme, with the brand mark carried inside each message as an inline image (it
is not remote content: the mail client makes no request to show it). What each message says is
unchanged, and so are the plain-text parts and the subjects, so mail rules that match a subject
keep working. A message is a few kilobytes larger (the mark). A mail client's own dark or light
mode may recolour parts of it; every state is also written as a word. The console's digest
preview and the /design/mail samples load the mark from a new public route, `GET /mail/mark`,
on the admin listener; a reverse proxy that allows only listed paths must allow it as it allows
`/static/`.
