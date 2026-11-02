// Package store is the console's data layer: SQLite opened the way ADR 0002 D3
// (as amended) and architecture DH1 require, the embedded migrations (0001 is
// replica/schema.sql, DH20), hand-written SQL for every table the first
// milestone touches, the mandatory Scope on customer-owned data (§3.5), typed
// errors, the facts loader that feeds internal/state (DH7) and the fixtures
// loader behind `cryo-console serve --fixtures`. Ticket P1-T4.
//
// There is no ORM and no query builder: the SQL is in the Go files, one
// function per operation, next to the type it fills. The store never logs
// (callers log through logx), never reads the wall clock except through the
// injectable Options.Clock, never imports an agent package, and never holds a
// secret: hashes and AES-GCM ciphertext (wrapped by the console, which owns the
// master key) are stored as given.
//
// # Open discipline (D3 amended, DH1)
//
// Open applies, on every connection, journal_mode=WAL, synchronous=NORMAL,
// foreign_keys=ON and busy_timeout (5 s by default), runs PRAGMA quick_check
// (a failure is ErrCorrupt) and then the migrations. There is exactly one
// write connection per process: Write queues callers on it, runs the callback
// inside BEGIN IMMEDIATE and commits or rolls back, so two goroutines never see
// SQLITE_BUSY from each other. The other process that opens the file
// (cryo-console maintain) does the same and the two coordinate through the
// database lock: millisecond transactions, BEGIN IMMEDIATE, busy_timeout. Read
// serves queries from a pool of query_only connections, each in a deferred
// transaction, which in WAL mode is a consistent snapshot that neither blocks
// nor is blocked by the writer. Calling Write from inside Write on the same
// goroutine is ErrNestedWrite, not a deadlock.
//
// OpenMemory (tests and --fixtures) is a private file in a temporary directory,
// removed by Close, not ":memory:": ":memory:" gives every pooled connection its
// own empty database, and shared-cache mode swaps busy_timeout for table locks,
// so only a real file runs the production code path (§3.5 item 6: "there is no
// second code path").
//
// # Migrations
//
// internal/console/migrations embeds NNNN_name.sql files. Each runs in one
// transaction and is recorded in schema_migrations with a SHA-256 checksum.
// A recorded checksum that no longer matches the embedded file is
// ErrMigrationChanged and the store refuses to open; a database that records a
// version this binary does not know is ErrMigrationAhead. A test fails if
// 0001_initial.sql differs from replica/schema.sql by one byte.
//
// # Scope (§3.5)
//
// Every method that reads or writes customer-owned data takes a Scope as its
// first argument after ctx. ScopeAll grants every customer; ScopeOf(ids...)
// grants those; the zero Scope is invalid and every scoped method returns
// ErrScope for it, so a forgotten Scope fails closed. List queries append
// customer_id IN (...); by-id queries join through devices.customer_id. A row
// outside the scope is ErrNotFound, exactly as if it did not exist (404, not
// 403: existence is not leaked). Operations that are global by nature (creating
// a customer, editing the default policy) need scope.All and return ErrScope
// otherwise, as do the owner-level customer fields (storage target, key custody,
// device cap). No cross-customer reference passes through the store: an id in
// the input (an adopted or replacement device, a token's policy) is resolved
// inside the same transaction; a device outside the Scope is ErrNotFound and
// one of another customer is ErrInvalid. Methods used by the agent API are named Agent..., take the
// authenticated device id (never an id from a request body) and can touch only
// that device's rows. scope_test.go enumerates *Store's exported methods by
// reflection and fails on any that is neither in the generated scope matrix,
// an Agent method, nor listed as global with a reason.
//
// # Errors
//
//	ErrNotFound         no such row, or a row outside the Scope
//	ErrConflict         unique / primary key collision (a *ConstraintError names the
//	                    index, e.g. uq_policies_one_default), exhausted/revoked/expired
//	                    enrollment token, nonce or idempotency key reused with other content
//	ErrInvalid          CHECK / NOT NULL / FOREIGN KEY / trigger violation (a
//	                    *ConstraintError carries SQLite's message), or a rejected argument
//	ErrScope            zero Scope, or a global operation without scope.All
//	ErrMigrationChanged, ErrMigrationAhead   the database and binary disagree about history
//	ErrCorrupt          quick_check failed, or SQLite reported corruption
//	ErrBusy             another process held the write lock longer than busy_timeout
//	ErrNestedWrite, ErrClosed   misuse
//	ErrNoDefaultPolicy  no default policy row: no effective policy can be composed
//
// All driver errors are mapped in one function (mapErr in errors.go); callers
// use errors.Is and errors.As, never string matching.
//
// # Idempotency contracts
//
// Enrollment: AgentEnroll is idempotent on devices.enroll_nonce (DH17). A retry
// with the same nonce and token before the device's first authenticated call
// returns the same device, the same wrapped gateway secret and the same wrapped
// repository key, spends no token use, and replaces the device token (a hash
// cannot be turned back into the plaintext a lost response carried). After
// enroll_confirmed_at, or with a different token, the nonce is ErrConflict.
// Runs: AgentIngestRuns is idempotent on the run id: created, updated (an open
// run closed later, or abandoned overwritten by a late real outcome),
// duplicate, rejected (another device's id, or a rule violation with a fixed
// reason code); one bad run never fails the batch. Timestamps are converted to
// server time by the device's clock offset and clamped to the receive time
// (DH5). Commands: EnqueueCommand is idempotent on (device, idempotency key);
// the same key with different content is ErrConflict.
//
// # Facts and the state engine
//
// LoadWorld and LoadDeviceFacts read, in one read transaction and per device,
// exactly the facts of architecture §4.1 with queries pinned (INDEXED BY) to the
// partial indexes the schema built for them: idx_runs_last_success,
// idx_runs_last_backup_done, idx_runs_open, idx_runs_maint_done, device_presence,
// the active escrow row, maintenance_schedule, the active alarm_ack and
// idx_agent_releases_channel. Nothing is cached and no health state is ever
// written anywhere (DH7, D10). Per-device thresholds come from the effective
// policy (default, then the customer's policy or template, then the active
// device override, each an RFC 7386 merge patch over the nine sections, DH6) via
// ThresholdsFromPolicy; the retention and selection unions are replaced, not
// merged, when a patch names one side. RunFacts.VSSFallbackOnRoot is set by
// intersecting the run's vss_fallback_volumes (stats) with the drive letters of
// the effective policy's roots (profile presets count as the system drive,
// LoadOptions.SystemDrive, default C; fixed_drives counts every drive).
//
// The loader fails loud: a query error or cancelled context for one device sets
// Complete=false and a text-free LoadErrorClass on it and adds to
// World.LoadErrors; World.LoadComplete is true only when nothing failed; the
// engine turns that into UNKNOWN devices and banner C09. The previous device
// count (state.World.PreviousDeviceCount) is remembered per Scope for this
// process; an empty poll does not replace a non-empty one.
//
// The runs.stats keys the loader reads are total_files_processed, failed_files
// (both required for CountsKnown) and vss_fallback_volumes (drive letters).
// A device is "initial" (a wider stuck limit while progress advances) when it
// has no success since its current enrollment.
//
// # Fixtures
//
// LoadFixtures seeds the fictional fleet of the design page (Summit Eye Care,
// Harbor Family Practice, Northwind Dental, Pinecrest Legal, Blue Ridge
// Accounting, plus an onboarding customer): 26 devices that cover every
// reachable state of the precedence table, with timestamps relative to the
// "now" passed in. FixtureExpectations and FixtureCustomerExpectations state
// the intended result, and a test asserts it through the real facts loader and
// state.Evaluate. Fixtures go through the same schema and constraints as
// production data.
//
// # What the store deliberately does not do
//
// It does not persist computed health, hash or generate secrets, enforce roles
// (the handler layer does; the store re-asserts only scope), write audit rows for
// admin changes on its own (call AppendAudit, or Tx.AppendAudit inside Write to
// commit it with the change; AgentEnroll writes its own), or support
// adoption tokens (DH29, milestone M5). An enrollment token's policy_id is applied
// at enrollment as the new device's override layer (enroll_policy_m2.go), since
// devices have no policy pointer.
package store
