package state

import (
	"sort"
	"time"
)

const (
	custodyEscrowed     = "escrowed"
	custodyCustomerHeld = "customer_held"
	serviceDeferred     = "deferred"
)

// withinTolerance applies the §3.4 / §4.4 partial-tolerance rule to a partial
// run. It is true only when every one of these holds; any doubt is "over":
//
//   - the counts parsed (CountsKnown), and none is negative;
//   - no volume that held a configured root fell back from VSS (VSSFallbackOnRoot);
//   - every error class, in ErrorClass and in ErrorCounts, is tolerable
//     (permission, locked or unsupported_file), so missing_source is always over;
//   - failed files are attributed to some class (a partial with failed files and no
//     class at all cannot be proven to be the expected kind);
//   - FailedFiles <= MaxFailedFiles, or the failed share of
//     (TotalFiles + FailedFiles) is <= MaxFailedPct percent.
func withinTolerance(r *RunFacts, th Thresholds) bool {
	if r == nil || !r.CountsKnown || r.FailedFiles < 0 || r.TotalFiles < 0 || r.VSSFallbackOnRoot {
		return false
	}
	if r.ErrorClass != "" && !isTolerableClass(r.ErrorClass) {
		return false
	}
	for class := range r.ErrorCounts {
		if !isTolerableClass(class) {
			return false
		}
	}
	if r.FailedFiles > 0 && r.ErrorClass == "" && len(r.ErrorCounts) == 0 {
		return false
	}
	if r.FailedFiles <= int64(th.MaxFailedFiles) {
		return true
	}
	if denom := r.TotalFiles + r.FailedFiles; denom > 0 {
		return float64(r.FailedFiles)/float64(denom)*100 <= th.MaxFailedPct
	}
	return false
}

// pauseInForce reports whether a pause that has a paused_at is still active at
// now. An absent expiry is an indefinite pause. An expiry that is in the past
// is ignored (B11). An expiry that does not parse is also ignored: a pause
// the engine cannot read must not keep a device quiet.
func pauseInForce(until Millis, now time.Time) bool {
	if until == 0 {
		return true
	}
	t, ok := until.Time()
	return ok && t.After(now)
}

// custodyOf is the effective key custody: the active escrow row wins, then the
// customer default, and an empty value means escrowed (the schema default).
func custodyOf(f *DeviceFacts) string {
	if f.Escrow != nil && f.Escrow.Custody != "" {
		return f.Escrow.Custody
	}
	if f.CustomerKeyCustody != "" {
		return f.CustomerKeyCustody
	}
	return custodyEscrowed
}

// EscrowRecheckEnforced says whether ESCROW_UNVERIFIED is raised for a
// verification that is merely older than the recheck interval
// (escrow_recheck_days, reason escrow_recheck_overdue). It is false until the
// periodic escrow verification job exists (scheduled for M5): nothing re-runs
// the verification before then, so every verified device would turn
// ESCROW_UNVERIFIED on day 31 with no way to clear it (M2 lab finding F7,
// ADR 0014). Set it to true in the change that adds that job;
// TestEscrowRecheckIsNotEnforcedUntilTheVerifyJobExists documents the switch.
// Missing escrow, a recorded verify failure and "never verified after the
// grace" are raised either way.
const EscrowRecheckEnforced = false

// escrowIssue evaluates §4.2 row 12. For an escrowed key it reports a problem
// when there is no active escrow row, the last verification failed, the key was
// never verified and the grace is gone, or (only when EscrowRecheckEnforced) the
// verification is older than the recheck interval. Customer-held keys never
// raise it (B36).
func escrowIssue(f *DeviceFacts, th Thresholds, now, enrolled time.Time, enrolledOK bool) (reason Reason, since time.Time, bad bool) {
	return escrowIssueRecheck(f, th, now, enrolled, enrolledOK, EscrowRecheckEnforced)
}

// escrowIssueRecheck is escrowIssue with the recheck rule switched explicitly,
// so the rule stays tested while it is off.
func escrowIssueRecheck(f *DeviceFacts, th Thresholds, now, enrolled time.Time, enrolledOK, recheck bool) (reason Reason, since time.Time, bad bool) {
	if custodyOf(f) == custodyCustomerHeld {
		return "", time.Time{}, false
	}
	e := f.Escrow
	if e == nil {
		if enrolledOK {
			since = enrolled
		}
		return ReasonEscrowMissing, since, true
	}
	created, createdOK := e.CreatedAt.Time()
	if createdOK {
		since = created.Add(th.EscrowVerifyGrace)
	}
	if e.LastVerifyErrorClass != "" {
		return ReasonEscrowVerifyFailed, since, true
	}
	if e.VerifiedAt == 0 {
		if !createdOK || now.Sub(created) > th.EscrowVerifyGrace {
			return ReasonEscrowNeverVerified, since, true
		}
		return "", time.Time{}, false
	}
	verified, verifiedOK := e.VerifiedAt.Time()
	if !verifiedOK {
		return ReasonEscrowNeverVerified, since, true
	}
	if recheck && now.Sub(verified) > th.EscrowRecheck {
		return ReasonEscrowRecheckOverdue, verified.Add(th.EscrowRecheck), true
	}
	return "", time.Time{}, false
}

