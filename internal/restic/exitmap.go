package restic

// CancelCause records who asked a run to stop, which decides whether exit 130
// is "cancelled" or "interrupted".
type CancelCause int

const (
	// CancelNone means nobody asked: a 130 that arrives anyway (a stray SIGINT,
	// a console close) is treated as an interruption.
	CancelNone CancelCause = iota
	// CancelAgent means the agent itself stopped the run: operator request, a
	// command, or max_runtime.
	CancelAgent
	// CancelService means the service is going away: stop, pre-shutdown or suspend.
	CancelService
)

// Evidence is everything Map needs to know about a finished restic process
// besides its exit code. The supervisor fills it from the decoded streams.
type Evidence struct {
	// SummarySeen is true when a backup or restore "summary" message was decoded.
	SummarySeen bool
	// Cancel says who requested the stop, if anyone.
	Cancel CancelCause
	// Stderr is the dominant stderr class (StderrClass); ClassUnknown when
	// there was none.
	Stderr ErrorClass
	// CheckErrors is NumErrors + BrokenPacks of a check summary.
	CheckErrors int
}

// Result is the run outcome Map derives.
type Result struct {
	Outcome Outcome
	// DeferredReason is set if and only if Outcome is OutcomeDeferred.
	DeferredReason DeferredReason
	// ErrorClass is set for failed and deferred results and empty otherwise.
	ErrorClass ErrorClass
}

// Map turns a restic exit code and the evidence from its output into a run
// result. It implements the table in architecture §6.4 and engine §1.10; every
// cell is covered by a test. Any exit code not in the table, including negative
// values from a process that could not be started or was killed, is a failure
// ("an unknown exit code MUST be treated as a command failure", restic scripting
// docs).
func Map(kind Kind, exitCode int, ev Evidence) Result {
	// A check that counted errors or damaged packs has found damage in the
	// repository, whatever the exit code and whatever stderr said: restic exits
	// 1 for it, and exit 2, 255 or 0 must not hide it behind a gateway deferral,
	// an unknown class or a success. Only a lock (11, nothing was checked) and a
	// cancel (130, the run did not finish) keep their own rows.
	if kind == KindCheck && ev.CheckErrors > 0 && exitCode != 11 && exitCode != 130 {
		return Result{Outcome: OutcomeFailed, ErrorClass: ClassStorage}
	}
	switch exitCode {
	case 0:
		return mapSuccess(kind, ev)
	case 3:
		return Result{Outcome: OutcomePartial}
	case 10:
		return Result{Outcome: OutcomeFailed, ErrorClass: ClassRepoMissing}
	case 11:
		return Result{Outcome: OutcomeDeferred, DeferredReason: DeferredLock, ErrorClass: ClassLock}
	case 12:
		return Result{Outcome: OutcomeFailed, ErrorClass: ClassWrongPassword}
	case 130:
		if ev.Cancel == CancelAgent {
			return Result{Outcome: OutcomeCancelled}
		}
		return Result{Outcome: OutcomeInterrupted}
	case 1, 2:
		return mapByStderr(ev.Stderr)
	}
	return Result{Outcome: OutcomeFailed, ErrorClass: ClassUnknown}
}

func mapSuccess(kind Kind, ev Evidence) Result {
	// (A check with errors never gets here: Map handles it first.)
	if kind == KindBackup && !ev.SummarySeen {
		return Result{Outcome: OutcomeFailed, ErrorClass: ClassParse}
	}
	return Result{Outcome: OutcomeSuccess}
}

// mapByStderr is the exit 1 / exit 2 row: restic failed, and the dominant
// stderr class decides whether that is a deferral (the path to the gateway or
// the lock is the problem, try again later) or a failure.
func mapByStderr(class ErrorClass) Result {
	switch class {
	case ClassNetwork, ClassTLS, ClassAuth, ClassStorage:
		return Result{Outcome: OutcomeDeferred, DeferredReason: DeferredGateway, ErrorClass: class}
	case ClassLock:
		return Result{Outcome: OutcomeDeferred, DeferredReason: DeferredLock, ErrorClass: ClassLock}
	case ClassDisk, ClassMemory, ClassPermission, ClassVSS, ClassRepoMissing,
		ClassWrongPassword, ClassParse, ClassInternal:
		return Result{Outcome: OutcomeFailed, ErrorClass: class}
	}
	// locked, missing_source, unknown, guard_refused and anything not in the
	// schema: restic failed for a reason this table does not narrow.
	return Result{Outcome: OutcomeFailed, ErrorClass: ClassUnknown}
}
