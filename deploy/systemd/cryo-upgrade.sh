#!/usr/bin/env bash
# cryo-upgrade: replace the cryo-console binary on a systemd host, with a database copy first.
#
#   sudo cryo-upgrade <new-binary> <sha256>            dry run: checks only, changes nothing
#   sudo cryo-upgrade --apply <new-binary> <sha256>    does the upgrade
#
# Steps: check -> copy the binary in and verify its hash -> doctor (before) -> stop
# cryo-console and cryo-maintain -> copy the database -> install -> start -> doctor (after).
#
# Install it root-owned so the account that runs it cannot change it:
#   install -o root -g root -m 0755 cryo-upgrade.sh /usr/local/sbin/cryo-upgrade
# A narrow sudoers rule then lets a deploy account upgrade without a password (README.md,
# "Upgrades"). That account can then install any console binary it holds: the hash proves the
# file is the one the caller meant, not that it is trustworthy. Grant it only to an account you
# would also trust to build and copy the binary.
#
# Exit codes: 0 done (or dry run passed), 1 usage, 2 a check failed and nothing changed,
# 3 failed after the services were stopped (read the output: it says what state they are in),
# 4 upgraded and running, but doctor reported FAIL.

set -euo pipefail
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
umask 077

# --- Settings (match deploy/systemd/README.md; edit here if your host differs) ---
TARGET=/usr/local/bin/cryo-console
CONFIG=/etc/cryobackup/config.json
DB_PATH=/var/lib/cryobackup/cryobackup.db
BACKUP_ROOT=/var/lib/cryobackup/upgrade-backups   # stays inside the protected data directory
SERVICE_USER=cryobackup
UNITS_STOP=(cryo-console cryo-maintain)           # stop order
UNITS_START=(cryo-maintain cryo-console)          # start order
START_WAIT_S=30

APPLY=0
if [[ "${1:-}" == "--apply" ]]; then APPLY=1; shift; fi

usage() { echo "usage: cryo-upgrade [--apply] <new-binary> <sha256>" >&2; exit 1; }
say()   { printf '%s  %s\n' "$(date -u +%H:%M:%SZ)" "$*"; }
fail()  { say "FAIL: $2"; exit "$1"; }

