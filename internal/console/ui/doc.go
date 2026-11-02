// Package ui renders the admin console: the board, the device panel, the
// restore wizard and the login page. Ticket P1-T5.
//
// Pages are html/template sets built from web/templates (base, partials, one
// page each; login uses the bare shell). Interactivity is htmx plus the
// first-party fresh.js; the Content-Security-Policy forbids inline script, so
// everything is a file under web/static, vendored and embedded.
//
// Live updates: Hub fans out a coalesced re-render of the board to every SSE
// subscriber (at most one per two seconds, a refresh tick, a 15 s ping, and a
// slow subscriber is dropped rather than allowed to block). The client treats
// data older than the stale threshold as stale and shows the C10 banner; it
// never leaves old numbers looking current.
//
// State chips (chips.go) are a closed catalogue of 17 states, the RUNNING
// overlay and the outcome chips; testdata/state_chips.golden pins it. checks.go
// holds the built-in design checks (TokenCoverage, OrbitronAudit,
// ContrastReport) shown on /design in fixtures mode.
//
// Restore wizard: the browse listing is the single place a file path reaches a
// page, and it arrives as a POST response; the current directory travels in a
// hidden form field, never in a URL. The download is a POST form button that
// mints a session-bound two-minute token and 303-redirects to the GET URL.
// Every page carries Referrer-Policy: same-origin (Referer and Origin reach this
// origin only, never a third party; "no-referrer" would make a browser send
// Origin: null on a form POST and defeat the console's own CSRF origin check)
// and Cache-Control: no-store. Only the restore download routes, where a
// single-use token travels in the URL, send Referrer-Policy: no-referrer.
//
// Sign-in CSRF: GET /login issues a login nonce: the cryo_login cookie (random,
// Path /login, 10 minutes) and, in the hidden login_nonce field, the server's
// HMAC of that cookie value, so an injected cookie cannot be paired with an
// invented field. POST /login first refuses any request whose headers point at
// another site (foreign Origin or Referer, Sec-Fetch-Site cross-site or
// same-site) whatever the nonce says; then a valid pair carries a post whose
// browser stripped Origin, Referer and Sec-Fetch-Site (privacy-hardened
// browsers send none); with no valid nonce it falls back to auth.LoginOriginOK.
// A successful login clears the cookie; every re-rendered login page (bad
// password, refused, unreadable form) carries a fresh nonce. After sign-in, the
// session-bound CSRF token is the defence (see package auth); the origin check
// refuses contrary headers and otherwise applies only to headers the browser
// did send.
package ui
