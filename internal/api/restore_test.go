package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

func TestValidateRestoreSelection(t *testing.T) {
	paths := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("/C/Users/x/f%03d.txt", i)
		}
		return out
	}
	runValidate(t, okRestoreSelection, []vcase[RestoreSelection]{
		{"valid (original, if_changed)", nil, nil},
		{"original, never", func(s *RestoreSelection) { s.Overwrite = DeviceOverwriteNever }, nil},
		{"side, never", func(s *RestoreSelection) { s.Mode, s.Overwrite = DeviceRestoreSide, DeviceOverwriteNever }, nil},
		{"id lower case", func(s *RestoreSelection) { s.ID = strings.ToLower(s.ID) }, []string{"id"}},
		{"id empty", func(s *RestoreSelection) { s.ID = "" }, []string{"id"}},
		{"snapshot short", func(s *RestoreSelection) { s.SnapshotID = hex6[:63] }, []string{"snapshot_id"}},
		{"snapshot upper case", func(s *RestoreSelection) { s.SnapshotID = strings.ToUpper(hex6) }, []string{"snapshot_id"}},
		{"mode empty", func(s *RestoreSelection) { s.Mode = "" }, []string{"mode"}},
		{"mode is the stored name", func(s *RestoreSelection) { s.Mode = DeviceRestoreMode(RestoreDevicePath) }, []string{"mode"}},
		{"mode relay", func(s *RestoreSelection) { s.Mode = DeviceRestoreMode(RestoreDeviceRelay) }, []string{"mode"}},
		{"overwrite is the stored name", func(s *RestoreSelection) { s.Overwrite = DeviceRestoreOverwrite(OverwriteIfChanged) }, []string{"overwrite"}},
		{"overwrite always", func(s *RestoreSelection) { s.Overwrite = "always" }, []string{"overwrite"}},
		{"overwrite empty", func(s *RestoreSelection) { s.Overwrite = "" }, []string{"overwrite"}},
		{"side with if_changed", func(s *RestoreSelection) { s.Mode = DeviceRestoreSide }, []string{"overwrite"}},
		{"no paths", func(s *RestoreSelection) { s.Paths = nil }, []string{"paths"}},
		{"empty list", func(s *RestoreSelection) { s.Paths = []string{} }, []string{"paths"}},
		{"100 paths", func(s *RestoreSelection) { s.Paths = paths(MaxRestorePaths) }, nil},
		{"101 paths", func(s *RestoreSelection) { s.Paths = paths(MaxRestorePaths + 1) }, []string{"paths"}},
		{"second path bad", func(s *RestoreSelection) { s.Paths[1] = "Users/x" }, []string{"paths[1]"}},
		{"two bad paths", func(s *RestoreSelection) { s.Paths = []string{"x", "/C/../y"} }, []string{"paths[0]", "paths[1]"}},
		{"total too large", func(s *RestoreSelection) {
			long := "/C/" + strings.Repeat("a", MaxRestorePathBytes-3)
			s.Paths = make([]string, 17) // 17 x 4096 > 64 KiB, each one alone is fine
			for i := range s.Paths {
				s.Paths[i] = long
			}
		}, []string{"paths"}},
		{"expires zero", func(s *RestoreSelection) { s.ExpiresAt = time.Time{} }, []string{"expires_at"}},
		{"expires before 2001", func(s *RestoreSelection) { s.ExpiresAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC) }, []string{"expires_at"}},
	})
}

// The limits are exact: at the bound passes, one over fails, and the reason is a
// fixed phrase that never holds the path.
func TestRestorePathLimits(t *testing.T) {
	atMax := "/C/" + strings.Repeat("a", MaxRestorePathBytes-3)
	if err := ValidateRestorePath(atMax); err != nil {
		t.Errorf("a path of exactly %d bytes: %v", MaxRestorePathBytes, err)
	}
	if err := ValidateRestorePath(atMax + "a"); err == nil || !strings.Contains(err.Error(), msgTooLong) {
		t.Errorf("a path of %d bytes: %v", MaxRestorePathBytes+1, err)
	}
	// 16 x 4096 = 64 KiB exactly: passes; one more byte does not.
	sel := okRestoreSelection()
	sel.Paths = make([]string, 16)
	for i := range sel.Paths {
		sel.Paths[i] = atMax
	}
	if err := sel.Validate(); err != nil {
		t.Errorf("exactly %d total bytes: %v", MaxRestorePathsTotalBytes, err)
	}
	sel.Paths[15] = atMax[:len(atMax)-1] + "ab" // one byte over the total, the path itself 4097 bytes
	if sel.Validate() == nil {
		t.Error("one byte over the total passed")
	}
}

