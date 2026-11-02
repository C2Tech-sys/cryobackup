// Package agentapi serves the public agent API (architecture §5.1) on the agent
// listener, and nothing else: the routes are the api.Pattern* constants, the
// admin tree is never mounted here (the disjoint-mux test in package server
// proves it). M0 routes: enroll, policy, heartbeat, runs; plus snapshots and the
// commands long poll and result so the M0 agent is complete; M1 adds
// start-lease (ADR 0005). Token rotation, restores, progress, release and lobby
// arrive with their milestones.
//
// # Authentication
//
// The device id comes from the bearer token and nowhere else (§3.5 item 4):
// sha256 of the token is looked up (AgentLookupDeviceToken, which also refuses
// revoked, expired, retired and out-of-grace tokens), then compared in constant
// time. Every failure is counted per source IP; past 30 a minute the IP is
// blocked with exponential back-off up to 15 minutes, with one audit row per
// block (agent.auth.blocked), not per request. last_used is written at most once
// every five minutes per token.
//
// # Enroll (DH17, DH22, §7.2)
//
// Per-IP limit (10/min, audit agent.enroll.denied), one in-flight enrollment per
// token, body at most 4 KiB, api.EnrollRequest.Validate and api.AssertPathFree.
// Without a master key the answer is 503 master_key_unavailable and nothing is
// stored (banner C12). A customer-held customer is refused with 501
// custody_customer_held. Otherwise: a device token, a gateway password (bcrypt
// in the store, wrapped copy until acknowledged) and a repository password are
// generated, wrapped under the master key and the device id the handler chose
// (store.EnrollInput.DeviceID), and store.AgentEnroll does the whole thing in
// one transaction (spending a token use only on success). The repository is
// created and its key verified through the admin path (adminpath Init, CatConfig; on
// failure 503 with Retry-After, device left in place), htpasswd-sync is
// signalled and the secrets are returned once. A retry with the same nonce
// before the first heartbeat returns the same device and secrets with a fresh
// device token; afterwards it is 409.
//
// # Policy, heartbeat, runs
//
// Policy is an ETag pull: 304 when If-None-Match matches and nothing else is
// pending; a pending gateway secret (DH13) or a pause always travels in a 200,
// and pausing changes the ETag so a 304 cannot hide it. Heartbeat (1 per 20 s
// per device) stores presence and the 24 h ring, computes the clock offset
// (DH5), confirms enrollment on the first call, acknowledges a delivered gateway
// secret and reports pending commands. Runs (30/min, 512 KiB, 200 records)
// validate each record on its own and ingest idempotently on the run id; the
// stats document carries exactly total_files_processed, failed_files and
// vss_fallback_volumes for the facts loader. A body that fails AssertPathFree is
// 400 and reaches no store call.
//
// # Start lease (ADR 0005)
//
// POST start-lease (6/min per device, 1 KiB) asks before a scheduled backup.
// store.AgentStartLease decides in one BEGIN IMMEDIATE transaction against the
// device's storage target's max_concurrent_backups: granted with lease_until
// (a lease is held), granted without it (cap 0: nothing held), or refused with
// retry_after_s. Every heartbeat that names the run (active_run_id, else the
// progress run id) renews the lease to now + 15 min; the closed run upload
// releases it (store.AgentIngestRuns); expired rows are dropped on every
// request and by the heartbeat ring trim job.
//
// # Commands
//
// GET commands?wait=N holds the request up to 30 s; Notifier.Notify wakes it the
// moment a command is enqueued; one poll per device may be in flight, a second
// gets 409. POST commands/{id}/result is idempotent (204).
package agentapi
