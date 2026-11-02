package policy

// The policy document: nine sections, one JSON object each, exactly the
// field names of architecture §3.6. Every field is a plain value (no
// pointers, no omitempty) so the encoded form is complete and canonical; the
// only exceptions are the two "one of" unions in the document -
// retention.preset/retention.rules - which use omitempty so that an unused
// alternative is absent rather than null, and the tri-state
// notification_overrides.digest_include, which is a *bool because "inherit"
// is a third value that false cannot express.
//
// Zero values mean "none / unlimited / off / inherit" wherever a number or
// string is optional; each field comment says which.

// DeviceClass selects the class-dependent defaults of Default.
type DeviceClass string

// Device classes. The strings match policies.template_device_class in
// replica/schema.sql.
const (
	ClassUnknown DeviceClass = ""
	ClassLaptop  DeviceClass = "laptop"
	ClassDesktop DeviceClass = "desktop"
)

// Schedule modes (schedule.mode).
const (
	ModeInterval = "interval"
	ModeFixed    = "fixed"
)

// Metered-connection behaviours (conditions.metered).
const (
	MeteredDefer = "defer"
	MeteredAllow = "allow"
)

// Selection presets (selection.presets), architecture §3.6 and matrix r48.
const (
	PresetAllUserProfiles  = "all_user_profiles"
	PresetDocumentsDesktop = "documents_desktop"
	PresetFixedDrives      = "fixed_drives"
)

// Policy is the whole document. Its JSON form is one object per section; the
// database stores each section in its own column (replica/schema.sql, table
// policies), see Encode and Decode.
type Policy struct {
	Schedule              Schedule              `json:"schedule"`
	Conditions            Conditions            `json:"conditions"`
	Selection             Selection             `json:"selection"`
	Retention             Retention             `json:"retention"`
	Throttle              Throttle              `json:"throttle"`
	SLA                   SLA                   `json:"sla"`
	Maintenance           Maintenance           `json:"maintenance"`
	AgentPermissions      AgentPermissions      `json:"agent_permissions"`
	NotificationOverrides NotificationOverrides `json:"notification_overrides"`
}

// SectionNames lists the nine sections in document order.
var SectionNames = []string{
	"schedule", "conditions", "selection", "retention", "throttle",
	"sla", "maintenance", "agent_permissions", "notification_overrides",
}

// Schedule says when a device is due (architecture §6.1 "scheduler", matrix
// r22-r33). Retries, run-on-wake, startup delay, lock wait and the run-time
// limit are carried here but enforced by the agent, not by Due.
type Schedule struct {
	// Mode is "interval" or "fixed".
	Mode string `json:"mode"`
	// IntervalMinutes is the interval between successful runs (interval mode).
	IntervalMinutes int `json:"interval_minutes"`
	// Window is the backup-window mini-language, see ParseWindow. It gates
	// starts only; it never stops a running job.
	Window string `json:"window"`
	// FixedTimes are "HH:MM" local times (fixed mode).
	FixedTimes []string `json:"fixed_times"`
	// JitterMinutes bounds the per-slot start delay the caller applies with
	// Jitter. 0 disables jitter.
	JitterMinutes int `json:"jitter_minutes"`
	// Catchup makes any number of missed intervals produce one late run.
	Catchup bool `json:"catchup"`
	// RunOnWake triggers a due check on resume from sleep (agent behaviour).
	RunOnWake bool `json:"run_on_wake"`
	// StartupDelayS delays the first due check after service start (agent).
	StartupDelayS int `json:"startup_delay_s"`
	// Retries is the failure backoff (agent behaviour).
	Retries Retries `json:"retries"`
	// LockWaitS maps to restic --retry-lock.
	LockWaitS int `json:"lock_wait_s"`
	// MaxRuntimeHours cancels a run that exceeds it (agent behaviour).
	MaxRuntimeHours int `json:"max_runtime_hours"`
	// SkipIfRunning refuses a second concurrent restic job per device.
	SkipIfRunning bool `json:"skip_if_running"`
}