func TestRestorePathRules(t *testing.T) {
	good := []string{
		"/C/a",
		"/C/Users/x/Documents/a.docx",
		"/c/lower/case/drive",                // restic matches case-sensitively; the letter is kept as given
		"/D/Data/[a] $b/Report 2026.docx",    // [ and $ are allowed: the agent escapes them
		"/C/dir with spaces/in the middle/f", // interior spaces are fine
		"/C/Ünïcödé/日本語/файл.txt",
		"/C/a..b/..c/d..",                  // ".." inside a name is a name
		"/C/%APPDATA%/100%/#hash/a,b;c=d!", // not special to restic's include file (spike item 3)
		"/C/" + strings.Repeat("n", 255),
	}
	for _, p := range good {
		if err := ValidateRestorePath(p); err != nil {
			t.Errorf("%.40q refused: %v", p, err)
		}
	}
	bad := map[string]string{
		"":                "empty",
		"C/Users/x":       "relative, no leading slash",
		"Users/x/a.docx":  "relative",
		"./C/x":           "dot relative",
		`C:\Users\x`:      "windows form",
		`/C/Users\x`:      "backslash",
		"/C":              "drive root",
		"/C/":             "drive root with slash",
		"/":               "root",
		"//C/x":           "empty first segment",
		"/CC/x":           "two-letter drive",
		"/1/x":            "digit drive",
		"/ /x":            "space drive",
		"/C//x":           "empty segment",
		"/C/x/":           "trailing slash",
		"/C/x/..":         "trailing dotdot",
		"/C/../x":         "dotdot",
		"/C/x/../../y":    "dotdot twice",
		"/C/x/./y":        "dot segment",
		"/../C/x":         "dotdot first",
		"/C/x\x00y":       "NUL",
		"/C/x\ny":         "LF would add an include line",
		"/C/x\ry":         "CR",
		"/C/x\ty":         "tab",
		"/C/x\u0085y":     "NEL",
		"/C/x\x7fy":       "DEL",
		"/C/a*":           "wildcard star",
		"/C/a?.txt":       "wildcard question mark",
		"/C/a:b":          "colon (alternate data stream)",
		"/C/a|b":          "pipe",
		`/C/a"b`:          "quote",
		"/C/a<b":          "less than",
		"/C/a>b":          "greater than",
		"/C/ leading/x":   "leading space in a folder",
		"/C/trailing /x":  "trailing space in a folder",
		"/C/x/ file.txt":  "leading space in the name",
		"/C/x/file.txt ":  "trailing space in the name",
		"/C/x/\u00a0file": "leading no-break space",
		"/C/x/file\u3000": "trailing ideographic space",
		"/C/x/\xff":       "invalid UTF-8",
		"/C/" + strings.Repeat("a", MaxRestorePathBytes): "over the length cap",
	}
	for p, why := range bad {
		err := ValidateRestorePath(p)
		if err == nil {
			t.Errorf("%s: %q passed", why, p)
			continue
		}
		// A problem never repeats the path.
		if len(p) > 2 && strings.Contains(err.Error(), p) {
			t.Errorf("%s: the error repeats the path: %v", why, err)
		}
	}
}

// Paths survive a JSON round trip and a Validate byte for byte: no case folding,
// no normalisation, no trimming anywhere in the package.
func TestRestorePathsAreByteExact(t *testing.T) {
	sel := okRestoreSelection()
	sel.Paths = []string{
		"/c/Mixed/CASE/File.TXT",
		"/C/e\u0301/\u00e9", // decomposed and precomposed stay as they are
		"/D/[x] $y %z%/a b",
	}
	if err := sel.Validate(); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(sel)
	if err != nil {
		t.Fatal(err)
	}
	var back RestoreSelection
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sel.Paths, back.Paths) {
		t.Errorf("paths changed in transit: %q -> %q", sel.Paths, back.Paths)
	}
	if err := back.Validate(); err != nil {
		t.Error(err)
	}
}

