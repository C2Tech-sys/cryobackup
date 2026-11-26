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

## 0.4.0: the pilot slice (ADRs 0026 to 0031)

Release 0.4.0 puts restore in the console, the console's own backup, the escrow recheck and restore from a retired device
into production ([ADR 0026](decisions/0026-the-pilot-slice.md)). The console binary and the agent MSI carry the same version.

**Order: the console first, then the agents.** The console is the one that knows the new restore routes, so upgrade it and
check it before any agent is touched. Agents are pushed afterwards, one site at a time, as any MSI upgrade.

### The console

- **No schema change.** The database stays at schema version 8. A console coming from v0.3.0 (schema 6) still migrates to 8 on
  its first start: read "M4" below.
- **Run `cryo-console doctor` with the new binary first**, as above. It checks the new keys below.
- **New configuration.** None is required to start; a console without them behaves as before, except that C11 is raised (see
  the next section).

| key | environment | what it does | where it is described |
| --- | --- | --- | --- |
| `admin_url` | `CRYO_ADMIN_URL` | the address an operator types to open the console; the link in e-mails | "The "Open the console" link in e-mails is `admin_url`", below |
| `trusted_proxies` | `CRYO_TRUSTED_PROXIES` | the reverse proxies in front of the admin listener whose forwarded headers are believed | "The admin UI behind a reverse proxy", below |
| `drill` | `CRYO_DRILL` | marks a console restored from its self-backup for a drill; **never set it on the production console** | [`docs/runbooks/console-disaster-recovery.md`](runbooks/console-disaster-recovery.md), section 5 |
| (worker only) | `CRYO_MAINTAIN_SELF_PREFIX` in `maintain.env` | the storage prefix of the console's self-backup | the next section |

- **`cryo-console init` writes `"drill": false` and `"admin_url": ""` into every config.json it creates**, set or not. See
  "What a rollback must remove first".

### Turn the self-backup on (CRYO_MAINTAIN_SELF_PREFIX)

The console database is now backed up by the maintenance worker every 6 hours into a restic repository of its own
([ADR 0029](decisions/0029-console-self-backup-is-a-restic-repository.md)). **It is off until you give it a prefix, and an off
self-backup is an alarm**: right after the upgrade the C11 banner reads "no self-backup has been written" and the System page's
"Database self-backup" row says "no self-backup has been written (not_configured: CRYO_MAINTAIN_SELF_PREFIX is not set)". That is intended. To clear it:

1. **Choose the prefix.** It must not equal the device prefix (`CRYO_MAINTAIN_PREFIX`), lie inside it, or begin with it. With
   device prefix `<prefix>`, the worker refuses `<prefix>`, `<prefix>/x`, `<prefix>-console` and `<prefix>x`. Pick a name
   outside it, for example `<self-backup prefix>`. **A prefix that breaks the rule is a configuration error, not a job failure:** `cryo-maintain` refuses to start, prints the reason on stderr and exits 2, and systemd keeps restarting it, so look in `journalctl -u cryo-maintain` (and expect the worker banner C07 meanwhile). Refused: a prefix equal to the device prefix, inside it, beginning with it or containing it; any self prefix while `CRYO_MAINTAIN_PREFIX` is empty (device repositories at the bucket root); and a segment with characters other than letters, digits, `.`, `_` and `-`.
2. **Give the admin key its rights.** The worker writes through the `[admin]` remote, never the gateway. That key needs list,
   get, put and delete on `<bucket>/<self-backup prefix>/*`. An admin key with bucket-wide rights already has them. A key scoped
   by prefix needs the statement in [`deploy/systemd/README.md`](../deploy/systemd/README.md), "Console self-backup". The
   **gateway** key must have no right there: every resource in its policy ends with a slash (`<bucket>/<prefix>/*`, never
   `<bucket>/<prefix>*`).
3. **Set it and restart the worker:**

   ```sh
   # /etc/cryobackup/maintain.env
   CRYO_MAINTAIN_SELF_PREFIX=<self-backup prefix>
   ```

   ```sh
   sudo systemctl restart cryo-maintain
   ```

