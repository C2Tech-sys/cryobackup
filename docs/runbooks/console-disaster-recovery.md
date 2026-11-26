# Runbook: rebuild the console from its self-backup

The console database is backed up by the maintenance worker into a restic repository of its own, in the
same bucket as the device repositories, under a separate prefix
([ADR 0029](../decisions/0029-console-self-backup-is-a-restic-repository.md)). This runbook covers what
you must keep offline, how to rebuild a console on a clean Linux host from that backup, what a rebuild
loses, and how to drill it.

Paths are the standard layout of [`deploy/systemd`](../../deploy/systemd/README.md). If your
`config.json` names other paths, those win. The compose layout uses the container paths in
[`deploy/compose`](../../deploy/compose/README.md).

## 1. The offline kit

The self-backup is encrypted with a password derived from the master key. It therefore cannot hold the
master key, nor the credentials needed to reach it. Keep these items offline: a password manager, an
encrypted USB stick in a safe, or both. Test that you can read them.

| item | where it lives on a standard install | why the rebuild needs it |
| --- | --- | --- |
| master key | `/etc/cryobackup/master.key` (`master_key_path` in `config.json`) | it opens the self-backup and every escrowed device key |
| admin rclone file | `/etc/cryobackup/rclone-admin.conf` (the `[admin]` remote, the worker's `RCLONE_CONFIG`) | the only credential that can read the self-backup prefix |
| gateway rclone file | `/etc/cryobackup/rclone-gateway.conf` (the `[store]` remote) | the agents' path to storage |
| worker variables | `/etc/cryobackup/maintain.env` (`CRYO_MAINTAIN_BUCKET`, `CRYO_MAINTAIN_PREFIX`, `CRYO_MAINTAIN_SELF_PREFIX`, ...) | it says where the self-backup is |
| gateway variables | `/etc/cryobackup/gateway.env` | the gateway's port, bucket and prefix |
| mail password file | the file `smtp.password_file` in `config.json` names (for example `/etc/cryobackup/smtp-password`) | alert mail and the digest |

`config.json` itself is inside every self-backup, but keep a copy in the kit as well. You need the
bucket and prefixes before you can restore anything.

The kit does not hold TLS keys. Issue them again on the new host
([`deploy/rclone/tls.md`](../../deploy/rclone/tls.md)), or let `cryobackup-certsync` pick up what
your ACME client issues.

Update the kit whenever one of these changes: a new storage key pair, a new self-backup prefix, a new
mail password. The master key never changes in place.

## 2. Is the self-backup working?

- **System page, "Database self-backup", and the C11 banner.** C11 is raised (alarm) at once when no
  self-backup has been written, and when the newest is older than 26 h. It names the class of the failing run, for example
  `not_configured` (set `CRYO_MAINTAIN_SELF_PREFIX`) or `no_slot` (see section 6). C11 is critical when
  the newest tested restore failed, or when no verify verdict has been reached for more than 8 days
  (counted from the first backup while there is none yet). The text then names the class of the last
  attempt, for example `network`. The System page row follows the same rules and is never "ok" while
  C11 is critical for the self-backup: it reads failed, with the same text. With no backup written it
  reads not checked and names the class (`not_configured`, `no_slot`).
- **On the host**, as the worker's user and with the worker's environment:

```sh
sudo -u cryobackup sh -c 'set -a; . /etc/cryobackup/maintain.env; RCLONE_CONFIG=/etc/cryobackup/rclone-admin.conf \
  cryo-console self-backup list --master-key /etc/cryobackup/master.key'
sudo -u cryobackup sh -c 'set -a; . /etc/cryobackup/maintain.env; RCLONE_CONFIG=/etc/cryobackup/rclone-admin.conf \
  cryo-console self-backup verify --config /etc/cryobackup/config.json'
```

`list` prints the time (UTC), size and id of each snapshot, never file contents. `run` writes one
backup now, and `verify` runs the weekly tested restore now. Both make their job due and run one
maintenance pass, exactly as the scheduler does. If the running worker holds the self-backup's lease
at that moment, they print that the job stays due, and the worker runs it within a minute. Leave
`CRYO_MAINTAIN_SCRATCH_DIR` unset here (the default is fine). It must not be the running worker's
`/run/cryo-maintain`.

Exit codes: 0 ok; 1 the backup, verify, listing or restore failed; 2 a usage or configuration error,
which includes a self-backup that is not configured or has no restic slot.

## 3. Rebuild a console on a clean Linux host

You need the offline kit, a Linux host with network access to the object storage, and the
`cryo-console` binary of the same or a later version than the one that wrote the backup. The schema
version is in the restored manifest. The console migrates forward and never back.

1. **Install the binaries** as in [`deploy/systemd/README.md`](../../deploy/systemd/README.md),
   "Install": `cryo-console`, `restic` 0.19.1 and `rclone` 1.75.1 in `/usr/local/bin`, each checked
   against its published sha256. Create the `cryobackup` user and `/etc/cryobackup` (0750), and copy
   the three unit files.
2. **Put the offline kit in place.** The console and the worker run as `cryobackup`, so the files
   they read must be readable by that user, and two of them are refused when group or others can open
   them: `cryo-console doctor` warns about a `master.key` open to group or others, and the mail sender
   refuses a password file that is. Those two are therefore owned by `cryobackup` itself. The rest is
   `root:cryobackup` 0640, as in [`deploy/systemd/README.md`](../../deploy/systemd/README.md). From the
   folder that holds the kit:

```sh
install -o cryobackup -g cryobackup -m 0400 master.key /etc/cryobackup/master.key
install -o cryobackup -g cryobackup -m 0600 smtp-password /etc/cryobackup/smtp-password   # the file smtp.password_file names
install -o root -g cryobackup -m 0640 rclone-admin.conf rclone-gateway.conf maintain.env gateway.env /etc/cryobackup/
```

   Check it before you go on: `sudo -u cryobackup cat /etc/cryobackup/master.key >/dev/null` must
   print nothing. Section 2's commands depend on it.
3. **Restore.** This needs no database and no running console:

```sh
cryo-console self-backup restore \
  --rclone-config /etc/cryobackup/rclone-admin.conf --remote admin \
  --bucket <bucket> --prefix <self-backup prefix> \
  --master-key /etc/cryobackup/master.key \
  --out /root/console-restore
```

   It restores the newest snapshot, or `--snapshot <id>` from `self-backup list`. It restores
   `<id>:<staging path>`, so `console.db`, `config.json` and `manifest.json` land directly in
   `--out`. `--out` must be empty or not exist yet. Before printing `ok`, it checks a copy of the
   restored database against the manifest: sha256 and size, `PRAGMA integrity_check`, schema version,
   row counts per table, the master key's id, and one escrowed key unwrapped with your master key.
   On any failure, `--out` is left empty. A wrong master key exits 1 with class `wrong_password`. The
   command prints the bucket and prefixes from the manifest, and the next steps.
4. **Place the files.** These paths are the standard ones. Use the paths of the restored
   `config.json` if they differ.
   - `console.db` goes to `/var/lib/cryobackup/console.db`, owned by `cryobackup`, mode 0640. Delete
     any `console.db-wal` and `console.db-shm` beside it first.
     `install -d -o cryobackup -g cryobackup -m 0750 /var/lib/cryobackup && install -o cryobackup -g cryobackup -m 0640 /root/console-restore/console.db /var/lib/cryobackup/console.db`
   - `config.json` goes to `/etc/cryobackup/config.json`, `root:cryobackup`, 0640
     (`install -o root -g cryobackup -m 0640 /root/console-restore/config.json /etc/cryobackup/config.json`).
     Check its host names, listen addresses and TLS paths for the new host.
   - `manifest.json` is for your records. The console does not read it.
5. **Issue TLS certificates** for the agent listener and the gateway (`/etc/cryobackup/tls/`), for
   the same host names as before ([`deploy/rclone/tls.md`](../../deploy/rclone/tls.md)), and point
   DNS at the new host.
6. **Start and check:**

```sh
systemctl daemon-reload
systemctl enable --now cryo-console cryo-maintain rclone-gateway
cryo-console doctor --config /etc/cryobackup/config.json
```

   `cryo-console` writes the gateway's htpasswd into `/var/lib/cryobackup-auth` from the restored
   database. Sign in, then check the System page and the fleet board as the agents check in.

## 4. What a rebuild loses (ADR 0029)

- **Everything after the last self-backup.** Run reports and audit rows since then are gone. The
  backups the agents made since then are not: restic snapshots live in the device repositories, not
  in the console database. Only the console's records of those runs are lost.
- **Devices enrolled since the last self-backup.** The restored console does not know them, so it
  refuses their agents, which go REFUSED ([ADR 0018](../decisions/0018-agent-after-retire-and-after-an-outage.md)).
  Enroll them again with a new token. Their old repositories stay in storage as orphans. Remove them
  by hand once the new enrollment has a good backup.
- **Changes since then**: policies, users, settings and recipients changed after the snapshot. Redo
  them.
- **TLS keys** are not restored. Issue them again (step 5).

Self-backups run every 6 hours, so the usual loss is under 6 hours of console state. C11 tells you
when that gap grows.

## 5. The drill (ruling C8)

Drill on a throwaway Linux virtual machine in your lab, never on the production host and never on a
machine that can reach production in a way that matters.

1. Create the VM. Install the binaries and the offline kit (section 3, steps 1 and 2).
2. Run `self-backup restore` (step 3) with `--drill` (or `CRYO_DRILL=true` in your shell). The production
   self-backup repository is live under the production console; with `--drill` the read carries
   `--no-lock`, so it leaves no lock object there, and the restore writes only to `--out`. It must end in
   `ok`: the manifest checks and the escrow unwrap passed. `self-backup list --drill` is the same read.
3. Place the files (step 4). Skip step 5: a drill issues no certificate and changes no DNS.
4. **Turn drill mode on before anything starts.** In `/etc/cryobackup/config.json` set
   `"drill": true`. Both `cryo-console` and `cryo-maintain` read it from that file. `CRYO_DRILL=true`
   in a unit's environment does the same for that unit only, so prefer the file, and make sure no
   `CRYO_DRILL=false` is left in `maintain.env` (an environment value beats the file). The two programs
   decide independently; the worker reports its mode to the console, and a console and a worker that
   disagree raise the red "WORKER DRILL MODE MISMATCH" banner on every page and fail the
   `maintenance worker` line of `doctor`. Then check:

```sh
cryo-console doctor --config /etc/cryobackup/config.json
```

   It must print `WARN  drill  on: drill copy. ...`. If it prints `OK  drill  off`, stop and fix the
   file. Run it again after step 5 has started the worker: the `maintenance worker` line must be `OK`,
   not `FAIL ... drill mode differs`. The agent certificate is not needed: in drill mode `tls.cert` and `tls.key` are not
   checked, because no agent listener is opened. If `admin.tls` names certificate files, issue a
   throwaway self-signed pair on the VM or clear both keys. Keep `admin_listen` on `127.0.0.1` and
   reach it through an SSH tunnel (`ssh -L 8080:127.0.0.1:8080 <drill VM>`).
5. **Start the console and the worker, never the gateway:**

```sh
systemctl daemon-reload
systemctl start cryo-maintain cryo-console     # start, not enable; do not start rclone-gateway
journalctl -u cryo-console -n 20               # must show the line "DRILL COPY: this console is a restored copy. ..."
ss -ltn                                        # no listener on the agent port (agent_listen, 8443 by default)
```

   What drill mode does: `cryo-console` opens no agent listener (the port is not opened at all),
   sends no mail (every sender is an in-process fake, and mail counts as not configured), and runs
   no health prober, alarm evaluator with its mail, digest, mail retry, heartbeat ping, gateway login
   file sync, device purge or abandoned-run sweep. Nor does it probe its public agent address. It
   still expires sessions, queued commands and spent restores in its own database. `cryo-maintain`
   serves only `ping`, `ls`, `dump`, `cat_config` and `snapshots`, refuses every other admin-path
   method, including `purge_repo`, with `unavailable`, and runs no scheduler pass: no device
   maintenance, no usage sample, no escrow verify, no self-backup. Its restic runs carry `--no-lock`,
   so a read leaves no lock object, and a stalled download is stopped without the stale-lock unlock
   that production runs after it. A drill copy writes nothing to storage. `self-backup run` and
   `verify` exit 2 on a drill copy.
6. **Log in, browse a real snapshot, download one file.** Every page, the sign-in page included,
   carries the red "DRILL COPY" strip; if it is missing, stop. Sign in through the tunnel as an owner
   from the restored database. Open a device that had backups before the snapshot, open its restore page,
   browse a snapshot and download one small file as a zip. Open the file. The worker reads production
   storage with the admin credential for this, read-only: no lock object, nothing written.
7. **Confirm that no mail arrived.** No alert recipient and no digest recipient received anything
   from the drill copy, and the agents kept reporting to the production console.
8. **Destroy the VM**, disks included: it holds the master key and the admin credential.

**Never start `cryo-console` or `cryo-maintain` on a drill VM without drill mode on.** Without it,
a restored console runs every production job: it sends mail, pings the health monitor, rewrites the
gateway's login file, and its worker writes and prunes on production storage as a second console on
the same bucket, which is unsupported. If drill mode cannot be confirmed in step 4, stop after
step 3. Steps 1 to 3 already prove the parts that matter most: the kit opens the backup, the copy
is intact, and your master key unwraps its escrowed keys.

Record the date, the snapshot id and the result. Repeat the drill after every change to the offline
kit.

## 6. `CRYO_MAINTAIN_MAX_STREAMS`: keep the default 2

The worker runs at most 4 restic processes at once. Technician downloads may hold up to
`CRYO_MAINTAIN_MAX_STREAMS` of them, for as long as each download takes. The self-backup runs only
while the slots downloads can never take leave room for it plus at least one more. That last slot is
shared, first come, by device maintenance, browse, snapshots, the storage probe, escrow verify and
purge.

- At **2** (the default), the self-backup runs.
- At **3**, the self-backup never runs. Each due run records `no_slot` on the `db-self-backup` job row
  and `self-backup run` exits 2 with class `no_slot`. On a console that has never written one, C11
  raises "no self-backup has been written (no_slot: ...)" at once: that alarm does not wait. Only the
  other one waits: on a console that already has backups, the newest keeps ageing, and after 26 h
  C11 raises "self-backup older than 26 h (no_slot: ...)". The verify goes without a verdict as well,
  and C11 turns critical once no verdict has been reached for 8 days.
- At **1**, downloads queue sooner and nothing else changes.

Keep it at 2 unless technicians regularly need three downloads at once. If they do, raise it only for
as long as they need it, and accept that the console is not backed up meanwhile.
