package policy

import (
	"regexp"
	"sort"
	"strings"
)

// Rules is a resolved retention rule set: the restic forget --keep-* flags.
// A count of 0 means the rule is unused; -1 means unlimited (restic accepts
// -1 for every --keep-* count). KeepWithin is always set after Resolve.
type Rules struct {
	KeepLast, KeepHourly, KeepDaily, KeepWeekly, KeepMonthly, KeepYearly int
	// KeepWithin is a restic duration ("14d", "1y6m"). The rule is
	// OR-combined with the counts, and is always passed to forget so a
	// snapshot flood cannot push real history out (architecture §7.3 step 3).
	KeepWithin string
	// KeepTags are tags whose snapshots are always kept.
	KeepTags []string
}

// RetentionPresets are the named retention presets (matrix r62). Their
// numbers are proposals (architecture §3.6): the pilot tunes them. Treat the
// map as read-only; Resolve returns copies.
//
//	30_days      everything from the last 30 days, nothing older
//	90_days      everything from the last 90 days, nothing older
//	1_year_gfs   14 days of everything, then 30 daily, 13 weekly, 12 monthly
//	7_years      14 days of everything, then 30 daily, 13 weekly, 24 monthly, 7 yearly
var RetentionPresets = map[string]Rules{
	"30_days":    {KeepWithin: "30d"},
	"90_days":    {KeepWithin: "90d"},
	"1_year_gfs": {KeepDaily: 30, KeepWeekly: 13, KeepMonthly: 12, KeepWithin: "14d"},
	"7_years":    {KeepDaily: 30, KeepWeekly: 13, KeepMonthly: 24, KeepYearly: 7, KeepWithin: "14d"},
}

// PresetNames returns the retention preset names, sorted.
func PresetNames() []string {
	names := make([]string, 0, len(RetentionPresets))
	for n := range RetentionPresets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

var keepWithinRE = regexp.MustCompile(`^([0-9]+y)?([0-9]+m)?([0-9]+d)?([0-9]+h)?$`)

// validKeepWithin returns "" if s is a usable restic keep-within duration,
// otherwise the reason it is not.
func validKeepWithin(s string) string {
	switch {
	case s == "":
		return "is required: every retention form must carry keep_within (architecture §7.3)"
	case !keepWithinRE.MatchString(s):
		return "must look like 1y2m3d4h (each part optional, in that order): " + quote(s)
	case strings.Trim(s, "0ymdh") == "":
		// only zero components such as "0d": it would keep nothing
		return "must be greater than zero: " + quote(s)
	}
	return ""
}

func quote(s string) string { return `"` + s + `"` }

// checkRetention appends every problem of r to c.
func checkRetention(c *collector, r Retention) {
	switch {
	case r.Preset != "" && r.Rules != nil:
		c.add("retention", "set exactly one of preset or rules, not both")
	case r.Preset == "" && r.Rules == nil:
		c.add("retention", "set exactly one of preset or rules")
	}
	if r.Preset != "" {
		if _, ok := RetentionPresets[r.Preset]; !ok {
			c.add("retention.preset", "unknown preset %s (known: %s)", quote(r.Preset), strings.Join(PresetNames(), ", "))
		}
	}
	if k := r.Rules; k != nil {
		for _, f := range []struct {
			name string
			v    int
		}{
			{"keep_last", k.KeepLast}, {"keep_hourly", k.KeepHourly}, {"keep_daily", k.KeepDaily},
			{"keep_weekly", k.KeepWeekly}, {"keep_monthly", k.KeepMonthly}, {"keep_yearly", k.KeepYearly},
		} {
			if f.v < -1 {
				c.add("retention.rules."+f.name, "must be >= 0 (or -1 for unlimited), got %d", f.v)
			}
		}
	}
	if why := validKeepWithin(r.KeepWithin); why != "" {
		c.add("retention.keep_within", "%s", why)
	}
	for i, t := range r.KeepTag {
		switch {
		case strings.TrimSpace(t) == "":
			c.add(idx("retention.keep_tag", i), "must not be empty")
		case strings.Contains(t, ","):
			c.add(idx("retention.keep_tag", i), "must not contain a comma (restic splits tag lists on commas): %s", quote(t))
		}
	}
}

// Resolve turns a retention section into the rule set the maintain worker maps
// onto restic forget: the preset's counts or the explicit rules, the section's
// keep_within (which always wins over the preset's own) and the keep_tag
// entries. It returns a *ValidationError if the section is invalid, in
// particular if keep_within is missing.
func Resolve(r Retention) (Rules, error) {
	c := &collector{}
	checkRetention(c, r)
	if err := c.err(); err != nil {
		return Rules{}, err
	}
	var out Rules
	if r.Preset != "" {
		out = RetentionPresets[r.Preset]
	} else {
		k := r.Rules
		out = Rules{
			KeepLast: k.KeepLast, KeepHourly: k.KeepHourly, KeepDaily: k.KeepDaily,
			KeepWeekly: k.KeepWeekly, KeepMonthly: k.KeepMonthly, KeepYearly: k.KeepYearly,
		}
	}
	out.KeepWithin = r.KeepWithin
	out.KeepTags = append([]string(nil), r.KeepTag...)
	return out, nil
}