// The wire shape: exactly these keys, and no target, include list or artifact
// field of the old draft. The golden file pins the values.
func TestRestoreSelectionKeys(t *testing.T) {
	b, err := json.Marshal(okRestoreSelection())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	want := []string{"expires_at", "id", "mode", "overwrite", "paths", "snapshot_id"}
	var got []string
	for k := range m {
		got = append(got, k)
	}
	if len(got) != len(want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("key %q missing from %s", k, b)
		}
	}
	// The console never sends a target: a body that carries one is ignored as an
	// unknown field (compatibility rule), and nothing in the type can hold it.
	for _, f := range []string{"Target", "Includes", "IncludeFile", "ArtifactURL"} {
		if _, ok := reflect.TypeOf(RestoreSelection{}).FieldByName(f); ok {
			t.Errorf("RestoreSelection has a %s field", f)
		}
	}
}

// Nobody logs a selection: every way a value reaches a log prints the id and a
// count and nothing else.
func TestRestoreSelectionNeverPrintsPaths(t *testing.T) {
	sel := okRestoreSelection()
	sel.Paths = []string{"/C/Patients/Smith John/chart.docx"}
	var jsonLog, textLog bytes.Buffer
	slog.New(slog.NewJSONHandler(&jsonLog, nil)).Info("fetched", "sel", sel, "ptr", &sel)
	slog.New(slog.NewTextHandler(&textLog, nil)).Info("fetched", "sel", sel)
	outs := []string{
		fmt.Sprint(sel), fmt.Sprintf("%v", sel), fmt.Sprintf("%+v", sel), fmt.Sprintf("%#v", sel), fmt.Sprintf("%s", sel),
		fmt.Sprintf("%v", &sel), jsonLog.String(), textLog.String(),
	}
	for i, o := range outs {
		if strings.Contains(o, "Smith") || strings.Contains(o, "Patients") || strings.Contains(o, "chart") {
			t.Errorf("output %d leaks a path: %s", i, o)
		}
		if !strings.Contains(o, sel.ID) {
			t.Errorf("output %d lacks the id: %s", i, o)
		}
	}
}

// The restore command carries the request id and nothing else.
func TestRestoreParamsCarryOnlyTheID(t *testing.T) {
	rt := reflect.TypeOf(RestoreParams{})
	if rt.NumField() != 1 || rt.Field(0).Name != "RestoreRequestID" {
		t.Fatalf("RestoreParams must hold the restore request id only, has %d fields", rt.NumField())
	}
	good := Command{ID: ulidA, Type: CommandRestore, Params: json.RawMessage(`{"restore_request_id":"` + ulidC + `"}`), CreatedAt: t0, ExpiresAt: at(60)}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := AssertPathFree(good); err != nil {
		t.Errorf("a clean restore command: %v", err)
	}
	// Anyone who tries to put a path or a target into the params is caught by the guard.
	for _, params := range []string{
		`{"restore_request_id":"` + ulidC + `","target":"C:\\Users\\x"}`,
		`{"restore_request_id":"` + ulidC + `","paths":["/C/Users/x/a.docx"]}`,
		`{"restore_request_id":"` + ulidC + `","where":"/C/Users/x"}`,
	} {
		c := good
		c.Params = json.RawMessage(params)
		if err := AssertPathFree(c); err == nil {
			t.Errorf("params %s passed the path-free guard", params)
		}
	}
}

