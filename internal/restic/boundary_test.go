package restic

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The parse boundary (architecture §8.4 item 1): nothing path-bearing may come
// out of the non-browsing message types.

var sentinels = []string{"PHI-sentinel", "patient-record", "other.pdf"}

func assertNoSentinel(t *testing.T, what, text string) {
	t.Helper()
	for _, s := range sentinels {
		if strings.Contains(text, s) {
			t.Errorf("%s leaks %q: %s", what, s, text)
		}
	}
}

func TestSentinelPathNeverEscapes(t *testing.T) {
	stdout := fixtureBytes(t, "../handwritten/sentinel.stdout.jsonl")
	stderr := fixtureBytes(t, "../handwritten/sentinel.stderr.jsonl")
	for _, raw := range [][]byte{stdout, stderr} {
		if !strings.Contains(string(raw), `C:\\fixture\\PHI-sentinel\\patient-record.docx`) && !strings.Contains(string(raw), `C:\fixture\PHI-sentinel\patient-record.docx`) {
			t.Fatal("sentinel fixture does not contain the sentinel path")
		}
	}

	out := decodeAll(t, stdout, KindBackup)
	errs := decodeAll(t, stderr, KindBackup)
	all := append(append([]Message(nil), out...), errs...)

	counts := make(ErrorCounts)
	var sawNode bool
	for _, m := range all {
		switch v := m.(type) {
		case Node:
			// Browsing type: the path is the point.
			sawNode = true
			if v.Name != "patient-record.docx" || !strings.Contains(v.Path, "PHI-sentinel") {
				t.Errorf("node = %+v", v)
			}
			continue
		case LsSnapshot:
			continue
		case ErrorEvent:
			counts.Add(v)
		}
		assertNoSentinel(t, fmt.Sprintf("%%+v of %T", m), fmt.Sprintf("%+v", m))
		assertNoSentinel(t, fmt.Sprintf("%%#v of %T", m), fmt.Sprintf("%#v", m))
		assertNoSentinel(t, fmt.Sprintf("%%v of %T", m), fmt.Sprint(m))
		if enc, err := json.Marshal(m); err == nil {
			assertNoSentinel(t, fmt.Sprintf("JSON of %T", m), string(enc))
		}
	}
	if !sawNode {
		t.Error("the fixture's node line was not decoded")
	}

	// What the fixture must have produced.
	// Every string field of every non-browsing type carried the sentinel in the
	// fixture: verbose_status.action, summary.snapshot_id, the message_type of
	// the sixth stdout line and the "during" of the last stderr line.
	wantOut := []Message{
		BackupStatus{SecondsElapsed: 3, SecondsRemaining: 4, PercentDone: 0.5, TotalFiles: 2, FilesDone: 1, TotalBytes: 100, BytesDone: 50, ErrorCount: 1},
		VerboseStatus{Action: "?", DataSize: 10, DataSizeInRepo: 8, MetadataSize: 1, MetadataSizeInRepo: 1, TotalFiles: 1, Duration: 0.1},
		Unknown{Type: "info"},
		BackupSummary{
			FilesNew: 1, DirsNew: 1, DataBlobs: 1, TreeBlobs: 2, DataAdded: 10, DataAddedPacked: 8,
			TotalFilesProcessed: 1, TotalBytesProcessed: 10, TotalDuration: 0.2,
			BackupStart: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			BackupEnd:   time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC),
			// SnapshotID: the sentinel was dropped.
		},
	}
	if len(out) != 7 {
		t.Fatalf("stdout decoded to %d messages, want 7: %#v", len(out), out)
	}
	if !reflect.DeepEqual(out[:4], wantOut) {
		t.Errorf("stdout head = %#v\nwant %#v", out[:4], wantOut)
	}
	if u, ok := out[5].(Unknown); !ok || u.Type != "?" {
		t.Errorf("message_type sentinel = %#v, want Unknown{Type: \"?\"}", out[5])
	}
	// A well-formed id still comes through.
	if s, ok := out[6].(BackupSummary); !ok || s.SnapshotID != "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" || s.FilesUnmodified != 1 {
		t.Errorf("summary with a real id = %#v", out[6])
	}
	wantErr := []Message{
		ErrorEvent{During: "archival", Class: ClassLocked, Ext: "docx"},
		ErrorEvent{Class: ClassUnknown},
		Unparsed{Class: ClassMissingSource},
		ExitError{Code: 3, Class: ClassUnknown},
		ErrorEvent{During: "", Class: ClassLocked, Ext: "docx"}, // "during" carried the sentinel
	}
	if !reflect.DeepEqual(errs, wantErr) {
		t.Errorf("stderr = %#v\nwant %#v", errs, wantErr)
	}
	if got := Ext(`C:\fixture\PHI-sentinel\patient-record.docx`); got != "docx" {
		t.Errorf("Ext = %q, want docx", got)
	}

	// The projection that leaves the device.
	proj, err := json.Marshal(counts.Projection())
	if err != nil {
		t.Fatal(err)
	}
	assertNoSentinel(t, "Projection JSON", string(proj))
	if !strings.Contains(string(proj), `"ext":"docx"`) || strings.ContainsAny(string(proj), `\/`) {
		t.Errorf("projection = %s", proj)
	}
	if by, _ := json.Marshal(counts.ByClass()); strings.Contains(string(by), "sentinel") {
		t.Errorf("ByClass = %s", by)
	}
	if d := counts.Dominant(); d != ClassLocked && d != ClassUnknown {
		t.Errorf("Dominant = %s", d)
	}
}

