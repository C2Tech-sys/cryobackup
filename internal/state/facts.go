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
	// DisplayTZ is the customer's IANA display zone. The engine does not load zones (that is I/O); the
	// caller resolves it and hands the *time.Location to TenDayStrip (StripFacts.CustomerZone).
	DisplayTZ string
	// KeyCustody is the customer's default key custody.
	KeyCustody string
}

// ProbeRun is what the console last learned from one periodic probe: the
// job_state row of the health-prober (last_status, last_error_class) plus its
// in-memory ring. The zero value is a probe that has never reported, which the
// engine never reads as healthy (doc.go "Probe banners").
type ProbeRun struct {
	// LastRunAt is when the newest completed run finished; zero means the probe never completed a run.
	LastRunAt Millis
	// LastOKAt is when the newest passing run finished; zero means it never passed.
	LastOKAt Millis
	// ConsecutiveFailures is the number of failing runs since the newest pass (0 when the newest run passed).
	ConsecutiveFailures int
	// LastErrorClass is the newest failure's class, path-free; carried for the UI, never interpreted.
	LastErrorClass string
	// CertNotAfter is the leaf certificate's notAfter seen by the newest run that completed a TLS
	// handshake, for probes that dial TLS (the gateway); zero means none was recorded.
	CertNotAfter Millis
	// Unconfigured is set when the probe cannot run at all and says why, path-free ("SMTP is not
	// configured", "gateway_url is not set"). Such a probe is "not checked" with this reason, never
	// healthy, whatever the other fields say.
	Unconfigured string
}

// JobFacts is the heartbeat of a periodic console job (job_state.last_finished_at).
type JobFacts struct {
	// LastFinishedAt is when the newest run finished; zero means the job never finished a run.
	LastFinishedAt Millis
	// Period is the job's schedule; zero means the documented default of the job (§5.7).
	Period time.Duration
}

// SMTPFacts is the alert e-mail path (C03): the health-prober's connect + EHLO
// probe and the outcomes of real sends.
type SMTPFacts struct {
	// Probe is the connect + EHLO probe (no mail is sent).
	Probe ProbeRun
	// ConsecutiveSendFailures is the number of newest sends that failed in a row.
	ConsecutiveSendFailures int
	// LastSendOKAt is when an alert e-mail was last delivered; zero means never.
	LastSendOKAt Millis
	// LastTestFailed is true when the newest test send (settings page) failed.
	LastTestFailed bool
	// PendingAlerts counts notifications waiting to be delivered by e-mail.
	PendingAlerts int
}

// MaintainFacts is the maintenance worker (C07).
type MaintainFacts struct {
	// RPC is the reachability probe of the maintain RPC socket.
	RPC ProbeRun
	// Scheduler is the maintenance-scheduler job's heartbeat (job_state, written by `cryo-console
	// maintain`). Nil is "not supplied": a C07b warning, never quiet (doc.go "Not in this build").
	Scheduler *JobFacts
}

// ViewFacts is the render side of C10: the page or stream that will show this view.
type ViewFacts struct {
	// RefreshInterval is the page's refresh interval; zero means DefaultRefreshInterval.
	RefreshInterval time.Duration
	// StreamDisconnected is true when the live event stream (SSE) is known to be down.
	StreamDisconnected bool
}

// SelfBackupFacts is the console database's own backup (db-self-backup, M5).
type SelfBackupFacts struct {
	// NotInBuild marks a build that does not run db-self-backup (M5): the self-backup causes of C11
	// are then not judged. The loader sets it only while no job of that name exists (doc.go).
	NotInBuild bool
	// LastAt is when the newest self-backup was written; zero means never.
	LastAt Millis
	// VerifyFailed is true when the newest weekly tested restore failed.
	VerifyFailed bool
}

// DatabaseFacts is the console database's health (C11).
type DatabaseFacts struct {
	// IntegrityCheckedAt is when PRAGMA integrity_check last ran; zero means never.
	IntegrityCheckedAt Millis
	// IntegrityOK is the result of that check.
	IntegrityOK bool
	// MeasuredAt is when the disk and WAL figures below were measured; zero means never.
	MeasuredAt Millis
	// FreeBytes and TotalBytes describe the volume holding the database; TotalBytes <= 0 means unknown.
	FreeBytes, TotalBytes int64
	// WALBytes is the size of the write-ahead log.
	WALBytes int64
	// SelfBackup is the self-backup picture. Nil is "not supplied", a C11 cause; a build without
	// db-self-backup supplies &SelfBackupFacts{NotInBuild: true}.
	SelfBackup *SelfBackupFacts
}

// MasterKeyFacts is the console master key (C12).
type MasterKeyFacts struct {
	// Loaded is true when the master key is loaded and usable.
	Loaded bool
	// KeyID is the loaded key's id (repo_key_escrow.master_key_id of new escrows).
	KeyID string
	// EscrowKeyIDs are the distinct master_key_id values of active escrow rows.
	EscrowKeyIDs []string
}

