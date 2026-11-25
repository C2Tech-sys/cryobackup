package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

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

// The 64 KiB total on its own: every path well under MaxRestorePathBytes and
// the count under MaxRestorePaths, so the total is the only rule that can fire.
func TestRestorePathsTotalAlone(t *testing.T) {
	const n, each = 32, MaxRestorePathsTotalBytes / 32 // 32 x 2048 = 65536
	if each >= MaxRestorePathBytes || n > MaxRestorePaths {
		t.Fatalf("the test no longer isolates the total: each %d, n %d", each, n)
	}
	sel := okRestoreSelection()
	sel.Paths = make([]string, n)
	for i := range sel.Paths {
		p := fmt.Sprintf("/C/f%02d/", i)
		sel.Paths[i] = p + strings.Repeat("a", each-len(p))
	}
	total := 0
	for _, p := range sel.Paths {
		total += len(p)
	}
	if total != MaxRestorePathsTotalBytes {
		t.Fatalf("total = %d, want %d", total, MaxRestorePathsTotalBytes)
	}
	if err := sel.Validate(); err != nil {
		t.Fatalf("exactly %d total bytes refused: %v", MaxRestorePathsTotalBytes, err)
	}
	sel.Paths[n-1] += "a" // 2049 bytes, total 65537
	err := sel.Validate()
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Problems) != 1 || ve.Problems[0].Path != "paths" || ve.Problems[0].Message != "are too large together" {
		t.Fatalf("a total of %d bytes: want only paths: are too large together, got %v", MaxRestorePathsTotalBytes+1, err)
	}
}

