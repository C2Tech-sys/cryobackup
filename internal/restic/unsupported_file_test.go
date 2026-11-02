package restic

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// TestDecodeUnsupportedFile: restic 0.19.1 reports a file it cannot archive
// with the message unsupported file type "irregular" (or "") and exits 3. The
// archiver puts the item path in front of a message that does not already
// contain it (internal/archiver/archiver.go, Archiver.error:
// fmt.Errorf("%v: %w", item, err)), so the JSON line restic prints is
//
//	{"message_type":"error","error":{"message":"<item>: unsupported file type \"irregular\""},"during":"archival","item":"<item>"}
//
// The hand-written fixture has that shape; the paths are placeholders, and the
// fourth one is full of words earlier classifier rows react to (proxy, vss,
// certificate). Each error becomes a path-free unsupported_file event, the run
// is partial, and nothing of the path survives except the allow-listed
// extension.
func TestDecodeUnsupportedFile(t *testing.T) {
	raw, err := os.ReadFile("testdata/handwritten/unsupported_file.stderr.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	msgs := decodeAll(t, raw, KindBackup)
	requireNone[Unparsed](t, msgs)
	requireNone[Unknown](t, msgs)
	want := []ErrorEvent{
		{During: "archival", Class: ClassUnsupportedFile, Ext: ""},
		{During: "archival", Class: ClassUnsupportedFile, Ext: "docx"},
		{During: "archival", Class: ClassUnsupportedFile, Ext: ""},
		{During: "archival", Class: ClassUnsupportedFile, Ext: ""},
	}
	got := only[ErrorEvent](msgs)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("error events = %+v, want %+v", got, want)
	}
	exits := only[ExitError](msgs)
	if len(exits) != 1 || exits[0].Code != 3 || exits[0].Class != ClassUnknown {
		t.Fatalf("exit errors = %+v, want one exit 3 with class unknown", exits)
	}

	counts := make(ErrorCounts)
	for _, e := range got {
		counts.Add(e)
	}
	if by := counts.ByClass(); !reflect.DeepEqual(by, map[ErrorClass]int{ClassUnsupportedFile: 4}) {
		t.Fatalf("ByClass = %v", by)
	}
	if d := counts.Dominant(); d != ClassUnsupportedFile {
		t.Fatalf("Dominant = %s", d)
	}
	if c := StderrClass(&exits[0], counts); c != ClassUnsupportedFile {
		t.Fatalf("StderrClass = %s", c)
	}
	// Exit 3 is a partial run whatever the class: this class never fails a run on its own.
	if r := Map(KindBackup, 3, Evidence{Stderr: ClassUnsupportedFile, SummarySeen: true}); r != (Result{Outcome: OutcomePartial}) {
		t.Fatalf("Map(backup, 3, unsupported_file) = %+v, want partial", r)
	}
	if ClassUnsupportedFile.Hard() {
		t.Fatal("unsupported_file must not be a hard class")
	}
	if !ClassUnsupportedFile.Valid() {
		t.Fatal("unsupported_file must be valid")
	}
	// No path text in anything that would leave the device.
	if s := fmt.Sprintf("%+v %+v", got, counts.Projection()); strings.Contains(s, "fixture") || strings.Contains(s, "alias") || strings.Contains(s, "proxy") {
		t.Fatalf("path text leaked: %s", s)
	}
}

// errorLine builds one restic --json error line the way restic 0.19.1 prints
// it: the message as given (callers put the item prefix in themselves) and the
// item, JSON-escaped by encoding/json as restic's own encoder escapes them.
func errorLine(t *testing.T, message, item string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"message_type": "error",
		"error":        map[string]string{"message": message},
		"during":       "archival",
		"item":         item,
	})
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// TestDecodeErrorClassIgnoresTheItemPath: the class of an error line comes
// from what restic says went wrong, never from the words in the file's path.
// Each case is a real restic message shape with the item path prefixed (as
// Archiver.error does) or embedded (as an *os.PathError already is, so restic
// adds no prefix); the path is chosen to contain an earlier row's keyword.
func TestDecodeErrorClassIgnoresTheItemPath(t *testing.T) {
	tests := []struct {
		name, item, message string
		want                ErrorClass
	}{
		{"unsupported under proxy", `C:\proxy\x.exe`, `C:\proxy\x.exe: unsupported file type "irregular"`, ClassUnsupportedFile},
		{"unsupported empty type", `C:\data\vss\thing`, `C:\data\vss\thing: unsupported file type ""`, ClassUnsupportedFile},
		{"unsupported under every keyword", `C:\certificate\privilege\does not exist\no key found\500\x.exe`, `C:\certificate\privilege\does not exist\no key found\500\x.exe: unsupported file type "irregular"`, ClassUnsupportedFile},
		{"permission under vss", `C:\vss\proxy\x.docx`, `C:\vss\proxy\x.docx: Access is denied.`, ClassPermission},
		{"embedded path permission", `C:\shadow copy\x.docx`, `open C:\shadow copy\x.docx: Access is denied.`, ClassPermission},
		{"embedded path locked", `C:\certificate\book.xlsx`, `open C:\certificate\book.xlsx: The process cannot access the file because it is being used by another process.`, ClassLocked},
		{"forward slash item", `/data/proxy/x`, `/data/proxy/x: unsupported file type "irregular"`, ClassUnsupportedFile},
		// A path that contains the unsupported text must not make an unrelated error tolerable.
		{"unsupported text in path, unrelated error", `C:\unsupported file type "irregular"\x.bin`, `C:\unsupported file type "irregular"\x.bin: pack 1a2b3c4d is damaged`, ClassUnknown},
		{"unsupported text in path, keyword-free", `C:\unsupported file type\x`, `C:\unsupported file type\x: something odd happened`, ClassUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs := decodeAll(t, errorLine(t, tt.message, tt.item), KindBackup)
			ev := only[ErrorEvent](msgs)
			if len(ev) != 1 {
				t.Fatalf("got %d error events from %v", len(ev), msgs)
			}
			if ev[0].Class != tt.want {
				t.Fatalf("class = %s, want %s", ev[0].Class, tt.want)
			}
		})
	}
}

// TestUnsupportedFileNearMisses pins restic strings that start like the
// unsupported-file text but mean something else to unknown: the row matches
// restic's exact message and nothing near it, bare or behind an item prefix.
func TestUnsupportedFileNearMisses(t *testing.T) {
	for _, msg := range []string{
		"unsupported repository version 3",
		"unsupported ACL format version",
		"unsupported file system feature",
		"unsupported file",
		`unsupported file type "symlink"`,
		`unsupported file type "irregular" and more`,
		`error: unsupported file type "irregular"`,
	} {
		if got := Classify(msg); got != ClassUnknown {
			t.Errorf("Classify(%q) = %s, want unknown", msg, got)
		}
		item := `C:\data\x.bin`
		ev := only[ErrorEvent](decodeAll(t, errorLine(t, item+": "+msg, item), KindBackup))
		if len(ev) != 1 || ev[0].Class != ClassUnknown {
			t.Errorf("decoded %q: %+v, want one unknown event", msg, ev)
		}
	}
}
