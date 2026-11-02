package state

// State is one row of the architecture §4.2 precedence table. A device can
// satisfy several states at once; the first in Precedence() order becomes
// DeviceState.Primary and every applicable state is kept in DeviceState.Flags.
type State string

// The seventeen states, declared in the order of the §4.2 rows.
const (
	// StateRetired (row 1): retired_at is set. Suppresses everything.
	StateRetired State = "RETIRED"
	// StatePaused (row 2): an unexpired console pause.
	StatePaused State = "PAUSED"
	// StateUnknown (row 3): the device's facts could not be loaded completely.
	StateUnknown State = "UNKNOWN"
	// StateIdentityConflict (row 4): a second machine presented this identity.
	StateIdentityConflict State = "IDENTITY_CONFLICT"
	// StatePendingFirst (row 5): no success since (re)enrollment, still inside the grace window.
	StatePendingFirst State = "PENDING_FIRST"
	// StateBlank (row 6): no success since (re)enrollment and the grace is gone, or a hard failure, or a garbled success time.
	StateBlank State = "BLANK"
	// StateStuck (row 7): an open backup run has outlived its limit.
	StateStuck State = "STUCK"
	// StateFailed (row 8): the newest finished backup run failed and nothing has succeeded since.
	StateFailed State = "FAILED"
	// StatePartial (row 9): the newest finished backup run is a partial over tolerance.
	StatePartial State = "PARTIAL"
	// StateUnseen (row 10): no heartbeat for longer than unseen_after.
	StateUnseen State = "UNSEEN"
	// StateStale (row 11): the last success is older than stale_after.
	StateStale State = "STALE"
	// StateEscrowUnverified (row 12): the escrowed repository key has not been proven recoverable.
	StateEscrowUnverified State = "ESCROW_UNVERIFIED"
	// StateMaintFailed (row 13): retention maintenance failed or is starved.
	StateMaintFailed State = "MAINT_FAILED"
	// StateMissed (row 14): a scheduled run did not happen (early warning, becomes STALE).
	StateMissed State = "MISSED"
	// StateDeferred (row 15): the agent is deliberately not running (lock, gateway, battery, ...).
	StateDeferred State = "DEFERRED"
	// StateOutdated (row 16): the agent runs a version older than expected.
	StateOutdated State = "OUTDATED"
	// StateOK (row 17): none of the above.
	StateOK State = "OK"
)

var precedence = [...]State{
	StateRetired, StatePaused, StateUnknown, StateIdentityConflict,
	StatePendingFirst, StateBlank, StateStuck, StateFailed, StatePartial,
	StateUnseen, StateStale, StateEscrowUnverified, StateMaintFailed,
	StateMissed, StateDeferred, StateOutdated, StateOK,
}

// Precedence returns the seventeen states in §4.2 row order, highest first.
// The slice is a copy; callers may modify it.
func Precedence() []State {
	out := make([]State, len(precedence))
	copy(out, precedence[:])
	return out
}

// rank is the zero-based §4.2 row index of s; an unknown state sorts after OK.
func (s State) rank() int {
	for i, p := range precedence {
		if p == s {
			return i
		}
	}
	return len(precedence)
}

// Severity is how loudly a state or a customer alarms. It is ordered:
// SeverityNone < SeverityAlarm < SeverityCritical.
type Severity int

const (
	// SeverityNone never notifies.
	SeverityNone Severity = iota
	// SeverityAlarm is the "alarm" column of §4.2 (PARTIAL, ESCROW_UNVERIFIED, MAINT_FAILED).
	SeverityAlarm
	// SeverityCritical is the "critical" column of §4.2.
	SeverityCritical
)

