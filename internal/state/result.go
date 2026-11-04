package state

import "time"

// AckView is how an acknowledgement or snooze looks at read time. The state
// it sits on is unchanged; only notifications are muted (B43).
type AckView struct {
	// Muted is true while the acknowledgement or snooze is in force. A lapsed
	// snooze or a snooze cleared by a success keeps its view with Muted false.
	Muted bool
	// SnoozedUntil is the snooze expiry; zero when the ack is not a timed snooze.
	SnoozedUntil time.Time
	// AckedAt is when the operator acknowledged; zero when unknown.
	AckedAt time.Time
}

// ClassCount is one error class and how many files it affected.
type ClassCount struct {
	Class string
	N     int
}

// Detail holds the counts and labels the UI needs to render a state. It never
// carries a file path. Times are UTC; a zero time means "not known".
type Detail struct {
	// LastSuccess is the effective last success since (re)enrollment, including a partial under
	// tolerance; zero when there is none.
	LastSuccess time.Time
	// LastSeen is the heartbeat the engine judged against (falls back to enrollment).
	LastSeen time.Time
	// LastOutcome is the newest finished backup run's outcome.
	LastOutcome string
	// OpenRunAge, OpenRunLimit and OpenRunInitial describe an open backup run.
	OpenRunAge     time.Duration
	OpenRunLimit   time.Duration
	OpenRunInitial bool
	// FailedFiles and TotalFiles are from the newest finished backup run.
	FailedFiles, TotalFiles int64
	// ErrorClass is the newest finished backup run's error class; ErrorCounts its per-class counts, sorted by class.
	ErrorClass  string
	ErrorCounts []ClassCount
	// ConsecutiveFailures is the failure streak the loader counted.
	ConsecutiveFailures int
	// AgentVersion and ExpectedVersion are the compared versions.
	AgentVersion, ExpectedVersion string
	// DeferredReason is the raw deferred reason of the newest backup run, when it was deferred.
	DeferredReason string
	// GraceDeadline is the first-backup deadline, after any seed extension; zero when unknown.
	GraceDeadline time.Time
	// PauseUntil is the pause expiry (zero = indefinite) and PauseReason the operator's note.
	PauseUntil  time.Time
	PauseReason string
	// MaintOp names what made MAINT_FAILED: "forget", "prune", "check", "starved" or "forget_overdue".
	MaintOp string
	// EscrowIssue is the escrow problem, as a Reason, when ESCROW_UNVERIFIED applies.
	EscrowIssue Reason
	// VSSVolumes lists volumes that fell back from VSS on the newest backup, sorted.
	VSSVolumes []string
	// ClockOffset is the device clock offset, informational.
	ClockOffset time.Duration
	// LoadErrorClass is why the facts are incomplete, for UNKNOWN devices.
	LoadErrorClass string
	// GarbledFields names every timestamp the engine found unparseable and failed loud on, in a fixed order.
	GarbledFields []string
}

// DeviceState is the engine's verdict on one device. It is computed, never stored.
type DeviceState struct {
	ID, CustomerID, Hostname string
	// Primary is the first applicable state in Precedence() order.
	Primary State
	// Flags lists every applicable (unsuppressed) state in precedence order, Primary first.
	Flags []State
	// Since is the §4.2 "since" instant for Primary; zero when the facts do not carry one.
	Since time.Time
	// Reason qualifies Primary (BLANK chip, deferred reason, escrow or maintenance cause).
	Reason Reason
	// Alarm is the highest severity over every flag, not only Primary: a paused device with an
	// unverified escrow still alarms (B13). It is not reduced by an acknowledgement (B43).
	Alarm Severity
	// Attrs are the non-primary badges in a fixed order.
	Attrs []Attr
	// Ack is non-nil when the device has an acknowledgement or snooze row.
	Ack *AckView
	// Detail carries counts and labels for the UI.
	Detail Detail
}

// CustomerState is the §4.5 rollup of one customer.
type CustomerState struct {
	ID, Name string
	Status   CustomerStatus
	// Counts is the number of non-retired devices per Primary state.
	Counts map[State]int
	// Active counts devices that are neither retired nor paused; Paused and Retired count the rest.
	Active, Paused, Retired int
	// HealthPct is 100 x healthy / Active, valid only when HealthKnown. Paused and retired devices are
	// excluded from the denominator (D10).
	HealthPct   float64
	HealthKnown bool
	// Alarm is Critical for MISSING and UNKNOWN-containing customers, otherwise the highest device severity.
	Alarm Severity
	// Muted is true for a suspended customer: notifications are muted, states stay intact (D07).
	Muted bool
	// SortKey is the §4.5 position: 0 MISSING, 1 UNKNOWN, 2 ALARM, 3 WARN, 4 OK, 5 ONBOARDING, 6 EMPTY.
	SortKey int
	// Devices are the customer's devices in (precedence, hostname) order.
	Devices []DeviceState
}

// Banner is a console-level condition rendered above every page (§4.6).
type Banner struct {
	// ID is the §4.6 id ("C01" to "C16"), or the id with a "b" suffix for a "not checked" warning of
	// that condition ("C01b"), the pattern ADR 0013 set with C04b.
	ID       string
	Severity Severity
	Text     string
	// Count is the number the text is about (devices, errors, days, percent).
	Count int
	// At is the instant the text cites ("Last OK", "due by", "data as of"), UTC; zero when none.
	// The text already renders it in World.DisplayZone; At lets a page re-render it.
	At time.Time
}

// FleetView is the result of Evaluate: everything a page, the API, the digest
// or the alarm evaluator needs, stamped with when it was computed.
type FleetView struct {
	// Banners are ordered most severe first, then by id.
	Banners []Banner
	// Devices are ordered by (customer sort key, primary precedence, hostname).
	Devices []DeviceState
	// Customers are ordered by SortKey then name. Archived customers are not included.
	Customers []CustomerState
	// Collapses are the §4.3 collapses: the fleet-wide DEFERRED(gateway) one first (it also raises
	// C01), then customer-wide silence rows in customer order. The device rows they cover remain in
	// Devices; a Collapse is one row and one notification in their place.
	Collapses []Collapse
	// ComputedAt is the injected now (C23); LoadedAt is when the facts were read.
	ComputedAt, LoadedAt time.Time
	// Complete is false when any part of the load failed, was empty after a non-empty load, or any device is UNKNOWN.
	Complete bool
	// FirstRun is true for a fleet that has never enrolled a device: a neutral onboarding panel, not an alarm (C04).
	FirstRun bool
}

// AllClear reports whether nothing at all is wrong and the data is complete.
// It is true only if there are no banners, no device or customer alarms, the
// load was Complete, and the fleet is non-empty (or this is the first run).
// An unknown input can never produce true (C21): the question "is everything
// fine" is only answered yes on evidence.
func (f FleetView) AllClear() bool {
	if len(f.Banners) > 0 || !f.Complete {
		return false
	}
	if len(f.Devices) == 0 && !f.FirstRun {
		return false
	}
	for i := range f.Devices {
		if f.Devices[i].Alarm > SeverityNone || f.Devices[i].Primary == StateUnknown {
			return false
		}
	}
	for i := range f.Customers {
		c := &f.Customers[i]
		if c.Alarm > SeverityNone || c.Status == CustomerMissing || c.Status == CustomerUnknown {
			return false
		}
	}
	return true
}
