package policy

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Problem is one validation failure: Path is the dotted JSON path of the
// offending field (for example "retention.keep_within" or
// "throttle.windows[0].when") and Message says what is wrong.
type Problem struct {
	Path    string
	Message string
}

// ValidationError is the error Validate returns: every problem found, sorted
// by path (then message), so a UI can show them all at once and tests can
// compare them. Use errors.As to reach it.
type ValidationError struct {
	Problems []Problem
}

// Error lists the problems, one per line, as "path: message".
func (e *ValidationError) Error() string {
	lines := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		lines[i] = p.Path + ": " + p.Message
	}
	return strings.Join(lines, "\n")
}

type collector struct{ probs []Problem }

func (c *collector) add(path, format string, args ...any) {
	c.probs = append(c.probs, Problem{Path: path, Message: fmt.Sprintf(format, args...)})
}

// err returns nil when nothing was collected, else a sorted *ValidationError.
func (c *collector) err() error {
	if len(c.probs) == 0 {
		return nil
	}
	sort.SliceStable(c.probs, func(i, j int) bool {
		if c.probs[i].Path != c.probs[j].Path {
			return c.probs[i].Path < c.probs[j].Path
		}
		return c.probs[i].Message < c.probs[j].Message
	})
	return &ValidationError{Problems: c.probs}
}

func idx(path string, i int) string { return path + "[" + strconv.Itoa(i) + "]" }

// inRange records a problem unless lo <= v <= hi.
func (c *collector) inRange(path string, v, lo, hi int) {
	if v < lo || v > hi {
		c.add(path, "must be between %d and %d, got %d", lo, hi, v)
	}
}

// atLeast records a problem unless v >= lo.
func (c *collector) atLeast(path string, v, lo int) {
	if v < lo {
		c.add(path, "must be >= %d, got %d", lo, v)
	}
}