// TestNonBrowsingTypesCannotHoldText is a structural guard: apart from fields
// the decoder reduces to a closed vocabulary (a class, a sanitised word, an
// allow-listed extension), no non-browsing message type has a string, slice,
// map, pointer or interface field, so there is nowhere to keep a path.
//
// A string field is not exempt by being listed. Each one is listed with the
// reducer the decoder applies to it, and the test pushes the sentinel path
// through that reducer and requires something path-free to come out. That
// includes VerboseStatus.Action and BackupSummary.SnapshotID, which restic
// fills with free-form-looking values and which the decoder reduces to restic's
// action words and to a hex id. TestSentinelPathNeverEscapes proves the same
// end to end, from the fixture's raw lines.
func TestNonBrowsingTypesCannotHoldText(t *testing.T) {
	allowedStrings := map[string]func(string) string{
		"ErrorEvent.During":        func(s string) string { return safeWord(s, 32, "") },
		"ErrorEvent.Class":         func(s string) string { return string(Classify(s)) },
		"ErrorEvent.Ext":           Ext,
		"ExitError.Class":          func(s string) string { return string(Classify(s)) },
		"Unknown.Type":             func(s string) string { return safeWord(s, 64, "?") },
		"Unparsed.Class":           func(s string) string { return string(Classify(s)) },
		"VerboseStatus.Action":     safeAction,
		"BackupSummary.SnapshotID": safeSnapshotID,
	}
	const sentinelPath = `C:\fixture\PHI-sentinel\patient-record.docx`
	for name, reduce := range allowedStrings {
		got := reduce(sentinelPath)
		assertNoSentinel(t, name+" reducer output", got)
		if len(got) > 64 || strings.ContainsAny(got, `\/: `) {
			t.Errorf("%s reducer keeps %q of a path", name, got)
		}
	}
	types := []Message{
		BackupStatus{}, BackupSummary{}, VerboseStatus{}, ErrorEvent{}, ExitError{},
		RestoreStatus{}, RestoreSummary{}, CheckSummary{}, Unknown{}, Unparsed{},
	}
	for _, m := range types {
		rt := reflect.TypeOf(m)
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			name := rt.Name() + "." + f.Name
			switch f.Type.Kind() {
			case reflect.String:
				if allowedStrings[name] == nil {
					t.Errorf("%s is a string field", name)
				}
			case reflect.Slice, reflect.Map, reflect.Interface, reflect.Ptr, reflect.Array, reflect.Chan, reflect.Func:
				t.Errorf("%s has a container kind %s", name, f.Type.Kind())
			}
		}
	}
	for name := range allowedStrings {
		parts := strings.SplitN(name, ".", 2)
		found := false
		for _, m := range types {
			if reflect.TypeOf(m).Name() == parts[0] {
				_, found = reflect.TypeOf(m).FieldByName(parts[1])
			}
		}
		if !found {
			t.Errorf("allow-list names a field that does not exist: %s", name)
		}
	}
}

// TestPathBearingTypesAreExactlyTheDocumentedOnes pins the list in doc.go and
// proves that none of them can print itself.
func TestPathBearingTypesPrintNothing(t *testing.T) {
	stringer := reflect.TypeOf((*fmt.Stringer)(nil)).Elem()
	errorIface := reflect.TypeOf((*error)(nil)).Elem()
	goStringer := reflect.TypeOf((*fmt.GoStringer)(nil)).Elem()
	all := []any{
		// browsing types
		LsSnapshot{}, Node{}, Snapshot{}, FindResult{}, FindMatch{}, ForgetGroup{}, ForgetReason{},
		// everything else
		BackupStatus{}, BackupSummary{}, VerboseStatus{}, ErrorEvent{}, ExitError{}, RestoreStatus{},
		RestoreSummary{}, CheckSummary{}, Unknown{}, Unparsed{}, StatsResult{}, VersionInfo{}, Config{},
		InitResult{}, PruneStats{}, ErrorKey{}, ErrorProjection{}, Command{}, Env{}, Result{}, Evidence{},
	}
	for _, v := range all {
		for _, rt := range []reflect.Type{reflect.TypeOf(v), reflect.PointerTo(reflect.TypeOf(v))} {
			for _, iface := range []reflect.Type{stringer, errorIface, goStringer} {
				if rt.Implements(iface) {
					t.Errorf("%s implements %s", rt, iface)
				}
			}
		}
	}
}

