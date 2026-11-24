# ADR 0025 - The admin listener believes forwarded headers only from the proxies it is told to trust

**Date:** 2026-11-24 - **Status:** Accepted (foreman, 2026-11-24; the operator was told and did not object) - **Decider:** the foreman, for the operator (ticket P21-T7)

## Context

An operator may put a reverse proxy in front of the admin UI on the same host: it terminates TLS, keeps the
`Host` header, sets `X-Forwarded-Proto` and appends the client to `X-Forwarded-For`. Until now the console took
the client address from the TCP peer only (`httpx.ClientIP`), so behind such a proxy every audit row, every
session row and `last_login_ip` showed the proxy's address, and the per-address login limit (10 a minute) became
one bucket shared by every operator. `X-Forwarded-Proto` was already believed from any loopback or private peer
(FIX17), so that a TLS front could keep the session cookie `Secure`.

A forwarded header is only as good as the hop that wrote it: any client can send `X-Forwarded-For`. Believing it
from anyone would let a caller choose the address the audit trail records and escape the login limit.

## Decision

1. **New optional key `trusted_proxies`** (environment `CRYO_TRUSTED_PROXIES`, comma-separated): a list of IP
   addresses or CIDR prefixes. Default empty. Empty means exactly the behaviour before this key existed.
2. **Validation.** Every entry must parse as an IP address or a CIDR prefix. A prefix of length 0 (`0.0.0.0/0`,
   `::/0`, or any address with `/0`) is refused with a plain message: it would trust the world. So is a list whose
   entries together cover every IPv4 or every IPv6 address (`0.0.0.0/1` with `128.0.0.0/1`): the same world in
   pieces. A single wide prefix that leaves a gap (`0.0.0.0/1` alone) is the operator's choice. Zones and ports are
   refused. IPv4-mapped IPv6 entries are compared unmapped. In `CRYO_TRUSTED_PROXIES` an empty entry (`,` alone, a
   doubled or trailing comma) is malformed and refused, never dropped. The console does not start with an invalid
   list, and `cryo-console doctor` reports it as a config failure.
3. **One middleware, on the admin listener only** (`httpx.ForwardedFor`, wrapped around every admin route at
   registration). The agent listener never gets it: it is public, and its per-address limits must count the peer.
4. **The client address.** When the direct peer is inside the list, the client address that everything downstream
   reads (`httpx.ClientIP`: the login limit, audit rows, session rows, `last_login_ip`) is the **rightmost
   `X-Forwarded-For` entry that is not itself a trusted proxy**. All `X-Forwarded-For` lines count, in order, as
   one comma-separated chain. Each entry must be a plain IP address (no port, no brackets, no zone). The peer stays
   the client when the chain is empty, when an entry met before the client is not a plain IP address, or when every
   entry is a trusted proxy. Entries to the left of the first untrusted one are never read: only the hops the
   trusted proxies appended are believed. When the peer is **not** in the list, the header is ignored entirely.
   RFC 7239 `Forwarded` is not read.
5. **The forwarded scheme follows the same list when it is set.** With `trusted_proxies` set, only a peer in the
   list may assert `X-Forwarded-Proto: https`; a loopback or private peer outside the list may not. With the list
   empty, the FIX17 rule stays: a loopback or private peer may assert it. A connection that is itself TLS is
   secure whoever the peer is, as before. Reason: an operator who names the proxy has said which hop speaks for
   the connection; letting other local or private peers speak for it as well would keep an exception the operator
   just closed. **Which value is read** (as built, unchanged from FIX17 and pinned by a test): only the FIRST
   `X-Forwarded-Proto` line, and of a comma-separated value on it the LAST entry. The proxy is asked to overwrite
   the header (deploy README), and an overwriting proxy sends exactly one line holding its own value, so that value
   decides under this rule. A client's line can come first only behind a proxy that adds a second line instead of
   overwriting; it then sets the `Secure` flag of that client's own cookie and nobody else's. Switching to the last
   line would have changed the unset behaviour for multi-line requests, which this decision does not touch.
6. **A listed address that does not match the proxy is not silent.** With the list set, a loopback or private
   peer that is not listed but sends `X-Forwarded-For` or `X-Forwarded-Proto` is most likely the proxy, listed under
   the wrong address. Its headers are ignored as for any unlisted peer, and the console logs one warning per such
   peer address per process (at most 32 addresses), naming the peer address and the key `trusted_proxies`, never a
   header value. A public unlisted peer is not reported: forged headers from the open internet are not a
   misconfiguration. Doctor cannot see the proxy's source address, so the warning is the signal.
7. **The decision is made once per request.** A page's in-process call to the admin API runs on the page
   request's context and keeps the client address that request was given.
8. **Origin check.** The host of `admin_url`, when it is set, is accepted in an `Origin` or `Referer` beside the
   request's own `Host` and the `public_url` host, in the form a browser writes there: the host in lower case, an
   IPv6 literal in canonical compressed form (an IPv4-mapped one in hex groups), the port without leading zeros and
   left out when it is the scheme's default (443 for https, 80 for http). What is validated and what goes into
   e-mails is unchanged. This keeps a proxy or name that differs from the bind address from refusing every
   unsafe request; any other host is still refused.

## Consequences

- Behind a trusted proxy the audit trail, the session list and the login limit see the operator's address, not
  the proxy's.
- A process that can connect to the admin listener **from a trusted address** can name any client address. With
  the proxy on the same host and `trusted_proxies: ["127.0.0.1"]`, every local process can. Trust the narrowest
  address that works: bind the admin listener to loopback and name only the proxy's source address.
- A proxy that appends `ip:port` or a bracketed address to `X-Forwarded-For` is not understood; the console then
  records the proxy's address (fail safe, never a forged one). Configure the proxy to write the bare address.
- Doctor prints one line naming the list when it is set, and nothing when it is not.
- An older binary refuses a config that contains `trusted_proxies` (unknown key); `init` does not write the key
  while it is empty. See `docs/upgrade-notes.md`.