var (
	hhmmRE     = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	driveRE    = regexp.MustCompile(`^[A-Za-z]:\\`)
	uncRE      = regexp.MustCompile(`^\\\\[^\\/]+\\[^\\/]+`)
	sizeRE     = regexp.MustCompile(`^[0-9]+[kKmMgGtT]?$`)
	percentRE  = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?%$`)
	fractionRE = regexp.MustCompile(`^([0-9]+)/([0-9]+)$`)
)

// isAbsWindowsPath reports whether p is a drive-absolute (C:\...) or UNC
// (\\server\share...) path. Forward slashes are not accepted.
func isAbsWindowsPath(p string) bool { return driveRE.MatchString(p) || uncRE.MatchString(p) }

// Validate checks every rule of the policy document (architecture §3.6) and
// returns nil or a *ValidationError holding every problem, sorted by path.
// The numeric ranges are the bounds the agent and console can honour; they are
// not product promises.
func (p Policy) Validate() error {
	c := &collector{}
	checkSchedule(c, p.Schedule)
	checkConditions(c, p.Conditions)
	checkSelection(c, p.Selection)
	checkRetention(c, p.Retention)
	checkThrottle(c, p.Throttle)
	checkSLA(c, p.SLA)
	checkMaintenance(c, p.Maintenance)
	if p.NotificationOverrides.FailureThreshold < 0 {
		c.add("notification_overrides.failure_threshold", "must be >= 1 when set (0 = inherit), got %d", p.NotificationOverrides.FailureThreshold)
	}
	// agent_permissions and the remaining notification_overrides fields are
	// booleans: nothing further to check.
	return c.err()
}

func checkWindow(c *collector, path, s string) {
	if _, err := ParseWindow(s); err != nil {
		c.add(path, "invalid window %s: %v", quote(s), err)
	}
}

func checkSchedule(c *collector, s Schedule) {
	switch s.Mode {
	case ModeInterval:
		c.inRange("schedule.interval_minutes", s.IntervalMinutes, 15, 10080)
	case ModeFixed:
		if len(s.FixedTimes) == 0 {
			c.add("schedule.fixed_times", "must list at least one HH:MM time in fixed mode")
		}
	default:
		c.add("schedule.mode", "must be %q or %q, got %s", ModeInterval, ModeFixed, quote(s.Mode))
	}
	for i, t := range s.FixedTimes {
		if !hhmmRE.MatchString(t) {
			c.add(idx("schedule.fixed_times", i), "must be a 24-hour HH:MM time, got %s", quote(t))
		}
	}
	if s.Window == "" {
		c.add("schedule.window", "is required (use */0-24 for always)")
	} else {
		checkWindow(c, "schedule.window", s.Window)
	}
	c.inRange("schedule.jitter_minutes", s.JitterMinutes, 0, 240)
	c.inRange("schedule.startup_delay_s", s.StartupDelayS, 0, 3600)
	c.inRange("schedule.retries.max", s.Retries.Max, 0, 20)
	c.atLeast("schedule.retries.base_s", s.Retries.BaseS, 30)
	if s.Retries.CapS < s.Retries.BaseS {
		c.add("schedule.retries.cap_s", "must be >= retries.base_s (%d), got %d", s.Retries.BaseS, s.Retries.CapS)
	}
	c.inRange("schedule.lock_wait_s", s.LockWaitS, 0, 3600)
	c.inRange("schedule.max_runtime_hours", s.MaxRuntimeHours, 1, 168)
}

func checkConditions(c *collector, k Conditions) {
	c.inRange("conditions.battery_floor_pct", k.BatteryFloorPct, 0, 100)
	if k.Metered != MeteredDefer && k.Metered != MeteredAllow {
		c.add("conditions.metered", "must be %q or %q, got %s", MeteredDefer, MeteredAllow, quote(k.Metered))
	}
	c.inRange("conditions.run_anyway_after_minutes", k.RunAnywayAfterMinutes, 0, 10080)
	for i, s := range k.WifiSSIDBlock {
		if strings.TrimSpace(s) == "" {
			c.add(idx("conditions.wifi_ssid_block", i), "must not be empty")
		}
	}
}

// ExclusionPresetRef reports whether an excludes entry selects a published
// exclusion preset ("@name") and returns the name.
func ExclusionPresetRef(entry string) (name string, ok bool) {
	if strings.HasPrefix(entry, "@") {
		return entry[1:], true
	}
	return "", false
}

func checkPattern(c *collector, path, s string) {
	switch {
	case strings.TrimSpace(s) == "":
		c.add(path, "must not be empty")
	case strings.ContainsAny(s, "\r\n"):
		c.add(path, "must be a single line")
	case strings.HasPrefix(strings.TrimSpace(s), "#"):
		c.add(path, "must not start with '#' (it would be written as a comment)")
	}
}

func normRoot(r string) string {
	if len(r) > 3 && strings.HasSuffix(r, `\`) {
		r = strings.TrimRight(r, `\`)
		if len(r) == 2 && r[1] == ':' {
			r += `\`
		}
	}
	return r
}

func checkSelection(c *collector, s Selection) {
	if len(s.Roots) == 0 && len(s.Presets) == 0 {
		c.add("selection", "needs at least one root or preset")
	}
	listed := map[string]bool{}
	for i, r := range s.Roots {
		if !isAbsWindowsPath(r) {
			c.add(idx("selection.roots", i), `must be an absolute Windows path (C:\... or \\server\share), got %s`, quote(r))
			continue
		}
		listed[strings.ToLower(normRoot(r))] = true
	}
	for i, name := range s.Presets {
		switch name {
		case PresetAllUserProfiles, PresetDocumentsDesktop, PresetFixedDrives:
		default:
			c.add(idx("selection.presets", i), "unknown preset %s (known: %s, %s, %s)", quote(name),
				PresetAllUserProfiles, PresetDocumentsDesktop, PresetFixedDrives)
		}
	}
	for i, in := range s.Includes {
		checkPattern(c, idx("selection.includes", i), in)
	}
	for i, ex := range s.Excludes {
		path := idx("selection.excludes", i)
		if name, ok := ExclusionPresetRef(ex); ok {
			if _, known := ExclusionPresets[name]; !known {
				c.add(path, "unknown exclusion preset %s (known: %s)", quote(ex), strings.Join(exclusionPresetNames(), ", "))
			}
			continue
		}
		checkPattern(c, path, ex)
	}
	for key, f := range s.PathFlags {
		path := "selection.path_flags[" + key + "]"
		if !listed[strings.ToLower(normRoot(key))] {
			c.add(path, "key must be one of the listed roots")
		}
		if f.Required && f.Optional {
			c.add(path, "a root cannot be both required and optional")
		}
	}
	c.inRange("selection.vss.timeout_s", s.VSS.TimeoutS, 10, 3600)
	for i, v := range s.VSS.ExcludeVolumes {
		if strings.TrimSpace(v) == "" {
			c.add(idx("selection.vss.exclude_volumes", i), "must not be empty")
		}
	}
	if s.ExcludeLargerThan != "" && !sizeRE.MatchString(s.ExcludeLargerThan) {
		c.add("selection.exclude_larger_than", "must be a size in bytes or with a k, m, g or t suffix, got %s", quote(s.ExcludeLargerThan))
	}
}

func checkThrottle(c *collector, t Throttle) {
	c.atLeast("throttle.upload_kibps", int(t.Upload), 0)
	c.atLeast("throttle.download_kibps", int(t.Download), 0)
	for i, w := range t.Windows {
		base := idx("throttle.windows", i)
		if _, err := ParseWindow(w.When); err != nil {
			c.add(base+".when", "invalid window %s: %v", quote(w.When), err)
		}
		c.atLeast(base+".up", int(w.Up), 0)
		c.atLeast(base+".down", int(w.Down), 0)
	}
	if m := t.MemoryLimitMiB; m != MemoryLimitAuto && m != MemoryLimitNone && (m < MemoryLimitMinMiB || m > MemoryLimitMaxMiB) {
		c.add("throttle.memory_limit_mib", "must be 0 (auto), -1 (no cap) or between %d and %d MiB, got %d", MemoryLimitMinMiB, MemoryLimitMaxMiB, m)
	}
	if g := t.GatewayDownGraceMinutes; g != 0 && (g < GatewayDownGraceMinMinutes || g > GatewayDownGraceMaxMinutes) {
		c.add("throttle.gateway_down_grace_minutes", "must be 0 (the default, %d) or between %d and %d minutes, got %d",
			GatewayDownGraceDefaultMinutes, GatewayDownGraceMinMinutes, GatewayDownGraceMaxMinutes, g)
	}
}

func checkSLA(c *collector, s SLA) {
	nonNeg := []struct {
		name string
		v    int
	}{
		{"first_backup_grace_hours", s.FirstBackupGraceHours},
		{"seed_extension_hours", s.SeedExtensionHours},
		{"partial_tolerance.max_failed_files", s.PartialTolerance.MaxFailedFiles},
		{"max_run_hours", s.MaxRunHours},
		{"stall_after_minutes", s.StallAfterMinutes},
		{"escrow_verify_grace_hours", s.EscrowVerifyGraceHours},
		{"escrow_recheck_days", s.EscrowRecheckDays},
		{"maintenance_deferral_alarm_after", s.MaintenanceDeferralAlarmAfter},
	}
	for _, f := range nonNeg {
		c.atLeast("sla."+f.name, f.v, 0)
	}
	c.atLeast("sla.stale_after_hours", s.StaleAfterHours, 1)
	if s.UnseenAfterHours < s.StaleAfterHours {
		c.add("sla.unseen_after_hours", "must be >= stale_after_hours (%d): a device must be stale before it can be unseen, got %d", s.StaleAfterHours, s.UnseenAfterHours)
	}
	pct := s.PartialTolerance.MaxFailedPct
	if math.IsNaN(pct) || pct < 0 || pct > 100 {
		c.add("sla.partial_tolerance.max_failed_pct", "must be between 0 and 100, got %v", pct)
	}
	c.atLeast("sla.consecutive_failures_to_alarm", s.ConsecutiveFailuresToAlarm, 1)
	if s.InitialMaxRunHours < s.MaxRunHours {
		c.add("sla.initial_max_run_hours", "must be >= max_run_hours (%d): the first backup is allowed longer, got %d", s.MaxRunHours, s.InitialMaxRunHours)
	}
	if math.IsNaN(s.MissedFactor) || s.MissedFactor < 1 {
		c.add("sla.missed_factor", "must be >= 1, got %v", s.MissedFactor)
	}
}

func checkMaintenance(c *collector, m Maintenance) {
	c.atLeast("maintenance.forget_every_days", m.ForgetEveryDays, 1)
	c.atLeast("maintenance.prune_every_days", m.PruneEveryDays, 1)
	c.atLeast("maintenance.check_every_days", m.CheckEveryDays, 1)
	if !validMaxUnused(m.MaxUnused) {
		c.add("maintenance.max_unused", `must be "<n>%%" (0-100), "unlimited" or a size such as 10g, got %s`, quote(m.MaxUnused))
	}
	if !validReadDataSubset(m.ReadDataSubset) {
		c.add("maintenance.read_data_subset", `must be "n/t" (1 <= n <= t), "x%%" (0 < x <= 100) or a size such as 500m, got %s`, quote(m.ReadDataSubset))
	}
	if m.WindowHint != "" {
		checkWindow(c, "maintenance.window_hint", m.WindowHint)
	}
}

func percentValue(s string) (float64, bool) {
	if !percentRE.MatchString(s) {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
	return v, err == nil
}

func validMaxUnused(s string) bool {
	if s == "unlimited" || sizeRE.MatchString(s) {
		return true
	}
	v, ok := percentValue(s)
	return ok && v <= 100
}

func validReadDataSubset(s string) bool {
	if sizeRE.MatchString(s) {
		return true
	}
	if v, ok := percentValue(s); ok {
		return v > 0 && v <= 100
	}
	if m := fractionRE.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		t, _ := strconv.Atoi(m[2])
		return n >= 1 && t >= 1 && n <= t
	}
	return false
}