4. **Check the worker stayed up** (`systemctl status cryo-maintain`), **wait for one pass, then look.** The System page shows the time of the first self-backup and C11 clears. Run
   `cryo-console self-backup verify` once (the disaster-recovery runbook, section 2); it must exit 0.
5. **Run the four storage checks of ADR 0029** on the real account before relying on it: the admin key's rights on the prefix,
   the gateway key's lack of any, the cost of `forget --prune` under your bucket's versioning, and the gateway policy's
   prefix scope.
6. **Keep `CRYO_MAINTAIN_MAX_STREAMS` at 2 (the default).** At 3 the self-backup has no restic slot, the job row says `no_slot`
   and C11 alarms (the runbook, section 6).
7. **Make the offline kit and drill it** (the runbook, sections 1 and 5). The self-backup cannot hold the master key or the
   storage credentials.

### Other behaviour that changes on upgrade

- **Restore.** The device page's "Restore files" opens a wizard: several files and folders as one zip, to the original place,
  or to "A new folder on <host>" ([`docs/runbooks/restore.md`](runbooks/restore.md)). Every restore needs the operator role and
  a two-factor check made in the last 5 minutes. A device restore is refused at once for a device whose agent is older than
  0.4.0 (`agent_too_old`); the zip works for every device.
- **The escrow recheck is on.** Every escrowed key, retired devices included, is opened with `restic cat config` once a week
  by the worker (at most 5 devices a pass), and a key not verified for `escrow_recheck_days` (30 by default) raises
  ESCROW_UNVERIFIED ([ADR 0030](decisions/0030-escrow-rechecked-and-retired-devices-restorable.md)). A console with many devices
  verified only at enrollment can show that warning until the worker has been through the fleet. A retired device whose check
  fails raises a fleet banner (C22, C22b) instead of its own alarm. The device page has a "Verify now" button (admin role); where your build has none, `POST /api/v1/devices/<device id>/escrow/verify` with an admin API token does the same.
- **A retired device can be restored by download.** Device modes stay refused for it.
- **No forget, prune or check on device repositories during the pilot** (ADR 0026, a 45-day box from the pilot client's first
  backup). Repositories only grow.

### The agent 0.4.0

Install the new MSI over the old one (`msiexec /i`, or the RMM wrapper; `TOKEN` is not needed, the device stays enrolled;
[`deploy/windows/README.md`](../deploy/windows/README.md)). 0.4.0 adds restore to the device
([ADR 0027](decisions/0027-restore-to-the-original-device.md)): it takes a restore command, fetches the selection with its
device token, restores with its own login through the gateway, and reports counts by class, never a path.

- **A console older than this build refuses nothing new.** A 0.4.0 agent under a console older than 0.4.0 backs up exactly as a
  0.3.x agent does: the restore statistics are omitted from backup records, and an older console ignores a field it does not
  know. Such a console cannot create a restore command, so the agent's restore code never runs.
- **A 0.3.x agent under a 0.4.0 console** backs up as before. Device restores to it are refused when the request is made; the
  zip works.
- **Run the SYSTEM lab checks before the first device restore on a production machine**
  ([`docs/lab/p22-agent-system.md`](lab/p22-agent-system.md)). They need the agent's own account, which the build machine could
  not give. If one fails, that device mode does not ship (ADR 0027). The zip download is not affected.
- At its start the 0.4.0 agent removes restore staging folders a cut-short restore left (`<L>:\.cryobackup-restore\<id>`) and
  side folders whose restore did not succeed.

### What a rollback must remove first

A console binary older than the keys refuses a config.json that contains them and does not start
(`json: unknown field "drill"`). This extends "Rolling back after setting `admin_url` or `trusted_proxies`", below. **Before you
put an older binary back, remove from config.json:** `admin_url`, `trusted_proxies` and `drill`. Remove them from the
environment too (`CRYO_ADMIN_URL`, `CRYO_TRUSTED_PROXIES`, `CRYO_DRILL`); an older binary ignores the variables, but a file that
still carries one invites the mistake.