// FuzzRestoreSelectionValidate: Validate never panics, and whatever it accepts
// is safe to turn into include-file lines: absolute, a drive letter, no "."
// or ".." segment, no control character, no forbidden character, no leading or
// trailing space on a name, within the limits.
func FuzzRestoreSelectionValidate(f *testing.F) {
	for _, s := range []string{
		"/C/a", "/C/Users/x/Documents/a.docx", "/c/x/y", "/D/[a] $b/c", "/C/../x", "/C/x/..", "/C/./x", "/C//x", "/C/x/",
		"/C", "/", "", "C/x", `/C/a\b`, "/C/a\nb", "/C/a\x00b", "/C/a*", "/C/a?", "/C/ a", "/C/a ", "/C/\u00a0a", "/C/a\xffb",
		"/C/a:b", "/C/a..b", "/C/" + strings.Repeat("a", 300), "/\u0130/x",
	} {
		f.Add(s, s, uint8(1))
		f.Add(s, "/C/ok", uint8(0))
	}
	f.Fuzz(func(t *testing.T, p1, p2 string, mode uint8) {
		sel := okRestoreSelection()
		sel.Paths = []string{p1, p2}
		if mode%2 == 1 {
			sel.Mode, sel.Overwrite = DeviceRestoreSide, DeviceOverwriteNever
		}
		err := sel.Validate()
		perr1, perr2 := ValidateRestorePath(p1), ValidateRestorePath(p2)
		if (err == nil) != (perr1 == nil && perr2 == nil && len(p1)+len(p2) <= MaxRestorePathsTotalBytes) {
			t.Fatalf("Validate and ValidateRestorePath disagree: %v / %v / %v", err, perr1, perr2)
		}
		if err != nil {
			if strings.Contains(err.Error(), p1) && len(p1) > 3 || strings.Contains(err.Error(), p2) && len(p2) > 3 {
				t.Fatalf("the error repeats a path: %v", err)
			}
			return
		}
		for _, p := range sel.Paths {
			assertSafeAccepted(t, p)
		}
	})
}

func assertSafeAccepted(t *testing.T, p string) {
	t.Helper()
	if len(p) > MaxRestorePathBytes || !utf8.ValidString(p) || p[0] != '/' {
		t.Fatalf("accepted a bad path: %q", p)
	}
	if strings.Contains(p, "\x00") || strings.Contains(p, "..") && containsSegment(p, "..") {
		t.Fatalf("accepted a NUL or a .. segment: %q", p)
	}
	segs := strings.Split(p[1:], "/")
	if len(segs) < 2 || len(segs[0]) != 1 || !unicode.IsLetter(rune(segs[0][0])) || segs[0][0] >= 0x80 {
		t.Fatalf("accepted a path without a drive letter and a name: %q", p)
	}
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			t.Fatalf("accepted an empty, . or .. segment: %q", p)
		}
		first, _ := utf8.DecodeRuneInString(seg)
		last, _ := utf8.DecodeLastRuneInString(seg)
		if unicode.IsSpace(first) || unicode.IsSpace(last) {
			t.Fatalf("accepted a name with leading or trailing white space: %q", p)
		}
	}
	for _, r := range p {
		if unicode.IsControl(r) || strings.ContainsRune(nameForbidden, r) {
			t.Fatalf("accepted a control or forbidden character %U: %q", r, p)
		}
	}
}

func containsSegment(p, seg string) bool {
	for _, s := range strings.Split(p, "/") {
		if s == seg {
			return true
		}
	}
	return false
}

// oldRunRecordJSON is testdata/run_record.json as it was before P22 (a backup
// record with no restore block), kept verbatim: an agent that predates P22 sends
// exactly this shape.
const oldRunRecordJSON = `{
  "agent_version": "1.2.3", "attempt": 2, "deferred_reason": "lock", "error_class": "lock",
  "error_counts": [
    {"class": "permission", "count": 3, "during": "archival", "ext": "docx"},
    {"class": "locked", "count": 1, "during": "scan", "ext": "pdf"}
  ],
  "exit_code": 11, "finished_at": "2026-10-22T14:08:09.512Z", "id": "01JAZK3Q8W5N6X7Y8Z9A0B1C2D",
  "initial": true, "kind": "backup", "outcome": "deferred", "policy_version": 7,
  "snapshot_id": "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12",
  "started_at": "2026-10-22T14:03:09.512Z",
  "stats": {
    "data_added": 8, "data_added_packed": 9, "dirs_changed": 5, "dirs_new": 4, "dirs_unmodified": 6,
    "failed_files": 12, "files_changed": 2, "files_new": 1, "files_unmodified": 3, "lease": "granted",
    "long_run": true, "total_bytes_processed": 10, "total_duration_s": 11.5, "total_files_processed": 7,
    "vss_fallback_volumes": ["C", "D"]
  },
  "trigger": "catchup"
}`

