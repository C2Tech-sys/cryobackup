package state

import "time"

// Millis is a raw epoch-milliseconds timestamp exactly as the store holds it.
// Zero means NULL. The engine never trusts a Millis: it parses it with Time
// and judges the parsed result, so a garbled value cannot masquerade as a
// healthy one (tests A03-A05, A12).
type Millis int64

const (
	// minValidMillis is 1e12 ms (2001-09-09): the schema's CHECK for agent-supplied times.
	minValidMillis Millis = 1_000_000_000_000
	// maxValidMillis is 2100-01-01T00:00:00Z.
	maxValidMillis Millis = 4_102_444_800_000
)

// Time parses m. ok is false for NULL (zero) and for every value outside
// [1e12, 4102444800000], which covers negatives, seconds-epoch values and
// far-future garbage; those are "garbled" and the engine fails loud on them.
// The returned time is in UTC.
func (m Millis) Time() (time.Time, bool) {
	if m < minValidMillis || m > maxValidMillis {
		return time.Time{}, false
	}
	return time.UnixMilli(int64(m)).UTC(), true
}

// RunFacts is one finished (or, for OpenRun, running) run as the loader read
// it. It carries counts and class names only, never file paths.
type RunFacts struct {
	// Outcome is runs.outcome; empty for an open run.
	Outcome string
	// StartedAt is when the run started.
	StartedAt Millis
	// FinishedAt is the finish time, loader-clamped to received_at (B40); the engine trusts it.
	FinishedAt Millis
	// FailedFiles is the number of files restic could not read.
	FailedFiles int64
	// TotalFiles is total_files_processed.
	TotalFiles int64
	// CountsKnown is false when the stats could not be parsed (B21: fail loud, over tolerance).
	CountsKnown bool
	// ErrorCounts is {error class: count}; never paths.
	ErrorCounts map[string]int
	// ErrorClass is runs.error_class.
	ErrorClass string
	// DeferredReason is runs.deferred_reason for a deferred run.
	DeferredReason string
	// VSSFallback lists volume letters that fell back from a VSS snapshot, from stats.
	VSSFallback []string
	// VSSFallbackOnRoot is true when a volume that held a configured backup root fell back from VSS.
	// §3.4 makes that always over tolerance. The loader sets it by intersecting VSSFallback with the
	// policy roots (M3); until it does, VSSFallback alone is an information attribute (test B22).
	VSSFallbackOnRoot bool
	// Initial marks the first backup after (re)enrollment.
	Initial bool
}

// EscrowFacts is the active repo_key_escrow row.
type EscrowFacts struct {
	// Custody is "escrowed" or "customer_held".
	Custody string
	// CreatedAt is when the active key was escrowed.
	CreatedAt Millis
	// VerifiedAt is the last successful `cat config` verification; zero means never.
	VerifiedAt Millis
	// LastVerifyErrorClass is non-empty when the most recent verification failed.
	LastVerifyErrorClass string
}

// MaintFacts is the newest maintenance picture for one device.
type MaintFacts struct {
	// LastForget, LastPrune and LastCheck are the newest finished run of each operation; nil when none.
	LastForget, LastPrune, LastCheck *RunFacts
	// LastForgetSuccessAt is the finish time of the newest successful forget; zero when none.
	LastForgetSuccessAt Millis
	// DeferredCount is maintenance_schedule.deferred_count.
	DeferredCount int
	// ForgetCadence is the policy's forget interval; zero disables the "no forget within 2x cadence" rule.
	ForgetCadence time.Duration
}

// AckFacts is the active acknowledgement or snooze for a device. The schema
// keys acks by alarm type; v0 treats one device's ack as covering the device.
type AckFacts struct {
	// AckedAt is when the operator acknowledged.
	AckedAt Millis
	// SnoozeUntil is the snooze expiry; zero when the ack is not a timed snooze.
	SnoozeUntil Millis
	// SnoozeUntilNextSuccess clears the ack at the next successful backup.
	SnoozeUntilNextSuccess bool
	// AckedAtSuccess is the device's last success time when the ack was made; zero if there was none.
	AckedAtSuccess Millis
}

