package state

import "testing"

// unsupported_file (M1-T3) is a tolerable per-file class like permission and
// locked: files restic cannot archive (reparse points) are expected on a
// workstation, so a partial run that carries only them is judged by the usual
// counts and does not raise PARTIAL on its own.
func TestUnsupportedFileIsTolerable(t *testing.T) {
	runClassifyCases(t, []classifyCase{
		{
			name: "M1T3_partial_only_unsupported_file_under_tolerance",
			mod: func(f *DeviceFacts) {
				graceExpired(f)
				f.LastBackup = partialRun(14, 90000, map[string]int{"unsupported_file": 14})
			},
			want: expectation{primary: StateOK, alarm: "none", attrs: []Attr{AttrPartialUnderTolerance}},
		},
		{
			name: "M1T3_partial_unsupported_file_in_error_class_field",
			mod: func(f *DeviceFacts) {
				graceExpired(f)
				f.LastBackup = partialRun(3, 90000, map[string]int{"unsupported_file": 3})
				f.LastBackup.ErrorClass = "unsupported_file"
			},
			want: expectation{primary: StateOK, alarm: "none", attrs: []Attr{AttrPartialUnderTolerance}},
		},
		{
			name: "M1T3_partial_mixed_tolerable_classes",
			mod: func(f *DeviceFacts) {
				graceExpired(f)
				f.LastBackup = partialRun(6, 90000, map[string]int{"unsupported_file": 2, "locked": 3, "permission": 1})
			},
			want: expectation{primary: StateOK, alarm: "none", attrs: []Attr{AttrPartialUnderTolerance}},
		},
		{
			name: "M1T3_partial_unsupported_file_still_bound_by_the_counts",
			mod: func(f *DeviceFacts) {
				f.LastBackup = partialRun(25, 1000, map[string]int{"unsupported_file": 25}) // 25 > 20 and 2.4 % > 0.5 %
			},
			want: expectation{primary: StatePartial, flags: []State{StatePartial}, alarm: "alarm"},
		},
		{
			name: "M1T3_partial_unsupported_file_with_missing_source_is_over",
			mod: func(f *DeviceFacts) {
				f.LastBackup = partialRun(3, 90000, map[string]int{"unsupported_file": 2, "missing_source": 1})
			},
			want: expectation{primary: StatePartial, flags: []State{StatePartial}, alarm: "alarm"},
		},
		{
			name: "M1T3_partial_unsupported_file_on_a_vss_fallback_root_is_over",
			mod: func(f *DeviceFacts) {
				f.LastBackup = partialRun(2, 90000, map[string]int{"unsupported_file": 2})
				f.LastBackup.VSSFallbackOnRoot = true
			},
			want: expectation{primary: StatePartial, flags: []State{StatePartial}, alarm: "alarm"},
		},
	})
}
