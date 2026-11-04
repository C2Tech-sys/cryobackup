package state

import "time"

const (
	offlineAfter        = 2 * time.Minute
	clockSkewLimit      = 2 * time.Minute
	defaultMissedFactor = 2.0
)

// normalize applies the documented defaults: an all-zero Thresholds becomes
// DefaultThresholds, a failure-streak threshold below one means one, and a
// non-positive missed factor means 2.0. Everything else is used as given, so a
// partly filled Thresholds fails loud (a zero StaleAfter alarms at once) rather
// than silently becoming lenient.
func normalize(th Thresholds) Thresholds {
	if th == (Thresholds{}) {
		return DefaultThresholds()
	}
	if th.ConsecutiveFailuresToAlarm < 1 {
		th.ConsecutiveFailuresToAlarm = 1
	}
	if th.MissedFactor <= 0 {
		th.MissedFactor = defaultMissedFactor
	}
	return th
}

// severityOf is the §4.2 "alarms?" column for one state.
func severityOf(s State, outdatedIsAlarm bool) Severity {
	switch s {
	case StateUnknown, StateIdentityConflict, StateBlank, StateStuck,
		StateFailed, StateUnseen, StateStale:
		return SeverityCritical
	case StatePartial, StateEscrowUnverified, StateMaintFailed:
		return SeverityAlarm
	case StateOutdated:
		if outdatedIsAlarm {
			return SeverityAlarm
		}
	}
	return SeverityNone
}

// timeOrZero is the parsed time of m, or the zero time when m is NULL or garbled.
func timeOrZero(m Millis) time.Time {
	t, _ := m.Time()
	return t
}