// Thresholds is the merged policy sla + schedule for one device (§3.6, DH6).
// An all-zero Thresholds is replaced by DefaultThresholds; otherwise values are
// used as given, except ConsecutiveFailuresToAlarm < 1 which means 1 and
// MissedFactor <= 0 which means 2.0.
type Thresholds struct {
	// StaleAfter: STALE when the last success is older than this (strict).
	StaleAfter time.Duration
	// UnseenAfter: UNSEEN when the last heartbeat is older than this (strict).
	UnseenAfter time.Duration
	// FirstBackupGrace: time from enrollment to the first-backup deadline when none is stored.
	FirstBackupGrace time.Duration
	// SeedExtension extends the deadline while an initial run keeps making progress; zero is off.
	SeedExtension time.Duration
	// MaxFailedFiles is the partial-tolerance file count.
	MaxFailedFiles int
	// MaxFailedPct is the partial-tolerance percentage of files.
	MaxFailedPct float64
	// ConsecutiveFailuresToAlarm is the failure streak that raises FAILED.
	ConsecutiveFailuresToAlarm int
	// MaxRun is the longest a backup run may stay open.
	MaxRun time.Duration
	// InitialMaxRun is the hard cap for an initial run that is still making progress.
	InitialMaxRun time.Duration
	// StallAfter is how long without progress makes an initial run stalled.
	StallAfter time.Duration
	// MissedFactor multiplies Interval to give the MISSED threshold.
	MissedFactor float64
	// EscrowVerifyGrace is how long a new escrow may stay unverified.
	EscrowVerifyGrace time.Duration
	// EscrowRecheck is how old a verification may get before it is overdue.
	EscrowRecheck time.Duration
	// MaintenanceDeferralAlarmAfter is the deferral count that means maintenance is starved; <= 0 disables.
	MaintenanceDeferralAlarmAfter int
	// Interval is the schedule interval; zero means the policy has no schedule (AttrNoSchedule, MISSED never fires).
	Interval time.Duration
	// WindowSlack is added to MissedFactor x Interval.
	WindowSlack time.Duration
	// OutdatedIsAlarm makes OUTDATED an alarm (notification_overrides.outdated_alarm).
	OutdatedIsAlarm bool
}

// DefaultThresholds returns the §3.6 sla defaults: stale 72 h, unseen 168 h,
// first-backup grace 72 h, seed extension off, partial tolerance 20 files or
// 0.5 %, one failure alarms, max run 12 h, initial max run 168 h, stall 180
// min, missed factor 2.0, escrow grace 24 h, escrow recheck 30 d, maintenance
// deferral alarm after 10, schedule interval 24 h.
func DefaultThresholds() Thresholds {
	const hour = time.Hour
	return Thresholds{
		StaleAfter:                    72 * hour,
		UnseenAfter:                   168 * hour,
		FirstBackupGrace:              72 * hour,
		SeedExtension:                 0,
		MaxFailedFiles:                20,
		MaxFailedPct:                  0.5,
		ConsecutiveFailuresToAlarm:    1,
		MaxRun:                        12 * hour,
		InitialMaxRun:                 168 * hour,
		StallAfter:                    180 * time.Minute,
		MissedFactor:                  2.0,
		EscrowVerifyGrace:             24 * hour,
		EscrowRecheck:                 30 * 24 * hour,
		MaintenanceDeferralAlarmAfter: 10,
		Interval:                      24 * hour,
	}
}