// EnrollmentAllowed reports whether enrollment may proceed: only with a loaded
// master key, so the console never stores an unwrapped repository key. The
// enrollment handler returns 503 otherwise (§4.6 C12, §4.8 C18).
func (m MasterKeyFacts) EnrollmentAllowed() bool { return m.Loaded }

// DigestFacts is the daily digest's delivery (C14).
type DigestFacts struct {
	// NotInBuild marks a build that does not run daily-digest (M4): C14 is then not judged.
	NotInBuild bool
	// Disabled is true when the operator turned the digest off; C14 then does not apply.
	Disabled bool
	// LocalTime is digest_local_time, "15:04" on a 24-hour clock in World.DisplayZone.
	LocalTime string
	// LastSentAt is the newest digest row in notification_log; zero means never.
	LastSentAt Millis
}

// UsageSample is one usage_samples row of a storage target.
type UsageSample struct {
	At    Millis
	Bytes int64
}

// TargetUsage is one storage target's usage against its soft quota (C15).
type TargetUsage struct {
	// ID and Name identify the storage target (operator configuration, not a client name).
	ID, Name string
	// SoftQuotaBytes is storage_targets.soft_quota_bytes; zero or less means no quota is set.
	SoftQuotaBytes int64
	// UsedBytes is the newest usage; MeasuredAt is when it was measured (zero means never).
	UsedBytes  int64
	MeasuredAt Millis
	// Samples are recent usage samples for the growth projection, any order.
	Samples []UsageSample
}

// QuotaFacts is every storage target's usage.
type QuotaFacts struct {
	// NotInBuild marks a build that does not run usage-sampler's provider-side listing (M5): C15 is
	// then not judged.
	NotInBuild bool
	// WithQuota is how many storage targets have a soft quota set, as the loader counted them
	// independently of Targets. Targets carrying fewer quota targets than this is C15b: a target
	// whose usage row went missing is never read as within quota.
	WithQuota int
	Targets   []TargetUsage
}

// ReleaseFacts is the release-check job's verdict on the release manifests (C16).
type ReleaseFacts struct {
	// NotInBuild marks a build that does not run release-check (M11): C16 is then not judged.
	NotInBuild bool
	// CheckedAt is when release-check last ran; zero means never.
	CheckedAt Millis
	// Rejected is true when a manifest's signature did not verify.
	Rejected bool
}

// ProbeFacts carries the inputs of the probe-driven console conditions of
// §4.6: C01, C02, C03, C05 (the gateway certificate; the agent-listener half
// stays in internal/console/health by ADR 0013), C06, C07, C10, C11, C12, C14,
// C15 and C16. C13 and the device-derived C01 come from the devices.
//
// Wired is the loader's statement that it supplies the probe facts at all.
// While it is false Evaluate raises one warning, C00 ("self-checks are not
// wired"), so a console without a prober can never read all clear. Once it is
// true every top-level check is expected: a nil one raises its "not checked"
// warning, so a loader that forgets a pointer is loud. A non-nil check at its
// zero value means "never reported", which is never read as healthy either: it
// raises the condition or a "not checked" warning, as doc.go lists per
// condition. View is the one pointer whose nil is quiet: it describes the page
// rendering the view, and a caller with no page (the alarm evaluator, the
// digest) has none. A check whose producer is a later milestone is never nil:
// the loader supplies it with its NotInBuild marker (doc.go "Not in this build").
type ProbeFacts struct {
	// Wired is true when the loader supplies probe facts (doc.go "Probe banners").
	Wired bool
	// StartedAt is when this console process started; the reference for "has not finished its first
	// run yet". Zero means unknown, and a job that never finished is then the condition itself.
	StartedAt Millis
	// ProberInterval is the health-prober period; zero means DefaultProberInterval. A probe whose
	// newest run is older than ProbeStaleIntervals intervals is "not checked".
	ProberInterval time.Duration

	// Gateway is the gateway probe (C01) and, through CertNotAfter, the gateway certificate (C05).
	Gateway *ProbeRun
	// Storage is the storage probe through the maintain RPC (C02).
	Storage *ProbeRun
	// SMTP is the alert e-mail path (C03).
	SMTP *SMTPFacts
	// Evaluator is the alarm-evaluator job (C06).
	Evaluator *JobFacts
	// Maintain is the maintenance worker (C07).
	Maintain *MaintainFacts
	// View is the render side (C10).
	View *ViewFacts
	// Database is the console database (C11).
	Database *DatabaseFacts
	// MasterKey is the console master key (C12).
	MasterKey *MasterKeyFacts
	// Digest is the daily digest (C14).
	Digest *DigestFacts
	// Quota is storage usage against quota (C15).
	Quota *QuotaFacts
	// Release is the release manifest check (C16).
	Release *ReleaseFacts
}

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
	// Probes carries the probe-driven inputs of §4.6.
	Probes ProbeFacts
	// DisplayZone is the operator's display zone: banner text cites times in it on a 12-hour clock, and
	// the digest time is read in it. Nil means UTC. The engine never loads a zone itself.
	DisplayZone *time.Location
}
