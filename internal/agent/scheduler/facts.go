package scheduler

import (
	"hash/fnv"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/journal"
	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// Facts are the scheduling facts derived from the journal's backup rows:
// nothing is stored, everything is recomputed from the rows (architecture §6.1:
// "no stored next-run").
type Facts struct {
	// LastSuccess is the finish time of the newest success or tolerated partial
	// run (zero = never).
	LastSuccess time.Time
	// LastAttemptStart is the start of the newest attempt (open rows included,
	// skipped rows excluded).
	LastAttemptStart time.Time
	// Retry is the backoff state after incomplete attempts since the last success.
	Retry RetryState
	// Attempts is the number of consecutive incomplete attempts since the last
	// success; the next run's RunRecord.Attempt is Attempts+1.
	Attempts int
	// Last is the newest finished, non-skipped backup row (nil = none).
	Last *journal.RunRow
}

// maxAttemptsCounted bounds the attempt counter so the backoff stays at its cap.
const maxAttemptsCounted = 30

// FactsFromRuns computes Facts from backup rows, newest start first (as
// journal.RecentRuns returns them). tolerance is the policy's partial
// tolerance: a partial run within it counts as a success for scheduling.
//
// Incomplete attempts are runs that finished as deferred, failed, interrupted,
// abandoned or cancelled, and partial runs over the tolerance: each owes a
// retry after RetryDelay.
//
// A run deferred for fleet_cap (the console refused the start lease, ADR 0005)
// is skipped like a "skipped" row: restic never started, so it is no attempt,
// it does not climb the retry-backoff ladder, it does not mark the slot as
// attempted (a fixed-time slot would otherwise be lost), and it is not Last.
// Its wait is the console's retry_after_s, which the agent passes to Plan as
// Input.LeaseNotBefore. A cancelled run is retried like the others (an
// operator who wants it to stay stopped pauses the device, an M1 command), and
// a run cancelled at max_runtime resumes from restic's index checkpoints
// (§6.2).
func FactsFromRuns(rows []journal.RunRow, pol policy.Policy) Facts {
	var f Facts
	for _, r := range rows {
		if r.Outcome == restic.OutcomeSkipped || FleetCapDeferral(r) {
			continue
		}
		if f.LastAttemptStart.IsZero() {
			f.LastAttemptStart = r.StartedAt
		}
		if r.FinishedAt == nil {
			continue // open: an attempt, not a result
		}
		if f.Last == nil {
			rr := r
			f.Last = &rr
		}
		if Completed(r, pol.SLA.PartialTolerance) {
			f.LastSuccess = *r.FinishedAt
			break
		}
		if f.Attempts < maxAttemptsCounted {
			f.Attempts++
		}
	}
	if f.Attempts > 0 && f.Last != nil && f.Last.FinishedAt != nil && !Completed(*f.Last, pol.SLA.PartialTolerance) {
		last := f.Last
		d := RetryDelay(pol.Schedule.Retries, f.Attempts, last.DeferredReason == restic.DeferredLock, stableFraction(last.ID))
		f.Retry = RetryState{Pending: true, NotBefore: last.FinishedAt.Add(d)}
		if AuthDeferral(*last) {
			f.Retry.AuthDeferredAt = *last.FinishedAt
		}
	}
	return f
}

// FleetCapDeferral reports whether a row is a run the console's start lease
// refused (outcome deferred, reason fleet_cap): not an attempt (ADR 0005).
func FleetCapDeferral(r journal.RunRow) bool {
	return r.Outcome == restic.OutcomeDeferred && r.DeferredReason == restic.DeferredFleetCap
}

// AuthDeferral reports whether a row is a run the gateway turned away: outcome
// deferred, error class auth (restic's own words for a refused login). Unlike a
// fleet-cap deferral it is an attempt and climbs the ladder; RetryState.
// AuthDeferredAt marks it for Input.AuthDeferralsWait.
func AuthDeferral(r journal.RunRow) bool {
	return r.Outcome == restic.OutcomeDeferred && r.ErrorClass == restic.ClassAuth
}

// Completed reports whether a finished row completes its slot: a success, or a
// partial within the tolerance. The partial rule is the console's own
// (internal/state withinTolerance, architecture §3.4 and §4.4), so the agent
// retries on the normal backoff exactly when the console alarms PARTIAL
// (L-VSS-2: a VSS fallback under the failed-file tolerance used to count as
// complete here while the console called it over, so the agent waited a full
// interval under an alarm). A partial completes only when every one holds:
//
//   - no volume fell back from VSS (stats.vss_fallback_volumes is empty):
//     restic reads a volume only to back up a root on it, so every fallback
//     the agent reports is on a root volume, which the console rules over
//     tolerance (its VSSFallbackOnRoot);
//   - the counts are not negative;
//   - every error class, the run's and each one in its error counts, is
//     permission, locked or unsupported_file (vss and missing_source never are);
//   - failed files are attributed to some class;
//   - failed files are at most MaxFailedFiles, or at most MaxFailedPct percent
//     of the files processed plus the failed ones.
func Completed(r journal.RunRow, tol policy.PartialTolerance) bool {
	switch r.Outcome {
	case restic.OutcomeSuccess:
		return true
	case restic.OutcomePartial:
		return partialWithinTolerance(r, tol)
	}
	return false
}

// partialWithinTolerance is internal/state's withinTolerance on a journal row.
func partialWithinTolerance(r journal.RunRow, tol policy.PartialTolerance) bool {
	failed, total := r.Stats.FailedFiles, r.Stats.TotalFilesProcessed
	if failed < 0 || total < 0 || len(r.Stats.VSSFallbackVolumes) > 0 {
		return false
	}
	if r.ErrorClass != "" && !tolerableClass(r.ErrorClass) {
		return false
	}
	for _, e := range r.ErrorCounts {
		if !tolerableClass(e.Class) {
			return false
		}
	}
	if failed > 0 && r.ErrorClass == "" && len(r.ErrorCounts) == 0 {
		return false
	}
	if failed <= int64(tol.MaxFailedFiles) {
		return true
	}
	if denom := total + failed; denom > 0 {
		return float64(failed)/float64(denom)*100 <= tol.MaxFailedPct
	}
	return false
}

// tolerableClass: the classes a partial may carry and still count (the
// console's isTolerableClass).
func tolerableClass(c restic.ErrorClass) bool {
	return c == restic.ClassPermission || c == restic.ClassLocked || c == restic.ClassUnsupportedFile
}

// stableFraction maps an id to [0, 1) so the backoff spread of one failed run
// is the same on every plan.
func stableFraction(id string) float64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return float64(h.Sum32()%10000) / 10000
}
