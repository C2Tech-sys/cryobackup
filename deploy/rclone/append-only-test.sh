#!/usr/bin/env bash
# L-APPEND-1: device credentials must not be able to destroy history through the
# gateway (architecture 12.2, engine 3.2). Run it on every release against the
# DEPLOYED gateway, with a throw-away device login.
#
# Usage:
#   DEVICE_ID=<login> GATEWAY_PASSWORD=<password> RESTIC_PASSWORD=<repo password> \
#     append-only-test.sh https://gateway.example.net:9443
#
# The secrets travel in the environment, never in argv. Optional environment:
#   RESTIC          restic binary (default: restic on PATH; must be 0.19.1)
#   RESTIC_CACERT   CA bundle for a gateway certificate your system does not trust
#   VERBOSE=1       print each step's output (passwords redacted)
#
# No secret is ever placed on a command line: restic gets them through the environment
# and a 0600 file, and the redaction script is a file too.
#
# Prints PASS or FAIL per step. Exit status: 0 every step behaved, 1 at least one
# step did not (a destructive command SUCCEEDED, a required command failed, or a
# destructive command failed for a reason other than 403), 2 bad usage or restic
# missing. The gateway must have been started with --append-only on the command
# line (never via environment); this script is how you prove it.
#
# Steps that must FAIL with HTTP 403: forget, forget --prune, rewrite --forget
# --exclude, key remove, key passwd. Steps that must SUCCEED: backup, snapshots,
# unlock. Two informational steps record behaviour that append-only allows by
# design and that is not a violation: a plain "rewrite --exclude" (without
# --forget) only adds a tagged snapshot, and "key add" adds a key file.

set -u

URL="${1:-}"
if [ -z "$URL" ] || [ -z "${DEVICE_ID:-}" ] || [ -z "${GATEWAY_PASSWORD:-}" ] || [ -z "${RESTIC_PASSWORD:-}" ]; then
  echo "usage: DEVICE_ID=... GATEWAY_PASSWORD=... RESTIC_PASSWORD=... $0 <gateway base URL>" >&2
  exit 2
fi
RESTIC_BIN="${RESTIC:-restic}"
if ! command -v "$RESTIC_BIN" >/dev/null 2>&1; then
  echo "restic not found (set RESTIC)" >&2
  exit 2
fi

scheme="${URL%%://*}"
hostport="${URL#*://}"
hostport="${hostport%%/*}"
case "$scheme" in
  https|http) ;;
  *) echo "gateway URL must start with https:// (or http:// for a loopback lab)" >&2; exit 2 ;;
esac
export RESTIC_REPOSITORY="rest:${scheme}://${DEVICE_ID}:${GATEWAY_PASSWORD}@${hostport}/${DEVICE_ID}/"
export RESTIC_PASSWORD

umask 077
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
export RESTIC_CACHE_DIR="$work/cache"
mkdir -p "$work/tree" "$work/cache"
echo "l-append-1 $(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$work/tree/one.txt"
printf '%s' "$RESTIC_PASSWORD-second" > "$work/second-key-password"

failures=0
out="$work/out.txt"

# redact removes the secrets from a stream. The sed script lives in a 0600 file in the
# 0700 work directory: a secret on sed's command line would be visible in the process list.
printf 's/%s/<gateway-password>/g
s/%s/<repo-password>/g
' "$GATEWAY_PASSWORD" "$RESTIC_PASSWORD" > "$work/redact.sed"
redact() { sed -f "$work/redact.sed"; }

# run executes restic with the given arguments, capturing stdout+stderr in $out.
run() {
  "$RESTIC_BIN" "$@" >"$out" 2>&1
  rc=$?
  if [ "${VERBOSE:-0}" = "1" ]; then redact <"$out" | sed 's/^/    | /'; fi
  return $rc
}

pass() { echo "PASS $1"; }
fail() { echo "FAIL $1"; failures=$((failures + 1)); }

# must_succeed NAME restic-args...
must_succeed() {
  name="$1"; shift
  if run "$@"; then pass "$name"; else fail "$name (exit $rc, expected 0)"; fi
}

# must_be_forbidden NAME restic-args...: exit non-zero AND the failure is the gateway's 403.
must_be_forbidden() {
  name="$1"; shift
  if run "$@"; then
    fail "$name (SUCCEEDED: the device login can do this)"
  elif grep -qE '403|[Ff]orbidden' "$out"; then
    pass "$name (refused with 403)"
  else
    fail "$name (failed with exit $rc but not with 403: inconclusive, run with VERBOSE=1)"
  fi
}

echo "L-APPEND-1 against ${scheme}://${hostport}/ as ${DEVICE_ID}"

# Setup: the repository must exist. exit 10 = no repository yet, create it.
run cat config
case $rc in
  0) pass "repository reachable" ;;
  10)
    if run init; then pass "repository created"; else fail "init (exit $rc)"; fi ;;
  *) fail "repository reachable (cat config exit $rc: check URL, TLS, login; 401 means the htpasswd is not loaded)" ;;
esac

# Must succeed: normal operation, lock create/delete included.
must_succeed "backup" backup --host "$DEVICE_ID" --json -- "$work/tree"
must_succeed "backup (second snapshot)" backup --host "$DEVICE_ID" --json --force -- "$work/tree"
must_succeed "snapshots" snapshots --json
must_succeed "unlock" unlock

# Snapshot ids for the destructive steps.
run snapshots --json
snap="$(grep -o '"id":"[0-9a-f]\{64\}"' "$out" | head -1 | cut -d'"' -f4)"
if [ -z "$snap" ]; then
  fail "no snapshot id could be read; the destructive steps cannot run"
  echo "RESULT: FAIL ($failures)"
  exit 1
fi

# Must fail with 403: anything that deletes or replaces repository data.
must_be_forbidden "forget <id>" forget "$snap"
must_be_forbidden "forget --prune <id>" forget --prune "$snap"
must_be_forbidden "rewrite --forget --exclude" rewrite --forget --exclude 'one.txt' "$snap"

# Informational: a plain rewrite only adds a tagged snapshot (allowed by design).
if run rewrite --exclude 'one.txt' "$snap"; then
  echo "INFO rewrite --exclude without --forget added a tagged snapshot (not a violation)"
else
  echo "INFO rewrite --exclude without --forget failed (exit $rc)"
fi

# key remove needs a key that is not the one in use (restic refuses to remove its
# own key whatever the server says), so add one first. Adding a key file is a
# POST of a new object, which append-only allows.
if run key add --new-password-file "$work/second-key-password"; then
  echo "INFO key add succeeded (a new key file can be created; not a violation)"
  other="$(grep -oE 'ID [0-9a-f]{64}' "$out" | head -1 | cut -d' ' -f2)"
  if [ -n "$other" ]; then
    must_be_forbidden "key remove <other key>" key remove "$other"
  else
    fail "key remove (key add printed no key id)"
  fi
else
  fail "key add (exit $rc): cannot build the key-remove test"
fi
must_be_forbidden "key passwd" key passwd --new-password-file "$work/second-key-password"

# The history must still be there.
must_succeed "snapshots after the attacks" snapshots --json

if [ "$failures" -eq 0 ]; then
  echo "RESULT: PASS (append-only holds)"
  exit 0
fi
echo "RESULT: FAIL ($failures step(s)); do not release"
exit 1
