// Package adminpath is the RPC between the two halves of the console: `cryo-console
// serve` (the web-facing process) and `cryo-console maintain` (the maintenance
// worker). It holds the request and response types of every admin-path method,
// the path-free error model, a Client that serve uses, and a Handler and Serve
// that maintain uses. The package is a contract: P1-T5 (serve) is written
// against Client and Backend, P1-T6 (maintain) implements Backend, and neither
// needs the other to exist.
//
// # Why the admin path exists
//
// Architecture DH1: serve holds no storage credential and no repository
// password beyond one request. Every repository operation that needs one - init,
// ls, find, dump, stats, cat config, forget, prune, check, unlock - therefore
// runs in the maintain process, which holds the storage admin credential and the
// master key, and which serve reaches over a unix-domain socket
// (adminpath.sock, mode 0660, on a volume shared by the two containers,
// architecture §1.1, DH18). Both processes open the same SQLite file; this RPC
// carries no database rows. The engine stays restic's: maintain executes restic
// through internal/restic and the types here reuse its documents (Config,
// Node, LsSnapshot, Snapshot, FindResult, StatsResult, ForgetGroup, PruneStats,
// CheckSummary, Outcome, DeferredReason, ErrorClass).
//
// # What travels, and what never does
//
// Travels: a device id, an opaque request id, snapshot ids, and - because
// restore browsing needs them - snapshot paths and file names in the ls, find,
// dump and forget calls and their results. Those fields are marked PATH-BEARING
// and must never reach a log line, a metric or an error (architecture §8.4).
// Exactly seven results embed ResultMeta (RequestID, Elapsed, ExitCode,
// Outcome, DeferredReason, ErrorClass) so the console can write a runs row
// (architecture §7.5): InitResult, LsResult, DumpResult, UnlockResult,
// ForgetResult, PruneResult and CheckResult. CatConfig, Find, Snapshots, Stats
// and Ping return bare restic or ping types (restic.Config, []restic.FindResult,
// []restic.Snapshot, restic.StatsResult, PingResult) that carry no RequestID,
// Elapsed, ExitCode or Outcome. That is acceptable: none of them is a runs kind,
// and stats feeds usage_samples, not runs. A failed restic run in one of them
// is an *Error with Code and Class, not a result.
//
// Never travels: a repository password, a gateway password, a storage
// credential, a master key or an escrow blob. Requests name a device; the worker
// unwraps that device's key itself.
//
// Restore bytes travel only over the socket, never to disk (ADR 0008, which
// supersedes the staging part of DH27): Dump streams the zip archive as the
// HTTP response body, and its size and SHA-256 are known only once the last
// byte went out (the TrailerDumpResult trailer). Neither side writes the
// archive, or any part of it, to a file.
//
// Error bodies are path-free by construction: an Error is a Code from a closed
// set, a restic ErrorClass, and a fixed sentence for that code. Neither side
// ever serialises or accepts free text: Handler rewrites Message from Code, and
// Client does the same for what it reads. This package writes no log lines.
//
// # Wire format
//
// HTTP/1.1 over the unix socket. POST /v1/<method> with a JSON body
// (Content-Type application/json, at most MaxRequestBytes = 1 MiB, unknown fields
// rejected) answers 200 and the JSON result, or an error status and
// {"error": {"code": ..., "class": ..., "message": ...}}. GET /v1/ping is an
// alias of POST /v1/ping for shell tools (curl --unix-socket adminpath.sock
// http://x/v1/ping). Durations travel as integer nanoseconds (elapsed_ns,
// uptime_ns). Method names are lower-case.
//
// # Methods
//
// Default client timeouts (ClientOptions.Timeouts overrides each by name):
//
//	method      timeout  request           result                   notes
//	ping        5s       -                 PingResult               liveness: version, uptime, master key loaded, last storage probe
//	init        120s     InitRequest       InitResult               idempotent: an existing repository whose key verifies is success (Created false), architecture DH17
//	cat_config  60s      DeviceRequest     restic.Config            escrow verification, storage probe (architecture §4.6 C02)
//	ls          120s     LsRequest         LsResult                 one level; Dir "/" lists roots; capped (DefaultLsLimit 5000, Truncated)
//	find        120s     FindRequest       []restic.FindResult      capped (DefaultFindLimit 1000)
//	snapshots   120s     DeviceRequest     []restic.Snapshot
//	dump        12h      DumpRequest       zip stream + DumpResult  the zip is the body, the result a trailer; one path per call (ADR 0008)
//	stats       120s     StatsRequest      restic.StatsResult       mode raw-data for usage sampling
//	unlock      60s      UnlockRequest     UnlockResult             RemoveAll only with Quiesced, else invalid_request (architecture §6.3)
//	forget      10m      ForgetRequest     ForgetResult             never --prune; KeepWithin required; DryRun first (architecture §7.3)
//	prune       6h       PruneRequest      PruneResult              no JSON from restic: parsed numbers only
//	check       6h       CheckRequest      CheckResult              num_errors or broken packs make Outcome failed
//
// Idempotency: ping, cat_config, ls, find, snapshots, stats, dump and init are
// safe to repeat (a dump writes nothing; a second one streams the archive
// again from the beginning). unlock, forget, prune and check are restic
// operations that may be repeated; retrying one after a lost answer re-runs it.
//
// Deferral is not an error: when restic exits 11 (repository locked) the call
// returns a result with Outcome "deferred", DeferredReason "lock" and ExitCode 11
// (architecture §7.3 step 2). A call that could not run at all returns an Error.
//
// # Errors
//
// Code, HTTP status, meaning (Message gives the fixed sentence of each):
//
//	invalid_request        400  Validate failed, body not JSON, too large (413), wrong verb (405) or media type (415)
//	not_found              404  no such device, snapshot or route
//	custody_customer_held  409  customer-held key: browse, restore, escrow and maintenance refused (architecture DH22)
//	repo_missing           404  the device repository does not exist (restic exit 10)
//	wrong_password         401  the key did not unlock the repository (restic exit 12)
//	locked                 423  the repository is locked and the call reports that as an error
//	storage_unreachable    503  the storage target could not be reached
//	restic_failed          502  restic failed; Error.Class says how
//	timeout                504  the operation exceeded its deadline inside the worker
//	unavailable            503  the worker is shutting down or not ready
//	busy                   503  dump only: every stream slot is in use; refused at once, never queued
//	internal               500  a bug or an unclassified failure; also any panic
//
// HTTPStatus maps a code to its status for the console to pass through. Client
// returns *Error for a wire error, an error satisfying
// errors.Is(err, ErrUnavailable) when the socket could not be reached (its text
// never contains the socket path), and context.DeadlineExceeded or
// context.Canceled for the caller's own context and the per-method timeout. A
// Backend returns *Error, context errors or anything else: Handler maps a
// *ValidationError to invalid_request, a deadline to timeout, a cancellation
// (context.Canceled) to unavailable and everything else that is not an *Error
// to internal, without serialising the underlying error.
//
// The client reads at most 64 MiB of a JSON response (maxResponseBytes: a
// snapshot list of a large repository is the biggest legitimate answer). A
// response over the cap is reported as internal; the body is not parsed and not
// repeated. The dump stream has no cap: it is copied to the caller's writer as
// it arrives and never held in memory or on disk.
//
// # The server
//
// Handler(b) is the http.Handler: routing, validation (requests that fail
// Validate never reach b), strict JSON, the body cap, panic recovery into
// internal, error translation, RequestID echo and an Ls cut-off at the
// request's limit. Serve(ctx, path, h) removes a stale socket file (one nothing
// answers on; a live socket or a file that is not a socket is an error),
// listens, chmods 0660, serves until ctx is done, lets in-flight requests finish
// for ten seconds, then closes them (which cancels their contexts) and removes
// the file. Listen is the listen-and-chmod half, for tests and for callers that
// run their own http.Server. The directory holding the socket should itself be
// restricted to the shared group: the file exists with the process umask for the
// instant between bind and chmod.
//
// # Testing
//
// The tests start a real server on a unix socket in a temporary directory with an
// in-process fake Backend that records requests and returns canned results built
// from internal/restic fixture types. They need no restic binary and no network:
//
//	go test -count=1 ./internal/console/adminpath/
//
// Consumers do the same: implement Backend with a struct of canned answers, pass
// it to Handler, and point a Client at a Serve on a temporary path (keep the
// path short: unix socket paths are limited to about 100 bytes). The socket-mode
// assertion is skipped on Windows, which has no POSIX modes; everything else runs
// on Windows 10 and later and on Linux. CI runs the suite with -race.
package adminpath
