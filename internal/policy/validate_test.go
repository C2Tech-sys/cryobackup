package policy

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"testing"
	"time"
)

type validateCase struct {
	name   string
	mutate func(*Policy)
	path   string // "" = the policy must validate
	sub    string // substring of the message at path
}

func runValidateCases(t *testing.T, cases []validateCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Default(ClassUnknown)
			tc.mutate(&p)
			err := p.Validate()
			if tc.path == "" {
				if err != nil {
					t.Fatalf("want valid, got:\n%v", err)
				}
				return
			}
			if !hasProblem(err, tc.path, tc.sub) {
				t.Fatalf("want a problem at %q containing %q, got:\n%v", tc.path, tc.sub, err)
			}
			// A single mutation must produce problems in its own section only.
			section := tc.path
			if i := strings.IndexAny(section, ".["); i >= 0 {
				section = section[:i]
			}
			var ve *ValidationError
			if errors.As(err, &ve) {
				for _, pr := range ve.Problems {
					if !strings.HasPrefix(pr.Path, section) {
						t.Errorf("unrelated problem %s: %s", pr.Path, pr.Message)
					}
				}
			}
		})
	}
}

func TestValidateSchedule(t *testing.T) {
	runValidateCases(t, []validateCase{
		{"defaults", func(p *Policy) {}, "", ""},
		{"mode weekly", func(p *Policy) { p.Schedule.Mode = "weekly" }, "schedule.mode", "interval"},
		{"mode empty", func(p *Policy) { p.Schedule.Mode = "" }, "schedule.mode", ""},
		{"interval 14", func(p *Policy) { p.Schedule.IntervalMinutes = 14 }, "schedule.interval_minutes", "between 15 and 10080"},
		{"interval 15", func(p *Policy) { p.Schedule.IntervalMinutes = 15 }, "", ""},
		{"interval 10080", func(p *Policy) { p.Schedule.IntervalMinutes = 10080 }, "", ""},
		{"interval 10081", func(p *Policy) { p.Schedule.IntervalMinutes = 10081 }, "schedule.interval_minutes", ""},
		{"fixed without times", func(p *Policy) { p.Schedule.Mode = ModeFixed }, "schedule.fixed_times", "at least one"},
		{"fixed with times", func(p *Policy) { p.Schedule.Mode = ModeFixed; p.Schedule.FixedTimes = []string{"08:00", "23:59"} }, "", ""},
		{"fixed ignores interval_minutes", func(p *Policy) {
			p.Schedule.Mode = ModeFixed
			p.Schedule.FixedTimes = []string{"00:00"}
			p.Schedule.IntervalMinutes = 5
		}, "", ""},
		{"time without leading zero", func(p *Policy) { p.Schedule.FixedTimes = []string{"8:00"} }, "schedule.fixed_times[0]", "HH:MM"},
		{"time 24:00", func(p *Policy) { p.Schedule.FixedTimes = []string{"08:00", "24:00"} }, "schedule.fixed_times[1]", ""},
		{"time 12:60", func(p *Policy) { p.Schedule.FixedTimes = []string{"12:60"} }, "schedule.fixed_times[0]", ""},
		{"time with seconds", func(p *Policy) { p.Schedule.FixedTimes = []string{"08:00:00"} }, "schedule.fixed_times[0]", ""},
		{"window empty", func(p *Policy) { p.Schedule.Window = "" }, "schedule.window", "required"},
		{"window garbage", func(p *Policy) { p.Schedule.Window = "Foo/1" }, "schedule.window", "at 0"},
		{"window offset reported", func(p *Policy) { p.Schedule.Window = "Mon/8-" }, "schedule.window", "at 6: expected hour"},
		{"window ok", func(p *Policy) { p.Schedule.Window = "Mon-Fri/8-18;Sat,Sun/0-24" }, "", ""},
		{"jitter -1", func(p *Policy) { p.Schedule.JitterMinutes = -1 }, "schedule.jitter_minutes", ""},
		{"jitter 241", func(p *Policy) { p.Schedule.JitterMinutes = 241 }, "schedule.jitter_minutes", ""},
		{"jitter 0 and 240", func(p *Policy) { p.Schedule.JitterMinutes = 0 }, "", ""},
		{"jitter 240", func(p *Policy) { p.Schedule.JitterMinutes = 240 }, "", ""},
		{"startup delay -1", func(p *Policy) { p.Schedule.StartupDelayS = -1 }, "schedule.startup_delay_s", ""},
		{"startup delay 3601", func(p *Policy) { p.Schedule.StartupDelayS = 3601 }, "schedule.startup_delay_s", ""},
		{"startup delay 0", func(p *Policy) { p.Schedule.StartupDelayS = 0 }, "", ""},
		{"startup delay 3600", func(p *Policy) { p.Schedule.StartupDelayS = 3600 }, "", ""},
		{"retries max -1", func(p *Policy) { p.Schedule.Retries.Max = -1 }, "schedule.retries.max", ""},
		{"retries max 21", func(p *Policy) { p.Schedule.Retries.Max = 21 }, "schedule.retries.max", ""},
		{"retries max 0", func(p *Policy) { p.Schedule.Retries.Max = 0 }, "", ""},
		{"retries max 20", func(p *Policy) { p.Schedule.Retries.Max = 20 }, "", ""},
		{"retries base 29", func(p *Policy) { p.Schedule.Retries.BaseS = 29 }, "schedule.retries.base_s", ">= 30"},
		{"retries base 30", func(p *Policy) { p.Schedule.Retries.BaseS = 30 }, "", ""},
		{"retries cap below base", func(p *Policy) { p.Schedule.Retries.BaseS, p.Schedule.Retries.CapS = 300, 299 }, "schedule.retries.cap_s", ">= retries.base_s"},
		{"retries cap equals base", func(p *Policy) { p.Schedule.Retries.BaseS, p.Schedule.Retries.CapS = 300, 300 }, "", ""},
		{"lock wait -1", func(p *Policy) { p.Schedule.LockWaitS = -1 }, "schedule.lock_wait_s", ""},
		{"lock wait 3601", func(p *Policy) { p.Schedule.LockWaitS = 3601 }, "schedule.lock_wait_s", ""},
		{"lock wait 0", func(p *Policy) { p.Schedule.LockWaitS = 0 }, "", ""},
		{"lock wait 3600", func(p *Policy) { p.Schedule.LockWaitS = 3600 }, "", ""},
		{"max runtime 0", func(p *Policy) { p.Schedule.MaxRuntimeHours = 0 }, "schedule.max_runtime_hours", ""},
		{"max runtime 169", func(p *Policy) { p.Schedule.MaxRuntimeHours = 169 }, "schedule.max_runtime_hours", ""},
		{"max runtime 1", func(p *Policy) { p.Schedule.MaxRuntimeHours = 1 }, "", ""},
		{"max runtime 168", func(p *Policy) { p.Schedule.MaxRuntimeHours = 168 }, "", ""},
	})
}

