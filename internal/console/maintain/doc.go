// Package maintain is `cryo-console maintain`: the server-side worker that holds
// the storage admin credential and runs restic against the device repositories on
// behalf of a console that holds neither. It implements the admin-path Backend
// (internal/console/adminpath) over the pinned restic and its rclone: backend, and
// runs the maintenance scheduler. Ticket P1-T6; architecture §7.1 to §7.5.
//
// The engine is restic. This package orchestrates it: it builds command lines
// with internal/restic, starts the process through one runner, and decodes the
// output with the same package. It never reimplements anything restic does.
//
// # What the worker holds, and why (architecture DH1, §8.1)
//
// The web-facing process (`cryo-console serve`) holds no storage credential and no
// repository password beyond one request. Everything that needs either runs here:
//
//   - the storage ADMIN credential, in a mounted rclone.conf (the [admin] remote)
//     that rclone reads. This process never reads it into memory; it only passes
//     RCLONE_CONFIG to the children.
//   - the master key, behind a KeyUnwrapper. The worker asks for a device's
//     repository password per call, passes it to restic through the environment
//     (restic.Env, never argv) and drops the reference when the call ends. Go
//     strings are immutable, so the password cannot be overwritten in place; it is
//     released, not wiped. The child's environment block lives as long as the child.
//   - the database (rw), shared with serve under SQLite's one-writer-per-process
//     discipline.
//
// The admin path is a second, non-append-only path to the storage, never exposed to
// clients (engine §3.4). The gateway (rclone serve restic --append-only) is the
// client path and holds a different credential.
//
// # KeyUnwrapper
//
// KeyUnwrapper is the seam to the master key. The real implementation is
// internal/console/escrow (ticket P1-T5); it cannot be imported here without a
// cycle, so the console binary assigns DefaultKeyUnwrapper before it calls Main.
// Tests use a fake. Until that wiring exists Main refuses to start (ExitUsage).
//
// # The admin-path methods
//
// Worker implements adminpath.Backend. For every device call the worker loads the
// device and its customer (an unknown id is not_found; a customer-held key,
// architecture DH22, is custody_customer_held, before any key is requested),
// unwraps the repository password, builds restic.Env with
// restic.AdminRepository(remote, bucket, prefix, device id) and the no-cache form
// (restic.Env.NoCache), adds --no-cache and restic.AdminOptions(rclone program,
// false) to Common.Extra, runs the command
// under the per-method timeout and a concurrency cap, and translates the exit code
// with restic.Map:
//
//	method      restic                         result / error
//	ping        -                              version, uptime, master key loaded, last storage probe
//	init        init --json, then cat config   idempotent (below)
//	cat_config  cat config                     restic.Config
//	ls          ls --json <snap> <dir>         one level; "/" lists the roots; capped, Truncated
//	find        find --json                    capped over all snapshots
//	snapshots   snapshots --json               every snapshot of the device repository
//	dump        ls, then dump -a zip (stdout)  a zip streamed over the socket; nothing on disk (ADR 0008)
//	stats       stats --json --mode <mode>     raw-data feeds usage_samples; debug is not offered
//	unlock      unlock [--remove-all]          RemoveAll only with Quiesced; recorded as a run
//	forget      -                              unavailable, names M5
//	prune       -                              unavailable, names M5
//	check       -                              unavailable, names M5
//
// Read-only calls turn a failed run into an *adminpath.Error: exit 10 repo_missing,
// 12 wrong_password, 11 locked, network, tls and storage classes
// storage_unreachable, anything else restic_failed with the path-free class, a
// timeout timeout. Init and Unlock return results instead (a restic run that
// reached an exit code is a result): the ResultMeta carries the exit code and the
// restic.Map outcome, and exit 11 is "deferred", never an error.
//
// Init is idempotent (architecture DH17). If `restic init` fails, the worker runs
// `cat config`: success means the repository exists and the key verifies, and the
// result is success with Created false and the existing repository id. A wrong key
// on an existing repository is a result with ErrorClass wrong_password. Any other
// failure is the init run's own outcome.
//
// Ls always passes a directory to restic ("/" for the snapshot roots; without one
// restic 0.19.1 walks the whole snapshot, architecture §13.3) and drops the
// directory's own node, which restic prints before the children, so Nodes are the
// children of Dir. It keeps at most Limit nodes and discards the rest of the
// stream.
//
// Dump finds out what the path is (one `ls`), because restic 0.19.1 writes a zip
// only for a directory: `dump -a zip` of a single file writes the file's raw
// bytes (verified live; engine §1.13 and architecture DH27 assumed an archive).
// restic writes to its stdout (no --target), and the worker copies the pipe to
// the admin-path socket as it arrives (ADR 0008: restores stream, they are never
// staged). A directory is restic's own zip stream; a file's raw bytes are wrapped
// on the fly into a one-entry zip by archive/zip, so the result is a zip in both
// cases and a file that is itself a zip (a .docx) is never mistaken for the
// archive. A failed write (the console or the technician went away) cancels
// restic at once. No file is created anywhere, not even briefly. A stream
// holds its restic slot for as long as the browser takes, so streams have their
// own budget below the worker's total (Config.MaxStreams,
// CRYO_MAINTAIN_MAX_STREAMS, default 2 of 4): one over it is refused at once
// with the admin-path code busy, never queued, so browse, snapshots, size
// sampling, prune and check always keep slots. The console cuts a stream that
// is too slow (restores.Service.MinRateBytes / MinRateWindow) or whose first
// byte does not come (restores.Service.FirstByteTimeout); either cancels the
// request, which cancels the restic run and frees its slot.
//
// # No repository data on disk (ADR 0008)
//
// Every restic call runs with --no-cache, so no index, snapshot or pack copy is
// kept between runs. restic's and rclone's temp files (TMPDIR, RCLONE_TEMP_DIR;
// TEMP/TMP on Windows) and rclone's cache (RCLONE_CACHE_DIR) live under
// Config.ScratchDir, which must be a tmpfs: New creates it 0700, empties its tmp
// and rclone subdirectories, and logs a warning (class scratch_not_tmpfs) when it
// is on a disk filesystem. The worker has no persistent state directory: its
// bookkeeping is in the database and in memory.
//
// # Runs (architecture §7.5)
//
// The kinds that are runs (forget, prune, check, unlock) get a runs row with
// origin console: Unlock does in v0 (trigger manual for the RPC, schedule for the
// scheduler's step 2). Read-only calls and init are not runs. A run that was
// opened is always closed, with outcome interrupted when the process never
// finished.
//
// # The scheduler (architecture §7.3)
//
// Loop serves the socket (adminpath.Serve over the Worker) and calls RunOnce every
// Config.TickEvery. RunOnce takes the maintenance-scheduler job lease, applies
//
//	step 0  applicability: maintenance enabled, device not retired, enrollment
//	        confirmed, escrowed custody (customer-held devices have no server-side
//	        maintenance)
//	step 1  pre-flight: skip a device whose latest heartbeat says running or that
//	        has an open backup run; retry 15 to 60 minutes later, jittered per device
//	step 2  lock handling: exit 11 is a deferral (deferred_count, last_deferred_at),
//	        never a failure; after 3 consecutive deferrals `restic unlock` (stale
//	        locks only) runs and is recorded
//	step 8  usage refresh: stats --mode raw-data into usage_samples, one row per
//	        device per hour bucket, when Config.UsageInterval has passed
//
// and, before those steps, an escrow phase (M2-FIX-B1, ADR 0014): a candidate
// whose active escrowed key has never been verified (verified_at NULL, a device
// enrolled before enrollment recorded the verification) gets one `cat config`
// with that key, retried at most hourly. Success sets verified_at; wrong_password
// or repo_missing is recorded as last_verify_error_class; any other failure
// records only the attempt. The phase starts at most 5 checks per pass,
// never-attempted keys first, and none after 2 minutes, so it ends well inside
// the scheduler lease. It is not the 30-day recheck, which is M5's;
//
// and records the pass with store.RecordJobRun (job_state, which banner C07 reads).
// Because C07 reads job_state, ONLY the lease holder writes it: a process that does
// not get the lease logs the skip locally, returns CycleReport.Skipped and writes no
// row, so it can neither refresh last_finished_at nor overwrite the holder's "error"
// status (fail loudly: a failing scheduler must not look alive because a standby ran).
// Forget, prune and check that are due are counted and logged as "scheduled for M5"
// (at most once an hour) and not run; the schedule columns are left untouched so M5
// picks them up. A device whose usage refresh failed makes the job's status
// "error" with the dominant path-free class: no data is an alarm, not a quiet pass.
// The store has no methods for the scheduler's reads and the usage upsert, so
// loop.go runs that SQL through store.Read and store.Write.
//
// # Logging
//
// The worker logs through internal/logx (it never names the standard library's
// structured-logging package; a repository test enforces that). Its call sites
// build attributes with the typed constructors only, through the helpers of
// log.go: device id, request id, kind, method, exit code, class, outcome, counts
// and elapsed time. A value that is not a short token is logged as "invalid", and
// logx's handler rewrites anything path-shaped behind that. No restic text line,
// no path and no ls, find or dump payload is ever logged; error text from restic
// never reaches this package (the decoders drop it).
//
// # Starting and pinning
//
// New refuses to start unless the restic binary hashes to Config.ResticSHA256 (when
// set) and `restic version --json` reports PinnedResticVersion. The children get a
// minimal environment (PATH, SystemRoot on Windows, RCLONE_CONFIG, TMPDIR,
// RCLONE_TEMP_DIR and RCLONE_CACHE_DIR in the scratch space, and the RESTIC_*
// entries of restic.Env): never the parent's
// full environment. restic splits the rclone.program option like a shell command
// line, so Config.RcloneProgram is written with forward slashes and may hold no
// whitespace or quotes.
//
// # Main
//
// Main is `cryo-console maintain`: flags --config, --loop, --once, --socket; exit
// codes ExitOK, ExitFailed, ExitUsage. The console config schema is owned by
// P1-T5; Main reads only db_path, master_key_path and adminpath_socket from it
// (each overridable as CRYO_<KEY>) and takes the worker's
// own settings from CRYO_MAINTAIN_* environment variables (see Main).
//
// # Tests
//
//	go test -count=1 ./internal/console/maintain
//
// runs everything that needs no binaries: the worker against a fake restic (this
// test binary, copied under another name, replaying internal/restic/testdata
// fixtures), the scheduler against a fixtures-seeded store, and structural checks of
// the deployment files (compose, Dockerfiles, units, scripts) in ../../../deploy.
// Two live tests need the pinned binaries and a loopback network; they are skipped
// unless both variables are set:
//
//	CRYO_RESTIC=<restic 0.19.1> CRYO_RCLONE=<rclone 1.75.1> \
//	  go test -count=1 -run Live -v ./internal/console/maintain
//
// TestLiveAdminPathLocal runs the worker through the real rclone: backend over a
// local directory (an alias remote in a temporary rclone.conf; no S3). The
// agent's backup is a direct restic run. TestLiveAppendOnlyGateway starts
// `rclone serve restic --append-only --private-repos` with an htpasswd rendered by
// internal/console/gateway and proves L-APPEND-1 (forget, forget --prune, rewrite
// --forget, key remove and key passwd refused with 403; backup, snapshots and
// unlock succeed; a second login gets 403 on the first login's repository), runs
// deploy/rclone/append-only-test.sh against it (must pass) and against a gateway
// without --append-only (must fail). TestLiveSmokeGatewayScript runs
// test/lab/smoke-gateway.sh against a TLS gateway. The race detector needs cgo and
// runs in CI.
package maintain
