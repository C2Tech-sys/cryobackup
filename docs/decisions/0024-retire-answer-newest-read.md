# ADR 0024 - A retire whose wait runs out answers with the newest finished read

**Date:** 2026-11-23 - **Status:** Accepted (operator, 2026-11-23) - **Decider:** the operator (ticket P21-T5), recorded by the foreman

ADR 0016 stands. It says a retire signals the `htpasswd-sync` job and reads the gateway's login file for at most 5 s,
that `gateway_login: removed` means a read after the commit found the login gone, and that anything else is `pending`
with a reason class. It does not say which reason a wait that runs out carries. This ADR records that.

## Context

The wait reads the file repeatedly until the login is gone, the file cannot be read, a sync run after the retire has
failed, or the bound (`GatewaySyncWait`, 5 s by default) ends. Until now, when the bound ended while a read was in
flight, the answer was `check_timed_out`, and an earlier read from the same wait that had finished, for example one
that found the login still in the file, was thrown away. Only a read that met a busy slot (`sync_busy`) was kept.

On a loaded machine that made the answer depend on timing: the same file, the same login, and the answer was
`still_in_file` or `check_timed_out` depending on whether the bound fell between reads or during one. The device page
reads the file the same way and named the cause the read found, so the answer and the page could disagree.

## Decision

When the wait ends with a read in flight, the answer is the **newest read that finished during this wait**, whatever
it found. `check_timed_out` remains only when no read finished at all.

- A **finished read** is one that returned anything other than the bound running out: the login in the file (with its
  reason: `still_in_file`, or the class of a failed sync run or of a failed read that carries an earlier clean read's
  list), or `sync_busy`.
- A read that finds the login **gone**, or the file **unreadable** with no login seen in it, still answers at once, as
  before. A kept read never holds the wait open.
- Only reads made by this wait count. Each began after the wait began, so the answer never rests on a read this wait did not make
  itself. A read that met a busy slot can carry the newest clean read's list of logins (ADR 0016, `HtpasswdSync.Check`);
  that list is what a single read reports today, on the device page as well, and it can only make the answer `pending`,
  never `removed`.

Unchanged: the default wait (5 s), what `removed` and an unreadable file answer, every wire and JSON field name, and
the outcome's audit row, which records the same status and reason as the answer.

## Consequences

- The answer to a retire whose sync is slow is the cause a read found (`still_in_file`, a failed run's class, or
  `sync_busy`), not the bound running out. `check_timed_out` now means no read of the file came back inside the wait:
  a hung or very slow mount.
- The tests end the wait at a chosen read (through the request's context, the parent of the wait's) instead of on a
  timer, so the pinned answers do not depend on machine load.
- The UI's retire goes through the UI's own in-process admin API, built inside the `ui` package. Its wait is
  `ui.Service.GatewaySyncWait` (zero keeps the 5 s default, and serve leaves it zero); `cli.BuildOptions.GatewaySyncWait`
  sets it, with the admin API's, so a test can lengthen both. That is a test seam, not a product setting: there is no
  config key, and a test pins both the pass-through and the 5 s constant.
