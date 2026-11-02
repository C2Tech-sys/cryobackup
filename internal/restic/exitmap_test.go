package restic

import (
	"fmt"
	"testing"
)

// checkSchemaShape asserts the invariants of Result that the schema enforces:
// a deferred outcome has a reason and no other outcome has one.
func checkSchemaShape(t *testing.T, r Result) {
	t.Helper()
	if (r.Outcome == OutcomeDeferred) != (r.DeferredReason != "") {
		t.Errorf("DeferredReason %q with outcome %q", r.DeferredReason, r.Outcome)
	}
	wantClass := r.Outcome == OutcomeFailed || r.Outcome == OutcomeDeferred
	if wantClass != (r.ErrorClass != "") {
		t.Errorf("ErrorClass %q with outcome %q", r.ErrorClass, r.Outcome)
	}
	if r.ErrorClass != "" && !r.ErrorClass.Valid() {
		t.Errorf("ErrorClass %q is not a schema value", r.ErrorClass)
	}
}

func TestMapExitCodes(t *testing.T) {
	failed := func(c ErrorClass) Result { return Result{Outcome: OutcomeFailed, ErrorClass: c} }
	partial := Result{Outcome: OutcomePartial}
	success := Result{Outcome: OutcomeSuccess}
	lock := Result{Outcome: OutcomeDeferred, DeferredReason: DeferredLock, ErrorClass: ClassLock}
	cancelled := Result{Outcome: OutcomeCancelled}
	interrupted := Result{Outcome: OutcomeInterrupted}

	// Rows of architecture §6.4 that are identical for every kind.
	kinds := []Kind{KindBackup, KindRestore, KindForget, KindPrune, KindCheck, KindUnlock}
	common := []struct {
		name string
		code int
		ev   Evidence
		want Result
	}{
		{"3 partial", 3, Evidence{}, partial},
		{"10 repo missing", 10, Evidence{}, failed(ClassRepoMissing)},
		{"11 lock", 11, Evidence{}, lock},
		{"12 wrong password", 12, Evidence{}, failed(ClassWrongPassword)},
		{"130 agent cancelled", 130, Evidence{Cancel: CancelAgent}, cancelled},
		{"130 service stopping", 130, Evidence{Cancel: CancelService}, interrupted},
		{"130 nobody asked", 130, Evidence{Cancel: CancelNone}, interrupted},
		{"255 unknown", 255, Evidence{}, failed(ClassUnknown)},
		{"-1 unknown", -1, Evidence{}, failed(ClassUnknown)},
		{"4 unknown", 4, Evidence{}, failed(ClassUnknown)},
		{"9 unknown", 9, Evidence{}, failed(ClassUnknown)},
		{"13 unknown", 13, Evidence{}, failed(ClassUnknown)},
		{"129 unknown", 129, Evidence{}, failed(ClassUnknown)},
		{"131 unknown", 131, Evidence{}, failed(ClassUnknown)},
	}
	for _, k := range kinds {
		for _, tt := range common {
			t.Run(string(k)+"/"+tt.name, func(t *testing.T) {
				got := Map(k, tt.code, tt.ev)
				if got != tt.want {
					t.Fatalf("Map(%s, %d, %+v) = %+v, want %+v", k, tt.code, tt.ev, got, tt.want)
				}
				checkSchemaShape(t, got)
			})
		}
		// the evidence does not change the fixed rows
		for _, code := range []int{3, 10, 11, 12} {
			noisy := Evidence{SummarySeen: true, Stderr: ClassNetwork, CheckErrors: 9, Cancel: CancelAgent}
			if k == KindCheck && code != 11 {
				// check errors override these rows; TestMapCheckErrors covers them
				noisy.CheckErrors = 0
			}
			if a, b := Map(k, code, Evidence{}), Map(k, code, noisy); a != b {
				t.Errorf("%s exit %d depends on evidence: %+v vs %+v", k, code, a, b)
			}
		}
	}

	// Exit 0.
	exit0 := []struct {
		name string
		kind Kind
		ev   Evidence
		want Result
	}{
		{"backup with summary", KindBackup, Evidence{SummarySeen: true}, success},
		{"backup without summary", KindBackup, Evidence{}, failed(ClassParse)},
		{"backup without summary ignores stderr", KindBackup, Evidence{Stderr: ClassNetwork}, failed(ClassParse)},
		{"restore without summary", KindRestore, Evidence{}, success},
		{"restore with summary", KindRestore, Evidence{SummarySeen: true}, success},
		{"forget", KindForget, Evidence{}, success},
		{"prune", KindPrune, Evidence{}, success},
		{"unlock", KindUnlock, Evidence{}, success},
		{"check clean", KindCheck, Evidence{}, success},
		{"check with errors", KindCheck, Evidence{CheckErrors: 1}, failed(ClassStorage)},
		{"check with many errors", KindCheck, Evidence{CheckErrors: 50, SummarySeen: true}, failed(ClassStorage)},
		{"forget ignores CheckErrors", KindForget, Evidence{CheckErrors: 3}, success},
		{"read-only kinds succeed", KindSnapshots, Evidence{}, success},
		{"maintenance kind succeeds", KindMaintenance, Evidence{}, success},
	}
	for _, tt := range exit0 {
		t.Run("0/"+tt.name, func(t *testing.T) {
			got := Map(tt.kind, 0, tt.ev)
			if got != tt.want {
				t.Fatalf("got %+v want %+v", got, tt.want)
			}
			checkSchemaShape(t, got)
		})
	}

	// Exits 1 and 2: the stderr class decides. Every class, every kind.
	byClass := map[ErrorClass]Result{
		ClassNetwork:       {Outcome: OutcomeDeferred, DeferredReason: DeferredGateway, ErrorClass: ClassNetwork},
		ClassTLS:           {Outcome: OutcomeDeferred, DeferredReason: DeferredGateway, ErrorClass: ClassTLS},
		ClassAuth:          {Outcome: OutcomeDeferred, DeferredReason: DeferredGateway, ErrorClass: ClassAuth},
		ClassStorage:       {Outcome: OutcomeDeferred, DeferredReason: DeferredGateway, ErrorClass: ClassStorage},
		ClassLock:          lock,
		ClassDisk:          failed(ClassDisk),
		ClassMemory:        failed(ClassMemory),
		ClassPermission:    failed(ClassPermission),
		ClassVSS:           failed(ClassVSS),
		ClassRepoMissing:   failed(ClassRepoMissing),
		ClassWrongPassword: failed(ClassWrongPassword),
		ClassParse:         failed(ClassParse),
		ClassInternal:      failed(ClassInternal),
		ClassLocked:        failed(ClassUnknown),
		ClassMissingSource: failed(ClassUnknown),
		// restic exits 3 for these files, never 1 or 2; if stderr ever says so on a
		// failing exit, the table does not narrow it (like locked and missing_source).
		ClassUnsupportedFile: failed(ClassUnknown),
		ClassUnknown:         failed(ClassUnknown),
		ClassGuardRefused:    failed(ClassUnknown),
	}
	if len(byClass) != len(AllClasses()) {
		t.Fatalf("table covers %d classes, schema has %d", len(byClass), len(AllClasses()))
	}
	for _, k := range append(kinds, KindMaintenance, KindSnapshots) {
		for _, code := range []int{1, 2} {
			for class, want := range byClass {
				got := Map(k, code, Evidence{Stderr: class})
				if got != want {
					t.Errorf("Map(%s, %d, stderr=%s) = %+v, want %+v", k, code, class, got, want)
				}
				checkSchemaShape(t, got)
			}
			for _, odd := range []ErrorClass{"", "bogus"} {
				if got := Map(k, code, Evidence{Stderr: odd}); got != failed(ClassUnknown) {
					t.Errorf("Map(%s, %d, stderr=%q) = %+v, want failed/unknown", k, code, odd, got)
				}
			}
		}
	}
}