- `cryo-console init` of this release writes `"drill": false` and `"admin_url": ""`, so a config made by it always needs the
  keys removed. `cryo-upgrade.sh` does not edit config.json, and its own recovery path starts the old binary on the config as it
  is: add the new keys **after** the upgrade has been checked, not before.
- If the version you ran before is v0.3.0, the schema-8 rule of "M4" below applies: restore the database copy `cryo-upgrade.sh`
  made, and accept that what was written after it is lost.
- An older console has no restore route: let every queued or running device restore end, or cancel it, before rolling back.
- The self-backup repository stays in storage and stays valid; an older worker does not write to it, so nothing is backed up
  while it runs. Leave the repository where it is.
- Agents: install the older MSI. An older agent does not know `<L>:\.cryobackup-restore` or `C:\CryoBackup-Restore`; look for
  those folders on a machine where a restore was cut short and remove them by hand.

## The admin UI behind a reverse proxy: `trusted_proxies` (ADR draft "trusted proxies")

**New optional config key `trusted_proxies`** (environment override `CRYO_TRUSTED_PROXIES`, entries
comma-separated): the IP addresses or CIDR prefixes of the reverse proxies in front of the admin
listener. Unset, the default, nothing changes. Set, a request whose direct peer is in the list is
recorded with the client address the proxy put in `X-Forwarded-For` (the rightmost entry that is not
itself a listed proxy): audit rows, session rows, the last sign-in address and the per-address login
limit then see the operator's address instead of the proxy's. A request from any other peer is
judged on its own address, whatever headers it carries. With the list set, only a listed proxy may
say `X-Forwarded-Proto: https`; with it unset a loopback or private peer still may, as before. The
agent listener never reads any of these headers. `0.0.0.0/0` and `::/0` are refused (they would trust
everyone), as is any entry that is not an IP address or prefix; `cryo-console doctor` shows the
problem, and prints one `trusted proxies` line naming the list when it is set. Setup:
[`deploy/README.md`](../deploy/README.md), "Admin UI behind a reverse proxy on the same host".

The host of `admin_url` is now also accepted in the `Origin` and `Referer` of a form post or API
call, beside the address the browser connected to and the `public_url` host.

`public_url` and `admin_url` now refuse brackets around anything that is not an IPv6 address, for
example `https://[10.0.0.5]:8080`; write `https://10.0.0.5:8080`. Such a value was accepted before,
but no URL parser of this toolchain reads it (an IPv4 address in brackets is not a valid URL host),
so it never worked: agents could not use it as `public_url` and e-mails carried no link for it as
`admin_url`. A bracketed IPv6 address such as `https://[2001:db8::1]:8443` is unchanged.

## Rolling back after setting `admin_url` or `trusted_proxies`

A console binary older than the key refuses a config.json that contains it, and does not start:
`config: parsing the config file: json: unknown field "admin_url"` (or `"trusted_proxies"`). Before
you put an older binary back, remove the keys it does not know from config.json (and from the
environment, `CRYO_ADMIN_URL` and `CRYO_TRUSTED_PROXIES`, which an older binary ignores but which
are harmless). Two things to know:

- `cryo-console init` from the release that added `admin_url` writes `"admin_url": ""` into the
  config.json it creates, set or not, so a config made by that `init` needs the key removed before
  an older binary can read it. `init` does not write `trusted_proxies` while it is empty.
- `cryo-upgrade.sh` does not handle this; you must. It reads config.json (for `db_path` and the
  listener addresses) but never copies or edits it. The rollback commands it prints on a failed
  start restore the old binary and the database copy, not the config. Its own early recovery path,
  which starts the services again on the old binary when something fails after they were stopped and
  before the new binary is installed, uses config.json as it is: if you added either key before
  running the upgrade, the old binary refuses it and the services stay down. The "doctor before"
  step shows this first, as a `FAIL config` line about the unknown field that the script reports
  only as a warning. Upgrade first, check it, and add the new keys afterwards.

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