// The Windows-name rules say which rule a name broke.
func TestWindowsNameRulesSayWhy(t *testing.T) {
	for p, want := range map[string]string{
		"/C/a.":        "has a name that ends with a dot",
		"/C/x/.../y":   "has a name that ends with a dot",
		"/C/CON":       "has a name Windows reserves for a device",
		"/C/lpt\u00b2": "has a name Windows reserves for a device",
		"/C/x/nul.txt": "has a name Windows reserves for a device",
	} {
		err := ValidateRestorePath(p)
		var ve *ValidationError
		if !errors.As(err, &ve) || len(ve.Problems) != 1 || ve.Problems[0].Message != want {
			t.Errorf("%q: got %v, want %q", p, err, want)
		}
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
		"/C/a..b/..c/d..e",                 // ".." inside a name is a name
		"/C/%APPDATA%/100%/#hash/a,b;c=d!", // not special to restic's include file (spike item 3)
		"/C/" + strings.Repeat("n", 255),
		// Look-alike dots and invisible characters are matched literally (harmless).
		"/C/\u2024\u2024/x", "/C/\uff0e\uff0e/x", "/C/.\u0301/x", "/C/..\u200b/x", "/C/\u200bx",
		"/C/x\u00ady", "/C/\u202ex", "/C/x\ufeff",
		// Names that only resemble a reserved device name are ordinary names.
		"/C/CONSOLE", "/C/con1", "/C/COM10", "/C/LPT", "/C/COM", "/C/NULL.txt", "/C/xCON", "/C/AUX_x.txt",
		"/C/COM\u2074", "/C/c\u00f6n", "/C/CON\u200b", "/C/.CON", "/C/.hidden", "/C/a.b.c",
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
		// Win32 cannot create these, so no Windows snapshot holds them.
		"/C/a.":          "trailing dot",
		"/C/a..":         "two trailing dots",
		"/C/...":         "dots only",
		"/C/....":        "four dots",
		"/C/. .":         "dot space dot",
		"/C/x./y":        "trailing dot in a folder",
		"/C/CON":         "CON",
		"/C/con":         "con, lower case",
		"/C/PRN":         "PRN",
		"/C/AUX":         "AUX",
		"/C/NUL":         "NUL",
		"/C/nUl":         "NUL, mixed case",
		"/C/CONIN$":      "CONIN$",
		"/C/conout$":     "CONOUT$, lower case",
		"/C/COM0":        "COM0",
		"/C/COM1":        "COM1",
		"/C/com9":        "COM9, lower case",
		"/C/LPT0":        "LPT0",
		"/C/LPT1":        "LPT1",
		"/C/lpt9":        "LPT9, lower case",
		"/C/COM\u00b9":   "COM superscript one",
		"/C/COM\u00b2":   "COM superscript two",
		"/C/lpt\u00b3":   "LPT superscript three",
		"/C/con.txt":     "CON with an extension",
		"/C/NUL.tar.gz":  "NUL with two extensions",
		"/C/CON .txt":    "CON, a space before the extension",
		"/C/COM1.log":    "COM1 with an extension",
		"/C/LPT\u00b9.x": "LPT superscript one with an extension",
		"/C/CONIN$.txt":  "CONIN$ with an extension",
		"/C/x/PRN/y":     "PRN as a folder",
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

// The reserved-name fold is ASCII only (windowsReservedName, asciiUpper). The
// dotless i (U+0131) upper-cases to I under strings.ToUpper, so a Unicode fold
// would turn "conın$" into the device CONIN$; Win32 does not, and the name is a
// different file, so it is accepted and matched literally.
func TestReservedNamesFoldASCIIOnly(t *testing.T) {
	dotlessI := string(rune(0x131))
	for _, p := range []string{"/C/con" + dotlessI + "n$", "/C/con" + dotlessI + "n$.txt", "/C/x/CON" + dotlessI + "N$"} {
		if err := ValidateRestorePath(p); err != nil {
			t.Errorf("%q is not a reserved name and must pass: %v", p, err)
		}
	}
	// The same names with the ASCII i are the device and stay refused.
	for _, p := range []string{"/C/conin$", "/C/conin$.txt", "/C/CONIN$"} {
		if ValidateRestorePath(p) == nil {
			t.Errorf("%q is the CONIN$ device and passed", p)
		}
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
// trailing space on a name, no name ending in '.', no reserved device name,
// within the limits.
func FuzzRestoreSelectionValidate(f *testing.F) {
	for _, s := range []string{
		"/C/a", "/C/Users/x/Documents/a.docx", "/c/x/y", "/D/[a] $b/c", "/C/../x", "/C/x/..", "/C/./x", "/C//x", "/C/x/",
		"/C", "/", "", "C/x", `/C/a\b`, "/C/a\nb", "/C/a\x00b", "/C/a*", "/C/a?", "/C/ a", "/C/a ", "/C/\u00a0a", "/C/a\xffb",
		"/C/a:b", "/C/a..b", "/C/" + strings.Repeat("a", 300), "/\u0130/x",
		"/C/a.", "/C/...", "/C/. .", "/C/x./y", "/C/CON", "/C/con.txt", "/C/NUL.tar.gz", "/C/CON .txt", "/C/COM\u00b9",
		"/C/lpt0", "/C/CONIN$", "/C/conout$.x", "/C/x/PRN/y", "/C/CONSOLE", "/C/COM10", "/C/\u2024\u2024/x", "/C/..\u200b/x",
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
		if strings.HasSuffix(seg, ".") {
			t.Fatalf("accepted a name that ends with a dot: %q", p)
		}
		if reservedOracleRE.MatchString(reservedOracleBase(seg)) {
			t.Fatalf("accepted a reserved device name: %q", p)
		}
	}
	for _, r := range p {
		if unicode.IsControl(r) || strings.ContainsRune(nameForbidden, r) {
			t.Fatalf("accepted a control or forbidden character %U: %q", r, p)
		}
	}
}

// The fuzz oracle for reserved names, written apart from windowsReservedName:
// the part before the first dot, trailing spaces removed, ASCII upper-cased.
var reservedOracleRE = regexp.MustCompile(`^(CON|PRN|AUX|NUL|CONIN\$|CONOUT\$|COM[0-9\x{00b9}\x{00b2}\x{00b3}]|LPT[0-9\x{00b9}\x{00b2}\x{00b3}])$`)

func reservedOracleBase(seg string) string {
	base, _, _ := strings.Cut(seg, ".")
	base = strings.TrimRight(base, " ")
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' {
			return r - 'a' + 'A'
		}
		return r
	}, base)
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

// MaxRestoreCount is 2^53-1, the largest integer an IEEE-754 double carries
// exactly. The test pins the literal so a change to the constant is a decision
// and not a typo (runs.go, MaxRestoreCount).
func TestMaxRestoreCountIsTheJSONSafeInteger(t *testing.T) {
	if MaxRestoreCount != 9007199254740991 {
		t.Errorf("MaxRestoreCount = %d, want 9007199254740991 (2^53-1)", int64(MaxRestoreCount))
	}
	// 2^53 is the last integer whose successor is still exact; 2^53+1 rounds back to 2^53.
	a := int64(MaxRestoreCount)
	if int64(float64(a)) != a || float64(a+2) != float64(a+1) {
		t.Error("2^53-1 is not the edge of exact double arithmetic")
	}
}

func TestValidateRunStatsRestoreBlock(t *testing.T) {
	// One row per class is the most; every row past the first repeats "other".
	tooMany := []string{"restore_errors"}
	for i := 1; i <= len(restoreResultClasses); i++ {
		tooMany = append(tooMany, fmt.Sprintf("restore_errors[%d].class", i))
	}
	runValidate(t, func() RunStats { return okRun().Stats }, []vcase[RunStats]{
		{"valid", nil, nil},
		{"every class a result", func(s *RunStats) { s.RestoreResult = RestoreResultAuth }, nil},
		{"result empty", func(s *RunStats) { s.RestoreResult = "" }, nil},
		{"errors empty", func(s *RunStats) { s.RestoreErrors = nil }, nil},
		{"nothing_matched counted as restic runs", func(s *RunStats) {
			s.RestoreErrors = []RestoreClassCount{{Class: RestoreResultNothingMatched, Count: 1}, {Class: RestoreResultOther, Count: 4}}
		}, nil},
		{"guard_refused counted per item", func(s *RunStats) {
			s.RestoreErrors = []RestoreClassCount{{Class: RestoreResultGuardRefused, Count: 2}}
		}, nil},
		{"guard_refused with the other counted classes", func(s *RunStats) {
			s.RestoreErrors = []RestoreClassCount{
				{Class: RestoreResultGuardRefused, Count: 2}, {Class: RestoreResultAccessDenied, Count: 1}, {Class: RestoreResultTargetLocked, Count: 3},
			}
		}, nil},
		{"files_restored negative", func(s *RunStats) { s.FilesRestored = -1 }, []string{"files_restored"}},
		{"files_skipped negative", func(s *RunStats) { s.FilesSkipped = -1 }, []string{"files_skipped"}},
		{"result bogus", func(s *RunStats) { s.RestoreResult = "oops" }, []string{"restore_result"}},
		{"result is a path", func(s *RunStats) { s.RestoreResult = `C:\x` }, []string{"restore_result"}},
		{"error class bogus", func(s *RunStats) { s.RestoreErrors[0].Class = "oops" }, []string{"restore_errors[0].class"}},
		{"error class not countable", func(s *RunStats) { s.RestoreErrors[1].Class = RestoreResultPartial }, []string{"restore_errors[1].class"}},
		{"error class twice", func(s *RunStats) { s.RestoreErrors[1].Class = s.RestoreErrors[0].Class }, []string{"restore_errors[1].class"}},
		{"files_restored at the JSON-safe bound", func(s *RunStats) { s.FilesRestored = MaxRestoreCount }, nil},
		{"files_restored over the bound", func(s *RunStats) { s.FilesRestored = MaxRestoreCount + 1 }, []string{"files_restored"}},
		{"files_skipped at the bound", func(s *RunStats) { s.FilesSkipped = MaxRestoreCount }, nil},
		{"files_skipped over the bound", func(s *RunStats) { s.FilesSkipped = MaxRestoreCount + 1 }, []string{"files_skipped"}},
		{"bytes_restored at the bound", func(s *RunStats) { s.BytesRestored = MaxRestoreCount }, nil},
		{"bytes_restored over the bound", func(s *RunStats) { s.BytesRestored = MaxRestoreCount + 1 }, []string{"bytes_restored"}},
		{"bytes_restored max uint64", func(s *RunStats) { s.BytesRestored = math.MaxUint64 }, []string{"bytes_restored"}},
		{"bytes_skipped at the bound", func(s *RunStats) { s.BytesSkipped = MaxRestoreCount }, nil},
		{"bytes_skipped over the bound", func(s *RunStats) { s.BytesSkipped = MaxRestoreCount + 1 }, []string{"bytes_skipped"}},
		{"error count at the bound", func(s *RunStats) { s.RestoreErrors[0].Count = MaxRestoreCount }, nil},
		{"error count over the bound", func(s *RunStats) { s.RestoreErrors[1].Count = math.MaxInt64 }, []string{"restore_errors[1].count"}},
		{"error count 0", func(s *RunStats) { s.RestoreErrors[0].Count = 0 }, []string{"restore_errors[0].count"}},
		{"error count negative", func(s *RunStats) { s.RestoreErrors[0].Count = -3 }, []string{"restore_errors[0].count"}},
		{"too many error rows", func(s *RunStats) {
			s.RestoreErrors = make([]RestoreClassCount, len(restoreResultClasses)+1)
			for i := range s.RestoreErrors {
				s.RestoreErrors[i] = RestoreClassCount{Class: RestoreResultOther, Count: 1}
			}
		}, tooMany},
	})
}

// The coupling table of a restore run (restoreCouplings): every row has a test
// of its own here, with at least one record it accepts and one it refuses, and
// no test names a row the table does not have.
func TestRestoreCouplings(t *testing.T) {
	closeAs := func(r *RunRecord, o restic.Outcome, c RestoreResultClass) {
		r.Outcome, r.Stats.RestoreResult = o, c
	}
	success := func(r *RunRecord) {
		r.Outcome, r.ExitCode, r.Stats.RestoreResult, r.Stats.RestoreErrors = restic.OutcomeSuccess, ptr(0), "", nil
	}
	nothing := func(r *RunRecord) {
		r.Outcome, r.Stats = restic.OutcomeFailed, RunStats{RestoreResult: RestoreResultNothingMatched}
	}
	open := func(r *RunRecord) { r.Outcome, r.FinishedAt, r.ExitCode = "", nil, nil }
	backup := func(r *RunRecord) { *r = okBackupRun() }

	rows := map[string][]vcase[RunRecord]{
		"kind": {
			{"a restore carries the block", nil, nil},
			{"a backup without the block (old shape)", backup, nil},
			{"files_restored on a backup", func(r *RunRecord) { backup(r); r.Stats.FilesRestored = 1 }, []string{"stats.files_restored"}},
			{"files_skipped on a backup", func(r *RunRecord) { backup(r); r.Stats.FilesSkipped = 1 }, []string{"stats.files_skipped"}},
			{"bytes_restored on a backup", func(r *RunRecord) { backup(r); r.Stats.BytesRestored = 1 }, []string{"stats.bytes_restored"}},
			{"bytes_skipped on a backup", func(r *RunRecord) { backup(r); r.Stats.BytesSkipped = 1 }, []string{"stats.bytes_skipped"}},
			{"restore_result on a backup", func(r *RunRecord) { backup(r); r.Stats.RestoreResult = RestoreResultPartial }, []string{"stats.restore_result"}},
			{"nothing_matched on a failed backup", func(r *RunRecord) {
				backup(r)
				r.Outcome, r.DeferredReason, r.Stats.RestoreResult = restic.OutcomeFailed, "", RestoreResultNothingMatched
			}, []string{"stats.restore_result"}},
			{"restore_errors on a prune", func(r *RunRecord) {
				backup(r)
				r.Kind, r.SnapshotID = restic.KindPrune, ""
				r.Stats.RestoreErrors = []RestoreClassCount{{Class: RestoreResultOther, Count: 1}}
			}, []string{"stats.restore_errors"}},
			{"the whole block on a backup", func(r *RunRecord) { *r = okRun() }, []string{
				"stats.bytes_restored", "stats.bytes_skipped", "stats.files_restored", "stats.files_skipped", "stats.restore_errors", "stats.restore_result",
			}},
		},
		"open": {
			{"an open restore with an empty block", func(r *RunRecord) { open(r); r.Stats = RunStats{} }, nil},
			{"files_restored on an open restore", func(r *RunRecord) { open(r); r.Stats = RunStats{FilesRestored: 1} }, []string{"stats.files_restored"}},
			{"files_skipped on an open restore", func(r *RunRecord) { open(r); r.Stats = RunStats{FilesSkipped: 1} }, []string{"stats.files_skipped"}},
			{"bytes_restored on an open restore", func(r *RunRecord) { open(r); r.Stats = RunStats{BytesRestored: 1} }, []string{"stats.bytes_restored"}},
			{"bytes_skipped on an open restore", func(r *RunRecord) { open(r); r.Stats = RunStats{BytesSkipped: 1} }, []string{"stats.bytes_skipped"}},
			{"restore_result on an open restore", func(r *RunRecord) { open(r); r.Stats = RunStats{RestoreResult: RestoreResultDisk} }, []string{"stats.restore_result"}},
			{"error rows on an open restore", func(r *RunRecord) {
				open(r)
				r.Stats = RunStats{RestoreErrors: []RestoreClassCount{{Class: RestoreResultAccessDenied, Count: 1}}}
			}, []string{"stats.restore_errors"}},
			{"the whole block on an open restore", open, []string{
				"stats.bytes_restored", "stats.bytes_skipped", "stats.files_restored", "stats.files_skipped", "stats.restore_errors", "stats.restore_result",
			}},
		},
		"success": {
			{"files restored", func(r *RunRecord) { success(r); r.Stats.FilesSkipped = 0 }, nil},
			{"every file kept (skipped), nothing replaced", func(r *RunRecord) { success(r); r.Stats.FilesRestored, r.Stats.BytesRestored = 0, 0 }, nil},
			{"success with a result", func(r *RunRecord) { success(r); r.Stats.RestoreResult = RestoreResultPartial }, []string{"stats.restore_result"}},
			{"success with error rows", func(r *RunRecord) {
				success(r)
				r.Stats.RestoreErrors = []RestoreClassCount{{Class: RestoreResultAccessDenied, Count: 1}}
			}, []string{"stats.restore_errors"}},
			{"success that placed no file", func(r *RunRecord) { success(r); r.Stats = RunStats{BytesRestored: 9} }, []string{"stats.files_restored"}},
			{"success with no block at all (restic exit 0, empty summary)", func(r *RunRecord) { success(r); r.Stats = RunStats{} }, []string{"stats.files_restored"}},
			{"nothing matched reported as a success", func(r *RunRecord) {
				success(r)
				r.Stats = RunStats{RestoreResult: RestoreResultNothingMatched}
			}, []string{"stats.files_restored", "stats.restore_result"}},
		},
		"nothing_matched": {
			{"failed, nothing counted", nothing, nil},
			{"outcome partial", func(r *RunRecord) { nothing(r); r.Outcome = restic.OutcomePartial }, []string{"stats.restore_result"}},
			{"outcome cancelled", func(r *RunRecord) { nothing(r); r.Outcome = restic.OutcomeCancelled }, []string{"stats.restore_result"}},
			{"on an open record", func(r *RunRecord) { nothing(r); open(r) }, []string{"stats.restore_result"}},
			{"a file restored", func(r *RunRecord) { nothing(r); r.Stats.FilesRestored = 5 }, []string{"stats.files_restored"}},
			{"every count set", func(r *RunRecord) {
				nothing(r)
				r.Stats.FilesRestored, r.Stats.FilesSkipped, r.Stats.BytesRestored, r.Stats.BytesSkipped = 1, 2, 3, 4
			}, []string{"stats.bytes_restored", "stats.bytes_skipped", "stats.files_restored", "stats.files_skipped"}},
		},
		"nothing_rows": {
			{"nothing matched, no rows", nothing, nil},
			{"nothing matched with the runs counted", func(r *RunRecord) {
				nothing(r)
				r.Stats.RestoreErrors = []RestoreClassCount{{Class: RestoreResultNothingMatched, Count: 2}}
			}, []string{"stats.restore_errors"}},
			{"nothing matched with a file error row", func(r *RunRecord) {
				nothing(r)
				r.Stats.RestoreErrors = []RestoreClassCount{{Class: RestoreResultAccessDenied, Count: 1}}
			}, []string{"stats.restore_errors"}},
			{"some runs matched nothing inside a partial restore", nil, nil},
		},
		"partial": {
			{"outcome partial", nil, nil},
			{"outcome failed", func(r *RunRecord) { r.Outcome = restic.OutcomeFailed }, []string{"stats.restore_result"}},
			{"outcome interrupted", func(r *RunRecord) { r.Outcome = restic.OutcomeInterrupted }, []string{"stats.restore_result"}},
			{"placement refusals counted under guard_refused", func(r *RunRecord) {
				r.Stats.RestoreErrors = []RestoreClassCount{{Class: RestoreResultGuardRefused, Count: 2}}
			}, nil},
			{"every countable class counted", func(r *RunRecord) {
				r.Stats.RestoreErrors = []RestoreClassCount{
					{Class: RestoreResultTargetLocked, Count: 1}, {Class: RestoreResultAccessDenied, Count: 2},
					{Class: RestoreResultGuardRefused, Count: 3}, {Class: RestoreResultOther, Count: 4}, {Class: RestoreResultNothingMatched, Count: 5},
				}
			}, nil},
			{"on an open record is the open row's", open, []string{
				"stats.bytes_restored", "stats.bytes_skipped", "stats.files_restored", "stats.files_skipped", "stats.restore_errors", "stats.restore_result",
			}},
		},
		"partial_rows": {
			{"partial with error rows", nil, nil},
			{"partial with no error rows", func(r *RunRecord) { r.Stats.RestoreErrors = nil }, []string{"stats.restore_errors"}},
			{"partial with an empty row list", func(r *RunRecord) { r.Stats.RestoreErrors = []RestoreClassCount{} }, []string{"stats.restore_errors"}},
			{"the other classes need no rows", func(r *RunRecord) { closeAs(r, restic.OutcomeFailed, RestoreResultAuth); r.Stats.RestoreErrors = nil }, nil},
		},
		"outcome_partial": {
			{"outcome partial, result partial", nil, nil},
			{"outcome partial, no class", func(r *RunRecord) { r.Stats.RestoreResult = "" }, []string{"stats.restore_result"}},
			{"outcome partial, no class, no block", func(r *RunRecord) { r.Stats = RunStats{} }, []string{"stats.restore_result"}},
			{"outcome failed needs no class", func(r *RunRecord) { closeAs(r, restic.OutcomeFailed, "") }, nil},
			{"outcome partial on a backup is not a restore's", func(r *RunRecord) { backup(r); r.Outcome, r.DeferredReason = restic.OutcomePartial, "" }, nil},
		},
		"cancelled": {
			{"outcome cancelled", func(r *RunRecord) { closeAs(r, restic.OutcomeCancelled, RestoreResultCancelled) }, nil},
			{"outcome partial", func(r *RunRecord) { closeAs(r, restic.OutcomePartial, RestoreResultCancelled) }, []string{"stats.restore_result"}},
			{"outcome failed", func(r *RunRecord) { closeAs(r, restic.OutcomeFailed, RestoreResultCancelled) }, []string{"stats.restore_result"}},
		},
		"interrupted": {
			{"outcome interrupted", func(r *RunRecord) { closeAs(r, restic.OutcomeInterrupted, RestoreResultInterrupted) }, nil},
			{"outcome failed", func(r *RunRecord) { closeAs(r, restic.OutcomeFailed, RestoreResultInterrupted) }, []string{"stats.restore_result"}},
			{"outcome cancelled", func(r *RunRecord) { closeAs(r, restic.OutcomeCancelled, RestoreResultInterrupted) }, []string{"stats.restore_result"}},
		},
		"failed": {
			{"auth failed", func(r *RunRecord) { closeAs(r, restic.OutcomeFailed, RestoreResultAuth) }, nil},
			{"disk refused before writing", func(r *RunRecord) {
				closeAs(r, restic.OutcomeFailed, RestoreResultDisk)
				r.Stats = RunStats{RestoreResult: RestoreResultDisk}
			}, nil},
			{"failed before any class was known", func(r *RunRecord) { closeAs(r, restic.OutcomeFailed, "") }, nil},
			{"auth on a cancelled run", func(r *RunRecord) { closeAs(r, restic.OutcomeCancelled, RestoreResultAuth) }, []string{"stats.restore_result"}},
			{"disk on a partial run", func(r *RunRecord) { closeAs(r, restic.OutcomePartial, RestoreResultDisk) }, []string{"stats.restore_result"}},
			{"target_locked on an interrupted run", func(r *RunRecord) { closeAs(r, restic.OutcomeInterrupted, RestoreResultTargetLocked) }, []string{"stats.restore_result"}},
			{"guard_refused refuses the whole restore", func(r *RunRecord) {
				closeAs(r, restic.OutcomeFailed, RestoreResultGuardRefused)
				r.Stats = RunStats{RestoreResult: RestoreResultGuardRefused}
			}, nil},
			{"guard_refused on an open record", func(r *RunRecord) { r.Stats = RunStats{RestoreResult: RestoreResultGuardRefused}; open(r) }, []string{"stats.restore_result"}},
		},
	}
	for _, rc := range restoreCouplings {
		if rc.id == "" || rc.reason == "" || rc.check == nil {
			t.Errorf("coupling row %q lacks an id, a reason or a check", rc.id)
		}
		cases, ok := rows[rc.id]
		if !ok {
			t.Errorf("coupling row %q has no test", rc.id)
			continue
		}
		delete(rows, rc.id)
		t.Run(rc.id, func(t *testing.T) { runValidate(t, okRestoreRun, cases) })
	}
	for id := range rows {
		t.Errorf("a test names coupling row %q, which the table does not have", id)
	}
	// An open restore with no class yet is fine: that is how the agent writes the
	// row before restic starts.
	r := okRestoreRun()
	open(&r)
	r.Stats = RunStats{}
	if err := r.Validate(); err != nil {
		t.Errorf("an open restore with an empty block: %v", err)
	}
}

// Every run-level class is accepted with exactly one outcome, the one the
// coupling table names, and never with success.
func TestEveryRestoreClassHasExactlyOneOutcome(t *testing.T) {
	want := map[RestoreResultClass]restic.Outcome{
		RestoreResultNothingMatched: restic.OutcomeFailed, RestoreResultPartial: restic.OutcomePartial,
		RestoreResultCancelled: restic.OutcomeCancelled, RestoreResultInterrupted: restic.OutcomeInterrupted,
	}
	for _, c := range restoreResultClasses {
		var accepted []restic.Outcome
		for _, o := range outcomes {
			r := okRestoreRun()
			r.Outcome, r.Stats.RestoreResult = o, c
			if o == restic.OutcomeDeferred {
				r.DeferredReason = restic.DeferredLock
			}
			if c == RestoreResultNothingMatched {
				r.Stats.FilesRestored, r.Stats.FilesSkipped, r.Stats.BytesRestored, r.Stats.BytesSkipped = 0, 0, 0, 0
				r.Stats.RestoreErrors = nil
			}
			if r.Validate() == nil {
				accepted = append(accepted, o)
			}
		}
		w, ok := want[c]
		if !ok {
			w = restic.OutcomeFailed
		}
		if len(accepted) != 1 || accepted[0] != w {
			t.Errorf("class %q is accepted with outcomes %v, want exactly [%s]", c, accepted, w)
		}
	}
}

func TestRestoreVocabularies(t *testing.T) {
	want := map[string][]string{
		EnumDeviceRestoreMode:      {"device_original", "device_side"},
		EnumDeviceRestoreOverwrite: {"never", "if_changed"},
		EnumRestoreResultClass: {
			"nothing_matched", "target_locked", "access_denied", "partial", "interrupted",
			"guard_refused", "auth", "repo_unreachable", "cancelled", "disk", "other",
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
	// Run-level only: partial, interrupted, auth, repo_unreachable, cancelled and
	// disk (a refusal before anything was written) are never tallied. The
	// countable set is every class ADR 0027 says is "counted under" (guard_refused
	// for each refused placement, access_denied for a read-only or system file,
	// target_locked for a file in use, nothing_matched per restic run) plus other.
	countable := map[RestoreResultClass]bool{
		RestoreResultNothingMatched: true, RestoreResultTargetLocked: true, RestoreResultAccessDenied: true,
		RestoreResultGuardRefused: true, RestoreResultOther: true,
	}
	for _, c := range restoreResultClasses {
		if c.Countable() != countable[c] {
			t.Errorf("%q Countable() = %v, want %v", c, c.Countable(), countable[c])
		}
	}
	// Problem codes of the console's refusals: the final list.
	for code, status := range map[string]int{
		CodeAgentTooOld: 409, CodeDeviceRetiredDownloadOnly: 409, CodeDevicePurging: 409, CodeDeviceRestoreActive: 409, CodeRestoreExpired: 410,
	} {
		if !has(ErrorCodes(), code) || HTTPStatus(code) != status {
			t.Errorf("%q is not in the error table with status %d", code, status)
		}
		if looksLikePath(code) || MessageFor(code) == unknownCodeMessage {
			t.Errorf("%q has no fixed path-free message", code)
		}
	}
	// Removed (blind check of P22-A, L4 and L6): another device's restore answers
	// 404 not_found exactly like a missing one, and a customer-held key answers
	// the existing custody_customer_held.
	for _, gone := range []string{"restore_not_yours", "customer_held"} {
		if has(ErrorCodes(), gone) {
			t.Errorf("%q is in the error table; it was removed", gone)
		}
	}
	if HTTPStatus(CodeNotFound) != 404 || HTTPStatus(CodeCustodyCustomerHeld) != 409 {
		t.Error("the codes that replace the removed ones changed status")
	}
}

// The path-free guard has exactly two exemptions. Put a path into every string
// of every wire type, one at a time (struct fields, slice and array elements,
// map keys and values, the value behind an interface; plantPaths): each must
// be caught, except inside a RestoreSelection (the marker) and
// restic.Snapshot.Paths (the field skip). A new field anywhere, the restore
// block of RunStats included, is covered without anyone remembering to extend
// a test.
func TestAssertPathFreeCatchesAPathInEveryStringFieldButTheTwoExemptions(t *testing.T) {
	visited := map[string]bool{}
	exemptSeen := map[string]bool{}
	for _, v := range wireValues() {
		typ := reflect.TypeOf(v)
		ptr := reflect.New(typ)
		ptr.Elem().Set(reflect.ValueOf(v))
		if _, ok := v.(pathBearing); ok {
			exemptSeen[typ.Name()] = true
			if err := AssertPathFree(ptr.Interface()); err != nil {
				t.Errorf("%s is the marked type and must pass: %v", typ.Name(), err)
			}
			continue
		}
		res := plantPaths(ptr, func() error { return AssertPathFree(ptr.Interface()) })
		for _, m := range res.missed {
			t.Errorf("%s.%s: a path here passed the guard", typ.Name(), m)
		}
		for p := range res.visited {
			visited[typ.Name()+"."+p] = true
		}
		for e := range res.exempt {
			exemptSeen[e] = true
		}
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
	req := RunsRequest{Runs: []RunRecord{okRestoreRun()}}
	req.Runs[0].Stats.RestoreErrors[1].Class = RestoreResultClass(plantLeak)
	if pe := pathErr(t, req); pe.Path != "runs[0].stats.restore_errors[1].class" {
		t.Errorf("path = %q", pe.Path)
	}
	// A path in a problem code is caught too.
	if err := AssertPathFree(Error{Code: "/C/Users/x", Title: "t", Type: "t"}); err == nil {
		t.Error("a path in Error.Code passed")
	}
}