// Retries is the exponential backoff after a failed run: the delay starts at
// BaseS, doubles per consecutive failure and is capped at CapS.
type Retries struct {
	Max   int `json:"max"`
	BaseS int `json:"base_s"`
	CapS  int `json:"cap_s"`
}

// Conditions delay a due start; they never cancel it (matrix r35-r37). The
// agent evaluates them.
type Conditions struct {
	// BatteryFloorPct blocks starts below this charge unless on AC; 0 = no floor.
	BatteryFloorPct int `json:"battery_floor_pct"`
	// Metered is "defer" or "allow".
	Metered string `json:"metered"`
	// WifiSSIDBlock lists SSIDs on which backups are deferred.
	WifiSSIDBlock []string `json:"wifi_ssid_block"`
	// VPNRequired defers starts while no VPN is up.
	VPNRequired bool `json:"vpn_required"`
	// IdleOnly defers starts while the user is active.
	IdleOnly bool `json:"idle_only"`
	// RunAnywayAfterMinutes bounds how long conditions may delay a due run;
	// 0 = never run anyway.
	RunAnywayAfterMinutes int `json:"run_anyway_after_minutes"`
}

// Selection says what is backed up and what is left out (matrix r47-r55).
type Selection struct {
	// Roots are explicit absolute Windows paths (drive-absolute or UNC).
	Roots []string `json:"roots"`
	// Presets expand to roots at run time, see Roots.
	Presets []string `json:"presets"`
	// Includes are restic --iinclude patterns (applied by the agent).
	Includes []string `json:"includes"`
	// Excludes are custom exclude patterns appended after the published
	// baseline. An entry "@name" selects the published exclusion preset
	// "name" (see ExclusionPresets) instead of being a pattern.
	Excludes []string `json:"excludes"`
	// PathFlags carries per-root flags, keyed by an explicit root.
	PathFlags map[string]PathFlags `json:"path_flags"`
	// ExcludeCloudFiles maps to --exclude-cloud-files.
	ExcludeCloudFiles bool `json:"exclude_cloud_files"`
	// ExcludeLargerThan maps to --exclude-larger-than; "" = no limit.
	ExcludeLargerThan string `json:"exclude_larger_than"`
	// VSS configures volume shadow copies.
	VSS VSS `json:"vss"`
}

// PathFlags are per-root flags (matrix r55). A root cannot be both Required
// and Optional.
type PathFlags struct {
	// Required: a missing root fails the run (the default for roots).
	Required bool `json:"required"`
	// Optional: a missing root is skipped without error.
	Optional bool `json:"optional"`
	// RequireSnapshot: fail if no VSS snapshot could be taken for this root.
	RequireSnapshot bool `json:"require_snapshot"`
	// FollowSymlinks: descend into symlinks and junctions under this root.
	FollowSymlinks bool `json:"follow_symlinks"`
}

// VSS configures --use-fs-snapshot (architecture §6.3).
type VSS struct {
	Enabled bool `json:"enabled"`
	// TimeoutS maps to -o vss.timeout.
	TimeoutS              int      `json:"timeout_s"`
	ExcludeAllMountPoints bool     `json:"exclude_all_mount_points"`
	ExcludeVolumes        []string `json:"exclude_volumes"`
}

// Retention selects the keep rules (matrix r61-r62). Exactly one of Preset or
// Rules is set; KeepWithin is required in both forms and always wins over the
// preset's own keep-within, so a snapshot flood can never push real history
// out of the keep windows (architecture §7.3 step 3).
type Retention struct {
	// Preset names an entry of RetentionPresets.
	Preset string `json:"preset,omitempty"`
	// Rules are explicit keep counts; -1 means unlimited.
	Rules *KeepCounts `json:"rules,omitempty"`
	// KeepWithin is a restic duration such as "14d" or "1y6m". Required.
	KeepWithin string `json:"keep_within"`
	// KeepTag lists tags whose snapshots are always kept.
	KeepTag []string `json:"keep_tag"`
}

