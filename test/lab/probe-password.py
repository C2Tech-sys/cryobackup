#!/usr/bin/env python3
"""Derive the gateway's probe-login password from the console master key.

The master key is read from STANDARD INPUT, never from the command line (a command
line is visible to every local process). The password is printed on standard output.

    python3 probe-password.py < /path/to/master.key > probe-password.txt   # then chmod 0600

Input: the base64 text of the master key file written by `cryo-console init`
(whitespace is ignored). Output: 64 lower-case hex characters, which is exactly
internal/console/gateway.ProbePassword: HKDF-SHA256 (RFC 5869), no salt, info
"cryobackup/probe/v1", 32 bytes. Standard library only.
"""
import base64
import hashlib
import hmac
import sys

INFO = b"cryobackup/probe/v1"
MIN_KEY_LEN = 16


def hkdf_sha256(key: bytes, info: bytes, length: int) -> bytes:
    prk = hmac.new(b"\x00" * hashlib.sha256().digest_size, key, hashlib.sha256).digest()  # empty salt
    out, block, counter = b"", b"", 1
    while len(out) < length:
        block = hmac.new(prk, block + info + bytes([counter]), hashlib.sha256).digest()
        out += block
        counter += 1
    return out[:length]


def main() -> int:
    text = "".join(sys.stdin.read().split())
    try:
        key = base64.b64decode(text, validate=True)
    except ValueError:
        sys.stderr.write("stdin is not base64\n")
        return 2
    if len(key) < MIN_KEY_LEN:
        sys.stderr.write("master key is too short\n")
        return 2
    sys.stdout.write(hkdf_sha256(key, INFO, 32).hex() + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
