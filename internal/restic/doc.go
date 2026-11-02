// Package restic builds restic command lines, decodes restic's JSON output,
// maps exit codes to run outcomes and classifies error messages. It is shared
// by the agent (backup, restore, ls) and by the console's maintenance worker
// (forget, prune, check, stats), and it is the only place in CryoBackup that
// knows what restic's flags and output look like.
//
// The engine is restic. This package orchestrates it and never reimplements
// anything it does: chunking, deduplication, encryption and the repository
// format are restic's. The package is pure: it starts no process, writes no
// log, reads no clock and touches no file. The supervisors (the agent's
// runner and the console's maintenance worker) execute the commands it builds
// and feed the output streams back in. It imports only the standard library
// and has no build tags, so the same code behaves the same on Windows and on
// the Linux CI runner.
//
// The pinned version is restic 0.19.1; every fixture and every live test was
// captured from that binary.
//
// # Building commands
//
// Backup, Restore, Snapshots, Ls, Find, Dump, Unlock, CatConfig, Stats,
// Version, Forget, Prune and Check return a Command: a Kind and the argument
// vector after the program name. A builder validates every option and returns
// an error that wraps ErrInvalidOption (never containing the offending value)
// instead of producing a command line restic would reject or, worse, accept.
//
// The repository location and password never appear in argv. They travel in
// the environment (Env.Environ), together with the cache directory, the temp
// directory and GOMAXPROCS. GatewayRepository builds the endpoint's
// "rest:https://<device>:<password>@<gateway>/<device>/" location and
// AdminRepository the maintenance path's "rclone:" location; AdminOptions
// gives the -o options that start the admin rclone, for Common.Extra.
//
// These flags are never emitted, under any combination of options, and
// Common.Extra is rejected when it contains one:
//
//	--one-file-system (fails on Windows)   --no-extra-verify
//	--ignore-inode, --ignore-ctime          --unsafe-allow-remove-all
//	restore --delete                        forget --prune
//	ls --recursive                          --remove-all (except Unlock(true))
//	--repo/-r, --repository-file            --password-file/-p, --password-command,
//	--insecure-no-password
//
// # The parse boundary
//
// A backup of a medical practice produces restic output that names patient
// files. Architecture §8.4 forbids any path or fragment of one in a log, a
// cache, a metric, an e-mail or an upload. The first line of defence is here:
// the Decoder converts each line of restic's JSON streams into a Message that
// has no field in which a path could be kept. restic's "item" and "message"
// fields and the "current_files" list are classified or counted and then
// dropped. An error line becomes an ErrorEvent (the phase word, the Class from
// Classify, and the allow-listed extension from Ext); the final exit_error line
// becomes an ExitError (code and class); a line that is not JSON becomes
// Unparsed (a class and nothing else); a message type nobody has heard of
// becomes Unknown. None of these is an error: restic may add message types at
// any time. The decoder never returns a raw line.
//
// The non-browsing types have exactly one kind of string field: a value the
// decoder reduced to a closed vocabulary, so a string restic filled with a path
// does not survive. The reductions are:
//
//	VerboseStatus.Action     one of new, unchanged, modified, scan_finished
//	                         (backup -v) or restored, updated, deleted (restore
//	                         -v; unchanged is shared), exactly as restic prints
//	                         them; "" if absent; "?" for anything else
//	BackupSummary.SnapshotID 8 to 64 lower-case hex digits; "" otherwise
//	ErrorEvent.During        [a-z0-9_-], at most 32 bytes; "" otherwise
//	Unknown.Type             [a-z0-9_-], at most 64 bytes; "?" otherwise
//	ErrorEvent.Class,        ErrorClass values (Classify)
//	ExitError.Class,
//	Unparsed.Class
//	ErrorEvent.Ext           an allow-listed extension (Ext); "" otherwise
//
// The raw string is kept nowhere. RestoreSummary and CheckSummary carry no
// string at all. The boundary tests feed the sentinel path
// C:\fixture\PHI-sentinel\patient-record.docx into every one of these fields and
// require that it comes out as nothing.
//
// Counting goes through ErrorCounts, whose Projection is the wire shape
// {during, class, ext, count} (DH15) that is allowed to leave the device.
//
// # Path-bearing types and the never-log rule
//
// Some types do carry paths, because browsing a snapshot or planning a restore
// needs them: LsSnapshot and Node (from "ls --json"), Snapshot (from
// "snapshots --json" and "forget --json"), FindResult and FindMatch, and
// ForgetGroup and ForgetReason (through their snapshots). They exist for the
// restore browser and the snapshot cache only. They must never be passed to a
// logger, an error, a metric or an upload. None of them implements String or
// Error, and the tests assert that, as well as that no other type has a field
// that could hold text. Command.Args may carry paths too (roots, the exclude
// file, a restore target); do not log it either. InitResult.Repository is a
// repository location; treat it the same way.
//
// # Exit codes
//
// Map implements architecture §6.4. The supervisor collects Evidence (was a
// summary line seen, who asked for a cancel, the dominant stderr class, the
// number of check errors) and Map returns the Outcome, the DeferredReason for
// a deferral and the ErrorClass for a failure or deferral:
//
//	exit  backup                          restore    forget/prune/check/unlock
//	0     success only with a summary     success    success
//	3     partial                         partial    partial
//	10    failed/repo_missing             same       same
//	11    deferred/lock                   same       same
//	12    failed/wrong_password           same       same
//	130   cancelled if the agent asked, else interrupted
//	1, 2  network, tls, auth, storage -> deferred/gateway; lock -> deferred/lock;
//	      disk, memory, permission, vss, repo_missing, wrong_password, parse,
//	      internal -> failed with that class; anything else -> failed/unknown
//	other failed/unknown
//
// One rule overrides the whole table for check (architecture addendum 13.3):
// when Evidence.CheckErrors > 0 (num_errors plus broken packs) the result is
// failed/storage at every exit code (0, 1, 2, 3, 10, 12, 255, ...) and whatever
// Stderr says, so damage is never hidden behind a gateway deferral, an unknown
// class or a success. The two exceptions are 11 (the lock: nothing was checked,
// so deferred/lock) and 130 (the run did not finish: cancelled or interrupted).
// No other kind looks at CheckErrors.
//
// StderrClass picks the stderr class for Evidence.
//
// # The classifier
//
// Classify maps restic's message text to one of the error classes. It is case
// insensitive and the first row that matches wins, so the order below is part
// of the contract. The table is in code (Rules) and this list is checked
// against it by a test.
//
//	 1  wrong_password   "wrong password", "no key found"
//	 2  repo_missing     "repository does not exist", "Is there a repository at"
//	 3  lock             "unable to create lock", "already locked", "failed to refresh lock",
//	                     or "lock" together with "exclusive"
//	 4  vss              "vss", "shadow copy", "volume shadow"
//	 5  tls              "x509", "tls:", "certificate", "tls handshake"
//	 6  auth             "401", "403" (as whole tokens), "unauthorized", "forbidden", "authentication"
//	 7  storage          "500", "502", "503", "504", "507" (as whole tokens), "internal server error",
//	                     "bad gateway", "service unavailable", "gateway timeout", "insufficient storage"
//	 8  network          "connection refused", "no such host", "i/o timeout", "connection reset",
//	                     "network is unreachable", "dial tcp", "eof" (as a whole token), "broken pipe",
//	                     "proxy", "wsarecv", "wsasend", "host is down"
//	 9  disk             "no space left", "not enough space", "disk full", "disk quota"
//	10  memory           "cannot allocate memory", "out of memory", "runtime: out of memory"
//	11  locked           "being used by another process", "sharing violation", "lock violation"
//	12  permission       "access is denied", "permission denied", "operation not permitted", "privilege"
//	13  missing_source   "cannot find the file", "cannot find the path", "no such file or directory",
//	                     "does not exist", "is not a directory"
//	14  parse            "unexpected end of json", "invalid character", "unmarshal", "cannot parse", "invalid json"
//	15  internal         "panic", "runtime error", "goroutine", "nil pointer"
//	16  unsupported_file exactly unsupported file type "irregular" or unsupported file type ""
//	                     as the whole message (restic's text for a file it cannot archive);
//	                     anything near it ("unsupported repository version 3") is unknown
//	17  unknown          everything else
//
// What decides the class of an error line. The decoder classifies a backup,
// restore or check error line with ClassifyItem(message, item), which takes
// the paths and names out of the text before the table runs:
//
//   - the item: as the "<item>: " prefix restic's archiver adds to an error that
//     does not name it, and wherever else it occurs (when it is longer than a
//     drive root);
//   - every absolute-path token: one starting with a drive letter and a
//     separator ("C:\"), with two backslashes ("\\?\", "\\.\", "\\host\"; this
//     includes the shadow-copy path \\?\GLOBALROOT\Device\
//     HarddiskVolumeShadowCopyN\... that an OS error names under
//     --use-fs-snapshot instead of the item) or with a slash and a name. A token
//     runs to the next ": ", double quote or line break, or to the end, so a path
//     with spaces goes whole;
//   - every bare name: restic check and the restorer name a file by its name
//     alone, not by a path ("file %q blob %v not found in index", "dir node %q
//     has no subtree", "file %q has nil blob list", "invalid child node name
//     %s"), so every Go %q operand (double-quoted, escapes honoured) and the
//     rest of the line after "invalid child node name " are removed too. A
//     quoted URL goes with them; the words after it still decide. The Exact
//     row (16) alone sees the text before this step, since restic quotes the
//     node type there, not a name.
//
// So a file or directory called "proxy", "vss" or "certificate" does not decide
// the class, in a backup, under VSS, in a restore target or in a check. Not
// guaranteed: an unquoted relative path is not recognised (restic 0.19.1 names
// a file by its absolute path, by a %q-quoted name or after "invalid child node
// name "), a path containing ": " is cut there, a path followed by more words
// and no ": " takes those words with it, and a keyword restic itself puts in
// double quotes no longer counts, so the class can fall to a later row or to
// unknown, but no path or name word decides it. Classify itself removes
// nothing; it is what the exit_error line and an unparsable line get, text
// restic does not build from file names. Tests:
// TestClassifyItemVSSAndRestorePaths, TestClassifyItemPathWordsNeverDecide,
// TestClassifyItemQuotedNames, TestClassifyItemNameWordsNeverDecide.
//
// Whole-token matching keeps "500" in a directory called "500" from looking
// like an HTTP error; see Rule for the exact boundary. The class is advisory
// all the same (a localized message is unknown, a rarely-seen message may land
// in the wrong row), and the verbatim run log stays on the device (architecture
// §8.4).
//
// # Locale
//
// The tables match the English text that restic and the Go runtime produce.
// On a Windows installation with another display language the system messages
// ("Access is denied", "The system cannot find the path specified") are
// localized by Windows and fall through to the unknown class, while restic's
// own messages stay English. The exit code, not the text, decides the outcome,
// so a localized machine loses detail, not correctness. If localized machines
// matter, run the agent with the display language forced to English for the
// restic child, or extend Rules with the Windows message identifiers.
//
// # Fixtures
//
// testdata/v0.19.1 holds output captured from restic 0.19.1 by
// testdata/regen, with the machine scrubbed out: the temporary root is
// C:\fixture, the account is fixture-user and the host is fixture-host.
// MANIFEST.md lists each file with the exact command line and exit code that
// produced it and records where restic 0.19.1 differs from the ticket and the
// architecture. To regenerate (Windows, from the repository root):
//
//	go run ./internal/restic/testdata/regen -restic <path to restic 0.19.1> -out ./internal/restic/testdata/v0.19.1
//
// The tool fails loudly if anything of the machine survives the scrub, and a
// test walks testdata/ to check the same. The "testdata" directory keeps the
// tool out of "go build ./..." and "go test ./..."; it never runs in CI.
//
// # Tests
//
//	go test -race -count=1 ./internal/restic
//	CRYO_RESTIC=<path to restic 0.19.1> go test -count=1 -run Live -v ./internal/restic
//	go test -run=^$ -fuzz=FuzzDecoder -fuzztime=10s ./internal/restic
//
// The live tests are skipped unless CRYO_RESTIC names the pinned binary; they
// run every builder against a throw-away repository in a temp directory.
//
// Ticket P1-T1. Contract: architecture §6.3 (invocations), §6.4 (exit codes),
// §7.3 (maintenance), §8.4 (path-free parse boundary).
package restic
