package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "time/tzdata" // the display zone must resolve on hosts without an OS zone database
)

// Console settings (M2-W3A, architecture §5.2 "Settings, keys, system"). The
// settings table is a key/value store of non-secret values; this file adds the
// registry of the keys the console knows, the validation of each, the audited
// write, and DisplayPrefs: the live copy of the display zone and the refresh
// interval that the UI and the JSON API both read per request, so a change
// applies to the next page without a restart. Times stay UTC in the store;
// the zone is applied only at render time.

// Setting keys.
const (
	SettingDisplayTimezone = "display_timezone"
	SettingRefreshInterval = "refresh_interval_s"
)

// SettingDef describes one setting the console knows. Editable settings can be
// changed through the console and the API; the others are shown read-only with
// the milestone that gives them an effect, so an operator cannot set a value
// that nothing honours yet.
type SettingDef struct {
	Key   string
	Label string
	Help  string
	// Kind is zone, int, time or flag; Unit labels an int.
	Kind     string
	Unit     string
	Min, Max int
	Editable bool
	// Since names the milestone that makes a read-only setting effective.
	Since string
}

var settingDefs = []SettingDef{
	{Key: SettingDisplayTimezone, Label: "Display time zone", Kind: "zone", Editable: true,
		Help: "The IANA zone every page and API time is shown in. Times are stored in UTC and never change."},
	{Key: SettingRefreshInterval, Label: "Refresh interval", Kind: "int", Unit: "seconds", Min: 5, Max: 3600, Editable: true,
		Help: "How often open pages re-read the fleet. Between 5 and 3600 seconds."},
	{Key: "audit_retention_days", Label: "Audit retention", Kind: "int", Unit: "days", Min: 90, Max: 3650, Since: "M5",
		Help: "Audit rows older than this are deleted by the nightly retention job (never fewer than 90 days)."},
	{Key: "run_history_retention_days", Label: "Run history retention", Kind: "int", Unit: "days", Min: 7, Max: 3650, Since: "M5",
		Help: "Backup runs older than this are deleted nightly, never the newest success of a device."},
	{Key: "heartbeat_ring_hours", Label: "Heartbeat history", Kind: "int", Unit: "hours", Min: 1, Max: 168, Since: "M3",
		Help: "How long raw heartbeats are kept for the state engine."},
	{Key: "digest_local_time", Label: "Daily digest time", Kind: "time", Since: "M4",
		Help: "The local time the daily digest is sent, in the display zone."},
	{Key: "fleet_ever_enrolled", Label: "Fleet has enrolled a device", Kind: "flag", Since: "system",
		Help: "Set by the console when the first device enrolls. It turns an empty fleet from a first-run state into an alarm."},
}

// SettingDefs lists the settings the console knows, in display order.
func SettingDefs() []SettingDef { return append([]SettingDef(nil), settingDefs...) }

// SettingDefOf returns the definition of key.
func SettingDefOf(key string) (SettingDef, bool) {
	for _, d := range settingDefs {
		if d.Key == key {
			return d, true
		}
	}
	return SettingDef{}, false
}

// CommonZones is the short list the settings page offers; any IANA name is
// accepted. (The standard library cannot list the zone database.)
var CommonZones = []string{
	"UTC", "America/New_York", "America/Chicago", "America/Denver", "America/Phoenix", "America/Los_Angeles", "America/Anchorage",
	"Pacific/Honolulu", "America/Halifax", "America/St_Johns", "America/Toronto", "America/Vancouver", "America/Mexico_City",
	"America/Bogota", "America/Lima", "America/Santiago", "America/Argentina/Buenos_Aires", "America/Sao_Paulo",
	"Atlantic/Reykjavik", "Europe/London", "Europe/Dublin", "Europe/Lisbon", "Europe/Paris", "Europe/Berlin", "Europe/Madrid",
	"Europe/Rome", "Europe/Amsterdam", "Europe/Zurich", "Europe/Stockholm", "Europe/Warsaw", "Europe/Athens", "Europe/Helsinki",
	"Europe/Istanbul", "Europe/Moscow", "Africa/Cairo", "Africa/Johannesburg", "Africa/Lagos", "Africa/Nairobi",
	"Asia/Dubai", "Asia/Tehran", "Asia/Karachi", "Asia/Kolkata", "Asia/Dhaka", "Asia/Bangkok", "Asia/Jakarta", "Asia/Singapore",
	"Asia/Hong_Kong", "Asia/Shanghai", "Asia/Manila", "Asia/Seoul", "Asia/Tokyo", "Australia/Perth", "Australia/Adelaide",
	"Australia/Sydney", "Pacific/Auckland",
}

