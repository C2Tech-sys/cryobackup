// Package secrets is the agent's store for the three credentials it holds: the
// device token (device.token), the gateway password (gateway.cred) and the
// repository password (repo.key). The endpoint never holds a
// storage credential: the gateway login is for the append-only gateway only.
//
// On Windows every value is protected with DPAPI in machine scope
// (CryptProtectData with CRYPTPROTECT_LOCAL_MACHINE) and the directory carries
// owner SYSTEM and the SYSTEM + Administrators ACL (RestrictToSystemAndAdmins,
// config.SDDLStateDir O:SYG:SYD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)).
//
// Trust boundary: machine-scope DPAPI lets any process on the machine decrypt
// a blob it can read, so it is only as strong as the directory ACL. Put
// therefore reads the directory's owner and DACL back before every write and
// refuses (ErrUnprotectedDir) when they are not exactly that descriptor,
// unless Options.AllowUnprotected (the lab flag --insecure-unprotected-state)
// is set. The ACL is applied only by an elevated process or SYSTEM: an
// unelevated process would lock itself out of its own directory, so it gets
// config.ErrACLNotApplied, and a lab run needs the flag. DPAPI itself works for
// unelevated processes. Machine scope keeps the architecture's model (an
// administrator enrolls, the SYSTEM service reads); SYSTEM user-scope DPAPI
// with the service doing the enrollment is an M1 design note in the ledger.
//
// On other platforms there is no DPAPI. New refuses unless
// Options.InsecurePlaintext (the flag --insecure-plaintext-secrets) is set, in
// which case values are stored as plain files behind a clearly named header.
// That fallback exists for CI tests and is refused on Windows.
//
// Secrets reach restic only through restic.Env.Environ (DH3): never argv,
// never a log line, never the journal, never agent.json.
package secrets
