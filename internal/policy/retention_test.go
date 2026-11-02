package policy

import (
	"errors"
	"reflect"
	"sort"
	"testing"
)

func TestRetentionPresetsMatchTheTicketTable(t *testing.T) {
	want := map[string]Rules{
		"30_days":    {KeepWithin: "30d"},
		"90_days":    {KeepWithin: "90d"},
		"1_year_gfs": {KeepDaily: 30, KeepWeekly: 13, KeepMonthly: 12, KeepWithin: "14d"},
		"7_years":    {KeepDaily: 30, KeepWeekly: 13, KeepMonthly: 24, KeepYearly: 7, KeepWithin: "14d"},
	}
	if !reflect.DeepEqual(RetentionPresets, want) {
		t.Fatalf("RetentionPresets = %+v", RetentionPresets)
	}
	names := PresetNames()
	if !sort.StringsAreSorted(names) || len(names) != 4 {
		t.Fatalf("PresetNames = %v", names)
	}
}

func TestEveryPresetValidatesAndResolves(t *testing.T) {
	for name, preset := range RetentionPresets {
		t.Run(name, func(t *testing.T) {
			r := Retention{Preset: name, KeepWithin: preset.KeepWithin}
			p := Default(ClassUnknown)
			p.Retention = r
			if err := p.Validate(); err != nil {
				t.Fatalf("preset %s does not validate: %v", name, err)
			}
			got, err := Resolve(r)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, withTags(preset, nil)) {
				t.Fatalf("Resolve = %+v, want %+v", got, preset)
			}
			if why := validKeepWithin(preset.KeepWithin); why != "" {
				t.Fatalf("preset keep_within %q is not valid: %s", preset.KeepWithin, why)
			}
		})
	}
}

func withTags(r Rules, tags []string) Rules {
	r.KeepTags = tags
	return r
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name    string
		in      Retention
		want    Rules
		wantErr string // path of the expected problem
	}{
		{"preset with tags appended", Retention{Preset: "1_year_gfs", KeepWithin: "14d", KeepTag: []string{"legal", "archive"}},
			Rules{KeepDaily: 30, KeepWeekly: 13, KeepMonthly: 12, KeepWithin: "14d", KeepTags: []string{"legal", "archive"}}, ""},
		{"the section's keep_within wins over the preset's", Retention{Preset: "30_days", KeepWithin: "90d"},
			Rules{KeepWithin: "90d", KeepTags: []string{}}, ""},
		{"explicit rules", Retention{Rules: &KeepCounts{KeepLast: 3, KeepHourly: 24, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6, KeepYearly: -1}, KeepWithin: "1y6m"},
			Rules{KeepLast: 3, KeepHourly: 24, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6, KeepYearly: -1, KeepWithin: "1y6m", KeepTags: []string{}}, ""},
		{"rules without keep_within", Retention{Rules: &KeepCounts{KeepDaily: 7}}, Rules{}, "retention.keep_within"},
		{"preset without keep_within", Retention{Preset: "30_days"}, Rules{}, "retention.keep_within"},
		{"both preset and rules", Retention{Preset: "30_days", Rules: &KeepCounts{}, KeepWithin: "30d"}, Rules{}, "retention"},
		{"neither", Retention{KeepWithin: "30d"}, Rules{}, "retention"},
		{"unknown preset", Retention{Preset: "nope", KeepWithin: "30d"}, Rules{}, "retention.preset"},
		{"bad tag", Retention{Preset: "30_days", KeepWithin: "30d", KeepTag: []string{"a,b"}}, Rules{}, "retention.keep_tag[0]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(tc.in)
			if tc.wantErr != "" {
				var ve *ValidationError
				if !errors.As(err, &ve) || !hasProblem(err, tc.wantErr, "") {
					t.Fatalf("want a ValidationError at %s, got %v", tc.wantErr, err)
				}
				if !reflect.DeepEqual(got, Rules{}) {
					t.Fatalf("an error must come with zero Rules, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.KeepWithin == "" {
				t.Fatal("Resolve returned an empty KeepWithin")
			}
			if len(got.KeepTags) == 0 && len(tc.want.KeepTags) == 0 {
				got.KeepTags, tc.want.KeepTags = nil, nil
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestResolveReturnsCopies(t *testing.T) {
	r := Retention{Preset: "1_year_gfs", KeepWithin: "14d", KeepTag: []string{"a"}}
	got, err := Resolve(r)
	if err != nil {
		t.Fatal(err)
	}
	got.KeepDaily = 999
	got.KeepTags[0] = "mutated"
	if RetentionPresets["1_year_gfs"].KeepDaily != 30 || r.KeepTag[0] != "a" {
		t.Fatal("Resolve result aliases its inputs")
	}
}

func TestKeepWithinCheck(t *testing.T) {
	for _, ok := range []string{"1d", "14d", "30d", "90d", "1y", "6m", "12h", "1y2m", "1y2m3d4h", "2m3d", "100d"} {
		if why := validKeepWithin(ok); why != "" {
			t.Errorf("%q rejected: %s", ok, why)
		}
	}
	for _, bad := range []string{"", "d", "1", "1w", "14 d", "1D", "-1d", "1d1y", "1.5d", "0d", "0y0m0d0h", "00h"} {
		if why := validKeepWithin(bad); why == "" {
			t.Errorf("%q accepted", bad)
		}
	}
}
