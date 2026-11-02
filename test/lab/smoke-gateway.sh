#!/usr/bin/env bash
# Gateway smoke check: TLS, authentication and the gateway-to-storage hop, with the
# reserved `probe` login (architecture 7.2, banner C01).
#
# Usage:
#   [CACERT=<ca or self-signed cert>]
#   [PROBE_PASSWORD=<password> | PROBE_PASSWORD_FILE=<0600 file> | MASTER_KEY_FILE=<console master.key>]
#     smoke-gateway.sh https://<gateway host>:<port>
#
# The probe password is not stored anywhere: it is HKDF-SHA256(master key, info
# "cryobackup/probe/v1"), hex encoded. Give it as PROBE_PASSWORD or in a 0600 file
# (PROBE_PASSWORD_FILE), or give MASTER_KEY_FILE (the base64 file written by
# `cryo-console init`) and the script derives it with probe-password.py (python3, next
# to this script), which reads the key on its standard input. The console CLI owns the
# supported way to print the probe password (ticket P1-T5); the helper is the stand-in.
# No secret is ever placed on a command line (it would be visible to every local
# process): the password reaches curl through its --config on stdin.
#
# Checks and what a failure means:
#   1 listener   GET / with no login        401   anything else: TLS, port or listener problem
#   2 min TLS    a TLS 1.1 handshake        refused (--min-tls-version tls1.2 is set); a curl build
#                                           that cannot speak TLS 1.1 passes this trivially
#   3 probe      GET /probe/config as probe 404 (no repository) or 200 proves the listener, TLS,
#                                           authentication AND the gateway-to-storage hop
#                                           401: the htpasswd is not loaded or the key differs
#                                           5xx: the gateway cannot reach storage (credential,
#                                           endpoint, bucket, no_check_bucket)
#                                           000: TLS or connection failure
#   4 auth       probe with a wrong password 401 (authentication is enforced)
#
# Exit status: 0 all checks passed, 1 a check failed, 2 bad usage.

set -u

URL="${1:-}"
case "$URL" in
  https://*|http://*) ;;
  *) echo "usage: [CACERT=...] [PROBE_PASSWORD=... | PROBE_PASSWORD_FILE=... | MASTER_KEY_FILE=...] $0 https://<gateway host>:<port>" >&2; exit 2 ;;
esac
URL="${URL%/}"

PW="${PROBE_PASSWORD:-}"
if [ -z "$PW" ] && [ -n "${PROBE_PASSWORD_FILE:-}" ]; then
  PW="$(head -n 1 "$PROBE_PASSWORD_FILE" 2>/dev/null)"
fi
if [ -z "$PW" ] && [ -n "${MASTER_KEY_FILE:-}" ]; then
  # The key goes to the helper on its standard input: never on a command line.
  helper="$(dirname "$0")/probe-password.py"
  py="$(command -v python3 || command -v python || true)"
  if [ -z "$py" ] || [ ! -f "$helper" ]; then
    echo "deriving the probe password needs python3 and probe-password.py next to this script; or set PROBE_PASSWORD" >&2
    exit 2
  fi
  PW="$("$py" "$helper" < "$MASTER_KEY_FILE" 2>/dev/null)"
  if [ -z "$PW" ]; then
    echo "MASTER_KEY_FILE is not readable base64" >&2
    exit 2
  fi
fi
if [ -z "$PW" ]; then
  echo "set PROBE_PASSWORD, PROBE_PASSWORD_FILE or MASTER_KEY_FILE" >&2
  exit 2
fi

CURL=(curl -sS --max-time 15 -o /dev/null -w '%{http_code}')
[ -n "${CACERT:-}" ] && CURL+=(--cacert "$CACERT")

failures=0
pass() { echo "PASS $1"; }
fail() { echo "FAIL $1"; failures=$((failures + 1)); }

# 1. listener
code="$("${CURL[@]}" "$URL/" 2>/dev/null)"
if [ "$code" = "401" ]; then
  pass "listener answers 401 without a login"
else
  fail "listener: got ${code:-000} without a login, expected 401 (TLS, port or listener problem)"
fi

# 2. minimum TLS version
case "$URL" in
  https://*)
    if "${CURL[@]}" --tlsv1.0 --tls-max 1.1 "$URL/" >/dev/null 2>&1; then
      fail "min TLS: a TLS 1.1 handshake succeeded; the gateway must run --min-tls-version tls1.2"
    else
      pass "min TLS: a TLS 1.1 handshake is refused"
    fi ;;
  *) echo "SKIP min TLS (plain http)" ;;
esac

# 3. probe login
code="$(printf 'user = "probe:%s"\n' "$PW" | "${CURL[@]}" --config - "$URL/probe/config" 2>/dev/null)"
case "$code" in
  404) pass "probe login: 404 (authenticated, no repository: the listener, TLS, authentication and storage hop work)" ;;
  200) pass "probe login: 200 (authenticated, repository present)" ;;
  401) fail "probe login: 401 (the htpasswd is not loaded, has no probe line, or the master key differs)" ;;
  403) fail "probe login: 403" ;;
  5??) fail "probe login: $code (the gateway cannot reach the storage target: check credential, endpoint, bucket)" ;;
  *)   fail "probe login: ${code:-000} (TLS or connection failure)" ;;
esac

# 4. a wrong password must be refused
code="$(printf 'user = "probe:%s"\n' "wrong-$PW" | "${CURL[@]}" --config - "$URL/probe/config" 2>/dev/null)"
if [ "$code" = "401" ]; then
  pass "authentication is enforced: a wrong password gets 401"
else
  fail "wrong password: got ${code:-000}, expected 401"
fi

if [ "$failures" -eq 0 ]; then
  echo "RESULT: PASS"
  exit 0
fi
echo "RESULT: FAIL ($failures check(s))"
exit 1