// ValidZoneName reports whether name is an IANA zone this build can resolve. It
// is stricter than time.LoadLocation: "" (which loads as UTC) and "Local" are
// refused, and so is anything that is not a plain zone name.
func ValidZoneName(name string) bool {
	if name == "" || name == "Local" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '+', r == '/':
		default:
			return false
		}
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

// ValidateSetting checks a proposed value for key and returns its canonical
// JSON, or a *ValidationError (field "value") that says in a fixed sentence
// what is accepted; it never echoes the input. An unknown key is ErrNotFound; a
// known key that is not editable yet is a validation error on "key".
func ValidateSetting(key string, raw json.RawMessage) (json.RawMessage, error) {
	def, known := SettingDefOf(key)
	if !known {
		return nil, ErrNotFound
	}
	if !def.Editable {
		return nil, validation([]FieldError{fieldf("key", "This setting cannot be changed here yet (it takes effect in %s).", def.Since)})
	}
	raw = bytes.TrimSpace(raw)
	switch def.Kind {
	case "zone":
		var z string
		if err := json.Unmarshal(raw, &z); err != nil {
			return nil, validation([]FieldError{fieldf("value", "Use an IANA zone name such as America/Chicago.")})
		}
		z = strings.TrimSpace(z)
		if !ValidZoneName(z) {
			return nil, validation([]FieldError{fieldf("value", "Use an IANA zone name such as America/Chicago.")})
		}
		b, _ := json.Marshal(z)
		return b, nil
	case "int":
		var n json.Number
		if len(raw) > 0 && raw[0] == '"' { // json.Number would accept a quoted number
			return nil, validation([]FieldError{fieldf("value", "Use a whole number from %d to %d.", def.Min, def.Max)})
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&n); err != nil {
			return nil, validation([]FieldError{fieldf("value", "Use a whole number from %d to %d.", def.Min, def.Max)})
		}
		v, err := strconv.Atoi(n.String())
		if err != nil || v < def.Min || v > def.Max {
			return nil, validation([]FieldError{fieldf("value", "Use a whole number from %d to %d.", def.Min, def.Max)})
		}
		return json.RawMessage(strconv.Itoa(v)), nil
	}
	return nil, validation([]FieldError{fieldf("key", "This setting cannot be changed here.")})
}

// SettingValue is a stored setting with the name of who last changed it.
type SettingValue struct {
	SettingDef
	// Stored is the stored JSON (nil when the row does not exist).
	Stored        json.RawMessage
	UpdatedBy     string // admin id; "" for the system or a deleted admin
	UpdatedByName string
	UpdatedAt     time.Time
}