// Additive only: an old-shape record decodes, validates, and encodes back
// without any of the new keys.
func TestOldShapeRunRecordStillValidates(t *testing.T) {
	var r RunRecord
	if err := json.Unmarshal([]byte(oldRunRecordJSON), &r); err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("an old-shape record is refused: %v", err)
	}
	if err := (RunsRequest{Runs: []RunRecord{r}}).Validate(); err != nil {
		t.Fatalf("an old-shape batch is refused: %v", err)
	}
	st := r.Stats
	if st.FilesRestored != 0 || st.FilesSkipped != 0 || st.BytesRestored != 0 || st.BytesSkipped != 0 || st.RestoreResult != "" || st.RestoreErrors != nil {
		t.Errorf("an old record decoded with restore data: %+v", st)
	}
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"files_restored", "files_skipped", "bytes_restored", "bytes_skipped", "restore_result", "restore_errors"} {
		if bytes.Contains(out, []byte(`"`+k+`"`)) {
			t.Errorf("a backup record encodes %q: %s", k, out)
		}
	}
	// A new record read by a reader that predates the block: unknown keys are ignored.
	newer, err := json.Marshal(okRun())
	if err != nil {
		t.Fatal(err)
	}
	type oldStats struct {
		FilesNew int64 `json:"files_new"`
	}
	var o struct {
		Stats oldStats `json:"stats"`
	}
	if err := json.Unmarshal(newer, &o); err != nil || o.Stats.FilesNew != 1 {
		t.Errorf("an old reader cannot read a new record: %v", err)
	}
}

func TestValidateRunStatsRestoreBlock(t *testing.T) {
	runValidate(t, func() RunStats { return okRun().Stats }, []vcase[RunStats]{
		{"valid", nil, nil},
		{"every class a result", func(s *RunStats) { s.RestoreResult = RestoreResultAuth }, nil},
		{"result empty", func(s *RunStats) { s.RestoreResult = "" }, nil},
		{"errors empty", func(s *RunStats) { s.RestoreErrors = nil }, nil},
		{"nothing_matched counted as groups", func(s *RunStats) {
			s.RestoreErrors = []RestoreClassCount{{Class: RestoreResultNothingMatched, Count: 1}, {Class: RestoreResultOther, Count: 4}}
		}, nil},
		{"files_restored negative", func(s *RunStats) { s.FilesRestored = -1 }, []string{"files_restored"}},
		{"files_skipped negative", func(s *RunStats) { s.FilesSkipped = -1 }, []string{"files_skipped"}},
		{"result bogus", func(s *RunStats) { s.RestoreResult = "oops" }, []string{"restore_result"}},
		{"result is a path", func(s *RunStats) { s.RestoreResult = `C:\x` }, []string{"restore_result"}},
		{"error class bogus", func(s *RunStats) { s.RestoreErrors[0].Class = "oops" }, []string{"restore_errors[0].class"}},
		{"error class not countable", func(s *RunStats) { s.RestoreErrors[1].Class = RestoreResultPartial }, []string{"restore_errors[1].class"}},
		{"error class twice", func(s *RunStats) { s.RestoreErrors[1].Class = s.RestoreErrors[0].Class }, []string{"restore_errors[1].class"}},
		{"error count 0", func(s *RunStats) { s.RestoreErrors[0].Count = 0 }, []string{"restore_errors[0].count"}},
		{"error count negative", func(s *RunStats) { s.RestoreErrors[0].Count = -3 }, []string{"restore_errors[0].count"}},
		{"too many error rows", func(s *RunStats) {
			s.RestoreErrors = make([]RestoreClassCount, len(restoreResultClasses)+1)
			for i := range s.RestoreErrors {
				s.RestoreErrors[i] = RestoreClassCount{Class: RestoreResultOther, Count: 1}
			}
		}, []string{"restore_errors", "restore_errors[1].class", "restore_errors[2].class", "restore_errors[3].class", "restore_errors[4].class", "restore_errors[5].class", "restore_errors[6].class", "restore_errors[7].class", "restore_errors[8].class", "restore_errors[9].class", "restore_errors[10].class"}},
	})
}