// DeviceFacts is everything Classify needs to know about one device, as the
// loader read it at one instant. Zero values mean NULL. The engine reads no
// store and no clock.
type DeviceFacts struct {
	// ID, CustomerID, Hostname and DeviceClass identify the device. DeviceClass does not influence state (B47).
	ID, CustomerID, Hostname, DeviceClass string

	// EnrolledAt, EnrollConfirmedAt and FirstBackupDueBy come from devices; adoption by new hardware resets them.
	EnrolledAt, EnrollConfirmedAt, FirstBackupDueBy Millis
	// RetiredAt, PausedAt and PausedUntil are the console-owned lifecycle fields.
	RetiredAt, PausedAt, PausedUntil Millis
	// PauseReason is the operator's pause note.
	PauseReason string
	// IdentityConflictAt is set while a cloned identity is unresolved.
	IdentityConflictAt Millis

	// LastSeen is device_presence.received_at (server clock); zero falls back to EnrolledAt.
	LastSeen Millis
	// ServiceState is device_presence.service_state ("deferred" matters here).
	ServiceState string
	// ProgressAdvancedAt is when the open run last made progress.
	ProgressAdvancedAt Millis
	// ClockOffset is the device clock minus the server clock; informational only.
	ClockOffset time.Duration

	// LastSuccess is the newest backup run with outcome success. A partial that passes tolerance is
	// derived by the engine from LastBackup (B17, B22).
	LastSuccess Millis
	// LastBackup is the newest finished backup run; nil when there is none.
	LastBackup *RunFacts
	// OpenRun is the newest open (unfinished) backup run; nil when none.
	OpenRun *RunFacts
	// LastAttemptStart is when the newest backup attempt started.
	LastAttemptStart Millis
	// ConsecutiveFailures counts failed or abandoned runs since the last success; neutral outcomes do not count.
	ConsecutiveFailures int

	// AgentVersion is what the agent reports; ExpectedVersion is the target or newest release on its ring ("" = none).
	AgentVersion, ExpectedVersion string

	// Escrow is the active escrow row; nil when none exists.
	Escrow *EscrowFacts
	// CustomerKeyCustody is the customer's default custody ("escrowed" or "customer_held"; empty means escrowed).
	CustomerKeyCustody string
	// Maintenance is the maintenance picture; nil when maintenance does not apply (customer-held keys).
	Maintenance *MaintFacts

	// PolicyVersionApplied and PolicyVersion drive the policy-drift attribute.
	PolicyVersionApplied, PolicyVersion int
	// HasSchedule is the loader's note that the effective policy has a schedule. The engine uses
	// Thresholds.Interval alone (zero means no schedule); this field is informational.
	HasSchedule bool
	// Ack is the active acknowledgement or snooze; nil when none.
	Ack *AckFacts

	// Thresholds is the merged policy sla + schedule for this device.
	Thresholds Thresholds

	// Complete is facts_complete: false when any query for this device failed, timed out or was inconsistent.
	Complete bool
	// LoadErrorClass names why the facts are incomplete; carried to the UI, never interpreted.
	LoadErrorClass string
}

// CustomerFacts is the customer row the rollup needs.
type CustomerFacts struct {
	// ID and Name identify the customer.
	ID, Name string
	// State is "active", "suspended" or "archived".
	State string
	// Expected marks a customer that must have devices (§4.5 MISSING).
	Expected bool
	// OnboardingUntil suppresses MISSING until then.
	OnboardingUntil Millis
	// DisplayTZ is the customer's IANA display zone; the engine does not use it (the ten-day strip is M3).
	DisplayTZ string
	// KeyCustody is the customer's default key custody.
	KeyCustody string
}

// ProbeFacts is reserved for the M3/M4 console conditions of §4.6 (C01-C07,
// C10-C16): gateway, storage, SMTP and certificate probes, evaluator and
// maintenance-worker heartbeats, database and master-key health, clock
// suspicion, digest delivery, quota and the release manifest. It has no
// fields in v0 so World already carries it and signatures never change.
type ProbeFacts struct{}

// World is one complete load of the fleet, handed to Evaluate.
type World struct {
	// Devices and Customers are the loaded rows; order does not matter.
	Devices   []DeviceFacts
	Customers []CustomerFacts
	// FleetEverEnrolled is true once any device has ever enrolled (C04 versus C08).
	FleetEverEnrolled bool
	// PreviousDeviceCount is how many devices the previous load in this process saw; -1 means no previous load.
	PreviousDeviceCount int
	// LoadComplete is false when the loader itself failed; LoadErrors counts per-device load failures.
	LoadComplete bool
	LoadErrors   int
	// LoadedAt is when the facts were read.
	LoadedAt time.Time
	// Probes carries the M3/M4 probe results; unused in v0.
	Probes ProbeFacts
}