func TestValidateConditions(t *testing.T) {
	runValidateCases(t, []validateCase{
		{"battery -1", func(p *Policy) { p.Conditions.BatteryFloorPct = -1 }, "conditions.battery_floor_pct", ""},
		{"battery 101", func(p *Policy) { p.Conditions.BatteryFloorPct = 101 }, "conditions.battery_floor_pct", ""},
		{"battery 0", func(p *Policy) { p.Conditions.BatteryFloorPct = 0 }, "", ""},
		{"battery 100", func(p *Policy) { p.Conditions.BatteryFloorPct = 100 }, "", ""},
		{"metered maybe", func(p *Policy) { p.Conditions.Metered = "maybe" }, "conditions.metered", "defer"},
		{"metered empty", func(p *Policy) { p.Conditions.Metered = "" }, "conditions.metered", ""},
		{"metered allow", func(p *Policy) { p.Conditions.Metered = "allow" }, "", ""},
		{"run anyway -1", func(p *Policy) { p.Conditions.RunAnywayAfterMinutes = -1 }, "conditions.run_anyway_after_minutes", ""},
		{"run anyway 10081", func(p *Policy) { p.Conditions.RunAnywayAfterMinutes = 10081 }, "conditions.run_anyway_after_minutes", ""},
		{"run anyway 0", func(p *Policy) { p.Conditions.RunAnywayAfterMinutes = 0 }, "", ""},
		{"run anyway 10080", func(p *Policy) { p.Conditions.RunAnywayAfterMinutes = 10080 }, "", ""},
		{"empty ssid", func(p *Policy) { p.Conditions.WifiSSIDBlock = []string{"Guest", ""} }, "conditions.wifi_ssid_block[1]", "empty"},
		{"blank ssid", func(p *Policy) { p.Conditions.WifiSSIDBlock = []string{"  "} }, "conditions.wifi_ssid_block[0]", ""},
		{"ssids ok", func(p *Policy) { p.Conditions.WifiSSIDBlock = []string{"Guest", "Hotel WiFi"} }, "", ""},
	})
}