// Classify judges one device from its facts and the injected server time. It
// is pure: no clock, no I/O, no state. Every §4.2 row is evaluated
// independently, the §4.3 suppression rules remove the rows a stronger
// condition silences, and the first remaining row in precedence order is
// Primary while all of them are kept as Flags.
//
// Fail-loud rules: an unparseable timestamp is never read as healthy, an
// unknown outcome is a failure, and an incomplete load is UNKNOWN and critical.
// Attributes and acknowledgements never change the state.
func Classify(f DeviceFacts, now time.Time) DeviceState {
	now = now.UTC()
	th := normalize(f.Thresholds)
	ds := DeviceState{ID: f.ID, CustomerID: f.CustomerID, Hostname: f.Hostname}

	// Row 1: RETIRED suppresses everything, including an incomplete load.
	if f.RetiredAt != 0 {
		ds.Primary = StateRetired
		ds.Flags = []State{StateRetired}
		ds.Since = timeOrZero(f.RetiredAt)
		return ds
	}

	paused := f.PausedAt != 0 && pauseInForce(f.PausedUntil, now)
	pausedIndefinite := paused && f.PausedUntil == 0
	if f.PausedAt != 0 {
		ds.Detail.PauseUntil = timeOrZero(f.PausedUntil)
		ds.Detail.PauseReason = f.PauseReason
	}

	// Row 3: no other row is evaluated for a device whose facts are incomplete.
	if !f.Complete {
		ds.Primary = StateUnknown
		ds.Flags = []State{StateUnknown}
		ds.Reason = ReasonFactsUnavailable
		ds.Alarm = SeverityCritical
		ds.AlarmFlags = []State{StateUnknown}
		if paused {
			// §4.2 row 3 is defined for non-paused devices, but a pause does not suppress
			// IDENTITY, ESCROW or MAINT, which cannot be evaluated now: stay loud.
			ds.Primary = StatePaused
			ds.Flags = []State{StatePaused, StateUnknown}
			ds.Since = timeOrZero(f.PausedAt)
		}
		ds.Detail.LoadErrorClass = f.LoadErrorClass
		var attrs attrSet
		if pausedIndefinite {
			attrs.add(AttrPausedIndefinite)
		}
		if f.Ack != nil {
			view, acked, snoozed := ackView(f.Ack, time.Time{}, false, now)
			ds.Ack = view
			if view.Muted && acked {
				attrs.add(AttrAcknowledged)
			}
			if view.Muted && snoozed {
				attrs.add(AttrSnoozed)
			}
		}
		ds.Attrs = attrs.list()
		return ds
	}

	var found [len(precedence)]bool
	var since [len(precedence)]time.Time
	set := func(s State, at time.Time) {
		found[s.rank()] = true
		since[s.rank()] = at
	}
	var garbled []string
	note := func(m Millis, name string) {
		if _, ok := m.Time(); m != 0 && !ok {
			garbled = append(garbled, name)
		}
	}

	// ---- time inputs, each judged on the parsed value --------------------------------
	enrolled, enrolledOK := f.EnrolledAt.Time()
	note(f.EnrolledAt, "enrolled_at")
	note(f.FirstBackupDueBy, "first_backup_due_by")

	// The newest backup run counts only if it belongs to this (re)enrollment: adoption
	// by new hardware resets enrolled_at, and runs from the old hardware say nothing.
	lb := f.LastBackup
	var lbFin time.Time
	lbFinOK := false
	if lb != nil {
		lbFin, lbFinOK = lb.FinishedAt.Time()
		if lbFinOK && enrolledOK && lbFin.Before(enrolled) {
			lb = nil
		}
	}

	// Partial tolerance (§3.4, §4.4): a partial within tolerance is the effective success.
	tolerated, partialOver := false, false
	if lb != nil && lb.Outcome == outcomePartial {
		if lbFinOK && withinTolerance(lb, th) {
			tolerated = true
		} else {
			partialOver = true
		}
	}

	succ, succOK := f.LastSuccess.Time()
	note(f.LastSuccess, "last_success")
	garbledSuccess := f.LastSuccess != 0 && !succOK
	if tolerated && (!succOK || lbFin.After(succ)) {
		succ, succOK = lbFin, true
	}
	if succOK {
		garbledSuccess = false
	}
	// Row 5/6 premise: no success since (re)enrollment (B09).
	noSuccess := !succOK || (enrolledOK && succ.Before(enrolled))

	progress, progressOK := f.ProgressAdvancedAt.Time()
	progressFresh := progressOK && now.Sub(progress) <= th.StallAfter

	// ---- row 5 / 6: PENDING_FIRST and BLANK ------------------------------------------
	var deadline time.Time
	deadlineKnown := false
	if d, ok := f.FirstBackupDueBy.Time(); ok {
		deadline, deadlineKnown = d, true
	} else if enrolledOK {
		deadline, deadlineKnown = enrolled.Add(th.FirstBackupGrace), true
	}
	baseDeadline := deadline
	if deadlineKnown && th.SeedExtension > 0 && f.OpenRun != nil && f.OpenRun.Initial && progressFresh {
		deadline = deadline.Add(th.SeedExtension)
	}
	failureRun := lb != nil && OutcomeClass(lb.Outcome, false) == ClassFailure
	hardFailure := failureRun && isHardFailureClass(lb.ErrorClass)
	pending := noSuccess && !garbledSuccess && deadlineKnown && !now.After(deadline) && !hardFailure
	blank := noSuccess && !pending

	var blankReason Reason
	if pending {
		set(StatePendingFirst, enrolled)
	}
	if blank {
		switch {
		case failureRun:
			blankReason = ReasonOnlyFailures
		case f.EnrollConfirmedAt == 0:
			blankReason = ReasonNeverReported
		default:
			blankReason = ReasonNoSuccessfulRun
		}
		at := time.Time{}
		switch {
		case deadlineKnown && now.After(deadline):
			at = deadline
		case lbFinOK:
			at = lbFin
		case deadlineKnown:
			at = deadline
		}
		set(StateBlank, at)
	}

	// ---- row 7: STUCK -----------------------------------------------------------------
	var openAge, openLimit time.Duration
	if f.OpenRun != nil {
		started, ok := f.OpenRun.StartedAt.Time()
		if !ok {
			// An open run that cannot say when it began can never be proven healthy.
			garbled = append(garbled, "open_run_started_at")
			set(StateStuck, time.Time{})
		} else {
			openAge = now.Sub(started)
			openLimit = th.MaxRun
			if f.OpenRun.Initial && progressFresh {
				openLimit = th.InitialMaxRun
			}
			if openAge > openLimit {
				set(StateStuck, started.Add(openLimit))
			}
		}
	}

	// ---- row 8: FAILED ----------------------------------------------------------------
	streak := f.ConsecutiveFailures
	if streak < 1 {
		streak = 1 // the newest run itself is a failure
	}
	laterSuccess := succOK && lbFinOK && succ.After(lbFin)
	if failureRun && streak >= th.ConsecutiveFailuresToAlarm && !laterSuccess {
		set(StateFailed, lbFin)
	}

	// ---- row 9: PARTIAL ---------------------------------------------------------------
	if partialOver {
		set(StatePartial, lbFin)
	}

	// ---- row 10: UNSEEN ---------------------------------------------------------------
	var seenRef time.Time
	seenOK := false
	if f.LastSeen != 0 {
		seenRef, seenOK = f.LastSeen.Time()
		note(f.LastSeen, "last_seen")
	} else {
		seenRef, seenOK = enrolled, enrolledOK
	}
	rawUnseen := !seenOK || now.Sub(seenRef) > th.UnseenAfter
	if rawUnseen {
		at := time.Time{}
		if seenOK {
			at = seenRef.Add(th.UnseenAfter)
		}
		set(StateUnseen, at)
	}

	// ---- row 11: STALE ----------------------------------------------------------------
	rawStale := !noSuccess && now.Sub(succ) > th.StaleAfter
	if rawStale {
		set(StateStale, succ.Add(th.StaleAfter))
	}

	// ---- row 4, 12, 13: IDENTITY_CONFLICT, ESCROW_UNVERIFIED, MAINT_FAILED -----------
	if f.IdentityConflictAt != 0 {
		set(StateIdentityConflict, timeOrZero(f.IdentityConflictAt))
	}
	escReason, escSince, escBad := escrowIssue(&f, th, now, enrolled, enrolledOK)
	if escBad {
		set(StateEscrowUnverified, escSince)
	}
	maintOp, maintReason, maintSince, maintBad := maintIssue(&f, th, now, enrolled, enrolledOK)
	if maintBad {
		set(StateMaintFailed, maintSince)
	}

	// ---- row 15: DEFERRED (computed before MISSED, which defers to it) ----------------
	lbDeferred := lb != nil && lb.Outcome == outcomeDeferred
	rawDeferred := (lbDeferred || f.ServiceState == serviceDeferred) && f.OpenRun == nil && !rawStale
	var deferredWhy Reason
	if rawDeferred {
		at := time.Time{}
		deferredWhy = ReasonDeferredUnknown
		if lbDeferred {
			deferredWhy = deferredReason(lb.DeferredReason)
			if t, ok := lb.StartedAt.Time(); ok {
				at = t
			} else if lbFinOK {
				at = lbFin
			}
		}
		set(StateDeferred, at)
	}

	// ---- row 14: MISSED ---------------------------------------------------------------
	if th.Interval > 0 && !rawUnseen && !noSuccess && !rawStale && !rawDeferred && f.OpenRun == nil {
		ref := succ
		if a, ok := f.LastAttemptStart.Time(); ok && a.After(ref) {
			ref = a
		}
		limit := time.Duration(float64(th.Interval)*th.MissedFactor) + th.WindowSlack
		if now.Sub(ref) > limit {
			set(StateMissed, ref.Add(limit))
		}
	}

	// ---- row 16: OUTDATED -------------------------------------------------------------
	if f.ExpectedVersion != "" && CompareVersions(f.AgentVersion, f.ExpectedVersion) < 0 {
		set(StateOutdated, time.Time{})
	}

	// ---- §4.3 suppression -------------------------------------------------------------
	if paused {
		for _, s := range [...]State{StateBlank, StateStuck, StateFailed, StatePartial, StateUnseen,
			StateStale, StateMissed, StateDeferred, StateOutdated} {
			found[s.rank()] = false
		}
		set(StatePaused, timeOrZero(f.PausedAt))
	}
	if pending {
		// Inside the first-backup grace, freshness conditions stay quiet. A transient failure
		// (a hard one has already broken out of PENDING_FIRST) is shown as attempts, not FAILED.
		for _, s := range [...]State{StateFailed, StateUnseen, StateStale, StateMissed, StateDeferred, StateOutdated} {
			found[s.rank()] = false
		}
	}

	// ---- assemble ---------------------------------------------------------------------
	for i, s := range precedence {
		if s == StateOK || !found[i] {
			continue
		}
		ds.Flags = append(ds.Flags, s)
		sev := severityOf(s, th.OutdatedIsAlarm)
		if sev > SeverityNone {
			ds.AlarmFlags = append(ds.AlarmFlags, s)
		}
		if sev > ds.Alarm {
			ds.Alarm = sev
		}
	}
	if len(ds.Flags) == 0 {
		ds.Flags = []State{StateOK}
		set(StateOK, succ)
		ds.Primary = StateOK
		ds.Since = succ
	} else {
		ds.Primary = ds.Flags[0]
		ds.Since = since[ds.Primary.rank()]
	}

	switch ds.Primary {
	case StatePendingFirst:
		ds.Reason = ReasonWithinGrace
		if deadlineKnown && now.After(baseDeadline) {
			ds.Reason = ReasonSeedExtended
		}
	case StateBlank:
		ds.Reason = blankReason
	case StateEscrowUnverified:
		ds.Reason = escReason
	case StateMaintFailed:
		ds.Reason = maintReason
	case StateDeferred:
		ds.Reason = deferredWhy
	}

	// ---- attributes -------------------------------------------------------------------
	var attrs attrSet
	enable := attrs.add
	if ls, ok := f.LastSeen.Time(); ok && !rawUnseen && now.Sub(ls) > offlineAfter {
		enable(AttrOffline)
	}
	if custodyOf(&f) == custodyCustomerHeld {
		enable(AttrCustomerHeldKey)
	}
	if f.PolicyVersionApplied != f.PolicyVersion {
		enable(AttrPolicyDrift)
	}
	if f.ClockOffset > clockSkewLimit || f.ClockOffset < -clockSkewLimit {
		enable(AttrClockSkew)
	}
	if tolerated {
		enable(AttrPartialUnderTolerance)
	}
	if lb != nil && len(lb.VSSFallback) > 0 {
		enable(AttrVSSFallback)
	}
	if th.Interval <= 0 {
		enable(AttrNoSchedule)
	}
	if pausedIndefinite {
		enable(AttrPausedIndefinite)
	}
	if f.Ack != nil {
		var s time.Time
		if !noSuccess {
			s = succ
		}
		view, acked, snoozed := ackView(f.Ack, s, !noSuccess, now)
		ds.Ack = view
		if view.Muted && acked {
			enable(AttrAcknowledged)
		}
		if view.Muted && snoozed {
			enable(AttrSnoozed)
		}
	}
	ds.Attrs = attrs.list()

	// ---- detail -----------------------------------------------------------------------
	d := &ds.Detail
	if !noSuccess {
		d.LastSuccess = succ
	}
	if seenOK {
		d.LastSeen = seenRef
	}
	if lb != nil {
		d.LastOutcome = lb.Outcome
		d.FailedFiles, d.TotalFiles = lb.FailedFiles, lb.TotalFiles
		d.ErrorClass = lb.ErrorClass
		d.ErrorCounts = sortedClassCounts(lb.ErrorCounts)
		d.VSSVolumes = sortedCopy(lb.VSSFallback)
		if lb.Outcome == outcomeDeferred {
			d.DeferredReason = lb.DeferredReason
		}
	}
	if f.OpenRun != nil {
		d.OpenRunAge, d.OpenRunLimit, d.OpenRunInitial = openAge, openLimit, f.OpenRun.Initial
	}
	d.ConsecutiveFailures = f.ConsecutiveFailures
	d.AgentVersion, d.ExpectedVersion = f.AgentVersion, f.ExpectedVersion
	if deadlineKnown {
		d.GraceDeadline = deadline
	}
	d.MaintOp = maintOp
	d.EscrowIssue = escReason
	d.ClockOffset = f.ClockOffset
	d.GarbledFields = garbled
	return ds
}

// attrSet collects attributes and lists them in the fixed attrOrder, so the
// order never depends on the order the rules ran in.
type attrSet [len(attrOrder)]bool

func (s *attrSet) add(a Attr) {
	for i, x := range attrOrder {
		if x == a {
			s[i] = true
		}
	}
}

func (s *attrSet) list() []Attr {
	var out []Attr
	for i, x := range attrOrder {
		if s[i] {
			out = append(out, x)
		}
	}
	return out
}