// String renders the severity as "none", "alarm" or "critical".
func (s Severity) String() string {
	switch s {
	case SeverityNone:
		return "none"
	case SeverityAlarm:
		return "alarm"
	case SeverityCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// Reason is the short code that qualifies a primary state in the UI chip
// (architecture §4.2 "UI" column). It never carries a path.
type Reason string

const (
	// ReasonNeverReported: BLANK because the device was enrolled but never confirmed or reported.
	ReasonNeverReported Reason = "never_reported"
	// ReasonNoSuccessfulRun: BLANK because no successful run exists since (re)enrollment.
	ReasonNoSuccessfulRun Reason = "no_successful_run"
	// ReasonOnlyFailures: BLANK because the only runs since (re)enrollment failed.
	ReasonOnlyFailures Reason = "only_failures"

	// ReasonWithinGrace: PENDING_FIRST inside the first-backup grace window.
	ReasonWithinGrace Reason = "within_grace"
	// ReasonSeedExtended: PENDING_FIRST past the deadline only because an initial run is still making progress.
	ReasonSeedExtended Reason = "seed_extended"

	// ReasonFactsUnavailable: UNKNOWN, the loader could not produce complete facts.
	ReasonFactsUnavailable Reason = "facts_unavailable"

	// ReasonDeferredLock: DEFERRED, the repository was locked.
	ReasonDeferredLock Reason = "lock"
	// ReasonDeferredGateway: DEFERRED, the gateway was unreachable.
	ReasonDeferredGateway Reason = "gateway"
	// ReasonDeferredBattery: DEFERRED, battery below the floor.
	ReasonDeferredBattery Reason = "battery"
	// ReasonDeferredMetered: DEFERRED, metered connection.
	ReasonDeferredMetered Reason = "metered"
	// ReasonDeferredWindow: DEFERRED, outside the backup window.
	ReasonDeferredWindow Reason = "window"
	// ReasonDeferredConditions: DEFERRED, other run conditions not met.
	ReasonDeferredConditions Reason = "conditions"
	// ReasonDeferredFleetCap: DEFERRED, the console refused the start lease
	// because the storage target's max_concurrent_backups were all running
	// (ADR 0005). No alarm by itself; the SLA clock keeps running, so a cap
	// set too low surfaces as STALE.
	ReasonDeferredFleetCap Reason = "fleet_cap"
	// ReasonDeferredUnknown: DEFERRED with a reason this engine does not know (a newer agent) or none given.
	ReasonDeferredUnknown Reason = "unknown"

	// ReasonEscrowMissing: ESCROW_UNVERIFIED, no active escrow row exists.
	ReasonEscrowMissing Reason = "escrow_missing"
	// ReasonEscrowNeverVerified: ESCROW_UNVERIFIED, never verified and the grace is gone.
	ReasonEscrowNeverVerified Reason = "escrow_never_verified"
	// ReasonEscrowVerifyFailed: ESCROW_UNVERIFIED, the last verification failed.
	ReasonEscrowVerifyFailed Reason = "escrow_verify_failed"
	// ReasonEscrowRecheckOverdue: ESCROW_UNVERIFIED, the last verification is older than the recheck interval.
	ReasonEscrowRecheckOverdue Reason = "escrow_recheck_overdue"

	// ReasonMaintFailed: MAINT_FAILED, a forget, prune or check failed.
	ReasonMaintFailed Reason = "maintenance_failed"
	// ReasonMaintStarved: MAINT_FAILED, maintenance was deferred too many times in a row.
	ReasonMaintStarved Reason = "maintenance_starved"
	// ReasonMaintForgetOverdue: MAINT_FAILED, no successful forget within twice the cadence.
	ReasonMaintForgetOverdue Reason = "forget_overdue"
)

// Attr is a non-primary badge: it is shown next to the state but never
// changes it (architecture §4.2 "attributes").
type Attr string

const (
	// AttrOffline: no heartbeat for more than two minutes, while not UNSEEN. Connectivity is not health.
	AttrOffline Attr = "offline"
	// AttrCustomerHeldKey: the repository key is held by the customer, not escrowed.
	AttrCustomerHeldKey Attr = "customer_held_key"
	// AttrPolicyDrift: the applied policy version differs from the current one.
	AttrPolicyDrift Attr = "policy_drift"
	// AttrClockSkew: the device clock differs from the server clock by more than two minutes.
	AttrClockSkew Attr = "clock_skew"
	// AttrPartialUnderTolerance: the newest backup was a partial that passed the tolerance rule and counts as a success.
	AttrPartialUnderTolerance Attr = "partial_under_tolerance"
	// AttrVSSFallback: the newest backup fell back from a VSS snapshot on at least one volume.
	AttrVSSFallback Attr = "vss_fallback"
	// AttrNoSchedule: the policy has no schedule interval, so MISSED can never fire.
	AttrNoSchedule Attr = "no_schedule"
	// AttrAcknowledged: an operator acknowledged the alarm (muted until cleared).
	AttrAcknowledged Attr = "acknowledged"
	// AttrSnoozed: an operator snoozed the alarm and the snooze is still in force.
	AttrSnoozed Attr = "snoozed"
	// AttrPausedIndefinite: the pause has no expiry; the digest flags these.
	AttrPausedIndefinite Attr = "paused_indefinite"
)

// attrOrder is the fixed order attributes are emitted in.
var attrOrder = [...]Attr{
	AttrOffline, AttrCustomerHeldKey, AttrPolicyDrift, AttrClockSkew,
	AttrPartialUnderTolerance, AttrVSSFallback, AttrNoSchedule,
	AttrAcknowledged, AttrSnoozed, AttrPausedIndefinite,
}

// CustomerStatus is the §4.5 rollup state of one customer.
type CustomerStatus string

const (
	// CustomerMissing: an expected customer has no active devices and no onboarding grace.
	CustomerMissing CustomerStatus = "missing"
	// CustomerOnboarding: an expected customer with no devices yet, inside its onboarding window.
	CustomerOnboarding CustomerStatus = "onboarding"
	// CustomerAlarm: at least one device is in a critical or alarm state.
	CustomerAlarm CustomerStatus = "alarm"
	// CustomerWarn: no alarms, but something needs attention (MISSED, DEFERRED, OUTDATED, an indefinite pause, or every device parked).
	CustomerWarn CustomerStatus = "warn"
	// CustomerOK: every active device is OK or PENDING_FIRST.
	CustomerOK CustomerStatus = "ok"
	// CustomerEmpty: not expected and no active devices. Never green, never in health percentages.
	CustomerEmpty CustomerStatus = "empty"
	// CustomerUnknown: the customer contains a device whose state is UNKNOWN. Sorts with MISSING.
	CustomerUnknown CustomerStatus = "unknown"
)

// sortKey is the §4.5 customer ordering: MISSING, UNKNOWN-containing, ALARM,
// WARN, OK, ONBOARDING, EMPTY.
func (c CustomerStatus) sortKey() int {
	switch c {
	case CustomerMissing:
		return 0
	case CustomerUnknown:
		return 1
	case CustomerAlarm:
		return 2
	case CustomerWarn:
		return 3
	case CustomerOK:
		return 4
	case CustomerOnboarding:
		return 5
	case CustomerEmpty:
		return 6
	default:
		return 0 // an unrecognised status must never sort quietly to the bottom
	}
}

// ResultClass is the §4.4 class of a backup run outcome.
type ResultClass string

const (
	// ClassSuccess counts as a success and clears FAILED / BLANK.
	ClassSuccess ResultClass = "success"
	// ClassPartial is a partial run over tolerance: neither success nor failure, becomes PARTIAL.
	ClassPartial ResultClass = "partial"
	// ClassFailure counts as a failure and clears nothing.
	ClassFailure ResultClass = "failure"
	// ClassNeutral changes nothing: cancelled, skipped, interrupted, deferred.
	ClassNeutral ResultClass = "neutral"
)

// Run outcome vocabulary (schema: runs.outcome).
const (
	outcomeSuccess     = "success"
	outcomePartial     = "partial"
	outcomeFailed      = "failed"
	outcomeAbandoned   = "abandoned"
	outcomeCancelled   = "cancelled"
	outcomeSkipped     = "skipped"
	outcomeDeferred    = "deferred"
	outcomeInterrupted = "interrupted"
)

// OutcomeClass maps a run outcome string to its §4.4 result class.
// partialWithinTolerance is only consulted for "partial". Any value this
// engine does not know, including the empty string, is a failure: an unknown
// result from a newer agent "MUST be treated as a command failure" (engine
// §1.10, test B46).
func OutcomeClass(outcome string, partialWithinTolerance bool) ResultClass {
	switch outcome {
	case outcomeSuccess:
		return ClassSuccess
	case outcomePartial:
		if partialWithinTolerance {
			return ClassSuccess
		}
		return ClassPartial
	case outcomeFailed, outcomeAbandoned:
		return ClassFailure
	case outcomeCancelled, outcomeSkipped, outcomeInterrupted, outcomeDeferred:
		return ClassNeutral
	default:
		return ClassFailure
	}
}

// Error-class vocabulary (schema: runs.error_class). These strings duplicate
// the vocabulary owned by internal/restic; this package may not import it, so
// the two sets are asserted equal by a test in the console (see doc.go).
const (
	errRepoMissing     = "repo_missing"
	errWrongPassword   = "wrong_password"
	errPermission      = "permission"
	errDisk            = "disk"
	errParse           = "parse"
	errInternal        = "internal"
	errLocked          = "locked"
	errMissingSource   = "missing_source"
	errUnsupportedFile = "unsupported_file"
	deferredLock       = "lock"
	deferredGateway    = "gateway"
	deferredBattery    = "battery"
	deferredMetered    = "metered"
	deferredWindow     = "window"
	deferredCondition  = "conditions"
	deferredFleetCap   = "fleet_cap"
)

// HardFailureClasses returns the error classes that break a device out of
// PENDING_FIRST at once (§4.2 row 5), sorted. It exists so the console can
// assert that internal/restic and this package agree on the vocabulary.
func HardFailureClasses() []string {
	return []string{errDisk, errInternal, errParse, errPermission, errRepoMissing, errWrongPassword}
}

// TolerableErrorClasses returns the error classes a partial backup may carry
// and still pass tolerance (§3.4: permission, locked and unsupported files),
// sorted.
func TolerableErrorClasses() []string {
	return []string{errLocked, errPermission, errUnsupportedFile}
}

func isHardFailureClass(class string) bool {
	switch class {
	case errRepoMissing, errWrongPassword, errPermission, errDisk, errParse, errInternal:
		return true
	}
	return false
}

func isTolerableClass(class string) bool {
	return class == errPermission || class == errLocked || class == errUnsupportedFile
}