// KeepCounts are the restic forget --keep-* counts of a custom rule set.
// 0 = rule unused, -1 = unlimited.
type KeepCounts struct {
	KeepLast    int `json:"keep_last"`
	KeepHourly  int `json:"keep_hourly"`
	KeepDaily   int `json:"keep_daily"`
	KeepWeekly  int `json:"keep_weekly"`
	KeepMonthly int `json:"keep_monthly"`
	KeepYearly  int `json:"keep_yearly"`
}

// Throttle holds the static transfer limits and the scheduled windows
// (matrix r27, r34), and the memory cap of the restic child (r209). Transfer
// limits are KiB/s because restic takes nothing else.
type Throttle struct {
	// Upload is the default upload limit; 0 = unlimited.
	Upload Rate `json:"upload_kibps"`
	// Download is the default download limit; 0 = unlimited.
	Download Rate `json:"download_kibps"`
	// Windows override the defaults while open; later entries win on overlap.
	Windows []ThrottleWindow `json:"windows"`
	// MemoryLimitMiB caps the restic child's committed memory through its
	// Windows job object: 0 = auto, -1 = no cap, otherwise the cap in MiB
	// (MemoryLimitMinMiB..MemoryLimitMaxMiB). See MemoryCapMiB.
	//
	// It lives in throttle, not in a section of its own, because every
	// section is a column of the policies table; a stored policy without the
	// field decodes as 0 = auto.
	MemoryLimitMiB int `json:"memory_limit_mib"`
	// GatewayDownGraceMinutes is how long a running backup may go without
	// progress while the gateway cannot be reached before the agent stops
	// restic and journals the run deferred/gateway (M1-FIX4, the gateway
	// watchdog; see GatewayDownGrace). 0 = the default,
	// GatewayDownGraceDefaultMinutes; otherwise GatewayDownGraceMinMinutes..
	// GatewayDownGraceMaxMinutes.
	//
	// It lives in throttle for the reason memory_limit_mib does: a section is
	// a column of the policies table. A stored policy written before the field
	// existed decodes as 0, which is the default.
	GatewayDownGraceMinutes int `json:"gateway_down_grace_minutes"`
}

// Gateway watchdog grace values and bounds (Throttle.GatewayDownGraceMinutes).
// The default is restic's own: each backend operation of restic 0.19.1 retries
// for up to 15 minutes before it gives up.
const (
	GatewayDownGraceDefaultMinutes = 15
	GatewayDownGraceMinMinutes     = 5
	GatewayDownGraceMaxMinutes     = 120
)

// Memory cap values and bounds (Throttle.MemoryLimitMiB).
const (
	// MemoryLimitAuto sizes the cap from the machine: half its physical
	// memory, never below MemoryLimitAutoFloorMiB.
	MemoryLimitAuto = 0
	// MemoryLimitNone runs restic without a memory cap.
	MemoryLimitNone = -1
	// MemoryLimitAutoFloorMiB is the smallest cap auto mode sets.
	MemoryLimitAutoFloorMiB = 1024
	// MemoryLimitMinMiB and MemoryLimitMaxMiB bound an explicit cap.
	MemoryLimitMinMiB = 512
	MemoryLimitMaxMiB = 1 << 20 // 1 TiB
)

