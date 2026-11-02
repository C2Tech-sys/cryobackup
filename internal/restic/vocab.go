package restic

// This file holds the closed vocabularies shared with replica/schema.sql
// (table runs). Every constant here is exactly one schema value; the tests
// assert that no value is missing and that no extra value sneaks in.

// Kind names a restic operation. The first seven values are the values of
// runs.kind in the schema; the rest are read-only calls (or setup) that are
// never recorded as runs of their own.
type Kind string

// Kinds. KindBackup through KindMaintenance are the schema values of runs.kind.
const (
	KindBackup      Kind = "backup"
	KindRestore     Kind = "restore"
	KindForget      Kind = "forget"
	KindPrune       Kind = "prune"
	KindCheck       Kind = "check"
	KindUnlock      Kind = "unlock"
	KindMaintenance Kind = "maintenance"

	KindSnapshots Kind = "snapshots"
	KindLs        Kind = "ls"
	KindFind      Kind = "find"
	KindDump      Kind = "dump"
	KindStats     Kind = "stats"
	KindCatConfig Kind = "cat_config"
	KindVersion   Kind = "version"
	KindInit      Kind = "init"
)

// Recorded reports whether k is a value of runs.kind in the schema.
func (k Kind) Recorded() bool {
	switch k {
	case KindBackup, KindRestore, KindForget, KindPrune, KindCheck, KindUnlock, KindMaintenance:
		return true
	}
	return false
}

// Outcome is the final state of a run (runs.outcome in the schema).
type Outcome string

// Outcomes, one per schema value.
const (
	OutcomeSuccess     Outcome = "success"
	OutcomePartial     Outcome = "partial"
	OutcomeFailed      Outcome = "failed"
	OutcomeCancelled   Outcome = "cancelled"
	OutcomeSkipped     Outcome = "skipped"
	OutcomeDeferred    Outcome = "deferred"
	OutcomeInterrupted Outcome = "interrupted"
	OutcomeAbandoned   Outcome = "abandoned"
)

// DeferredReason says why a run was deferred (runs.deferred_reason).
type DeferredReason string

// Deferred reasons, one per schema value.
const (
	DeferredLock       DeferredReason = "lock"
	DeferredGateway    DeferredReason = "gateway"
	DeferredBattery    DeferredReason = "battery"
	DeferredMetered    DeferredReason = "metered"
	DeferredWindow     DeferredReason = "window"
	DeferredConditions DeferredReason = "conditions"
	// DeferredFleetCap: the console refused the start lease because the
	// storage target's max_concurrent_backups leases are taken (ADR 0005).
	// The run never started; it is retried after the console's retry_after_s
	// and is not a failure.
	DeferredFleetCap DeferredReason = "fleet_cap"
)

// ErrorClass is the path-free reason a run (or one error inside a run) went
// wrong (runs.error_class). It is the only description of an error that may
// leave the device, which is why the classifier produces it and drops the text.
type ErrorClass string

// Error classes, one per schema value.
const (
	ClassNetwork       ErrorClass = "network"
	ClassTLS           ErrorClass = "tls"
	ClassAuth          ErrorClass = "auth"
	ClassLock          ErrorClass = "lock"
	ClassVSS           ErrorClass = "vss"
	ClassDisk          ErrorClass = "disk"
	ClassPermission    ErrorClass = "permission"
	ClassMemory        ErrorClass = "memory"
	ClassRepoMissing   ErrorClass = "repo_missing"
	ClassWrongPassword ErrorClass = "wrong_password"
	ClassStorage       ErrorClass = "storage"
	ClassParse         ErrorClass = "parse"
	ClassGuardRefused  ErrorClass = "guard_refused"
	ClassInternal      ErrorClass = "internal"
	ClassUnknown       ErrorClass = "unknown"
	// ClassLocked is a source file held open by another process (sharing
	// violation). Not to be confused with ClassLock, the repository lock.
	ClassLocked ErrorClass = "locked"
	// ClassMissingSource is a source path that vanished or never existed.
	ClassMissingSource ErrorClass = "missing_source"
	// ClassUnsupportedFile is a source file restic cannot archive at all: a
	// reparse point or other special file it reports as unsupported file type
	// "irregular" (or ""). A per-file class, tolerable like permission and
	// locked; restic skips the file and exits 3.
	ClassUnsupportedFile ErrorClass = "unsupported_file"
)

// AllClasses lists every ErrorClass in schema order.
func AllClasses() []ErrorClass {
	return []ErrorClass{
		ClassNetwork, ClassTLS, ClassAuth, ClassLock, ClassVSS, ClassDisk,
		ClassPermission, ClassMemory, ClassRepoMissing, ClassWrongPassword,
		ClassStorage, ClassParse, ClassGuardRefused, ClassInternal,
		ClassUnknown, ClassLocked, ClassMissingSource, ClassUnsupportedFile,
	}
}

// Hard reports whether c is a hard failure class: retrying later cannot help
// until a human acts, so the state engine does not give a PENDING_FIRST device
// grace for it (architecture §4.2 row 5).
func (c ErrorClass) Hard() bool {
	switch c {
	case ClassRepoMissing, ClassWrongPassword, ClassPermission, ClassDisk, ClassParse, ClassInternal:
		return true
	}
	return false
}

// Valid reports whether c is one of the 18 schema values.
func (c ErrorClass) Valid() bool {
	for _, v := range AllClasses() {
		if c == v {
			return true
		}
	}
	return false
}