// A restore is a run of kind restore: the couplings of its result class.
func TestValidateRestoreRun(t *testing.T) {
	restoreRun := func() RunRecord {
		r := okRun()
		r.Kind, r.SnapshotID, r.DeferredReason, r.Trigger = restic.KindRestore, "", "", TriggerManual
		r.Outcome, r.ExitCode = restic.OutcomePartial, ptr(1)
		r.Stats.RestoreResult = RestoreResultPartial
		return r
	}
	runValidate(t, restoreRun, []vcase[RunRecord]{
		{"partial restore", nil, nil},
		{"nothing matched is failed", func(r *RunRecord) {
			r.Outcome, r.Stats.RestoreResult = restic.OutcomeFailed, RestoreResultNothingMatched
		}, nil},
		{"nothing matched may not be a success (restic exits 0)", func(r *RunRecord) {
			r.Outcome, r.ExitCode, r.Stats.RestoreResult = restic.OutcomeSuccess, ptr(0), RestoreResultNothingMatched
		}, []string{"stats.restore_result"}},
		{"nothing matched may not be partial", func(r *RunRecord) { r.Stats.RestoreResult = RestoreResultNothingMatched }, []string{"stats.restore_result"}},
		{"nothing matched may not be cancelled", func(r *RunRecord) {
			r.Outcome, r.Stats.RestoreResult = restic.OutcomeCancelled, RestoreResultNothingMatched
		}, []string{"stats.restore_result"}},
		{"nothing matched on an open run is not judged yet", func(r *RunRecord) {
			r.Outcome, r.FinishedAt, r.ExitCode, r.Stats.RestoreResult = "", nil, nil, RestoreResultNothingMatched
		}, nil},
		{"interrupted", func(r *RunRecord) {
			r.Outcome, r.Stats.RestoreResult = restic.OutcomeInterrupted, RestoreResultInterrupted
		}, nil},
		{"guard refused", func(r *RunRecord) { r.Outcome, r.Stats.RestoreResult = restic.OutcomeFailed, RestoreResultGuardRefused }, nil},
		{"a successful restore carries no result", func(r *RunRecord) {
			r.Outcome, r.Stats.RestoreResult, r.Stats.RestoreErrors = restic.OutcomeSuccess, "", nil
		}, nil},
		{"an open restore", func(r *RunRecord) {
			r.Outcome, r.FinishedAt, r.ExitCode, r.Stats.RestoreResult, r.Stats.RestoreErrors = "", nil, nil, "", nil
		}, nil},
	})
	// Every class of the vocabulary is accepted on a failed run; the other pairings
	// are conventions, not rules.
	for _, c := range restoreResultClasses {
		r := restoreRun()
		r.Outcome, r.Stats.RestoreResult = restic.OutcomeFailed, c
		if err := r.Validate(); err != nil {
			t.Errorf("class %q on a failed restore: %v", c, err)
		}
	}
}

func TestRestoreVocabularies(t *testing.T) {
	want := map[string][]string{
		EnumDeviceRestoreMode:      {"device_original", "device_side"},
		EnumDeviceRestoreOverwrite: {"never", "if_changed"},
		EnumRestoreResultClass: {
			"nothing_matched", "target_locked", "access_denied", "partial", "interrupted",
			"guard_refused", "auth", "repo_unreachable", "cancelled", "other",
		},
	}
	for name, vals := range want {
		if got := Enums()[name]; !reflect.DeepEqual(got, vals) {
			t.Errorf("%s = %v, want %v", name, got, vals)
		}
	}
	// The wire modes are not the stored ones: the console maps them.
	for _, stored := range []string{string(RestoreDevicePath), string(RestoreDeviceRelay), string(OverwriteIfChanged)} {
		for _, v := range append(want[EnumDeviceRestoreMode], want[EnumDeviceRestoreOverwrite]...) {
			if v == stored {
				t.Errorf("wire value %q equals a stored one; the mapping would be silent", v)
			}
		}
	}
	for _, c := range restoreResultClasses {
		if c.Countable() && c == RestoreResultPartial {
			t.Error("partial must not be countable")
		}
	}
	// Problem codes of the console's refusals.
	for _, code := range []string{CodeAgentTooOld, CodeCustomerHeld, CodeDeviceRetiredDownloadOnly, CodeDevicePurging, CodeDeviceRestoreActive, CodeRestoreExpired, CodeRestoreNotYours} {
		if !has(ErrorCodes(), code) {
			t.Errorf("%q is not in the error table", code)
		}
		if looksLikePath(code) || MessageFor(code) == unknownCodeMessage {
			t.Errorf("%q has no fixed path-free message", code)
		}
	}
}