// MemoryCapMiB returns the cap the agent applies to the restic child, in MiB,
// for a machine with physicalMiB of RAM (<= 0 = unknown); 0 means no cap.
//
// Why auto is the default and why it is generous: a cap that kills ordinary
// backups is worse than none. A run that hits it is a failed backup (class
// memory), and restic's memory grows with the repository index and the size
// of the tree, so a large file server legitimately needs several GiB. The cap
// is there to keep one runaway child from taking the whole machine down, not
// to budget restic. Half of physical memory leaves the user's applications the
// other half while letting every realistic backup finish; the 1 GiB floor
// keeps small machines (2 GiB and less) from capping restic below what a
// modest repository needs (the commit limit includes the page file, so a cap
// above physical memory pages rather than fails). When physical memory cannot
// be read, auto applies no cap rather than guess low. An explicit value is for
// the operator who has measured; -1 is for servers whose repository is known
// to need more than half the machine.
func (t Throttle) MemoryCapMiB(physicalMiB int64) int {
	switch v := t.MemoryLimitMiB; {
	case v > 0:
		return v
	case v != MemoryLimitAuto:
		return 0 // MemoryLimitNone; Validate rejects any other negative
	case physicalMiB <= 0:
		return 0
	}
	half := physicalMiB / 2
	if half < MemoryLimitAutoFloorMiB {
		half = MemoryLimitAutoFloorMiB
	}
	if half > MemoryLimitMaxMiB {
		half = MemoryLimitMaxMiB
	}
	return int(half)
}

// ThrottleWindow overrides the default limits while When is open.
type ThrottleWindow struct {
	// When is a window mini-language string, see ParseWindow.
	When string `json:"when"`
	Up   Rate   `json:"up"`
	Down Rate   `json:"down"`
}

// SLA holds the thresholds the console maps into the state engine
// (architecture §3.6, §4). Hours and days are integers; MissedFactor and
// PartialTolerance.MaxFailedPct are the only fractional values.
type SLA struct {
	StaleAfterHours               int              `json:"stale_after_hours"`
	UnseenAfterHours              int              `json:"unseen_after_hours"`
	FirstBackupGraceHours         int              `json:"first_backup_grace_hours"`
	SeedExtensionHours            int              `json:"seed_extension_hours"`
	PartialTolerance              PartialTolerance `json:"partial_tolerance"`
	ConsecutiveFailuresToAlarm    int              `json:"consecutive_failures_to_alarm"`
	MaxRunHours                   int              `json:"max_run_hours"`
	InitialMaxRunHours            int              `json:"initial_max_run_hours"`
	StallAfterMinutes             int              `json:"stall_after_minutes"`
	MissedFactor                  float64          `json:"missed_factor"`
	EscrowVerifyGraceHours        int              `json:"escrow_verify_grace_hours"`
	EscrowRecheckDays             int              `json:"escrow_recheck_days"`
	MaintenanceDeferralAlarmAfter int              `json:"maintenance_deferral_alarm_after"`
}

// PartialTolerance decides when a partial backup (restic exit 3) counts as a
// success (architecture §3.6, matrix r152).
type PartialTolerance struct {
	MaxFailedFiles int     `json:"max_failed_files"`
	MaxFailedPct   float64 `json:"max_failed_pct"`
}

// Maintenance is the server-side upkeep cadence (architecture §7.3-7.4).
type Maintenance struct {
	ForgetEveryDays int `json:"forget_every_days"`
	PruneEveryDays  int `json:"prune_every_days"`
	// MaxUnused is "<n>%", "unlimited" or a size, as restic prune --max-unused.
	MaxUnused           string `json:"max_unused"`
	RepackCacheableOnly bool   `json:"repack_cacheable_only"`
	CheckEveryDays      int    `json:"check_every_days"`
	// ReadDataSubset is "n/t", "x%" or a size, as restic check --read-data-subset.
	ReadDataSubset string `json:"read_data_subset"`
	// WindowHint is a window mini-language string for maintenance; "" derives
	// the complement of the device's backup window.
	WindowHint string `json:"window_hint"`
}

// AgentPermissions says what a local user may do from the tray.
type AgentPermissions struct {
	AllowLocalBackupNow bool `json:"allow_local_backup_now"`
	AllowLocalPause     bool `json:"allow_local_pause"`
	AllowLocalRestore   bool `json:"allow_local_restore"`
}

