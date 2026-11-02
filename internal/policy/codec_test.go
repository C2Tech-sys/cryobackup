package policy

import (
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestDefaultClasses(t *testing.T) {
	for _, class := range []DeviceClass{ClassLaptop, ClassDesktop, ClassUnknown} {
		t.Run(string(class), func(t *testing.T) {
			p := Default(class)
			if err := p.Validate(); err != nil {
				t.Fatalf("Default(%q) does not validate:\n%v", class, err)
			}
			want := 0
			if class == ClassLaptop {
				want = 30
			}
			if p.Conditions.BatteryFloorPct != want {
				t.Errorf("battery_floor_pct = %d, want %d", p.Conditions.BatteryFloorPct, want)
			}
		})
	}
	l, d := Default(ClassLaptop), Default(ClassDesktop)
	l.Conditions.BatteryFloorPct = 0
	if !reflect.DeepEqual(l, d) {
		t.Error("laptop and desktop defaults must differ only in battery_floor_pct")
	}
	if !reflect.DeepEqual(Default(ClassDesktop), Default(ClassUnknown)) {
		t.Error("desktop and unknown defaults must be identical")
	}
	// §3.6 spot checks.
	p := Default(ClassUnknown)
	if p.Schedule.Mode != "interval" || p.Schedule.IntervalMinutes != 1440 || p.Schedule.Window != "*/0-24" ||
		p.Schedule.JitterMinutes != 30 || !p.Schedule.Catchup || !p.Schedule.RunOnWake || p.Schedule.StartupDelayS != 120 ||
		p.Schedule.Retries != (Retries{3, 300, 3600}) || p.Schedule.LockWaitS != 120 || p.Schedule.MaxRuntimeHours != 12 || !p.Schedule.SkipIfRunning {
		t.Errorf("schedule defaults differ from §3.6: %+v", p.Schedule)
	}
	if p.Conditions.Metered != "defer" || p.Conditions.RunAnywayAfterMinutes != 240 || p.Conditions.VPNRequired || p.Conditions.IdleOnly {
		t.Errorf("conditions defaults differ from §3.6: %+v", p.Conditions)
	}
	if !p.Selection.ExcludeCloudFiles || !p.Selection.VSS.Enabled || p.Selection.VSS.TimeoutS != 120 || p.Selection.VSS.ExcludeAllMountPoints {
		t.Errorf("selection defaults differ from §3.6: %+v", p.Selection)
	}
	s := p.SLA
	if s.StaleAfterHours != 72 || s.UnseenAfterHours != 168 || s.FirstBackupGraceHours != 72 || s.SeedExtensionHours != 0 ||
		s.PartialTolerance != (PartialTolerance{20, 0.5}) || s.ConsecutiveFailuresToAlarm != 1 || s.MaxRunHours != 12 ||
		s.InitialMaxRunHours != 168 || s.StallAfterMinutes != 180 || s.MissedFactor != 2.0 || s.EscrowVerifyGraceHours != 24 ||
		s.EscrowRecheckDays != 30 || s.MaintenanceDeferralAlarmAfter != 10 {
		t.Errorf("sla defaults differ from §3.6: %+v", s)
	}
	m := p.Maintenance
	if m.ForgetEveryDays != 7 || m.MaxUnused != "5%" || m.CheckEveryDays != 30 || m.ReadDataSubset != "5%" {
		t.Errorf("maintenance defaults differ from §3.6: %+v", m)
	}
}

// wantFields is the field-name contract of architecture §3.6: the JSON keys of
// every section, exactly.
var wantFields = map[string][]string{
	"schedule": {"mode", "interval_minutes", "window", "fixed_times", "jitter_minutes", "catchup", "run_on_wake", "startup_delay_s",
		"retries", "lock_wait_s", "max_runtime_hours", "skip_if_running"},
	"conditions": {"battery_floor_pct", "metered", "wifi_ssid_block", "vpn_required", "idle_only", "run_anyway_after_minutes"},
	"selection":  {"roots", "presets", "includes", "excludes", "path_flags", "exclude_cloud_files", "exclude_larger_than", "vss"},
	"retention":  {"preset", "keep_within", "keep_tag"}, // rules is the alternative to preset
	"throttle":   {"upload_kibps", "download_kibps", "windows", "memory_limit_mib", "gateway_down_grace_minutes"},
	"sla": {"stale_after_hours", "unseen_after_hours", "first_backup_grace_hours", "seed_extension_hours", "partial_tolerance",
		"consecutive_failures_to_alarm", "max_run_hours", "initial_max_run_hours", "stall_after_minutes", "missed_factor",
		"escrow_verify_grace_hours", "escrow_recheck_days", "maintenance_deferral_alarm_after"},
	"maintenance": {"forget_every_days", "prune_every_days", "max_unused", "repack_cacheable_only", "check_every_days",
		"read_data_subset", "window_hint"},
	"agent_permissions":      {"allow_local_backup_now", "allow_local_pause", "allow_local_restore"},
	"notification_overrides": {"outdated_alarm", "failure_threshold", "digest_include"},
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func TestJSONFieldNamesAreTheDocumentedOnes(t *testing.T) {
	b, err := json.Marshal(Default(ClassLaptop))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if got := keysOf(map[string]any{"schedule": 0, "conditions": 0, "selection": 0, "retention": 0, "throttle": 0, "sla": 0, "maintenance": 0, "agent_permissions": 0, "notification_overrides": 0}); len(doc) != len(got) {
		t.Fatalf("document has %d sections, want %d", len(doc), len(got))
	}
	for section, want := range wantFields {
		got := keysOf(doc[section])
		if !reflect.DeepEqual(got, sorted(want)) {
			t.Errorf("%s keys = %v, want %v", section, got, sorted(want))
		}
	}
	// Nested objects.
	nested := map[string][]string{
		"retries":           {"max", "base_s", "cap_s"},
		"vss":               {"enabled", "timeout_s", "exclude_all_mount_points", "exclude_volumes"},
		"partial_tolerance": {"max_failed_files", "max_failed_pct"},
	}
	for name, want := range nested {
		var obj map[string]any
		for _, sec := range doc {
			if v, ok := sec[name].(map[string]any); ok {
				obj = v
			}
		}
		if got := keysOf(obj); !reflect.DeepEqual(got, sorted(want)) {
			t.Errorf("%s keys = %v, want %v", name, got, sorted(want))
		}
	}
	var rules map[string]any
	rb, _ := json.Marshal(Retention{Rules: &KeepCounts{}, KeepWithin: "1d"})
	_ = json.Unmarshal(rb, &rules)
	if got := keysOf(rules["rules"].(map[string]any)); !reflect.DeepEqual(got, sorted([]string{"keep_last", "keep_hourly", "keep_daily", "keep_weekly", "keep_monthly", "keep_yearly"})) {
		t.Errorf("rules keys = %v", got)
	}
	tw, _ := json.Marshal(ThrottleWindow{})
	if string(tw) != `{"when":"","up":0,"down":0}` {
		t.Errorf("throttle window JSON = %s", tw)
	}
	pf, _ := json.Marshal(PathFlags{})
	if string(pf) != `{"required":false,"optional":false,"require_snapshot":false,"follow_symlinks":false}` {
		t.Errorf("path flags JSON = %s", pf)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	custom := Default(ClassLaptop)
	custom.Schedule.Mode = ModeFixed
	custom.Schedule.FixedTimes = []string{"08:00", "20:30"}
	custom.Selection.Roots = []string{`C:\Data`}
	custom.Selection.Presets = []string{}
	custom.Selection.PathFlags = map[string]PathFlags{`C:\Data`: {Optional: true, FollowSymlinks: true}}
	custom.Selection.Excludes = []string{"@vm_images", "*.bak"}
	custom.Retention = Retention{Rules: &KeepCounts{KeepDaily: 7, KeepYearly: -1}, KeepWithin: "30d", KeepTag: []string{"legal"}}
	custom.Throttle = Throttle{Upload: 60, Download: 80, Windows: []ThrottleWindow{{When: "Mon-Fri/8-18", Up: 10, Down: 20}}}
	yes := true
	custom.NotificationOverrides = NotificationOverrides{OutdatedAlarm: true, FailureThreshold: 3, DigestInclude: &yes}

	for name, p := range map[string]Policy{"default": Default(ClassUnknown), "custom": custom} {
		t.Run(name, func(t *testing.T) {
			if err := p.Validate(); err != nil {
				t.Fatalf("fixture invalid: %v", err)
			}
			sections, err := Encode(p)
			if err != nil {
				t.Fatal(err)
			}
			if len(sections) != 9 {
				t.Fatalf("Encode returned %d sections, want 9", len(sections))
			}
			for _, n := range SectionNames {
				if _, ok := sections[n]; !ok {
					t.Errorf("section %q missing", n)
				}
			}
			for label, decode := range map[string]func(map[string]json.RawMessage) (Policy, error){"tolerant": Decode, "strict": DecodeStrict} {
				got, err := decode(sections)
				if err != nil {
					t.Fatalf("%s decode: %v", label, err)
				}
				if !reflect.DeepEqual(got, p.normalized()) {
					t.Errorf("%s round trip changed the policy:\n got %+v\nwant %+v", label, got, p)
				}
			}
			// Single-document form.
			canon, err := Canonical(p)
			if err != nil {
				t.Fatal(err)
			}
			for _, strict := range []bool{false, true} {
				got, err := DecodeDocument(canon, strict)
				if err != nil || !reflect.DeepEqual(got, p.normalized()) {
					t.Errorf("DecodeDocument(strict=%v) = %v, %v", strict, got, err)
				}
			}
		})
	}
}

func TestEncodeNormalisesNil(t *testing.T) {
	sections, err := Encode(Policy{})
	if err != nil {
		t.Fatal(err)
	}
	for sec, key := range map[string]string{"schedule": "fixed_times", "conditions": "wifi_ssid_block", "selection": "roots", "retention": "keep_tag", "throttle": "windows"} {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(sections[sec], &m); err != nil {
			t.Fatal(err)
		}
		if string(m[key]) != "[]" {
			t.Errorf("%s.%s encodes as %s, want []", sec, key, m[key])
		}
	}
	if !strings.Contains(string(sections["selection"]), `"path_flags":{}`) {
		t.Errorf("path_flags must encode as {}: %s", sections["selection"])
	}
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func TestDecodeTolerant(t *testing.T) {
	def := Default(ClassUnknown)

	t.Run("nil and empty give defaults", func(t *testing.T) {
		for _, in := range []map[string]json.RawMessage{nil, {}, {"schedule": raw("null"), "sla": raw("")}} {
			got, err := Decode(in)
			if err != nil || !reflect.DeepEqual(got, def) {
				t.Fatalf("Decode(%v) = %+v, %v", in, got, err)
			}
		}
	})
	t.Run("unknown sections and fields are ignored", func(t *testing.T) {
		got, err := Decode(map[string]json.RawMessage{
			"from_the_future": raw(`{"x":1}`),
			"schedule":        raw(`{"interval_minutes":60,"new_field":true}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		want := def
		want.Schedule.IntervalMinutes = 60
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v", got.Schedule)
		}
	})
	t.Run("a partial section keeps the other defaults, nested too", func(t *testing.T) {
		got, err := Decode(map[string]json.RawMessage{"schedule": raw(`{"retries":{"max":5}}`), "sla": raw(`{"partial_tolerance":{"max_failed_files":3}}`)})
		if err != nil {
			t.Fatal(err)
		}
		if got.Schedule.Retries.Max != 5 || got.Schedule.Retries.BaseS != 300 || got.SLA.PartialTolerance.MaxFailedFiles != 3 || got.SLA.PartialTolerance.MaxFailedPct != 0.5 {
			t.Fatalf("got %+v / %+v", got.Schedule.Retries, got.SLA.PartialTolerance)
		}
		if got.Schedule.IntervalMinutes != 1440 {
			t.Fatal("interval_minutes lost its default")
		}
	})
	t.Run("selection roots replace the default preset", func(t *testing.T) {
		got, err := Decode(map[string]json.RawMessage{"selection": raw(`{"roots":["C:\\Data"]}`)})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Selection.Roots, []string{`C:\Data`}) || len(got.Selection.Presets) != 0 {
			t.Fatalf("got roots %v presets %v", got.Selection.Roots, got.Selection.Presets)
		}
		if !got.Selection.ExcludeCloudFiles || got.Selection.VSS.TimeoutS != 120 {
			t.Fatal("other selection defaults lost")
		}
		if err := got.Validate(); err != nil {
			t.Fatalf("result invalid: %v", err)
		}
	})
	t.Run("selection presets replace the default preset", func(t *testing.T) {
		got, _ := Decode(map[string]json.RawMessage{"selection": raw(`{"presets":["fixed_drives"]}`)})
		if !reflect.DeepEqual(got.Selection.Presets, []string{"fixed_drives"}) {
			t.Fatalf("got %v", got.Selection.Presets)
		}
	})
	t.Run("retention preset without keep_within takes the preset's", func(t *testing.T) {
		got, err := Decode(map[string]json.RawMessage{"retention": raw(`{"preset":"90_days"}`)})
		if err != nil {
			t.Fatal(err)
		}
		if got.Retention.Preset != "90_days" || got.Retention.KeepWithin != "90d" || got.Retention.Rules != nil {
			t.Fatalf("got %+v", got.Retention)
		}
		if err := got.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("retention rules without keep_within stays invalid instead of defaulting", func(t *testing.T) {
		got, err := Decode(map[string]json.RawMessage{"retention": raw(`{"rules":{"keep_daily":7}}`)})
		if err != nil {
			t.Fatal(err)
		}
		if got.Retention.Preset != "" || got.Retention.KeepWithin != "" {
			t.Fatalf("a default leaked into an explicit rule set: %+v", got.Retention)
		}
		if err := got.Validate(); !hasProblem(err, "retention.keep_within", "required") {
			t.Fatalf("want keep_within required, got %v", err)
		}
	})
	t.Run("retention keep_within alone keeps the default preset", func(t *testing.T) {
		got, _ := Decode(map[string]json.RawMessage{"retention": raw(`{"keep_within":"30d"}`)})
		if got.Retention.Preset != "1_year_gfs" || got.Retention.KeepWithin != "30d" {
			t.Fatalf("got %+v", got.Retention)
		}
	})
	t.Run("type errors name the section", func(t *testing.T) {
		_, err := Decode(map[string]json.RawMessage{"schedule": raw(`{"interval_minutes":"soon"}`)})
		if err == nil || !strings.Contains(err.Error(), `"schedule"`) {
			t.Fatalf("got %v", err)
		}
		if _, err := Decode(map[string]json.RawMessage{"sla": raw(`[1]`)}); err == nil {
			t.Fatal("a non-object section must fail")
		}
	})
}

func TestDecodeStrict(t *testing.T) {
	full, err := Encode(Default(ClassUnknown))
	if err != nil {
		t.Fatal(err)
	}
	with := func(mod func(map[string]json.RawMessage)) map[string]json.RawMessage {
		m := make(map[string]json.RawMessage, len(full))
		for k, v := range full {
			m[k] = v
		}
		mod(m)
		return m
	}
	tests := []struct {
		name string
		in   map[string]json.RawMessage
		want string
	}{
		{"missing section", with(func(m map[string]json.RawMessage) { delete(m, "sla") }), `missing section "sla"`},
		{"null section", with(func(m map[string]json.RawMessage) { m["sla"] = raw("null") }), `missing section "sla"`},
		{"several missing are all reported", map[string]json.RawMessage{}, `missing section "conditions"`},
		{"unknown section", with(func(m map[string]json.RawMessage) { m["extra"] = raw("{}") }), `unknown section "extra"`},
		{"unknown field", with(func(m map[string]json.RawMessage) { m["schedule"] = raw(`{"mode":"interval","bogus":1}`) }), `unknown field "bogus"`},
		{"unknown nested field", with(func(m map[string]json.RawMessage) { m["schedule"] = raw(`{"retries":{"max":1,"bogus":1}}`) }), `unknown field "bogus"`},
		{"trailing data", with(func(m map[string]json.RawMessage) { m["sla"] = raw(`{} {}`) }), "unexpected data"},
		{"wrong type", with(func(m map[string]json.RawMessage) { m["schedule"] = raw(`{"catchup":"yes"}`) }), `section "schedule"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeStrict(tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
	t.Run("nothing is defaulted", func(t *testing.T) {
		in := with(func(m map[string]json.RawMessage) { m["schedule"] = raw(`{"mode":"interval"}`) })
		got, err := DecodeStrict(in)
		if err != nil {
			t.Fatal(err)
		}
		if got.Schedule.IntervalMinutes != 0 || got.Schedule.Catchup {
			t.Fatalf("strict decode filled in defaults: %+v", got.Schedule)
		}
		if err := got.Validate(); !hasProblem(err, "schedule.interval_minutes", "between") {
			t.Fatalf("Validate must catch what strict decode left out, got %v", err)
		}
	})
	t.Run("document form", func(t *testing.T) {
		if _, err := DecodeDocument([]byte(`{"schedule":{}`), true); err == nil {
			t.Fatal("truncated document must fail")
		}
		if _, err := DecodeDocument([]byte(`[]`), false); err == nil {
			t.Fatal("array document must fail")
		}
		if _, err := DecodeDocument([]byte(`{} {}`), true); err == nil {
			t.Fatal("trailing data must fail in strict mode")
		}
		got, err := DecodeDocument([]byte(`{"schedule":{"interval_minutes":60},"unknown":{}}`), false)
		if err != nil || got.Schedule.IntervalMinutes != 60 {
			t.Fatalf("tolerant document: %v, %v", got.Schedule, err)
		}
		if _, err := DecodeDocument([]byte(`{"schedule":{"interval_minutes":60}}`), true); err == nil {
			t.Fatal("strict document with missing sections must fail")
		}
	})
}

func TestCanonicalStableAndGolden(t *testing.T) {
	p := Default(ClassLaptop)
	a, err := Canonical(p)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "canonical_default_laptop.json", append(append([]byte(nil), a...), '\n'))

	// Same content, different key order and whitespace in the source, decoded
	// tolerantly: identical canonical bytes.
	reordered := `{
	  "notification_overrides": {"digest_include": null, "failure_threshold": 0, "outdated_alarm": false},
	  "schedule": {"skip_if_running": true, "max_runtime_hours": 12, "lock_wait_s": 120, "retries": {"cap_s": 3600, "base_s": 300, "max": 3},
	               "startup_delay_s": 120, "run_on_wake": true, "catchup": true, "jitter_minutes": 30, "fixed_times": [],
	               "window": "*/0-24", "interval_minutes": 1440, "mode": "interval"},
	  "conditions": {"run_anyway_after_minutes": 240, "idle_only": false, "vpn_required": false, "wifi_ssid_block": [], "metered": "defer", "battery_floor_pct": 30}
	}`
	q, err := DecodeDocument([]byte(reordered), false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Canonical(q)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("canonical bytes differ for equal policies:\n%s\n%s", a, b)
	}
	// nil and empty slices are the same policy.
	r := p
	r.Selection.Excludes = nil
	c, _ := Canonical(r)
	if string(a) != string(c) {
		t.Fatal("nil and empty slice must canonicalise alike")
	}
	if strings.ContainsAny(string(a), " \n\t") && !strings.Contains(string(a), "User Data") {
		// The fixture has no spaces in strings, so any whitespace is formatting.
		t.Fatalf("canonical form contains whitespace: %s", a)
	}
	// Keys sorted at every level: re-sorting changes nothing.
	again, err := canonicalJSON(a)
	if err != nil || string(again) != string(a) {
		t.Fatalf("canonical form is not a fixed point: %v", err)
	}
}

// mutateLeaf changes the n-th leaf of v (counting in field order) and reports
// whether it found one. It is how TestETagChangesWithEveryField reaches every
// field without listing them.
func mutateLeaf(v reflect.Value, n *int) bool {
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if mutateLeaf(v.Field(i), n) {
				return true
			}
		}
		return false
	case reflect.Pointer:
		if *n == 0 {
			if v.IsNil() {
				v.Set(reflect.New(v.Type().Elem()))
			} else {
				return mutateLeaf(v.Elem(), n)
			}
			return true
		}
		*n--
		if !v.IsNil() {
			return mutateLeaf(v.Elem(), n)
		}
		return false
	case reflect.Slice:
		if *n == 0 {
			v.Set(reflect.Append(v, reflect.New(v.Type().Elem()).Elem()))
			if v.Type().Elem().Kind() == reflect.String {
				v.Index(v.Len() - 1).SetString("x")
			}
			return true
		}
		*n--
		return false
	case reflect.Map:
		if *n == 0 {
			if v.IsNil() {
				v.Set(reflect.MakeMap(v.Type()))
			}
			v.SetMapIndex(reflect.ValueOf(`C:\mutated`), reflect.New(v.Type().Elem()).Elem())
			return true
		}
		*n--
		return false
	}
	if *n > 0 {
		*n--
		return false
	}
	switch v.Kind() {
	case reflect.Int, reflect.Int64:
		v.SetInt(v.Int() + 1)
	case reflect.Float64:
		v.SetFloat(v.Float() + 1)
	case reflect.String:
		v.SetString(v.String() + "x")
	case reflect.Bool:
		v.SetBool(!v.Bool())
	default:
		panic("unhandled kind " + v.Kind().String())
	}
	return true
}

func TestETagChangesWithEveryField(t *testing.T) {
	base := Default(ClassLaptop)
	baseTag, err := ETag(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(baseTag) != 66 || baseTag[0] != '"' || baseTag[65] != '"' {
		t.Fatalf("ETag %q is not a quoted sha256 hex", baseTag)
	}
	if again, _ := ETag(Default(ClassLaptop)); again != baseTag {
		t.Fatal("ETag is not deterministic")
	}
	seen := map[string]int{baseTag: -1}
	leaves := 0
	for i := 0; ; i++ {
		p := base
		// Deep-copy the pieces the mutation may touch by round-tripping.
		cp, err := Encode(base)
		if err != nil {
			t.Fatal(err)
		}
		p, err = DecodeStrict(cp)
		if err != nil {
			t.Fatal(err)
		}
		n := i
		if !mutateLeaf(reflect.ValueOf(&p).Elem(), &n) {
			break
		}
		leaves++
		tag, err := ETag(p)
		if err != nil {
			t.Fatal(err)
		}
		if prev, dup := seen[tag]; dup {
			t.Fatalf("mutating leaf %d left the ETag equal to that of leaf %d", i, prev)
		}
		seen[tag] = i
	}
	// 67 = the §3.6 leaf fields: schedule 14 (retries counted as 3), conditions 6,
	// selection 11 (vss as 4), retention 4 (preset, rules, keep_within, keep_tag),
	// throttle 5 (memory_limit_mib, r209; gateway_down_grace_minutes, M1-FIX4), sla 14 (partial_tolerance as 2), maintenance 7, agent 3, notification 3.
	if leaves != 67 {
		t.Fatalf("mutated %d leaves, want 67: the walker is not reaching every field (or a field was added: update the count)", leaves)
	}
}

func TestMergePatchRFC7396(t *testing.T) {
	tests := []struct{ doc, patch, want string }{
		{`{"a":"b"}`, `{"a":"c"}`, `{"a":"c"}`},
		{`{"a":"b"}`, `{"b":"c"}`, `{"a":"b","b":"c"}`},
		{`{"a":"b"}`, `{"a":null}`, `{}`},
		{`{"a":"b","b":"c"}`, `{"a":null}`, `{"b":"c"}`},
		{`{"a":["b"]}`, `{"a":"c"}`, `{"a":"c"}`},
		{`{"a":"c"}`, `{"a":["b"]}`, `{"a":["b"]}`},
		{`{"a":{"b":"c"}}`, `{"a":{"b":"d","c":null}}`, `{"a":{"b":"d"}}`},
		{`{"a":[{"b":"c"}]}`, `{"a":[1]}`, `{"a":[1]}`},
		{`["a","b"]`, `["c","d"]`, `["c","d"]`},
		{`{"a":"b"}`, `["c"]`, `["c"]`},
		{`{"a":"foo"}`, `null`, `null`},
		{`{"a":"foo"}`, `"bar"`, `"bar"`},
		{`{"e":null}`, `{"a":1}`, `{"a":1,"e":null}`},
		{`[1,2]`, `{"a":"b","c":null}`, `{"a":"b"}`},
		{`{}`, `{"a":{"bb":{"ccc":null}}}`, `{"a":{"bb":{}}}`},
		{`{"b":1,"a":2}`, `{}`, `{"a":2,"b":1}`},                            // result is canonical: keys sorted
		{`{"n":1.50}`, `{"m":2e3}`, `{"m":2e3,"n":1.50}`},                   // numbers keep their text
		{`{"a":{"b":1}}`, `{"a":{"c":{"d":null}}}`, `{"a":{"b":1,"c":{}}}`}, // null inside a new object still drops
	}
	for _, tc := range tests {
		t.Run(tc.doc+" + "+tc.patch, func(t *testing.T) {
			got, err := MergePatch(raw(tc.doc), raw(tc.patch))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
	for _, tc := range [][2]string{{``, `{}`}, {`{`, `{}`}, {`{}`, ``}, {`{}`, `{"a":`}, {`{} x`, `{}`}, {`{}`, `{} {}`}} {
		if _, err := MergePatch(raw(tc[0]), raw(tc[1])); err == nil {
			t.Errorf("MergePatch(%q, %q) must fail", tc[0], tc[1])
		}
	}
	// The inputs are not modified.
	doc, patch := raw(`{"a":{"b":1}}`), raw(`{"a":{"b":null}}`)
	if _, err := MergePatch(doc, patch); err != nil || string(doc) != `{"a":{"b":1}}` || string(patch) != `{"a":{"b":null}}` {
		t.Fatal("MergePatch modified its inputs")
	}
}

func TestPatch(t *testing.T) {
	base := Default(ClassDesktop)

	t.Run("valid patch applies and validates", func(t *testing.T) {
		got, err := Patch(base, raw(`{"schedule":{"interval_minutes":720,"window":"Mon-Fri/8-18"},"throttle":{"upload_kibps":128}}`))
		if err != nil {
			t.Fatal(err)
		}
		if got.Schedule.IntervalMinutes != 720 || got.Schedule.Window != "Mon-Fri/8-18" || got.Throttle.Upload != 128 {
			t.Fatalf("patch not applied: %+v", got.Schedule)
		}
		if got.Schedule.Catchup != base.Schedule.Catchup || got.SLA != base.SLA {
			t.Fatal("untouched fields changed")
		}
		if base.Schedule.IntervalMinutes != 1440 {
			t.Fatal("Patch modified its input")
		}
	})
	t.Run("an array replaces", func(t *testing.T) {
		p := base
		p.Conditions.WifiSSIDBlock = []string{"a", "b"}
		got, err := Patch(p, raw(`{"conditions":{"wifi_ssid_block":["c"]}}`))
		if err != nil || !reflect.DeepEqual(got.Conditions.WifiSSIDBlock, []string{"c"}) {
			t.Fatalf("got %v, %v", got.Conditions.WifiSSIDBlock, err)
		}
	})
	t.Run("switching retention from preset to rules needs preset:null", func(t *testing.T) {
		got, err := Patch(base, raw(`{"retention":{"preset":null,"rules":{"keep_last":3,"keep_hourly":0,"keep_daily":0,"keep_weekly":0,"keep_monthly":0,"keep_yearly":0},"keep_within":"7d"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if got.Retention.Preset != "" || got.Retention.Rules == nil || got.Retention.Rules.KeepLast != 3 || got.Retention.KeepWithin != "7d" {
			t.Fatalf("got %+v", got.Retention)
		}
		_, err = Patch(base, raw(`{"retention":{"rules":{"keep_last":3}}}`))
		if !hasProblem(err, "retention", "exactly one") {
			t.Fatalf("both preset and rules must be rejected, got %v", err)
		}
	})
	t.Run("a patch that leaves the policy invalid is rejected with every problem", func(t *testing.T) {
		_, err := Patch(base, raw(`{"retention":{"keep_within":null},"schedule":{"interval_minutes":5},"sla":{"unseen_after_hours":1}}`))
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("want *ValidationError, got %v", err)
		}
		for _, path := range []string{"retention.keep_within", "schedule.interval_minutes", "sla.unseen_after_hours"} {
			if !hasProblem(err, path, "") {
				t.Errorf("missing problem at %s in:\n%v", path, err)
			}
		}
	})
	t.Run("unknown keys are rejected on write", func(t *testing.T) {
		for _, patch := range []string{`{"schedule":{"bogus":1}}`, `{"bogus":{}}`, `{"schedule":{"retries":{"x":1}}}`} {
			if _, err := Patch(base, raw(patch)); err == nil || !strings.Contains(err.Error(), "unknown") {
				t.Errorf("Patch(%s) = %v, want an unknown-key error", patch, err)
			}
		}
	})
	t.Run("deleting a section is rejected", func(t *testing.T) {
		if _, err := Patch(base, raw(`{"sla":null}`)); err == nil || !strings.Contains(err.Error(), `missing section "sla"`) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("malformed and wrongly typed patches", func(t *testing.T) {
		for _, patch := range []string{``, `{`, `[]`, `"x"`, `null`, `{"schedule":{"interval_minutes":"soon"}}`} {
			if _, err := Patch(base, raw(patch)); err == nil {
				t.Errorf("Patch(%q) must fail", patch)
			}
		}
	})
	t.Run("empty patch is a no-op", func(t *testing.T) {
		got, err := Patch(base, raw(`{}`))
		if err != nil || !reflect.DeepEqual(got, base) {
			t.Fatalf("got %v", err)
		}
	})
}