[[ $# -eq 2 ]] || usage
SRC=$1
WANT=${2,,}
[[ $EUID -eq 0 ]] || fail 1 "run as root (sudo)"
[[ $WANT =~ ^[0-9a-f]{64}$ ]] || fail 1 "the sha256 must be 64 hex characters"

# --- Discovery: refuse early, before anything changes ---
[[ -f $SRC && ! -L $SRC ]] || fail 2 "$SRC is not a regular file (symlinks are refused)"
[[ -x $TARGET ]] || fail 2 "$TARGET does not exist; this script upgrades, it does not install"
[[ -r $CONFIG ]] || fail 2 "$CONFIG is missing"
[[ -f $DB_PATH ]] || fail 2 "database not found at $DB_PATH (set DB_PATH at the top of this script)"
id "$SERVICE_USER" >/dev/null 2>&1 || fail 2 "user $SERVICE_USER does not exist"
for u in "${UNITS_STOP[@]}"; do
	systemctl cat "$u" >/dev/null 2>&1 || fail 2 "unit $u is not installed"
done

# Copy first, then hash the copy: the file checked is the file installed, even if the
# caller's copy changes underneath us.
STAGE="$TARGET.new"
rm -f "$STAGE"
install -o root -g root -m 0755 "$SRC" "$STAGE"
trap 'rm -f "$STAGE"' EXIT
GOT=$(sha256sum "$STAGE" | cut -d' ' -f1)
[[ $GOT == "$WANT" ]] || fail 2 "sha256 mismatch: got $GOT; nothing changed"
say "OK    sha256 matches"

as_svc() { runuser -u "$SERVICE_USER" -- "$@"; }
first_line() { local s=$1; printf '%s' "${s%%$'\n'*}"; }
NEW_VER=$(as_svc "$STAGE" version 2>&1) || fail 2 "the new binary does not run: $(first_line "$NEW_VER")"
NEW_VER=$(first_line "$NEW_VER")
OLD_VER=$(as_svc "$TARGET" version 2>&1 || true)
OLD_VER=$(first_line "$OLD_VER")
say "OK    current: $OLD_VER"
say "OK    new:     $NEW_VER"

say "----  doctor before (current binary)"
as_svc "$TARGET" doctor --config "$CONFIG" || say "WARN  doctor reported FAIL before the upgrade (shown above)"

if [[ $APPLY -eq 0 ]]; then
	say "DRY RUN passed. Nothing changed. Run again with --apply to upgrade."
	exit 0
fi

# --- Quiesce ---
say "----  stopping ${UNITS_STOP[*]}"
for u in "${UNITS_STOP[@]}"; do systemctl stop "$u"; done
for u in "${UNITS_STOP[@]}"; do
	if systemctl is-active --quiet "$u"; then fail 3 "$u is still running after stop; nothing else changed"; fi
done

# From here a failure leaves the services stopped. Say so, and how to get back.
restart_old() {
	say "      starting the services again on the OLD binary"
	for u in "${UNITS_START[@]}"; do systemctl start "$u" || say "      $u did not start: journalctl -u $u"; done
}

# --- Act: database copy (services stopped, so the -wal and -shm files are settled) ---
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
BACKUP_DIR="$BACKUP_ROOT/$STAMP"
install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0750 "$BACKUP_ROOT"
install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0750 "$BACKUP_DIR"
for f in "$DB_PATH" "$DB_PATH-wal" "$DB_PATH-shm"; do
	if [[ -f $f ]]; then
		cp -p "$f" "$BACKUP_DIR/" || { restart_old; fail 3 "database copy failed; old binary restarted"; }
	fi
done
cp -p "$TARGET" "$BACKUP_DIR/cryo-console.prev" || { restart_old; fail 3 "binary copy failed; old binary restarted"; }
cmp -s "$DB_PATH" "$BACKUP_DIR/$(basename "$DB_PATH")" || { restart_old; fail 3 "database copy does not match; old binary restarted"; }
say "OK    database and old binary copied to $BACKUP_DIR"

# --- Act: install (rename is atomic on the same filesystem) ---
mv -f "$STAGE" "$TARGET"
trap - EXIT
say "OK    installed $NEW_VER"

say "----  starting ${UNITS_START[*]}"
for u in "${UNITS_START[@]}"; do systemctl start "$u" || true; done
deadline=$((SECONDS + START_WAIT_S))
while :; do
	down=()
	for u in "${UNITS_START[@]}"; do systemctl is-active --quiet "$u" || down+=("$u"); done
	[[ ${#down[@]} -eq 0 ]] && break
	if (( SECONDS >= deadline )); then
		say "FAIL: not running after ${START_WAIT_S}s: ${down[*]} (journalctl -u <unit> -n 50)"
		say "      Rollback, if the new schema is the cause (the old binary cannot open a newer schema):"
		say "        systemctl stop ${UNITS_STOP[*]}"
		say "        install -o root -g root -m 0755 $BACKUP_DIR/cryo-console.prev $TARGET"
		say "        cp -p $BACKUP_DIR/$(basename "$DB_PATH")* $(dirname "$DB_PATH")/"
		say "        systemctl start ${UNITS_START[*]}"
		say "      Restoring the database copy discards anything written since $STAMP."
		exit 3
	fi
	sleep 1
done
say "OK    ${UNITS_START[*]} active"

# --- Verify ---
say "----  doctor after (new binary)"
if as_svc "$TARGET" doctor --config "$CONFIG"; then
	say "DONE  $OLD_VER -> $NEW_VER. Copy kept in $BACKUP_DIR (remove old copies yourself)."
	exit 0
fi
say "DONE WITH FAIL: upgraded and running, but doctor reported FAIL (above). Copy in $BACKUP_DIR."
exit 4