func TestValidateSelection(t *testing.T) {
	runValidateCases(t, []validateCase{
		{"nothing selected", func(p *Policy) { p.Selection.Presets = nil }, "selection", "at least one root or preset"},
		{"roots only", func(p *Policy) { p.Selection.Presets = nil; p.Selection.Roots = []string{`C:\Data`} }, "", ""},
		{"unc root", func(p *Policy) { p.Selection.Roots = []string{`\\srv\share`, `\\srv\share\dir`} }, "", ""},
		{"drive root", func(p *Policy) { p.Selection.Roots = []string{`D:\`} }, "", ""},
		{"relative root", func(p *Policy) { p.Selection.Roots = []string{`Documents`} }, "selection.roots[0]", "absolute Windows path"},
		{"forward slash root", func(p *Policy) { p.Selection.Roots = []string{`C:\ok`, `C:/Data`} }, "selection.roots[1]", ""},
		{"unc without share", func(p *Policy) { p.Selection.Roots = []string{`\\srv`} }, "selection.roots[0]", ""},
		{"unknown preset", func(p *Policy) { p.Selection.Presets = []string{"everything"} }, "selection.presets[0]", "all_user_profiles"},
		{"known presets", func(p *Policy) {
			p.Selection.Presets = []string{"all_user_profiles", "documents_desktop", "fixed_drives"}
		}, "", ""},
		{"empty include", func(p *Policy) { p.Selection.Includes = []string{""} }, "selection.includes[0]", "empty"},
		{"include ok", func(p *Policy) { p.Selection.Includes = []string{`C:\Data\*.docx`} }, "", ""},
		{"empty exclude", func(p *Policy) { p.Selection.Excludes = []string{"*.tmp", " "} }, "selection.excludes[1]", "empty"},
		{"exclude comment", func(p *Policy) { p.Selection.Excludes = []string{"# not a pattern"} }, "selection.excludes[0]", "comment"},
		{"exclude newline", func(p *Policy) { p.Selection.Excludes = []string{"a\nb"} }, "selection.excludes[0]", "single line"},
		{"exclude unknown preset", func(p *Policy) { p.Selection.Excludes = []string{"@nope"} }, "selection.excludes[0]", "vm_images"},
		{"exclude known preset", func(p *Policy) { p.Selection.Excludes = []string{"@vm_images", "@installers"} }, "", ""},
		{"exclude re-include", func(p *Policy) { p.Selection.Excludes = []string{`!C:\Data\keep.tmp`} }, "", ""},
		{"path flags key not a root", func(p *Policy) {
			p.Selection.Roots = []string{`C:\Data`}
			p.Selection.PathFlags = map[string]PathFlags{`C:\Other`: {Optional: true}}
		}, `selection.path_flags[C:\Other]`, "listed roots"},
		{"path flags key for a preset-only selection", func(p *Policy) {
			p.Selection.PathFlags = map[string]PathFlags{`C:\Users\fixture`: {Optional: true}}
		}, `selection.path_flags[C:\Users\fixture]`, ""},
		{"path flags required and optional", func(p *Policy) {
			p.Selection.Roots = []string{`C:\Data`}
			p.Selection.PathFlags = map[string]PathFlags{`C:\Data`: {Required: true, Optional: true}}
		}, `selection.path_flags[C:\Data]`, "both required and optional"},
		{"path flags key matches case-insensitively and ignores a trailing backslash", func(p *Policy) {
			p.Selection.Roots = []string{`C:\Data\`}
			p.Selection.PathFlags = map[string]PathFlags{`c:\DATA`: {Optional: true, RequireSnapshot: true, FollowSymlinks: true}}
		}, "", ""},
		{"vss timeout 9", func(p *Policy) { p.Selection.VSS.TimeoutS = 9 }, "selection.vss.timeout_s", "between 10 and 3600"},
		{"vss timeout 3601", func(p *Policy) { p.Selection.VSS.TimeoutS = 3601 }, "selection.vss.timeout_s", ""},
		{"vss timeout 10", func(p *Policy) { p.Selection.VSS.TimeoutS = 10 }, "", ""},
		{"vss timeout 3600", func(p *Policy) { p.Selection.VSS.TimeoutS = 3600 }, "", ""},
		{"vss disabled still needs a sane timeout", func(p *Policy) { p.Selection.VSS.Enabled = false; p.Selection.VSS.TimeoutS = 0 }, "selection.vss.timeout_s", ""},
		{"vss empty volume", func(p *Policy) { p.Selection.VSS.ExcludeVolumes = []string{"D:", ""} }, "selection.vss.exclude_volumes[1]", ""},
		{"larger than bad suffix", func(p *Policy) { p.Selection.ExcludeLargerThan = "10x" }, "selection.exclude_larger_than", "size"},
		{"larger than negative", func(p *Policy) { p.Selection.ExcludeLargerThan = "-1" }, "selection.exclude_larger_than", ""},
		{"larger than decimal", func(p *Policy) { p.Selection.ExcludeLargerThan = "1.5g" }, "selection.exclude_larger_than", ""},
		{"larger than bytes", func(p *Policy) { p.Selection.ExcludeLargerThan = "5000" }, "", ""},
		{"larger than suffixes", func(p *Policy) { p.Selection.ExcludeLargerThan = "2G" }, "", ""},
		{"larger than k", func(p *Policy) { p.Selection.ExcludeLargerThan = "100k" }, "", ""},
		{"larger than t", func(p *Policy) { p.Selection.ExcludeLargerThan = "1t" }, "", ""},
	})
}

func TestValidateRetention(t *testing.T) {
	runValidateCases(t, []validateCase{
		{"preset form", func(p *Policy) { p.Retention = Retention{Preset: "30_days", KeepWithin: "30d"} }, "", ""},
		{"rules form", func(p *Policy) {
			p.Retention = Retention{Rules: &KeepCounts{KeepDaily: 7, KeepWeekly: -1}, KeepWithin: "1y2m3d4h", KeepTag: []string{"legal-hold"}}
		}, "", ""},
		{"neither", func(p *Policy) { p.Retention = Retention{KeepWithin: "14d"} }, "retention", "exactly one"},
		{"both", func(p *Policy) { p.Retention = Retention{Preset: "30_days", Rules: &KeepCounts{}, KeepWithin: "14d"} }, "retention", "not both"},
		{"unknown preset", func(p *Policy) { p.Retention = Retention{Preset: "forever", KeepWithin: "14d"} }, "retention.preset", "30_days"},
		{"preset without keep_within", func(p *Policy) { p.Retention = Retention{Preset: "30_days"} }, "retention.keep_within", "required"},
		{"rules without keep_within", func(p *Policy) { p.Retention = Retention{Rules: &KeepCounts{KeepDaily: 7}} }, "retention.keep_within", "required"},
		{"keep_within words", func(p *Policy) { p.Retention.KeepWithin = "14days" }, "retention.keep_within", "1y2m3d4h"},
		{"keep_within wrong order", func(p *Policy) { p.Retention.KeepWithin = "1d1y" }, "retention.keep_within", ""},
		{"keep_within zero", func(p *Policy) { p.Retention.KeepWithin = "0d" }, "retention.keep_within", "greater than zero"},
		{"keep_within zeros", func(p *Policy) { p.Retention.KeepWithin = "0y0m0d0h" }, "retention.keep_within", "greater than zero"},
		{"keep_within spaces", func(p *Policy) { p.Retention.KeepWithin = " 14d" }, "retention.keep_within", ""},
		{"keep_within hours only", func(p *Policy) { p.Retention.KeepWithin = "12h" }, "", ""},
		{"keep_within 10d", func(p *Policy) { p.Retention.KeepWithin = "10d" }, "", ""},
		{"count -2", func(p *Policy) {
			p.Retention = Retention{Rules: &KeepCounts{KeepYearly: -2}, KeepWithin: "14d"}
		}, "retention.rules.keep_yearly", "-1"},
		{"each count -2", func(p *Policy) {
			p.Retention = Retention{Rules: &KeepCounts{KeepLast: -2, KeepHourly: -2, KeepDaily: -2, KeepWeekly: -2, KeepMonthly: -2}, KeepWithin: "14d"}
		}, "retention.rules.keep_monthly", ""},
		{"tag with comma", func(p *Policy) { p.Retention.KeepTag = []string{"a,b"} }, "retention.keep_tag[0]", "comma"},
		{"empty tag", func(p *Policy) { p.Retention.KeepTag = []string{"ok", ""} }, "retention.keep_tag[1]", "empty"},
		{"tags ok", func(p *Policy) { p.Retention.KeepTag = []string{"legal", "archive 2026"} }, "", ""},
	})
}

func TestValidateThrottle(t *testing.T) {
	runValidateCases(t, []validateCase{
		{"upload -1", func(p *Policy) { p.Throttle.Upload = -1 }, "throttle.upload_kibps", ""},
		{"download -1", func(p *Policy) { p.Throttle.Download = -1 }, "throttle.download_kibps", ""},
		{"limits ok", func(p *Policy) { p.Throttle.Upload, p.Throttle.Download = 0, 5000 }, "", ""},
		{"window bad when", func(p *Policy) { p.Throttle.Windows = []ThrottleWindow{{When: "nope", Up: 1, Down: 1}} }, "throttle.windows[0].when", "invalid window"},
		{"window empty when", func(p *Policy) { p.Throttle.Windows = []ThrottleWindow{{When: "", Up: 1, Down: 1}} }, "throttle.windows[0].when", ""},
		{"window up -1", func(p *Policy) { p.Throttle.Windows = []ThrottleWindow{{When: "*/0-24", Up: -1}} }, "throttle.windows[0].up", ""},
		{"window down -1", func(p *Policy) { p.Throttle.Windows = []ThrottleWindow{{When: "*/0-24", Down: -1}} }, "throttle.windows[0].down", ""},
		{"second window bad", func(p *Policy) {
			p.Throttle.Windows = []ThrottleWindow{{When: "Mon/8-9"}, {When: "Mon/8-99"}}
		}, "throttle.windows[1].when", "at 6"},
		{"windows ok", func(p *Policy) {
			p.Throttle.Windows = []ThrottleWindow{{When: "Mon-Fri/8-18", Up: 10, Down: 10}, {When: "Sat,Sun/0-24", Up: 0, Down: 0}}
		}, "", ""},
		// r209 memory cap: 0 auto, -1 none, otherwise 512 MiB .. 1 TiB.
		{"memory auto", func(p *Policy) { p.Throttle.MemoryLimitMiB = 0 }, "", ""},
		{"memory none", func(p *Policy) { p.Throttle.MemoryLimitMiB = -1 }, "", ""},
		{"memory min", func(p *Policy) { p.Throttle.MemoryLimitMiB = 512 }, "", ""},
		{"memory max", func(p *Policy) { p.Throttle.MemoryLimitMiB = 1 << 20 }, "", ""},
		{"memory below min", func(p *Policy) { p.Throttle.MemoryLimitMiB = 511 }, "throttle.memory_limit_mib", "between 512 and"},
		{"memory one", func(p *Policy) { p.Throttle.MemoryLimitMiB = 1 }, "throttle.memory_limit_mib", "0 (auto), -1 (no cap)"},
		{"memory -2", func(p *Policy) { p.Throttle.MemoryLimitMiB = -2 }, "throttle.memory_limit_mib", ""},
		{"memory above max", func(p *Policy) { p.Throttle.MemoryLimitMiB = 1<<20 + 1 }, "throttle.memory_limit_mib", ""},
		// M1-FIX4 gateway watchdog grace: 0 = the default, otherwise 5..120 minutes.
		{"gateway grace default", func(p *Policy) { p.Throttle.GatewayDownGraceMinutes = 0 }, "", ""},
		{"gateway grace min", func(p *Policy) { p.Throttle.GatewayDownGraceMinutes = 5 }, "", ""},
		{"gateway grace max", func(p *Policy) { p.Throttle.GatewayDownGraceMinutes = 120 }, "", ""},
		{"gateway grace below min", func(p *Policy) { p.Throttle.GatewayDownGraceMinutes = 4 }, "throttle.gateway_down_grace_minutes", "between 5 and 120"},
		{"gateway grace above max", func(p *Policy) { p.Throttle.GatewayDownGraceMinutes = 121 }, "throttle.gateway_down_grace_minutes", "0 (the default, 15)"},
		{"gateway grace negative", func(p *Policy) { p.Throttle.GatewayDownGraceMinutes = -1 }, "throttle.gateway_down_grace_minutes", ""},
	})
}

func TestMemoryCapMiB(t *testing.T) {
	tests := []struct {
		name     string
		setting  int
		physical int64
		want     int
	}{
		{"auto, 16 GiB machine: half", MemoryLimitAuto, 16384, 8192},
		{"auto, 8 GiB laptop: half", MemoryLimitAuto, 8192, 4096},
		{"auto, 2 GiB: the floor", MemoryLimitAuto, 2048, 1024},
		{"auto, 1 GiB: the floor, not below", MemoryLimitAuto, 1024, 1024},
		{"auto, odd size rounds down", MemoryLimitAuto, 16383, 8191},
		{"auto, unknown memory: no cap rather than a guess", MemoryLimitAuto, 0, 0},
		{"auto, absurd memory: bounded", MemoryLimitAuto, 4 << 20, MemoryLimitMaxMiB},
		{"none", MemoryLimitNone, 16384, 0},
		{"explicit wins", 3000, 16384, 3000},
		{"explicit above physical is honoured", 32768, 16384, 32768},
		{"explicit with unknown memory", 2048, 0, 2048},
	}
	for _, tt := range tests {
		if got := (Throttle{MemoryLimitMiB: tt.setting}).MemoryCapMiB(tt.physical); got != tt.want {
			t.Errorf("%s: MemoryCapMiB(%d) with %d = %d, want %d", tt.name, tt.physical, tt.setting, got, tt.want)
		}
	}
	if d := Default(ClassLaptop).Throttle.MemoryLimitMiB; d != MemoryLimitAuto {
		t.Errorf("default memory_limit_mib = %d, want auto", d)
	}
	// A stored policy written before the field existed decodes as auto.
	p, err := Decode(map[string]json.RawMessage{"throttle": json.RawMessage(`{"upload_kibps":10,"download_kibps":0,"windows":[]}`)})
	if err != nil || p.Throttle.MemoryLimitMiB != MemoryLimitAuto || p.Throttle.Upload != 10 {
		t.Errorf("old throttle section: %+v, %v", p.Throttle, err)
	}
}

// M1-FIX4: the gateway watchdog's grace. The default is 15 minutes, and a policy
// stored before the field existed means the default, through either decoder:
// the console's tolerant Decode keeps the default, and the agent's plain
// json.Unmarshal of a stored policy response reads 0, which GatewayDownGrace
// turns into the default.
func TestGatewayDownGrace(t *testing.T) {
	tests := []struct {
		setting int
		want    time.Duration
	}{
		{0, 15 * time.Minute}, {5, 5 * time.Minute}, {15, 15 * time.Minute}, {120, 120 * time.Minute},
		{1, 5 * time.Minute}, {-3, 5 * time.Minute}, {500, 120 * time.Minute}, // outside Validate's range: clamped, never "off"
	}
	for _, tt := range tests {
		if got := (Throttle{GatewayDownGraceMinutes: tt.setting}).GatewayDownGrace(); got != tt.want {
			t.Errorf("GatewayDownGrace(%d) = %v, want %v", tt.setting, got, tt.want)
		}
	}
	for _, c := range []DeviceClass{ClassUnknown, ClassLaptop, ClassDesktop} {
		if d := Default(c).Throttle.GatewayDownGraceMinutes; d != GatewayDownGraceDefaultMinutes {
			t.Errorf("default gateway_down_grace_minutes (%q) = %d, want %d", c, d, GatewayDownGraceDefaultMinutes)
		}
	}
	old := `{"upload_kibps":10,"download_kibps":0,"windows":[],"memory_limit_mib":0}`
	p, err := Decode(map[string]json.RawMessage{"throttle": json.RawMessage(old)})
	if err != nil || p.Throttle.GatewayDownGrace() != 15*time.Minute || p.Validate() != nil {
		t.Errorf("old throttle section, tolerant decode: %+v, %v", p.Throttle, err)
	}
	var raw Throttle
	if err := json.Unmarshal([]byte(old), &raw); err != nil || raw.GatewayDownGraceMinutes != 0 || raw.GatewayDownGrace() != 15*time.Minute {
		t.Errorf("old throttle section, plain unmarshal: %+v, %v", raw, err)
	}
}

func TestValidateSLA(t *testing.T) {
	cases := []validateCase{
		{"stale 0", func(p *Policy) { p.SLA.StaleAfterHours = 0 }, "sla.stale_after_hours", ">= 1"},
		{"stale 1 unseen 1", func(p *Policy) { p.SLA.StaleAfterHours, p.SLA.UnseenAfterHours = 1, 1 }, "", ""},
		{"unseen below stale", func(p *Policy) { p.SLA.StaleAfterHours, p.SLA.UnseenAfterHours = 100, 99 }, "sla.unseen_after_hours", "a device must be stale before it can be unseen"},
		{"unseen equals stale", func(p *Policy) { p.SLA.StaleAfterHours, p.SLA.UnseenAfterHours = 72, 72 }, "", ""},
		{"pct negative", func(p *Policy) { p.SLA.PartialTolerance.MaxFailedPct = -0.1 }, "sla.partial_tolerance.max_failed_pct", "0 and 100"},
		{"pct over", func(p *Policy) { p.SLA.PartialTolerance.MaxFailedPct = 100.1 }, "sla.partial_tolerance.max_failed_pct", ""},
		{"pct NaN", func(p *Policy) { p.SLA.PartialTolerance.MaxFailedPct = math.NaN() }, "sla.partial_tolerance.max_failed_pct", ""},
		{"pct 0", func(p *Policy) { p.SLA.PartialTolerance.MaxFailedPct = 0 }, "", ""},
		{"pct 100", func(p *Policy) { p.SLA.PartialTolerance.MaxFailedPct = 100 }, "", ""},
		{"consecutive 0", func(p *Policy) { p.SLA.ConsecutiveFailuresToAlarm = 0 }, "sla.consecutive_failures_to_alarm", ">= 1"},
		{"initial below max", func(p *Policy) { p.SLA.MaxRunHours, p.SLA.InitialMaxRunHours = 12, 11 }, "sla.initial_max_run_hours", ">= max_run_hours"},
		{"initial equals max", func(p *Policy) { p.SLA.MaxRunHours, p.SLA.InitialMaxRunHours = 12, 12 }, "", ""},
		{"missed factor below 1", func(p *Policy) { p.SLA.MissedFactor = 0.99 }, "sla.missed_factor", ">= 1"},
		{"missed factor NaN", func(p *Policy) { p.SLA.MissedFactor = math.NaN() }, "sla.missed_factor", ""},
		{"missed factor 1", func(p *Policy) { p.SLA.MissedFactor = 1 }, "", ""},
	}
	negatives := map[string]func(*Policy){
		"first_backup_grace_hours":           func(p *Policy) { p.SLA.FirstBackupGraceHours = -1 },
		"seed_extension_hours":               func(p *Policy) { p.SLA.SeedExtensionHours = -1 },
		"partial_tolerance.max_failed_files": func(p *Policy) { p.SLA.PartialTolerance.MaxFailedFiles = -1 },
		"max_run_hours":                      func(p *Policy) { p.SLA.MaxRunHours = -1 },
		"stall_after_minutes":                func(p *Policy) { p.SLA.StallAfterMinutes = -1 },
		"escrow_verify_grace_hours":          func(p *Policy) { p.SLA.EscrowVerifyGraceHours = -1 },
		"escrow_recheck_days":                func(p *Policy) { p.SLA.EscrowRecheckDays = -1 },
		"maintenance_deferral_alarm_after":   func(p *Policy) { p.SLA.MaintenanceDeferralAlarmAfter = -1 },
	}
	names := make([]string, 0, len(negatives))
	for n := range negatives {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		cases = append(cases, validateCase{n + " -1", negatives[n], "sla." + n, ">= 0"})
	}
	cases = append(cases,
		validateCase{"zeros allowed where 0 means off", func(p *Policy) {
			p.SLA.FirstBackupGraceHours, p.SLA.SeedExtensionHours, p.SLA.StallAfterMinutes = 0, 0, 0
			p.SLA.PartialTolerance.MaxFailedFiles, p.SLA.EscrowRecheckDays = 0, 0
		}, "", ""})
	runValidateCases(t, cases)
}

func TestValidateMaintenance(t *testing.T) {
	runValidateCases(t, []validateCase{
		{"forget 0", func(p *Policy) { p.Maintenance.ForgetEveryDays = 0 }, "maintenance.forget_every_days", ">= 1"},
		{"prune 0", func(p *Policy) { p.Maintenance.PruneEveryDays = 0 }, "maintenance.prune_every_days", ">= 1"},
		{"check 0", func(p *Policy) { p.Maintenance.CheckEveryDays = 0 }, "maintenance.check_every_days", ">= 1"},
		{"cadences 1", func(p *Policy) {
			p.Maintenance.ForgetEveryDays, p.Maintenance.PruneEveryDays, p.Maintenance.CheckEveryDays = 1, 1, 1
		}, "", ""},
		{"max_unused percent", func(p *Policy) { p.Maintenance.MaxUnused = "20%" }, "", ""},
		{"max_unused decimal percent", func(p *Policy) { p.Maintenance.MaxUnused = "2.5%" }, "", ""},
		{"max_unused 100%", func(p *Policy) { p.Maintenance.MaxUnused = "100%" }, "", ""},
		{"max_unused unlimited", func(p *Policy) { p.Maintenance.MaxUnused = "unlimited" }, "", ""},
		{"max_unused size", func(p *Policy) { p.Maintenance.MaxUnused = "10g" }, "", ""},
		{"max_unused 101%", func(p *Policy) { p.Maintenance.MaxUnused = "101%" }, "maintenance.max_unused", "unlimited"},
		{"max_unused word", func(p *Policy) { p.Maintenance.MaxUnused = "five" }, "maintenance.max_unused", ""},
		{"max_unused empty", func(p *Policy) { p.Maintenance.MaxUnused = "" }, "maintenance.max_unused", ""},
		{"max_unused negative", func(p *Policy) { p.Maintenance.MaxUnused = "-5%" }, "maintenance.max_unused", ""},
		{"subset fraction", func(p *Policy) { p.Maintenance.ReadDataSubset = "1/10" }, "", ""},
		{"subset whole", func(p *Policy) { p.Maintenance.ReadDataSubset = "10/10" }, "", ""},
		{"subset percent", func(p *Policy) { p.Maintenance.ReadDataSubset = "2.5%" }, "", ""},
		{"subset size", func(p *Policy) { p.Maintenance.ReadDataSubset = "500m" }, "", ""},
		{"subset zero numerator", func(p *Policy) { p.Maintenance.ReadDataSubset = "0/10" }, "maintenance.read_data_subset", "n/t"},
		{"subset n above t", func(p *Policy) { p.Maintenance.ReadDataSubset = "11/10" }, "maintenance.read_data_subset", ""},
		{"subset zero percent", func(p *Policy) { p.Maintenance.ReadDataSubset = "0%" }, "maintenance.read_data_subset", ""},
		{"subset 101%", func(p *Policy) { p.Maintenance.ReadDataSubset = "101%" }, "maintenance.read_data_subset", ""},
		{"subset word", func(p *Policy) { p.Maintenance.ReadDataSubset = "some" }, "maintenance.read_data_subset", ""},
		{"subset empty", func(p *Policy) { p.Maintenance.ReadDataSubset = "" }, "maintenance.read_data_subset", ""},
		{"window hint bad", func(p *Policy) { p.Maintenance.WindowHint = "Mon/" }, "maintenance.window_hint", "at 4"},
		{"window hint ok", func(p *Policy) { p.Maintenance.WindowHint = "*/0-5" }, "", ""},
		{"window hint empty is ok", func(p *Policy) { p.Maintenance.WindowHint = "" }, "", ""},
	})
}

func TestValidateAgentAndNotification(t *testing.T) {
	yes := true
	runValidateCases(t, []validateCase{
		{"agent permissions all on", func(p *Policy) {
			p.AgentPermissions = AgentPermissions{true, true, true}
		}, "", ""},
		{"agent permissions all off", func(p *Policy) { p.AgentPermissions = AgentPermissions{} }, "", ""},
		{"failure threshold -1", func(p *Policy) { p.NotificationOverrides.FailureThreshold = -1 }, "notification_overrides.failure_threshold", ">= 1"},
		{"failure threshold 0 inherits", func(p *Policy) { p.NotificationOverrides.FailureThreshold = 0 }, "", ""},
		{"failure threshold 1", func(p *Policy) { p.NotificationOverrides.FailureThreshold = 1 }, "", ""},
		{"overrides set", func(p *Policy) {
			p.NotificationOverrides = NotificationOverrides{OutdatedAlarm: true, FailureThreshold: 5, DigestInclude: &yes}
		}, "", ""},
	})
}

func TestValidationErrorShape(t *testing.T) {
	if err := Default(ClassLaptop).Validate(); err != nil {
		t.Fatalf("a valid policy must return a nil error interface, got %v", err)
	}
	p := Default(ClassUnknown)
	p.Schedule.JitterMinutes = 999
	p.Retention.KeepWithin = ""
	p.Conditions.Metered = "x"
	p.Throttle.Windows = []ThrottleWindow{{When: "bad"}}
	p.Selection.Roots = []string{"rel"}
	err := p.Validate()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want *ValidationError, got %T", err)
	}
	if len(ve.Problems) != 5 {
		t.Fatalf("got %d problems, want 5:\n%v", len(ve.Problems), err)
	}
	paths := make([]string, len(ve.Problems))
	for i, pr := range ve.Problems {
		paths[i] = pr.Path
		if pr.Message == "" {
			t.Errorf("problem at %s has no message", pr.Path)
		}
	}
	if !sort.StringsAreSorted(paths) {
		t.Fatalf("problems are not sorted by path: %v", paths)
	}
	lines := strings.Split(err.Error(), "\n")
	if len(lines) != 5 {
		t.Fatalf("Error() has %d lines, want one per problem:\n%v", len(lines), err)
	}
	for i, l := range lines {
		if !strings.HasPrefix(l, paths[i]+": ") {
			t.Errorf("line %d = %q, want prefix %q", i, l, paths[i]+": ")
		}
	}
	// A wrapped error is still reachable.
	wrapped := errors.Join(errors.New("context"), err)
	if !errors.As(wrapped, &ve) {
		t.Error("errors.As must see through wrapping")
	}
	// Validating the zero value reports a lot, never panics, stays sorted.
	zero := (Policy{}).Validate()
	if !errors.As(zero, &ve) || len(ve.Problems) < 10 {
		t.Fatalf("zero policy must be invalid in many ways, got %v", zero)
	}
	paths = paths[:0]
	for _, pr := range ve.Problems {
		paths = append(paths, pr.Path)
	}
	if !sort.StringsAreSorted(paths) {
		t.Fatal("zero-policy problems not sorted")
	}
}