// TestFixtureScrub walks every file under testdata (the regeneration tool
// included) and fails when anything of the capturing machine is left.
func TestFixtureScrub(t *testing.T) {
	forbidden := []string{`C:\\Users`, `C:\Users`, `/Users/`, `AppData`, `\\Temp\\`, `\Temp\`, `AzureAD`}
	jsonValue := func(key string) *regexp.Regexp {
		return regexp.MustCompile(`"` + key + `"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	}
	username, hostname := jsonValue("username"), jsonValue("hostname")

	files := 0
	err := filepath.WalkDir("testdata", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(raw)
		for _, f := range forbidden {
			if strings.Contains(text, f) {
				t.Errorf("%s contains %q", path, f)
			}
		}
		for _, m := range username.FindAllStringSubmatch(text, -1) {
			if m[1] != "fixture-user" {
				t.Errorf("%s: username %q", path, m[1])
			}
		}
		for _, m := range hostname.FindAllStringSubmatch(text, -1) {
			if m[1] != "fixture-host" {
				t.Errorf("%s: hostname %q", path, m[1])
			}
		}
		// The machine's own name and account never appear, whatever their spelling.
		if host, err := os.Hostname(); err == nil && len(host) > 3 && strings.Contains(strings.ToLower(text), strings.ToLower(host)) {
			t.Errorf("%s contains the host name of the machine running the test", path)
		}
		if home, err := os.UserHomeDir(); err == nil {
			if name, ok := scrubbableProfileName(filepath.Base(home)); ok && strings.Contains(strings.ToLower(text), name) {
				t.Errorf("%s contains the profile name of the user running the test", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 30 {
		t.Fatalf("scrub test saw only %d files under testdata", files)
	}
}

// commonAccountNames are profile names that say nothing about a person and that
// occur in ordinary text (a lab or CI host runs as root or runner), so finding one
// in a fixture is not a leak of the capturing machine.
var commonAccountNames = map[string]bool{"root": true, "runner": true, "admin": true, "user": true, "system": true}

// scrubbableProfileName reports whether the name of the running user's profile
// folder identifies a person well enough for the scrub to look for it, and
// returns it lower-cased. Names shorter than five characters match too much
// ordinary text, and the common system accounts are not personal.
func scrubbableProfileName(base string) (string, bool) {
	name := strings.ToLower(strings.TrimSpace(base))
	if len(name) < 5 || commonAccountNames[name] {
		return "", false
	}
	return name, true
}

// The scrub looks for the profile name of the machine that runs the test, so the
// name must be one that identifies a person: not a short name, not a system account.
func TestScrubIgnoresShortAndSystemProfileNames(t *testing.T) {
	for _, base := range []string{"root", "Root", "runner", "RUNNER", "admin", "user", "System", "abc", "ab", "a", "", "  ", "dave", "jo"} {
		if name, ok := scrubbableProfileName(base); ok {
			t.Errorf("profile name %q must not be scrubbed for, got %q", base, name)
		}
	}
	for base, want := range map[string]string{"sampleuser": "sampleuser", "Alexandria": "alexandria", "builder1": "builder1", "tech12": "tech12"} {
		if name, ok := scrubbableProfileName(base); !ok || name != want {
			t.Errorf("profile name %q: got %q, %v, want %q, true", base, name, ok, want)
		}
	}
}

func TestPathsInFixturesUseThePlaceholderRoot(t *testing.T) {
	// Every Windows drive path in the captured fixtures is under C:\fixture.
	drivePath := regexp.MustCompile(`\b[A-Za-z]:(?:\\\\|\\|/)[^"\s]*`)
	err := filepath.WalkDir(fixtureDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(path, "MANIFEST.md") {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, p := range drivePath.FindAllString(string(raw), -1) {
			norm := strings.NewReplacer(`\\\\`, `\`, `\\`, `\`, "/", `\`).Replace(p)
			if !strings.HasPrefix(strings.ToLower(norm), `c:\fixture`) {
				t.Errorf("%s holds the path %q", path, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
