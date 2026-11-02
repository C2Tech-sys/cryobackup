// Package auth is the admin side of console authentication (architecture §1.1
// "auth (admins)", ticket P1-T5): argon2id password hashing, login, sessions,
// CSRF and role gates. Device and enrollment tokens are the agent API's business
// (package agentapi); TOTP, WebAuthn and OIDC arrive at M2 and later, and the
// hooks are here: LoginResult.MFARequired, Principal.MFAVerifiedAt and the
// sessions.mfa_verified_at column.
//
// # Passwords
//
// argon2id with t=3, m=64 MiB, p=4, a 16-byte salt and a 32-byte key, encoded as
// a PHC string ($argon2id$v=19$m=65536,t=3,p=4$salt$hash, unpadded base64).
// VerifyPassword compares in constant time, rejects anything malformed or with
// parameters outside sane bounds, and reports needsRehash for weaker hashes.
// Login of an unknown, disabled or locked account still spends one verification,
// so response time does not reveal which accounts exist.
//
// # Sessions
//
// The cookie value is 32 random bytes in base64url; only its SHA-256 is stored.
// Idle expiry is 12 h (sliding, written at most once a minute), absolute expiry
// 7 d (both configurable). The cookie is HttpOnly and SameSite=Strict, and Secure
// exactly when the request that set it arrived over TLS (r.TLS) or, from a
// loopback or private peer only, carries X-Forwarded-Proto: https as the last
// value of the header (a TLS front on the host or mesh; the header is ignored
// from any other peer). The admin listener is plain HTTP by design, and browsers
// never return a Secure cookie over http://, so an unconditional Secure flag
// would lock every plain-HTTP deployment out. The CSRF
// cookie and the logout clears follow the same rule.
// Failed logins are counted per account (lock after N failures for a while) and
// per source IP (token bucket); every login outcome is an audit row that never
// contains the attempted username of an unknown account.
//
// # CSRF
//
// Evidence precedence (FIX23b): a token or nonce only makes up for MISSING
// header evidence, never overrides evidence that points elsewhere.
// HeadersContradict reports contrary evidence: an Origin other than "null" that
// does not name this host (or the configured public host); a Referer naming
// another host; Sec-Fetch-Site of cross-site or same-site (even beside a
// matching Origin). Contrary evidence is refused with 403 whatever token or
// nonce accompanies it.
//
// Session requests (Require): every unsafe request needs a CSRF token equal to
// HMAC-SHA256(session cookie, "cryobackup/csrf/v1"), sent in X-CSRF-Token or the
// csrf_token form field and equal to the readable cryo_csrf cookie (double
// submit). The token needs no storage and cannot be computed without the session
// cookie; it is the defence. The origin check is belt and braces whenever the
// browser sends headers (csrfOriginOK): after the contradiction check,
// Sec-Fetch-Site: same-origin (set by the browser, unforgeable by a page, holds
// even when Origin is serialised as "null"), or an Origin, or failing that a
// Referer, naming this host is accepted. A request that offers no evidence at
// all (no Sec-Fetch-Site, no Referer, Origin absent or exactly "null": a
// privacy-hardened browser strips them) is judged on the token alone. The
// console's Referrer-Policy is same-origin so Origin and Referer stay usable
// where the browser allows.
//
// # Login nonce
//
// The login endpoints have no session, so no session-bound token. The UI form
// uses a double-submit nonce that does not depend on request headers.
// Manager.IssueLoginNonce sets the cryo_login cookie (32 random bytes,
// base64url; HttpOnly, SameSite=Strict, Secure by the session-cookie rule,
// Path /login, 10 minutes) and the page renders, in the hidden login_nonce
// field, base64url(HMAC-SHA256(k, cookie)) where k is a per-Manager random
// server secret derived with the label "cryobackup/login-nonce/v1" (a restart
// invalidates outstanding nonces). The field is a MAC of the cookie, not the
// cookie, so a client that injects a cookie cannot invent the field.
// Manager.LoginNonceOK recomputes the MAC from the cookie and compares in
// constant time. The post is accepted when HeadersContradict is false and the
// nonce is valid (the Origin rule is then skipped); without a valid nonce it
// falls back to LoginOriginOK (a same-origin Sec-Fetch-Site, an Origin or
// Referer naming this host, or neither header, which only a script sends; a
// browser always sends Origin, possibly "null", on a cross-site POST). A
// successful login clears the cookie (ClearLoginNonce); a re-rendered login page
// issues a fresh nonce. Logs record that a nonce did not match, never either
// value. The API login reuses these helpers later.
//
// # Roles
//
// Require(min, mode, next) wraps a handler: authenticate, role check
// (RO < OP < AD < OW), CSRF, then the Principal (with its store.Scope) is in the
// request context. ModeUI redirects anonymous users to /login; ModeAPI answers
// RFC 9457 problem+json 401/403.
package auth
