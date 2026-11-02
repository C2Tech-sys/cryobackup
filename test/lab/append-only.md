# L-APPEND-1: append-only notes

The release gate of the gateway (architecture §12.2, engine §3.2): a device login against the
deployed gateway can back up, list and unlock, and **cannot** delete or rewrite history. The
script is [`deploy/rclone/append-only-test.sh`](../../deploy/rclone/append-only-test.sh); this
page is how to run it in the lab and how to read it.

## Run it

You need a device login and its repository password. The cleanest source is a throw-away
enrollment: the enroll response carries both, once.

```sh
# TOKEN is an enrollment token for a lab customer (admin UI or `cryo-console token`), in the
# environment. The header goes to curl on stdin (--config -), never on its command line.
printf 'header = "Authorization: Bearer %s"\n' "$TOKEN" | curl -sS --config - --cacert tls/agent.crt \
  -H 'Content-Type: application/json' \
  -d '{"nonce":"'"$(openssl rand -hex 16)"'","hostname":"lab-append-only","os":"windows","os_version":"lab","arch":"amd64","device_class":"unknown","hw_fingerprint":"'"$(openssl rand -hex 16)"'","agent_version":"lab","restic_version":"0.19.1"}' \
  https://<console-name>:<AGENT_PORT>/api/v1/agent/enroll > enroll.json
# gateway.username / gateway.password / repo_password are in enroll.json (delete the file afterwards)
```

(The request shape is `internal/api.EnrollRequest`; retire the test device in the console when
you are done.) Then:

```sh
DEVICE_ID=<gateway.username> GATEWAY_PASSWORD=<gateway.password> RESTIC_PASSWORD=<repo_password> \
RESTIC_CACERT=tls/gateway.crt \
  ../../deploy/rclone/append-only-test.sh https://<gateway-name>:<gateway-port>
```

No secret is placed on a command line anywhere in this procedure: variable assignments before a
command go to its environment, not its argv (use `read -rs VAR; export VAR` to keep them out of
your shell history), and the script redacts them from any output it prints. `VERBOSE=1` prints each restic step's output.

## Read the result

* `PASS <step> (refused with 403)`: the gateway refused with HTTP 403, the expected failure.
* `FAIL <step> (SUCCEEDED ...)`: the device login can do something destructive. **Do not
  release.** First check the flag is on the command line (`docker compose config | grep
  append-only`) and that no `RCLONE_APPEND_ONLY` is set anywhere.
* `FAIL <step> (failed ... but not with 403)`: the command failed for another reason (TLS,
  network, bad login), so nothing was proven; fix the cause and re-run.
* `INFO` lines record behaviour that append-only allows by design and that is not a violation:
  `rewrite --exclude` without `--forget` only adds a tagged snapshot, and `key add` adds a key
  file. Both are bounded by the per-device repository and the retention guard.

Exit status: 0 everything behaved, 1 a step did not, 2 bad usage.

## Known facts from the first pass (restic 0.19.1, rclone 1.75.1, local remote)

* `forget <id>` and `forget --prune <id>` exit **3** (partial) with the 403 in their output; every
  other refused command exits 1. The script checks for the 403, not a specific exit code.
* `rewrite --exclude x <id>` **without** `--forget` succeeds under append-only (it writes a new,
  tagged snapshot). The gate therefore uses `rewrite --forget --exclude`, which deletes the
  original and is refused. The ticket text says `rewrite --exclude`; the difference is recorded
  in the P1-T6 report.
* `key remove` of the key in use fails inside restic whatever the server says, so the script
  adds a second key first (allowed) and removes that one (refused with 403).
* `--private-repos`: a second login gets 403 on the first login's repository.

The same checks run in `go test` against real binaries on a loopback gateway
(`TestLiveAppendOnlyGateway`, gated on `CRYO_RESTIC` and `CRYO_RCLONE`), including the script
itself and a negative run against a gateway started **without** `--append-only`, which must
fail.