// ListSettingValues returns every setting the console knows with its stored
// value. Settings are console-wide, so the caller needs scope.All (ErrScope
// otherwise). Rows for keys the console does not know are not listed.
func (s *Store) ListSettingValues(ctx context.Context, scope Scope) ([]SettingValue, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if !scope.All {
		return nil, ErrScope
	}
	rows, err := s.ListSettings(ctx)
	if err != nil {
		return nil, err
	}
	byKey := map[string]Setting{}
	for _, r := range rows {
		byKey[r.Key] = r
	}
	names := map[string]string{}
	out := make([]SettingValue, 0, len(settingDefs))
	for _, d := range settingDefs {
		v := SettingValue{SettingDef: d}
		if r, ok := byKey[d.Key]; ok {
			v.Stored, v.UpdatedBy, v.UpdatedAt = r.Value, r.UpdatedBy, r.UpdatedAt
			if r.UpdatedBy != "" {
				if _, seen := names[r.UpdatedBy]; !seen {
					if a, err := s.GetAdmin(ctx, r.UpdatedBy); err == nil {
						names[r.UpdatedBy] = a.Username
					} else {
						names[r.UpdatedBy] = ""
					}
				}
				v.UpdatedByName = names[r.UpdatedBy]
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// SettingInput is one proposed change.
type SettingInput struct {
	Key   string
	Value json.RawMessage
}

// SettingChange is the outcome for one key. Unchanged is true when the stored
// value already equalled the proposal: nothing was written and no audit row.
type SettingChange struct {
	Key       string
	Old, New  json.RawMessage
	Unchanged bool
}

// SetSettingsAudited validates and applies changes in one transaction, writing
// one settings.update audit row per changed key in the same transaction (a
// change whose audit row cannot be written is refused: ErrAudit). Changing a
// console-wide setting needs scope.All (ErrScope otherwise). Every value is
// validated first and the first problem refuses the whole call, so the page
// never half-applies. The audit details hold the key and the old and new
// values: all editable settings are non-secret by definition.
func (s *Store) SetSettingsAudited(ctx context.Context, scope Scope, changes []SettingInput, actor AuditActor) ([]SettingChange, error) {
	if err := scope.check(); err != nil {
		return nil, err
	}
	if !scope.All {
		return nil, ErrScope
	}
	if len(changes) == 0 {
		return nil, validation([]FieldError{fieldf("value", "Send at least one setting to change.")})
	}
	type prepared struct {
		key string
		val json.RawMessage
	}
	var prep []prepared
	seen := map[string]bool{}
	for _, c := range changes {
		if seen[c.Key] {
			return nil, validation([]FieldError{fieldf("key", "Each setting can be changed once per request.")})
		}
		seen[c.Key] = true
		v, err := ValidateSetting(c.Key, c.Value)
		if err != nil {
			return nil, err
		}
		prep = append(prep, prepared{c.Key, v})
	}
	out := make([]SettingChange, 0, len(prep))
	err := s.Write(ctx, func(tx *Tx) error {
		out = out[:0]
		for _, p := range prep {
			var cur string
			err := tx.QueryRow(`SELECT value FROM settings WHERE key = ?`, p.key).Scan(&cur)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			ch := SettingChange{Key: p.key, New: p.val}
			if err == nil {
				ch.Old = json.RawMessage(cur)
				var a, b bytes.Buffer
				if json.Compact(&a, []byte(cur)) == nil && json.Compact(&b, p.val) == nil && a.String() == b.String() {
					// Equal to what is stored. It is only "nothing" when the operator already
					// owns the value: saving the migration's seed value makes it the operator's.
					var marker string
					merr := tx.QueryRow(`SELECT value FROM settings WHERE key = ?`, p.key+sourceSuffix).Scan(&marker)
					if merr != nil && !errors.Is(merr, sql.ErrNoRows) {
						return merr
					}
					if merr == nil && marker == sourceOperator {
						ch.Unchanged = true
						out = append(out, ch)
						continue
					}
				}
			}
			now := tx.nowMS()
			if err := putOperatorSetting(tx, p.key, p.val, actor.AdminID, now); err != nil {
				return err
			}
			details := map[string]any{"key": p.key, "to": json.RawMessage(p.val)}
			if ch.Old != nil {
				details["from"] = json.RawMessage(ch.Old)
			}
			ev, err := actor.event("settings.update", "setting", p.key, "", details)
			if err != nil {
				return err
			}
			if err := audited(tx, ev); err != nil {
				return err
			}
			out = append(out, ch)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return append([]SettingChange(nil), out...), nil
}

// sourceSuffix names the sibling row that marks a setting as chosen by the
// operator: "<key>.source" holds "operator". The migration seeds display_timezone
// with "UTC" for every database; that seed is nobody's choice and must not beat
// config.json (ADR 0012: the database wins once it holds a value the operator
// stored). init and the settings page write the marker with the value.
const (
	sourceSuffix   = ".source"
	sourceOperator = `"operator"`
)

func putOperatorSetting(tx *Tx, key string, val json.RawMessage, adminID string, now int64) error {
	up := `INSERT INTO settings (key, value, updated_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_by = excluded.updated_by, updated_at = excluded.updated_at`
	if _, err := tx.Exec(up, key, string(val), nullStr(adminID), now, now); err != nil {
		return err
	}
	_, err := tx.Exec(up, key+sourceSuffix, sourceOperator, nullStr(adminID), now, now)
	return err
}

// PutOperatorSetting stores value and marks it as chosen by the operator (used by
// `cryo-console init`, which stores the configured zone). adminID is "" for the CLI.
func (s *Store) PutOperatorSetting(ctx context.Context, key string, value json.RawMessage, adminID string) error {
	if len(value) == 0 || !json.Valid(value) {
		return invalidf("setting value is not valid JSON")
	}
	return s.Write(ctx, func(tx *Tx) error { return putOperatorSetting(tx, key, value, adminID, tx.nowMS()) })
}

// ---------------------------------------------------------------------------
// DisplayPrefs
// ---------------------------------------------------------------------------

// DisplaySnapshot is one consistent reading of the display preferences. A request
// takes one and uses it throughout, so the zone name, the zone and the refresh
// interval it shows always belong together.
type DisplaySnapshot struct {
	Zone     string
	Loc      *time.Location
	RefreshS int
}

// DisplayPrefs is the live display zone and refresh interval. The UI and the
// JSON API share one value per process; a settings change replaces it, so the
// next request on either transport uses the new zone.
type DisplayPrefs struct {
	mu   sync.RWMutex
	snap DisplaySnapshot
	// cfgZone and cfgRefresh are what config.json supplies; chosen says the
	// operator stored the value, so it wins over them (ADR 0012).
	cfgZone       string
	cfgRefresh    int
	zoneChosen    bool
	refreshChosen bool
	// note says why a stored value was not usable at load ("" = it was).
	note string
}

func resolveZone(zone string) (string, *time.Location) {
	if ValidZoneName(zone) {
		if loc, err := time.LoadLocation(zone); err == nil {
			return zone, loc
		}
	}
	return "UTC", time.UTC
}

func resolveRefresh(n int) int {
	if n < 5 || n > 3600 {
		return 30
	}
	return n
}

// NewDisplayPrefs returns prefs for the given zone and refresh interval; a zone
// that does not resolve falls back to UTC and an out-of-range interval to 30 s.
// They stand for the config values: nothing is chosen yet.
func NewDisplayPrefs(zone string, refreshS int) *DisplayPrefs {
	z, loc := resolveZone(zone)
	r := resolveRefresh(refreshS)
	return &DisplayPrefs{snap: DisplaySnapshot{Zone: z, Loc: loc, RefreshS: r}, cfgZone: z, cfgRefresh: r}
}

// Snapshot returns the preferences as one consistent value.
func (d *DisplayPrefs) Snapshot() DisplaySnapshot {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.snap
}

// Zone is the IANA name of the display zone.
func (d *DisplayPrefs) Zone() string { return d.Snapshot().Zone }

// Loc is the display zone.
func (d *DisplayPrefs) Loc() *time.Location { return d.Snapshot().Loc }

// RefreshS is the refresh interval in seconds.
func (d *DisplayPrefs) RefreshS() int { return d.Snapshot().RefreshS }

// Note is the reason a stored setting was not used at load, if any.
func (d *DisplayPrefs) Note() string { d.mu.RLock(); defer d.mu.RUnlock(); return d.note }

// ConfigNotes says, for each operator-chosen value that differs from config.json,
// that the saved value wins. Only zone names and numbers are named.
func (d *DisplayPrefs) ConfigNotes() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var out []string
	if d.zoneChosen && d.snap.Zone != d.cfgZone {
		out = append(out, "config.json says "+d.cfgZone+" as the display time zone; the value saved here ("+d.snap.Zone+") wins.")
	}
	if d.refreshChosen && d.snap.RefreshS != d.cfgRefresh {
		out = append(out, "config.json says "+strconv.Itoa(d.cfgRefresh)+" seconds as the refresh interval; the value saved here ("+strconv.Itoa(d.snap.RefreshS)+") wins.")
	}
	return out
}

// Apply updates the prefs from the changes SetSettingsAudited committed. The new
// values are computed from the current ones under one lock, so concurrent
// applies never lose each other's keys and a reader never sees a mixed pair. A
// nil receiver is a no-op (a service built without prefs keeps its boot values).
func (d *DisplayPrefs) Apply(changes []SettingChange) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	zone, refresh := d.snap.Zone, d.snap.RefreshS
	for _, c := range changes {
		switch c.Key {
		case SettingDisplayTimezone:
			var z string
			if json.Unmarshal(c.New, &z) == nil {
				zone, d.zoneChosen = z, true
			}
		case SettingRefreshInterval:
			if n, err := strconv.Atoi(string(c.New)); err == nil {
				refresh, d.refreshChosen = n, true
			}
		}
	}
	z, loc := resolveZone(zone)
	d.snap = DisplaySnapshot{Zone: z, Loc: loc, RefreshS: resolveRefresh(refresh)}
	d.note = ""
}

// operatorChosen reports whether a stored row was put there by the operator: it
// carries the operator marker, or (a database initialised before the marker
// existed) it was written by someone, written after the migration created it, or
// differs from the migration's seed.
func operatorChosen(byKey map[string]Setting, key, seed string) bool {
	if m, ok := byKey[key+sourceSuffix]; ok && string(m.Value) == sourceOperator {
		return true
	}
	r, ok := byKey[key]
	if !ok {
		return false
	}
	return r.UpdatedBy != "" || !r.UpdatedAt.Equal(r.CreatedAt) || (seed != "" && string(r.Value) != seed)
}

// LoadDisplayPrefs reads the stored settings. A value the operator stored wins
// over the fallback (the config file); the migration's seed is nobody's choice,
// so while a key has never been stored by the operator the fallback supplies it
// (ADR 0012). A chosen value that does not resolve falls back and the prefs say
// so in Note, which the settings page shows as a warning.
func (s *Store) LoadDisplayPrefs(ctx context.Context, fallbackZone string, fallbackRefreshS int) (*DisplayPrefs, error) {
	d := NewDisplayPrefs(fallbackZone, fallbackRefreshS)
	rows, err := s.ListSettings(ctx)
	if err != nil {
		return d, err
	}
	byKey := map[string]Setting{}
	for _, r := range rows {
		byKey[r.Key] = r
	}
	zone, refresh := d.snap.Zone, d.snap.RefreshS
	var notes []string
	if operatorChosen(byKey, SettingDisplayTimezone, `"UTC"`) {
		var z string
		if json.Unmarshal(byKey[SettingDisplayTimezone].Value, &z) == nil && ValidZoneName(z) {
			zone, d.zoneChosen = z, true
		} else {
			notes = append(notes, "The stored display time zone is not a valid IANA zone, so "+zone+" is used.")
		}
	}
	if operatorChosen(byKey, SettingRefreshInterval, "") {
		if n, err := strconv.Atoi(strings.TrimSpace(string(byKey[SettingRefreshInterval].Value))); err == nil && n >= 5 && n <= 3600 {
			refresh, d.refreshChosen = n, true
		} else {
			notes = append(notes, "The stored refresh interval is out of range, so "+strconv.Itoa(refresh)+" seconds is used.")
		}
	}
	z, loc := resolveZone(zone)
	d.snap = DisplaySnapshot{Zone: z, Loc: loc, RefreshS: resolveRefresh(refresh)}
	sort.Strings(notes)
	d.note = strings.Join(notes, " ")
	return d, nil
}
