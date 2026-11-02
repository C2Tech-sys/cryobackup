// Package journal is the agent's durable state: a SQLite file
// (journal\journal.db, WAL) holding the run journal, the outbox of undelivered
// results, a small key-value table and the command log (architecture §6.1
// "journal", §6.2, §6.7).
//
// # Tables
//
//	runs      one row per run, written before the child starts (StartRun) and
//	          closed with its outcome (FinishRun); local-only columns state,
//	          delivered_at, reject_reason, updated_at
//	outbox    one row per run id holding the newest wire body (api.RunRecord);
//	          an open body queued at start is replaced by the closed body at
//	          the end (Seq changes), so delivery is at-least-once and idempotent
//	          on the run id
//	kv        policy etag and last-known-good policy, clock offset, last
//	          heartbeat, the gateway-acknowledgement flag, the stop marker
//	commands  the command log: a command id is executed once, its result is
//	          posted until the console acknowledges it (2xx) or refuses it
//	          for good (a final 4xx or a redirect: MarkCommandRefused drops it)
//
// Secrets are never stored here (device token, gateway password and repository
// password live in the DPAPI store), and every outbox body passes
// api.AssertPathFree before it is queued: the journal is path-free by
// construction; the path-bearing detail is the run log under logs\runs.
//
// # Delivery
//
// OutboxDue lists entries whose retry time has passed. After the console
// answers created, updated or duplicate for a body, MarkDelivered removes it
// (only if the body was not replaced meanwhile) and a delivered closed record
// sets runs.delivered_at. A rejected answer is journaled on the run row with
// the console's fixed reason code and is not retried (MarkRejected). A failed
// attempt calls OutboxFailed with the next retry time.
//
// # Recovery
//
// OpenRunsOnStart closes rows a previous process left open: "interrupted" when
// that process recorded a stop (KVStopMarker), "abandoned" otherwise, with the
// last journal activity as the finish time.
//
// # Concurrency
//
// One connection serves all statements, so concurrent goroutines queue instead
// of failing; two processes (the service and a one-shot "check-in") share the
// file through SQLite's own lock with a busy timeout.
//
// Not in M0: the 50 000-row overflow summary of §6.7 (documented limit).
package journal
