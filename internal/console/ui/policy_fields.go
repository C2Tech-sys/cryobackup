package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/policy"
)

// The policy field catalogue (ticket M2-W2A). One table drives the policy
// editor, the per-device override form and the labels of the effective-policy
// view, so a field exists in exactly one place: its document path, its label,
// the control that edits it and how a posted value becomes JSON. Nothing here
// parses the mini-languages: windows, rates and the throttle form go to the
// policy package's own parsers, and the parser's error is what the form shows.

type pkind string

const (
	pInt     pkind = "int"
	pFloat   pkind = "float"
	pBool    pkind = "bool"
	pText    pkind = "text"
	pSelect  pkind = "select"
	pWindow  pkind = "window"
	pLines   pkind = "lines"
	pRate    pkind = "rate"
	pTri     pkind = "tri"
	pPresets pkind = "presets"
	pExcl    pkind = "excl"
	pRet     pkind = "ret"
	pTW      pkind = "tw"
)

type pOpt struct{ Value, Label string }

type pfield struct {
	// Path is the dotted document path ("schedule.interval_minutes"); for the
	// composite retention control it is "retention".
	Path  string
	Label string
	Help  string
	Unit  string
	Kind  pkind
	Opts  []pOpt
	// Adv puts the field behind its section's "Advanced" disclosure.
	Adv bool
	// Optional lets a text or window field be empty.
	Optional    bool
	Placeholder string
}

type psection struct {
	Key, Title, Lead string
	Fields           []pfield
}

func (f pfield) name() string { return "f." + f.Path }

var (
	optModes = []pOpt{{policy.ModeInterval, "Every N minutes (interval)"}, {policy.ModeFixed, "At fixed times"}}
	optMeter = []pOpt{{policy.MeteredDefer, "Wait for an unmetered connection"}, {policy.MeteredAllow, "Back up on metered connections"}}
)

var presetLabels = []pOpt{
	{policy.PresetAllUserProfiles, "All user profiles"},
	{policy.PresetDocumentsDesktop, "Documents and Desktop"},
	{policy.PresetFixedDrives, "Fixed drives"},
}

var exclusionLabels = []pOpt{
	{"browser_caches", "More browser caches (service workers, shader and start-up caches)"},
	{"vm_images", "Virtual disks and disc images (.vhd, .vhdx, .vmdk, .iso)"},
	{"installers", "Installers in Downloads (.msi, .exe)"},
	{"dev_caches", "Package and build caches (node_modules, __pycache__, .gradle, .m2, .nuget, .cargo)"},
}