// TestMapCheckErrors: architecture addendum 13.3 says num_errors > 0 or broken
// packs mean failed (class storage) at any exit code, not only at exit 0. The
// two exceptions are the lock (11) and the cancel (130), which did not finish
// checking anything.
func TestMapCheckErrors(t *testing.T) {
	failedStorage := Result{Outcome: OutcomeFailed, ErrorClass: ClassStorage}
	lock := Result{Outcome: OutcomeDeferred, DeferredReason: DeferredLock, ErrorClass: ClassLock}
	for _, stderr := range append(AllClasses(), "") {
		for _, tt := range []struct {
			name string
			code int
			ev   Evidence
			want Result
		}{
			{"exit 0", 0, Evidence{}, failedStorage},
			{"exit 1", 1, Evidence{}, failedStorage},
			{"exit 2", 2, Evidence{}, failedStorage},
			{"exit 3", 3, Evidence{}, failedStorage},
			{"exit 10", 10, Evidence{}, failedStorage},
			{"exit 12", 12, Evidence{}, failedStorage},
			{"exit 255", 255, Evidence{}, failedStorage},
			{"exit -1", -1, Evidence{}, failedStorage},
			{"exit 11 stays a lock deferral", 11, Evidence{}, lock},
			{"exit 130 interrupted", 130, Evidence{}, Result{Outcome: OutcomeInterrupted}},
			{"exit 130 cancelled by the agent", 130, Evidence{Cancel: CancelAgent}, Result{Outcome: OutcomeCancelled}},
			{"exit 130 service stopping", 130, Evidence{Cancel: CancelService}, Result{Outcome: OutcomeInterrupted}},
		} {
			ev := tt.ev
			ev.CheckErrors = 2 // NumErrors + BrokenPacks
			ev.Stderr = stderr
			t.Run(fmt.Sprintf("%s/stderr=%s", tt.name, stderr), func(t *testing.T) {
				got := Map(KindCheck, tt.code, ev)
				if got != tt.want {
					t.Fatalf("Map(check, %d, %+v) = %+v, want %+v", tt.code, ev, got, tt.want)
				}
				checkSchemaShape(t, got)
			})
		}
	}
	// the rule is for check only
	for _, k := range []Kind{KindBackup, KindRestore, KindForget, KindPrune, KindUnlock} {
		for _, code := range []int{1, 2, 255} {
			if got, want := Map(k, code, Evidence{CheckErrors: 2}), Map(k, code, Evidence{}); got != want {
				t.Errorf("Map(%s, %d) depends on CheckErrors: %+v vs %+v", k, code, got, want)
			}
		}
	}
	// and without errors a check keeps its ordinary rows
	if got := Map(KindCheck, 2, Evidence{Stderr: ClassNetwork}); got.Outcome != OutcomeDeferred || got.DeferredReason != DeferredGateway {
		t.Errorf("check exit 2 without errors = %+v", got)
	}
	if got := Map(KindCheck, 255, Evidence{}); got != (Result{Outcome: OutcomeFailed, ErrorClass: ClassUnknown}) {
		t.Errorf("check exit 255 without errors = %+v", got)
	}
}

func TestMapEveryResultMatchesSchema(t *testing.T) {
	for _, k := range []Kind{KindBackup, KindRestore, KindForget, KindPrune, KindCheck, KindUnlock} {
		for code := -2; code <= 260; code++ {
			for _, cancel := range []CancelCause{CancelNone, CancelAgent, CancelService} {
				for _, class := range append(AllClasses(), "") {
					for _, ev := range []Evidence{
						{Cancel: cancel, Stderr: class},
						{Cancel: cancel, Stderr: class, SummarySeen: true, CheckErrors: 2},
					} {
						checkSchemaShape(t, Map(k, code, ev))
					}
				}
			}
		}
	}
}