// NotificationOverrides override the global > customer > device inherit chain
// for alerting (architecture §3.6).
type NotificationOverrides struct {
	OutdatedAlarm bool `json:"outdated_alarm"`
	// FailureThreshold is the consecutive failures before alerting; 0 = inherit.
	FailureThreshold int `json:"failure_threshold"`
	// DigestInclude includes the device in the digest; nil = inherit.
	DigestInclude *bool `json:"digest_include"`
}

// Default returns the proposed defaults of architecture §3.6. The numbers are
// proposals the init wizard confirms and the pilot tunes, not promises. Only
// battery_floor_pct depends on class (laptop 30, otherwise 0 = no floor).
// Default(...).Validate() is always nil. All slices are non-nil so the
// encoded form is stable.
func Default(class DeviceClass) Policy {
	battery := 0
	if class == ClassLaptop {
		battery = 30
	}
	p := Policy{
		Schedule: Schedule{
			Mode:            ModeInterval,
			IntervalMinutes: 1440,
			Window:          "*/0-24",
			JitterMinutes:   30,
			Catchup:         true,
			RunOnWake:       true,
			StartupDelayS:   120,
			Retries:         Retries{Max: 3, BaseS: 300, CapS: 3600},
			LockWaitS:       120,
			MaxRuntimeHours: 12,
			SkipIfRunning:   true,
		},
		Conditions: Conditions{
			BatteryFloorPct:       battery,
			Metered:               MeteredDefer,
			RunAnywayAfterMinutes: 240,
		},
		Selection: Selection{
			Presets:           []string{PresetAllUserProfiles},
			ExcludeCloudFiles: true,
			VSS:               VSS{Enabled: true, TimeoutS: 120},
		},
		Retention: Retention{Preset: "1_year_gfs", KeepWithin: RetentionPresets["1_year_gfs"].KeepWithin},
		SLA: SLA{
			StaleAfterHours:               72,
			UnseenAfterHours:              168,
			FirstBackupGraceHours:         72,
			SeedExtensionHours:            0,
			PartialTolerance:              PartialTolerance{MaxFailedFiles: 20, MaxFailedPct: 0.5},
			ConsecutiveFailuresToAlarm:    1,
			MaxRunHours:                   12,
			InitialMaxRunHours:            168,
			StallAfterMinutes:             180,
			MissedFactor:                  2.0,
			EscrowVerifyGraceHours:        24,
			EscrowRecheckDays:             30,
			MaintenanceDeferralAlarmAfter: 10,
		},
		Maintenance: Maintenance{
			ForgetEveryDays:     7,
			PruneEveryDays:      30, // monthly: architecture §7.4 "no minimum duration" row
			MaxUnused:           "5%",
			RepackCacheableOnly: false,
			CheckEveryDays:      30,
			ReadDataSubset:      "5%",
		},
		AgentPermissions: AgentPermissions{AllowLocalBackupNow: true},
	}
	p.Throttle.GatewayDownGraceMinutes = GatewayDownGraceDefaultMinutes
	return p.normalized()
}

// normalized returns p with every nil slice and map replaced by an empty one,
// so that "no entries" always encodes as [] / {} and never as null.
func (p Policy) normalized() Policy {
	if p.Schedule.FixedTimes == nil {
		p.Schedule.FixedTimes = []string{}
	}
	if p.Conditions.WifiSSIDBlock == nil {
		p.Conditions.WifiSSIDBlock = []string{}
	}
	s := &p.Selection
	if s.Roots == nil {
		s.Roots = []string{}
	}
	if s.Presets == nil {
		s.Presets = []string{}
	}
	if s.Includes == nil {
		s.Includes = []string{}
	}
	if s.Excludes == nil {
		s.Excludes = []string{}
	}
	if s.PathFlags == nil {
		s.PathFlags = map[string]PathFlags{}
	}
	if s.VSS.ExcludeVolumes == nil {
		s.VSS.ExcludeVolumes = []string{}
	}
	if p.Retention.KeepTag == nil {
		p.Retention.KeepTag = []string{}
	}
	if p.Throttle.Windows == nil {
		p.Throttle.Windows = []ThrottleWindow{}
	}
	return p
}
