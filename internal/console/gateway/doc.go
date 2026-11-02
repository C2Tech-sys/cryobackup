// Package gateway renders and deploys the htpasswd file that authenticates
// clients of the CryoBackup gateway (rclone serve restic --htpasswd), and holds
// the helpers the console and the maintenance worker share for gateway
// logins: bcrypt hashing, random password generation, the derived probe
// password, and the reconcile and two-consoles checks.
//
// It depends only on the standard library and golang.org/x/crypto/bcrypt. It
// takes logins as []Login (username plus bcrypt hash) and does not import the
// store, so the maintenance worker can use it without pulling in the database.
// Reading active logins (store.ListGatewayLoginsForHtpasswd), taking the
// htpasswd-sync lease, the generation counter, htpasswd_synced_at and audit
// events belong to the calling job, not to this package. Nothing here reads
// the clock or logs.
//
// # File format
//
// The first line is a comment that carries the generation and a hash of the
// body (everything after that line):
//
//	# cryobackup generation <n> sha256=<hex of the body>
//	<username>:<bcrypt hash>
//	...
//
// Lines are sorted by username in byte order and every line ends in "\n", so
// the same logins always render to the same bytes. The reserved "probe" login
// is always present. Usernames may not be empty, contain ':' , spaces or
// control characters, be invalid UTF-8, or start with '#'; hashes must have the
// exact shape of a 60-byte bcrypt string. RenderE rejects anything else, and
// Parse and Sync refuse a file that does not satisfy the same rules and whose
// header hash matches its body.
//
// # Lifecycle (architecture 7.2)
//
// Create, during enrolment. The caller makes a password with NewPassword (32
// random bytes, 64 hex characters, no URL escaping needed), stores
// HashPassword of it in gateway_logins, runs the htpasswd-sync job, then
// confirms with one authenticated request as that login. A 401 after about 10
// seconds means rclone did not reload and enrolment returns 503 for retry.
//
// Sync. The job renders every active login plus the probe login with RenderE
// and the next generation number, then calls SyncIfOwner. Sync writes
// "htpasswd.tmp-<random>" in the same directory (created exclusively, mode
// 0640), fsyncs it, chmods it to 0640 whatever the umask, and renames it over
// "htpasswd", then fsyncs the directory on POSIX. On POSIX a concurrent reader
// sees the old file or the new one, never a partial or empty file. Sync
// refuses to deploy anything Parse would reject, so a nil or truncated
// rendering cannot replace a good file. rclone re-reads the file when it
// changes.
//
// Rotate is a swap, not an overlap. htpasswd holds one line per user, so the
// console revokes the active row and inserts the new row in one transaction
// and re-syncs; the old password stops working at once. The agent takes the
// resulting 401 as the cue to pull policy, which carries the new password.
//
// Revoke removes the login from the rendering and re-syncs; the entry
// disappears and the next request on an open connection fails authentication.
// Repository data stays until retention expires.
//
// Reconcile runs every five minutes. Drifted compares the body of the deployed
// file byte for byte with the body of a fresh rendering (the generation number
// alone is not drift; an absent, empty, truncated, hand-edited or header-less
// file is). On drift the job rewrites the file and records the audit event
// gateway.htpasswd.drift.
//
// Current reads the generation and the claimed body hash from the header of the
// deployed file. It has no ok flag: when no file is deployed the returned error
// satisfies errors.Is(err, fs.ErrNotExist), and callers test for that (as
// SyncIfOwner does) to tell "nothing deployed" from a failure. A file with a
// missing or malformed header yields an error satisfying errors.Is(err,
// ErrFormat).
//
// Two consoles is an unsupported topology that is detected, not prevented.
// Every rendering carries the generation of the database view that produced
// it. SyncIfOwner returns an error wrapping ErrNewerGeneration, and writes
// nothing, when the deployed file's generation is greater than the caller's;
// the job raises an alarm instead of overwriting. An equal or older generation,
// an absent file, or a deployed file with no valid header is replaced. The
// generation check and the write are not atomic with each other; the caller
// must hold the htpasswd-sync lease.
//
// Probe. "probe" is a reserved username (not a ULID, so it cannot collide with
// a device, and never stored in gateway_logins). Its password is
// ProbePassword(masterKey): HKDF-SHA256 from the standard library crypto/hkdf,
// no salt, info "cryobackup/probe/v1", 32 bytes, hex encoded. It is
// deterministic and never stored. The prober recomputes it from the master key;
// the sync job bcrypts it (HashPassword) into the rendering. ProbePassword
// requires a master key of at least 16 bytes and returns an error for a
// shorter (or nil) key rather than deriving a weak password.
//
// bcrypt is salted, so hashing the probe password afresh on every render gives
// a different probe line each time and the 5-minute reconcile would see drift
// forever. The job therefore reuses the probe line of the deployed file: it
// reads it with Parse, keeps it while CheckPassword(thatHash, ProbePassword(key))
// is true, and calls HashPassword only when it is absent or no longer matches
// (first sync, or after a master-key change). Device hashes are stored in
// gateway_logins and are stable.
//
// # The bcrypt prefix
//
// Go's bcrypt emits "$2a$". HashPassword rewrites the prefix to "$2y$", the
// spelling htpasswd tools (Apache, nginx) write and the one the architecture
// specifies; the two differ only in the version byte and CheckPassword accepts
// both. Verified live against rclone v1.75.1 (TestLiveRclone, gated on
// CRYO_RCLONE): rclone serve restic --htpasswd authenticates the rendered
// "$2y$" logins and the probe login, returns 401 for a wrong password, an
// unknown user and the probe login with a device password, and also accepts a
// raw "$2a$" hash. Either form therefore works with that rclone version; this
// package emits "$2y$" only.
//
// # Windows
//
// os.Rename on Windows replaces the target in one call but fails with "Access
// is denied" while another process holds the target open without
// FILE_SHARE_DELETE. rclone on Windows does that to the htpasswd file for as
// long as it runs (observed with v1.75.1), so Sync cannot replace a file a
// Windows rclone is serving, and a reader that has the file open briefly makes
// Sync retry for up to a second. This affects development hosts only: the
// gateway runs on Linux, where rename-over is atomic whatever readers hold
// open.
package gateway
