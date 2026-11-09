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
DB_PATH=                                          # empty: read db_path from $CONFIG (data_dir/console.db if unset)
BACKUP_ROOT=/var/backups/cryobackup               # root:root 0700; the service account cannot write here
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
# The source file is read with the CALLER's rights, never root's: otherwise a sudo rule for
# this script would let its user read any file on the host (the master key, /etc/shadow) by
# naming it as the "binary". Root running the script directly reads as root.
CALLER=${SUDO_USER:-root}
as_caller() { if [[ $CALLER == root ]]; then "$@"; else runuser -u "$CALLER" -- "$@"; fi; }
as_caller test -f "$SRC" -a ! -L "$SRC" || fail 2 "$SRC is not a regular file you can read (symlinks are refused)"
[[ -x $TARGET ]] || fail 2 "$TARGET does not exist; this script upgrades, it does not install"
[[ -r $CONFIG ]] || fail 2 "$CONFIG is missing"
# The database is wherever the console's own config says, resolved the way the console does it
# (internal/console/config Resolve): db_path, or data_dir/console.db when db_path is empty.
if [[ -z $DB_PATH ]]; then
	command -v python3 >/dev/null || fail 2 "python3 is needed to read db_path from $CONFIG; or set DB_PATH at the top of this script"
	DB_PATH=$(python3 -I -c 'import json, os, sys
c = json.load(open(sys.argv[1]))
print(c.get("db_path") or os.path.join(c.get("data_dir") or "", "console.db"))' "$CONFIG") \
		|| fail 2 "cannot read db_path from $CONFIG; set DB_PATH at the top of this script"
fi
[[ $DB_PATH == /* ]] || fail 2 "database path '$DB_PATH' is not absolute; set DB_PATH at the top of this script"
[[ -f $DB_PATH ]] || fail 2 "database not found at $DB_PATH (set DB_PATH at the top of this script)"
say "OK    database $DB_PATH"
id "$SERVICE_USER" >/dev/null 2>&1 || fail 2 "user $SERVICE_USER does not exist"
for u in "${UNITS_STOP[@]}"; do
	systemctl cat "$u" >/dev/null 2>&1 || fail 2 "unit $u is not installed"
done
# The database directory belongs to the service account, so a compromised service could swap a
# database file for a symlink. Refuse that here and again just before the copy.
for f in "$DB_PATH" "$DB_PATH-wal" "$DB_PATH-shm"; do
	[[ ! -L $f ]] || fail 2 "$f is a symlink; refusing"
done
# Backups (database and the rollback binary) live where only root can write: a directory the
# service account controls could be pre-seeded with symlinks, or the rollback binary swapped.
if [[ -e $BACKUP_ROOT ]]; then
	[[ -d $BACKUP_ROOT && ! -L $BACKUP_ROOT ]] || fail 2 "$BACKUP_ROOT is not a plain directory"
	[[ $(stat -c '%u %a' -- "$BACKUP_ROOT") == "0 700" ]] || fail 2 "$BACKUP_ROOT must be owned by root with mode 0700"
fi
[[ $(stat -c %u -- "$(dirname "$BACKUP_ROOT")") == 0 && ! -L $(dirname "$BACKUP_ROOT") ]] || fail 2 "$(dirname "$BACKUP_ROOT") must be a root-owned directory"

# Copy first, then hash the copy: the file checked is the file installed, even if the
# caller's copy changes underneath us. The copy stays root-only (umask 077 -> 0600) until the
# hash matches, and is made executable only then.
STAGE="$TARGET.new"
rm -f -- "$STAGE"
trap 'rm -f -- "$STAGE"' EXIT
as_caller cat -- "$SRC" >"$STAGE" || fail 2 "cannot read $SRC as $CALLER; nothing changed"
GOT=$(sha256sum -- "$STAGE" | cut -d' ' -f1)
[[ $GOT == "$WANT" ]] || fail 2 "sha256 does not match the one given; nothing changed"
[[ $(head -c4 -- "$STAGE") == $'\x7fELF' ]] || fail 2 "$SRC is not an ELF executable; nothing changed"
chown root:root -- "$STAGE"
chmod 0755 -- "$STAGE"
say "OK    sha256 matches"

as_svc() { runuser -u "$SERVICE_USER" -- "$@"; }
first_line() { local s=$1; printf '%s' "${s%%$'\n'*}"; }
NEW_VER=$(as_svc "$STAGE" version 2>&1) || fail 2 "the new binary does not run ('version' failed); nothing changed"
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
install -d -o root -g root -m 0700 "$BACKUP_ROOT"
BACKUP_DIR=$(mktemp -d "$BACKUP_ROOT/$STAMP.XXXXXX") || { restart_old; fail 3 "cannot create a backup directory; old binary restarted"; }
DB_FILES=()
for f in "$DB_PATH" "$DB_PATH-wal" "$DB_PATH-shm"; do
	[[ -L $f ]] && { restart_old; fail 3 "$f became a symlink; old binary restarted"; }
	if [[ -f $f ]]; then
		cp -p --no-dereference -- "$f" "$BACKUP_DIR/" || { restart_old; fail 3 "database copy failed; old binary restarted"; }
		DB_FILES+=("$(basename "$f")")
	fi
done
cp -p -- "$TARGET" "$BACKUP_DIR/cryo-console.prev" || { restart_old; fail 3 "binary copy failed; old binary restarted"; }
cmp -s -- "$DB_PATH" "$BACKUP_DIR/$(basename "$DB_PATH")" || { restart_old; fail 3 "database copy does not match; old binary restarted"; }
PREV_SHA=$(sha256sum -- "$BACKUP_DIR/cryo-console.prev" | cut -d' ' -f1)
say "OK    database and old binary copied to $BACKUP_DIR (root only)"

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
		say "        echo '$PREV_SHA  $BACKUP_DIR/cryo-console.prev' | sha256sum -c -"
		say "        install -o root -g root -m 0755 $BACKUP_DIR/cryo-console.prev $TARGET"
		# --remove-destination: never write through a link the service account may have planted.
		for f in "${DB_FILES[@]}"; do say "        cp -p --remove-destination --no-dereference -- '$BACKUP_DIR/$f' '$(dirname "$DB_PATH")/$f'"; done
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