// maintRunFailed reports whether the newest finished run of one maintenance
// operation counts as a failure: a failed (or guard-refused) run, an unknown
// outcome, a partial, or a check that reported errors. Neutral outcomes, such
// as a lock deferral, are not failures (B31).
func maintRunFailed(op string, r *RunFacts) bool {
	if r == nil {
		return false
	}
	switch OutcomeClass(r.Outcome, false) {
	case ClassFailure, ClassPartial:
		return true
	case ClassNeutral:
		return false
	}
	if op == "check" {
		n := 0
		for _, c := range r.ErrorCounts {
			n += c
		}
		return n > 0
	}
	return false
}

// maintIssue evaluates §4.2 row 13: a failed forget/prune/check, maintenance
// starved by deferrals, or no successful forget within twice the cadence.
func maintIssue(f *DeviceFacts, th Thresholds, now, enrolled time.Time, enrolledOK bool) (op string, reason Reason, since time.Time, bad bool) {
	m := f.Maintenance
	if m == nil {
		return "", "", time.Time{}, false
	}
	runs := [...]struct {
		op string
		r  *RunFacts
	}{{"forget", m.LastForget}, {"prune", m.LastPrune}, {"check", m.LastCheck}}
	for _, x := range runs {
		if maintRunFailed(x.op, x.r) {
			if t, ok := x.r.FinishedAt.Time(); ok {
				since = t
			}
			return x.op, ReasonMaintFailed, since, true
		}
	}
	if th.MaintenanceDeferralAlarmAfter > 0 && m.DeferredCount >= th.MaintenanceDeferralAlarmAfter {
		return "starved", ReasonMaintStarved, time.Time{}, true
	}
	if m.ForgetCadence > 0 {
		limit := 2 * m.ForgetCadence
		var ref time.Time
		refOK := false
		if m.LastForgetSuccessAt != 0 {
			ref, refOK = m.LastForgetSuccessAt.Time()
		} else if enrolledOK {
			ref, refOK = enrolled, true
		}
		if !refOK {
			return "forget_overdue", ReasonMaintForgetOverdue, time.Time{}, true
		}
		if now.Sub(ref) > limit {
			return "forget_overdue", ReasonMaintForgetOverdue, ref.Add(limit), true
		}
	}
	return "", "", time.Time{}, false
}

// deferredReason maps a run's deferred_reason onto the known chip vocabulary.
func deferredReason(raw string) Reason {
	switch raw {
	case deferredLock:
		return ReasonDeferredLock
	case deferredGateway:
		return ReasonDeferredGateway
	case deferredBattery:
		return ReasonDeferredBattery
	case deferredMetered:
		return ReasonDeferredMetered
	case deferredWindow:
		return ReasonDeferredWindow
	case deferredCondition:
		return ReasonDeferredConditions
	case deferredFleetCap:
		return ReasonDeferredFleetCap
	default:
		return ReasonDeferredUnknown
	}
}

// ackView evaluates an acknowledgement at read time (B43-B45). The device's
// state is untouched; only Muted and the attributes change. A plain ack mutes
// until cleared; a timed snooze mutes until it expires; a snooze-until-next-
// success mutes until a success newer than the one recorded at ack time.
func ackView(a *AckFacts, succ time.Time, succOK bool, now time.Time) (view *AckView, acked, snoozed bool) {
	view = &AckView{}
	if t, ok := a.AckedAt.Time(); ok {
		view.AckedAt = t
	}
	until, untilOK := a.SnoozeUntil.Time()
	if untilOK {
		view.SnoozedUntil = until
	}
	timedActive := untilOK && until.After(now)
	nextActive := false
	if a.SnoozeUntilNextSuccess {
		nextActive = true
		if succOK {
			base, baseOK := a.AckedAtSuccess.Time()
			if !baseOK || succ.After(base) {
				nextActive = false
			}
		}
	}
	plain := a.SnoozeUntil == 0 && !a.SnoozeUntilNextSuccess
	view.Muted = plain || timedActive || nextActive
	return view, plain, timedActive || nextActive
}

// sortedClassCounts turns an error-count map into a slice ordered by class, so
// no map iteration order leaks into the output.
func sortedClassCounts(m map[string]int) []ClassCount {
	if len(m) == 0 {
		return nil
	}
	out := make([]ClassCount, 0, len(m))
	for c, n := range m {
		out = append(out, ClassCount{Class: c, N: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Class < out[j].Class })
	return out
}

func sortedCopy(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}
