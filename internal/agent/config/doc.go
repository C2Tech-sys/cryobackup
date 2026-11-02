// Package config owns the agent's on-disk layout and agent.json
// (architecture §6.5).
//
// State lives under one directory (default C:\ProgramData\CryoBackup, override
// with --state-dir):
//
//	config\agent.json        server URL, device id, customer id, gateway URL, custody, optional
//	                         SPKI pin, update ring, proxy, lab flag, enrollment nonce; no secrets
//	secrets\                 device.token, gateway.cred, repo.key (DPAPI, package secrets)
//	policy\                  excludes.txt and roots.txt (generated before every run),
//	                         policy.json and etag (mirror of the journal's last-known-good policy)
//	journal\journal.db       package journal
//	cache\restic\            RESTIC_CACHE_DIR
//	tmp\                     TEMP for restic; emptied at start and after each run
//	logs\agent.log           rotating, 10 MiB x 5, path-bearing, local-only (DH15)
//	logs\runs\<run_id>.jsonl verbatim restic output of one run, path-bearing, local-only, kept 30 days
//
// Ensure creates the layout and secures it with SDDLStateDir
// (O:SYG:SYD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA): owner SYSTEM, full control
// for SYSTEM and Administrators only, no inheritance from ProgramData):
//
//   - a layout directory (root or child) that is a reparse point is refused
//     (ErrReparsePoint), checked with os.Lstat and again on the handle that
//     the security call uses (opened with FILE_FLAG_OPEN_REPARSE_POINT);
//   - when elevated, a missing root and every missing child are created with
//     the descriptor already set (CreateDirectory with SECURITY_ATTRIBUTES),
//     and an existing root must be owned by SYSTEM or BUILTIN\Administrators
//     (ErrForeignOwner);
//   - when not elevated (a lab run), an existing root or child whose owner can
//     be read must be owned by SYSTEM, Administrators or the current user
//     (ErrForeignOwner); missing directories are created with os.Mkdir after
//     os.Lstat, a window that exists only on this unprotected path;
//   - every layout directory (config, secrets, policy, journal, cache,
//     cache\restic, tmp, logs, logs\runs and the root) then gets RestrictDir.
//
// Ensure never takes a tree over. An untrusted state directory is moved aside
// whole by QuarantineRoot (--repair-acl, package agent): a rename through a
// no-follow handle to <name>.quarantine-<UTC stamp>-<8 random hex> next to
// the root (crypto/rand, so the name cannot be predicted and squatted), never
// replacing a target (ReplaceIfExists = FALSE) and never copying, refused
// when anything in the tree is a reparse point (scanned before the rename). A
// name that is already taken is retried with a fresh suffix, up to
// QuarantineAttempts (5) renames, and only then is the result
// ErrQuarantineExists. The quarantine's top directory then gets the owner
// Administrators and an admin-only DACL with SetKernelObjectSecurity (no
// propagation into the tree, best effort). Quarantines lists them by
// "<stamp>-<suffix>" (the stamp alone for the pre-FIX10 shape).
//
// The untrusted-repair-required verdict (Verdict, ReadVerdict, WriteVerdict,
// ClearVerdict) records an elevated refusal until a successful quarantine
// clears it. There is one per state directory. On Windows it is the REG_SZ
// value UntrustedRepairRequired-<VerdictID> of HKLM\SOFTWARE\CryoBackup\Agent
// ("<UTC stamp>|<reason>"; VerdictID is the first 16 hex digits of the
// SHA-256 of the lower-cased, cleaned absolute state directory path): written and
// cleared through golang.org/x/sys/windows/registry with SET_VALUE, which
// HKLM grants only to an elevated process, and read with QUERY_VALUE, which
// needs no privilege. Nothing is written next to the root, where any user may
// create files. Off Windows the CI and lab stand-in is <name>.untrusted-verdict
// next to the root, mode 0600, honoured only when root owns it.
//
// Owners: ownerRule is the one rule behind Ensure's root check, the lab check
// and VerifyDir. SYSTEM and Administrators are trusted; an owner that cannot
// be read is untrusted for every elevated verdict (never trusted), and let
// through only by an unelevated lab run, which never yields a verified state
// directory.
//
// Setting the owner to SYSTEM needs SeRestorePrivilege (and
// SeTakeOwnershipPrivilege; a quarantine also SeBackupPrivilege to scan a tree
// that shuts administrators out); they are enabled on an impersonation token
// of the calling thread for the call only, and a token without them is
// ErrPrivilege. Every security call, reads included (ReadOwnerAndDACL,
// VerifyDir), goes through a handle opened with FILE_FLAG_OPEN_REPARSE_POINT
// and refused if it is a reparse point; no path-based security call is used.
// A process that is not elevated would lock itself out, so RestrictDir
// reports ErrACLNotApplied, Ensure returns Applied=false, and the caller
// decides (package agent: refuse, or warn under the lab flag). VerifyDir reads
// a directory's owner and DACL back and checks them with the pure
// CheckStateSDDL. Off Windows nothing is ever protected: RestrictDir,
// VerifyDir and ReadOwnerAndDACL report ErrACLNotApplied.
package config