var policySections = []psection{
	{Key: "schedule", Title: "Schedule and backup window", Lead: "When a backup is due, and the hours in which a run may start. Hours are device-local time; a window decides when a run may start and never stops one that is already going.", Fields: []pfield{
		{Path: "schedule.mode", Label: "Schedule type", Kind: pSelect, Opts: optModes},
		{Path: "schedule.interval_minutes", Label: "Interval", Unit: "minutes", Kind: pInt, Help: "Used in interval mode, 15 to 10080. 1440 is once a day."},
		{Path: "schedule.window", Label: "Backup window", Kind: pWindow, Help: "Example: Mon-Fri/8-18;Sat,Sun/0-24. Use */0-24 for always."},
		{Path: "schedule.fixed_times", Label: "Fixed times", Kind: pLines, Help: "One 24-hour HH:MM per line. Used in fixed mode."},
		{Path: "schedule.catchup", Label: "Catch up after missed runs", Kind: pBool, Help: "Any number of missed intervals produces one late run."},
		{Path: "schedule.run_on_wake", Label: "Check when the device wakes or logs on", Kind: pBool},
		{Path: "schedule.skip_if_running", Label: "Never run two backups at once", Kind: pBool, Adv: true},
		{Path: "schedule.jitter_minutes", Label: "Start jitter", Unit: "minutes", Kind: pInt, Adv: true, Help: "0 to 240. Spreads starts so a customer's devices do not all begin together."},
		{Path: "schedule.startup_delay_s", Label: "Delay after service start", Unit: "seconds", Kind: pInt, Adv: true, Help: "0 to 3600."},
		{Path: "schedule.retries.max", Label: "Retries after a failure", Kind: pInt, Adv: true, Help: "0 to 20."},
		{Path: "schedule.retries.base_s", Label: "First retry delay", Unit: "seconds", Kind: pInt, Adv: true, Help: "At least 30; doubles per consecutive failure."},
		{Path: "schedule.retries.cap_s", Label: "Longest retry delay", Unit: "seconds", Kind: pInt, Adv: true},
		{Path: "schedule.lock_wait_s", Label: "Wait for a repository lock", Unit: "seconds", Kind: pInt, Adv: true, Help: "0 to 3600. A longer exclusive lock defers the run."},
		{Path: "schedule.max_runtime_hours", Label: "Cancel a run after", Unit: "hours", Kind: pInt, Adv: true, Help: "1 to 168."},
	}},
	{Key: "conditions", Title: "Start conditions", Lead: "Conditions delay a due run and record why; after the run-anyway time it starts regardless.", Fields: []pfield{
		{Path: "conditions.battery_floor_pct", Label: "Battery floor", Unit: "percent", Kind: pInt, Help: "0 means no floor; on AC power the floor does not apply."},
		{Path: "conditions.metered", Label: "Metered connections", Kind: pSelect, Opts: optMeter},
		{Path: "conditions.vpn_required", Label: "Require a VPN", Kind: pBool},
		{Path: "conditions.idle_only", Label: "Only while the user is idle", Kind: pBool},
		{Path: "conditions.run_anyway_after_minutes", Label: "Run anyway after", Unit: "minutes of delay", Kind: pInt, Help: "0 means conditions may delay a run forever."},
		{Path: "conditions.wifi_ssid_block", Label: "Wi-Fi networks to avoid", Kind: pLines, Help: "One network name per line."},
	}},
	{Key: "selection", Title: "What is backed up", Lead: "Presets expand to folders on each device; explicit folders replace them. The published exclusion baseline is always applied.", Fields: []pfield{
		{Path: "selection.presets", Label: "What to back up", Kind: pPresets, Help: "Choose at least one, or list folders below."},
		{Path: "selection.roots", Label: "Explicit folders", Kind: pLines, Help: `One absolute path per line, like C:\Data or \\server\share. Listing folders replaces the presets.`},
		{Path: "selection.includes", Label: "Extra include patterns", Kind: pLines, Adv: true, Help: "One restic include pattern per line."},
		{Path: "selection.excludes", Label: "Exclusions on top of the baseline", Kind: pExcl, Help: "Published presets plus your own patterns, one per line. A $ is written for you; do not double it."},
		{Path: "selection.exclude_cloud_files", Label: "Leave out cloud-only placeholder files", Kind: pBool},
		{Path: "selection.exclude_larger_than", Label: "Skip files larger than", Kind: pText, Optional: true, Adv: true, Placeholder: "no limit", Help: "A size such as 500m or 10g; empty means no limit."},
		{Path: "selection.vss.enabled", Label: "Use volume shadow copies", Kind: pBool, Adv: true},
		{Path: "selection.vss.timeout_s", Label: "Shadow copy timeout", Unit: "seconds", Kind: pInt, Adv: true, Help: "10 to 3600."},
		{Path: "selection.vss.exclude_all_mount_points", Label: "Leave mounted volumes out of the snapshot", Kind: pBool, Adv: true},
		{Path: "selection.vss.exclude_volumes", Label: "Volumes without a snapshot", Kind: pLines, Adv: true, Help: `One volume per line, like D:\`},
	}},
	{Key: "retention", Title: "Retention", Lead: "Rules combine with OR: a snapshot is kept if any rule keeps it. Keep within is always applied, so the newest snapshots are never pruned.", Fields: []pfield{
		{Path: "retention", Label: "Retention", Kind: pRet},
	}},
	{Key: "throttle", Title: "Bandwidth", Lead: "Limits are per device, in KiB/s as restic takes them. A rate is unlimited, a number (KiB/s), or a number with k or m.", Fields: []pfield{
		{Path: "throttle.upload_kibps", Label: "Upload limit", Kind: pRate, Help: "unlimited, 500, 500k or 2m."},
		{Path: "throttle.download_kibps", Label: "Download limit", Kind: pRate},
		{Path: "throttle.windows", Label: "Limits by time", Kind: pTW, Help: "One per line as rate@window, for example 10@Mon-Fri/8-18, or up/down@window for different rates. Later lines win where windows overlap."},
		{Path: "throttle.memory_limit_mib", Label: "Memory cap for restic", Unit: "MiB", Kind: pInt, Adv: true, Help: "0 sizes it from the machine, -1 removes the cap, otherwise 512 or more."},
		{Path: "throttle.gateway_down_grace_minutes", Label: "Gateway-down grace", Unit: "minutes", Kind: pInt, Adv: true, Help: "0 uses the default (15), otherwise 5 to 120."},
	}},
	{Key: "sla", Title: "Service level", Lead: "The thresholds that turn silence and failure into states on the fleet board.", Fields: []pfield{
		{Path: "sla.stale_after_hours", Label: "Stale after", Unit: "hours without a success", Kind: pInt, Help: "At least 1."},
		{Path: "sla.unseen_after_hours", Label: "Unseen after", Unit: "hours without contact", Kind: pInt, Help: "Not less than the stale threshold."},
		{Path: "sla.first_backup_grace_hours", Label: "First-backup grace", Unit: "hours", Kind: pInt},
		{Path: "sla.consecutive_failures_to_alarm", Label: "Failures before an alarm", Kind: pInt, Help: "At least 1."},
		{Path: "sla.max_run_hours", Label: "Longest normal run", Unit: "hours", Kind: pInt, Adv: true},
		{Path: "sla.initial_max_run_hours", Label: "Longest first run", Unit: "hours", Kind: pInt, Adv: true, Help: "Not less than the longest normal run."},
		{Path: "sla.seed_extension_hours", Label: "Seed extension", Unit: "hours", Kind: pInt, Adv: true, Help: "0 turns it off."},
		{Path: "sla.stall_after_minutes", Label: "Stalled after", Unit: "minutes without progress", Kind: pInt, Adv: true},
		{Path: "sla.missed_factor", Label: "Missed-run factor", Kind: pFloat, Adv: true, Help: "At least 1."},
		{Path: "sla.partial_tolerance.max_failed_files", Label: "Partial backup tolerance: files", Kind: pInt, Adv: true},
		{Path: "sla.partial_tolerance.max_failed_pct", Label: "Partial backup tolerance: percent", Unit: "percent", Kind: pFloat, Adv: true, Help: "0 to 100."},
		{Path: "sla.escrow_verify_grace_hours", Label: "Key escrow verify grace", Unit: "hours", Kind: pInt, Adv: true},
		{Path: "sla.escrow_recheck_days", Label: "Key escrow recheck", Unit: "days", Kind: pInt, Adv: true},
		{Path: "sla.maintenance_deferral_alarm_after", Label: "Alarm after maintenance deferrals", Kind: pInt, Adv: true},
	}},
	{Key: "maintenance", Title: "Server-side upkeep", Lead: "Forget, prune and check run on the maintenance worker, never on the endpoint.", Fields: []pfield{
		{Path: "maintenance.forget_every_days", Label: "Forget every", Unit: "days", Kind: pInt},
		{Path: "maintenance.prune_every_days", Label: "Prune every", Unit: "days", Kind: pInt},
		{Path: "maintenance.check_every_days", Label: "Check every", Unit: "days", Kind: pInt},
		{Path: "maintenance.max_unused", Label: "Allow unused space up to", Kind: pText, Help: `A percentage like 5%, "unlimited" or a size like 10g.`},
		{Path: "maintenance.read_data_subset", Label: "Read-data check subset", Kind: pText, Help: `Like 5%, 1/20 or 500m.`},
		{Path: "maintenance.repack_cacheable_only", Label: "Repack cacheable data only", Kind: pBool, Adv: true},
		{Path: "maintenance.window_hint", Label: "Maintenance window", Kind: pWindow, Optional: true, Adv: true, Help: "Empty uses the complement of the backup window."},
	}},
	{Key: "agent_permissions", Title: "Local permissions", Lead: "What a person at the device may do from the tray.", Fields: []pfield{
		{Path: "agent_permissions.allow_local_backup_now", Label: "Allow Back up now", Kind: pBool},
		{Path: "agent_permissions.allow_local_pause", Label: "Allow Pause", Kind: pBool},
		{Path: "agent_permissions.allow_local_restore", Label: "Allow Restore", Kind: pBool},
	}},
	{Key: "notification_overrides", Title: "Notifications", Lead: "Overrides of the global > customer > device alerting chain.", Fields: []pfield{
		{Path: "notification_overrides.outdated_alarm", Label: "Alarm on an outdated agent", Kind: pBool},
		{Path: "notification_overrides.failure_threshold", Label: "Consecutive failures before alerting", Kind: pInt, Help: "0 inherits."},
		{Path: "notification_overrides.digest_include", Label: "Include in the digest", Kind: pTri},
	}},
}

// fieldByPath indexes the catalogue; retention keeps its composite key.
var fieldByPath = func() map[string]pfield {
	m := map[string]pfield{}
	for _, s := range policySections {
		for _, f := range s.Fields {
			m[f.Path] = f
		}
	}
	return m
}()

var retentionKeep = []struct{ Key, Label string }{
	{"keep_last", "Keep the last"}, {"keep_hourly", "Hourly"}, {"keep_daily", "Daily"},
	{"keep_weekly", "Weekly"}, {"keep_monthly", "Monthly"}, {"keep_yearly", "Yearly"},
}

// extraLabels name the document paths that are not a catalogue field of their
// own (the parts of the composite controls) for the change and source lists.
var extraLabels = map[string]string{
	"retention.preset": "Retention preset", "retention.keep_within": "Keep within", "retention.keep_tag": "Keep tagged snapshots",
	"retention.rules.keep_last": "Keep the last", "retention.rules.keep_hourly": "Keep hourly", "retention.rules.keep_daily": "Keep daily",
	"retention.rules.keep_weekly": "Keep weekly", "retention.rules.keep_monthly": "Keep monthly", "retention.rules.keep_yearly": "Keep yearly",
	"selection.path_flags": "Per-folder flags",
}

// labelOf is the human label of a document path, falling back to the path.
func labelOf(path string) string {
	if f, ok := fieldByPath[path]; ok {
		return f.Label
	}
	if l, ok := extraLabels[path]; ok {
		return l
	}
	return path
}

// sectionTitle is the title of a section key.
func sectionTitle(key string) string {
	for _, s := range policySections {
		if s.Key == key {
			return s.Title
		}
	}
	return key
}

// fieldKeyFor maps a validation problem path to the catalogue key whose control
// should show it ("" when no control matches).
func fieldKeyFor(path string) string {
	if i := strings.IndexByte(path, '['); i >= 0 {
		path = path[:i]
	}
	switch {
	case path == "retention" || strings.HasPrefix(path, "retention."):
		return "retention"
	case path == "selection":
		return "selection.presets"
	case strings.HasPrefix(path, "selection.path_flags"):
		return "selection.roots"
	case strings.HasPrefix(path, "throttle.windows"):
		return "throttle.windows"
	}
	for p := path; p != ""; {
		if _, ok := fieldByPath[p]; ok {
			return p
		}
		i := strings.LastIndexByte(p, '.')
		if i < 0 {
			break
		}
		p = p[:i]
	}
	return ""
}

// ---- document tree access ----

func treeOf(p policy.Policy) (map[string]any, error) {
	b, err := policy.Canonical(p)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func getPath(tree map[string]any, path string) (any, bool) {
	var cur any = tree
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[seg]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func setPath(tree map[string]any, path string, v any) {
	segs := strings.Split(path, ".")
	cur := tree
	for _, seg := range segs[:len(segs)-1] {
		next, ok := cur[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[seg] = next
		}
		cur = next
	}
	cur[segs[len(segs)-1]] = v
}

// ---- values <-> form ----

func numText(v any) string {
	switch n := v.(type) {
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64)
	case json.Number:
		return n.String()
	}
	return ""
}

func linesOf(v any) string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return strings.Join(out, "\n")
}

func stringList(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// fmtRate writes a rate the way ParseRate reads it.
func fmtRate(kibps int) string {
	switch {
	case kibps <= 0:
		return "unlimited"
	case kibps%1024 == 0:
		return strconv.Itoa(kibps/1024) + "m"
	}
	return strconv.Itoa(kibps)
}

func twLines(v any) string {
	list, _ := v.([]any)
	var out []string
	for _, e := range list {
		m, _ := e.(map[string]any)
		up, _ := m["up"].(float64)
		down, _ := m["down"].(float64)
		when, _ := m["when"].(string)
		rate := fmtRate(int(up))
		if int(up) != int(down) {
			rate += "/" + fmtRate(int(down))
		}
		out = append(out, rate+"@"+when)
	}
	return strings.Join(out, "\n")
}

// formFromDoc fills the form values of every catalogue field from a document tree.
func formFromDoc(doc map[string]any) url.Values {
	v := url.Values{}
	for _, s := range policySections {
		for _, f := range s.Fields {
			fillField(v, f, doc)
		}
	}
	return v
}

func fillField(v url.Values, f pfield, doc map[string]any) {
	if f.Kind == pRet {
		ret, _ := doc["retention"].(map[string]any)
		if _, ok := ret["rules"].(map[string]any); ok {
			v.Set("f.retention.mode", "rules")
		} else {
			v.Set("f.retention.mode", "preset")
		}
		if s, ok := ret["preset"].(string); ok {
			v.Set("f.retention.preset", s)
		}
		rules, _ := ret["rules"].(map[string]any)
		for _, k := range retentionKeep {
			if n := numText(rules[k.Key]); n != "" {
				v.Set("f.retention.rules."+k.Key, n)
			}
		}
		if s, ok := ret["keep_within"].(string); ok {
			v.Set("f.retention.keep_within", s)
		}
		v.Set("f.retention.keep_tag", linesOf(ret["keep_tag"]))
		return
	}
	raw, _ := getPath(doc, f.Path)
	name := f.name()
	switch f.Kind {
	case pInt, pFloat:
		v.Set(name, numText(raw))
	case pBool:
		if b, _ := raw.(bool); b {
			v.Set(name, "1")
		}
	case pText, pSelect, pWindow:
		s, _ := raw.(string)
		v.Set(name, s)
	case pLines:
		v.Set(name, linesOf(raw))
	case pRate:
		n, _ := raw.(float64)
		v.Set(name, fmtRate(int(n)))
	case pTri:
		switch b := raw.(type) {
		case bool:
			if b {
				v.Set(name, "yes")
			} else {
				v.Set(name, "no")
			}
		default:
			v.Set(name, "inherit")
		}
	case pPresets:
		for _, p := range stringList(raw) {
			v.Add(name, p)
		}
	case pExcl:
		var custom []string
		for _, e := range stringList(raw) {
			if n, ok := policy.ExclusionPresetRef(e); ok {
				v.Add(name+".presets", n)
			} else {
				custom = append(custom, e)
			}
		}
		v.Set(name+".custom", strings.Join(custom, "\n"))
	case pTW:
		v.Set(name, twLines(raw))
	}
}

// splitLines turns a textarea into trimmed non-empty lines.
func splitLines(s string) []string {
	out := []string{}
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// parseErr is a rejected control with the sentence to show beside it.
type parseErr struct{ Key, Msg string }

func parseWindowText(s string) error {
	if _, err := policy.ParseWindow(s); err != nil {
		return err
	}
	return nil
}

// windowMessage is the parser's own error, with its position.
func windowMessage(err error) string {
	var pe *policy.ParseError
	if errors.As(err, &pe) {
		return fmt.Sprintf("Not a valid window, at character %d: %s.", pe.Offset+1, strings.TrimSuffix(pe.Msg, "."))
	}
	return "Not a valid window: " + err.Error()
}

// parseTW reads the throttle-window lines (rate@window).
func parseTW(text string) ([]any, string) {
	out := []any{}
	for i, line := range splitLines(text) {
		at := strings.IndexByte(line, '@')
		if at < 0 {
			return nil, fmt.Sprintf("Line %d: write it as rate@window, for example 10@Mon-Fri/8-18.", i+1)
		}
		rate, when := strings.TrimSpace(line[:at]), strings.TrimSpace(line[at+1:])
		upS, downS := rate, rate
		if j := strings.IndexByte(rate, '/'); j >= 0 {
			upS, downS = rate[:j], rate[j+1:]
		}
		up, err := policy.ParseRate(upS)
		if err != nil {
			return nil, fmt.Sprintf("Line %d: %s.", i+1, strings.TrimSuffix(err.Error(), "."))
		}
		down, err := policy.ParseRate(downS)
		if err != nil {
			return nil, fmt.Sprintf("Line %d: %s.", i+1, strings.TrimSuffix(err.Error(), "."))
		}
		if _, err := policy.ParseWindow(when); err != nil {
			return nil, fmt.Sprintf("Line %d: %s", i+1, windowMessage(err))
		}
		out = append(out, map[string]any{"when": when, "up": int(up), "down": int(down)})
	}
	return out, ""
}

func parseInt(s, what string) (any, string) {
	s = strings.TrimSpace(s)
	n, err := strconv.Atoi(s)
	if err != nil {
		return nil, "Enter a whole number" + what + "."
	}
	return n, ""
}

// parseField turns the posted control of f into the JSON value for its path.
// forPatch asks for the merge-patch form where it differs (retention names the
// alternative it drops with null).
func parseField(f pfield, vals url.Values, forPatch bool) (any, string) {
	name := f.name()
	get := func() string { return strings.TrimSpace(vals.Get(name)) }
	switch f.Kind {
	case pInt:
		return parseInt(get(), "")
	case pFloat:
		x, err := strconv.ParseFloat(get(), 64)
		if err != nil {
			return nil, "Enter a number."
		}
		return x, ""
	case pBool:
		return vals.Get(name) != "", ""
	case pText:
		if get() == "" && !f.Optional {
			return nil, "This value is required."
		}
		return get(), ""
	case pSelect:
		for _, o := range f.Opts {
			if o.Value == get() {
				return o.Value, ""
			}
		}
		return nil, "Choose one of the listed values."
	case pWindow:
		if get() == "" {
			if f.Optional {
				return "", ""
			}
			return nil, "A window is required. Use */0-24 for always."
		}
		if err := parseWindowText(get()); err != nil {
			return nil, windowMessage(err)
		}
		return get(), ""
	case pLines:
		return splitLines(vals.Get(name)), ""
	case pRate:
		r, err := policy.ParseRate(get())
		if err != nil {
			return nil, strings.ToUpper(err.Error()[:1]) + strings.TrimSuffix(err.Error()[1:], ".") + "."
		}
		return int(r), ""
	case pTri:
		switch get() {
		case "yes":
			return true, ""
		case "no":
			return false, ""
		case "inherit", "":
			return nil, ""
		}
		return nil, "Choose inherit, yes or no."
	case pPresets:
		out := []string{}
		for _, p := range vals[name] {
			out = append(out, p)
		}
		return out, ""
	case pExcl:
		out := []string{}
		for _, p := range vals[name+".presets"] {
			out = append(out, "@"+p)
		}
		return append(out, splitLines(vals.Get(name+".custom"))...), ""
	case pTW:
		return parseTW(vals.Get(name))
	case pRet:
		return parseRetention(vals, forPatch)
	}
	return nil, "Unsupported control."
}

func parseRetention(vals url.Values, forPatch bool) (any, string) {
	obj := map[string]any{}
	kw := strings.TrimSpace(vals.Get("f.retention.keep_within"))
	if kw == "" {
		return nil, "Keep within is required so the newest snapshots are never pruned. Use a duration such as 14d."
	}
	obj["keep_within"] = kw
	obj["keep_tag"] = splitLines(vals.Get("f.retention.keep_tag"))
	if vals.Get("f.retention.mode") == "rules" {
		rules := map[string]any{}
		for _, k := range retentionKeep {
			s := strings.TrimSpace(vals.Get("f.retention.rules." + k.Key))
			if s == "" {
				s = "0"
			}
			n, err := strconv.Atoi(s)
			if err != nil {
				return nil, k.Label + ": enter a whole number (0 = unused, -1 = unlimited)."
			}
			rules[k.Key] = n
		}
		obj["rules"] = rules
		if forPatch {
			obj["preset"] = nil
		}
		return obj, ""
	}
	preset := vals.Get("f.retention.preset")
	if _, ok := policy.RetentionPresets[preset]; !ok {
		return nil, "Choose one of the named presets, or switch to custom rules."
	}
	obj["preset"] = preset
	if forPatch {
		obj["rules"] = nil
	}
	return obj, ""
}

// applyForm writes every catalogue control of vals into doc (a full document
// tree) and returns the controls that could not be read.
func applyForm(doc map[string]any, vals url.Values) []parseErr {
	var errs []parseErr
	for _, s := range policySections {
		for _, f := range s.Fields {
			v, msg := parseField(f, vals, false)
			if msg != "" {
				errs = append(errs, parseErr{Key: f.Path, Msg: msg})
				continue
			}
			setPath(doc, f.Path, v)
		}
	}
	return errs
}

// ---- display ----

// fmtValue renders a compact-JSON leaf for the change and source lists.
func fmtValue(path, raw string) string {
	if raw == "" {
		return "not set"
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	switch path {
	case "throttle.upload_kibps", "throttle.download_kibps":
		if n, ok := v.(float64); ok {
			return fmtRate(int(n))
		}
	case "throttle.windows":
		if s := twLines(v); s != "" {
			return strings.ReplaceAll(s, "\n", "; ")
		}
		return "none"
	}
	switch x := v.(type) {
	case nil:
		return "inherit"
	case bool:
		if x {
			return "on"
		}
		return "off"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case string:
		if x == "" {
			return "(empty)"
		}
		return x
	case []any:
		parts := stringList(x)
		if len(parts) == 0 {
			return "none"
		}
		return strings.Join(parts, ", ")
	}
	return raw
}

// sourceOrder sorts paths for display by section then catalogue position.
func catalogueRank(path string) int {
	i := 0
	for _, s := range policySections {
		for _, f := range s.Fields {
			if path == f.Path || strings.HasPrefix(path, f.Path+".") {
				return i
			}
			i++
		}
	}
	return i + 1
}

func sortByCatalogue(paths []string) {
	sort.SliceStable(paths, func(i, j int) bool {
		a, b := catalogueRank(paths[i]), catalogueRank(paths[j])
		if a != b {
			return a < b
		}
		return paths[i] < paths[j]
	})
}
