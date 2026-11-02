// Package console is the control plane of CryoBackup: device registry, policy,
// fleet status, restore hand-off and the admin UI. It is one binary,
// cmd/cryo-console, built from the sub-packages below. Tickets P1-T4 to P1-T6.
//
// # Process layout
//
// One process, two listeners with disjoint route trees (server):
//
//   - agent listener (default :8443, TLS): enroll, policy, heartbeat, runs,
//     snapshots, commands. Device-token authentication only. Never serves a
//     page, a cookie or an admin route.
//   - admin listener (default 127.0.0.1:8080, plain HTTP): the UI and the admin
//     API. Session cookie authentication. Never serves an agent route. It
//     refuses a non-loopback, non-private bind unless admin_allow_public is
//     set; the operator terminates TLS in front of it (the session cookie is
//     Secure).
//
// A request that reaches the wrong listener gets that listener's 404. A test
// (server_test.go) enumerates both route tables and fails if they overlap.
//
// # Request flow
//
// requestID -> accessLog -> recover -> serverTime (agent) / securityHeaders
// (admin) -> auth (Require, or device token) -> handler -> store. The access
// log records the route pattern, status and duration; never the query string,
// a header or a body.
//
// # Packages
//
//	config      JSON config plus CRYO_* environment overrides
//	store       SQLite persistence (ticket P1-T4; not edited here)
//	escrow      master key, repository-password wrapping, gateway secrets
//	auth        argon2id passwords, sessions, CSRF, roles
//	ratelimit   token buckets, failed-auth blocker, single-flight
//	httpx       request ids, RFC 9457 problems, client IP
//	agentapi    handlers for the agent listener
//	adminapi    JSON handlers for the admin listener
//	ui          server-rendered pages, htmx fragments, SSE hub, state chips
//	health      evaluates fleet state (wraps internal/state) and console banners
//	enroll      enrollment-token mint and hash
//	restores    restore lifecycle and the bound, single-use download token
//	jobs        lease-based background jobs (htpasswd sync, trims, purge)
//	adminpath   socket to the maintenance worker (ticket P1-T6; not edited here)
//	maintain    the maintenance worker (ticket P1-T6; not edited here)
//	fixtures    frozen clock, fleet and fake admin-path backend for --fixtures
//	cli         the subcommands (serve, init, token, user, doctor)
//	server      listeners, TLS, middleware, route registry
//
// # Security spine
//
//   - Endpoints never hold a storage credential. The gateway secret reaches an
//     agent once, in the policy response, and is wrapped under the master key
//     at rest.
//   - No path appears in a log, metric, audit row or URL. Agent bodies pass
//     api.AssertPathFree. The restore browse listing is the single UI
//     exception and is a POST response body.
//   - Health state is computed on read and never stored (a silent agent is an
//     alarm, never a green board). Every no-data path produces a banner.
//   - A restore download needs the admin session and a two-minute token bound
//     to that session and that restore (restores.MintToken); the token is kept
//     only as a hash and is single use.
//   - Outbound traffic is the admin-path socket only. No telemetry, no CDN.
//
// # Fixtures mode
//
// cryo-console serve --fixtures runs with an in-memory store, a frozen clock
// (2026-10-20 18:42:10Z), a fake admin-path backend, no agent listener and no
// background jobs. It needs no config file, no certificate and no network, and
// serves the same pages the real console does, plus /design.
//
// # Running the live M0 slice
//
// The in-process slice is TestM0SliceInProcess (server/slice_test.go). It runs
// init, token, enroll, policy, heartbeat, runs, snapshots, board, silence,
// restore and download against the real handlers and a fake backend. The live
// variant, TestLiveM0SliceNeedsMaintain, additionally needs the maintenance
// worker wired into cmd/cryo-console and a restic binary; it is skipped until
// then.
package console