// The path-free guard has exactly two exemptions. Put a path into every string
// field of every wire type, one at a time: each must be caught, except inside a
// RestoreSelection (the marker) and restic.Snapshot.Paths (the field skip). A new
// field anywhere, the restore block of RunStats included, is covered without
// anyone remembering to extend a test.
func TestAssertPathFreeCatchesAPathInEveryStringFieldButTheTwoExemptions(t *testing.T) {
	const leak = `C:\Users\leak\secret.docx`
	visited := map[string]bool{}
	exemptSeen := map[string]bool{}
	for _, v := range wireValues() {
		typ := reflect.TypeOf(v)
		ptr := reflect.New(typ)
		ptr.Elem().Set(reflect.ValueOf(v))
		var walk func(rv reflect.Value, jsonPath string)
		walk = func(rv reflect.Value, jsonPath string) {
			if rv.Type() == reflect.TypeOf(policy.Policy{}) || rv.Type() == reflect.TypeOf(time.Time{}) || rv.Type() == reflect.TypeOf(json.RawMessage(nil)) {
				return
			}
			switch rv.Kind() {
			case reflect.Pointer:
				if !rv.IsNil() {
					walk(rv.Elem(), jsonPath)
				}
			case reflect.String:
				old := rv.String()
				rv.SetString(leak)
				err := AssertPathFree(ptr.Interface())
				rv.SetString(old)
				if err == nil {
					t.Errorf("%s.%s: a path in this field passed the guard", typ.Name(), jsonPath)
				}
				visited[typ.Name()+"."+jsonPath] = true
			case reflect.Struct:
				for i := 0; i < rv.NumField(); i++ {
					f := rv.Type().Field(i)
					if !f.IsExported() {
						continue
					}
					name, skip := jsonName(f)
					if skip {
						continue
					}
					if rv.Type() == reflect.TypeOf(restic.Snapshot{}) && f.Name == "Paths" {
						exemptSeen["Snapshot.Paths"] = true
						continue
					}
					child := joinPath(jsonPath, name)
					walk(rv.Field(i), child)
				}
			case reflect.Slice:
				for i := 0; i < rv.Len(); i++ {
					walk(rv.Index(i), jsonPath+"[]")
				}
			}
		}
		if _, ok := v.(pathBearing); ok {
			exemptSeen[typ.Name()] = true
			if err := AssertPathFree(ptr.Interface()); err != nil {
				t.Errorf("%s is the marked type and must pass: %v", typ.Name(), err)
			}
			continue
		}
		walk(ptr.Elem(), "")
	}
	if len(exemptSeen) != 2 || !exemptSeen["RestoreSelection"] || !exemptSeen["Snapshot.Paths"] {
		t.Errorf("the guard's exemptions seen = %v, want exactly RestoreSelection and Snapshot.Paths", exemptSeen)
	}
	for _, must := range []string{
		"RunRecord.stats.restore_result", "RunRecord.stats.restore_errors[].class", "RunRecord.id", "RunsRequest.runs[].stats.restore_result",
		"Error.code", "CommandsResponse.commands[].type",
	} {
		if !visited[must] {
			t.Errorf("the field walk never reached %s (visited %d fields)", must, len(visited))
		}
	}
	// And in the shape the console really ingests: a restore run with a path in a class.
	req := RunsRequest{Runs: []RunRecord{okRun()}}
	req.Runs[0].Stats.RestoreErrors[1].Class = RestoreResultClass(leak)
	if pe := pathErr(t, req); pe.Path != "runs[0].stats.restore_errors[1].class" {
		t.Errorf("path = %q", pe.Path)
	}
	// A path in a problem code is caught too.
	if err := AssertPathFree(Error{Code: "/C/Users/x", Title: "t", Type: "t"}); err == nil {
		t.Error("a path in Error.Code passed")
	}
}
