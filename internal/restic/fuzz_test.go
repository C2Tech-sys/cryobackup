package restic

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// FuzzDecoder feeds arbitrary bytes to the stream decoder (with every kind),
// the whole-document decoders, the prune parser, the classifier and Ext. None
// may panic, the stream decoder may only fail with io.EOF (a bytes.Reader never
// fails), and every message must satisfy the boundary invariants.
//
// Run it with:
//
//	go test -run=^$ -fuzz=FuzzDecoder -fuzztime=10s ./internal/restic
func FuzzDecoder(f *testing.F) {
	_ = filepath.WalkDir("testdata", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) == ".md" || filepath.Base(filepath.Dir(path)) == "regen" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		f.Add(raw) // the whole file, then each of its lines
		for _, line := range bytes.Split(raw, []byte("\n")) {
			if len(line) > 0 {
				f.Add(line)
			}
		}
		return nil
	})
	f.Add([]byte(`{"message_type":"status","current_files":["C:\\x\\y.docx"]}`))
	f.Add([]byte("\xff\xfe{\x00}\n{\"message_type\":\n\n"))
	f.Add([]byte(`{"message_type":"error","error":{"message":"x"},"during":"` + string(bytes.Repeat([]byte("a"), 100)) + `"}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		for _, kind := range []Kind{"", KindBackup, KindRestore, KindCheck, KindPrune} {
			d := NewDecoder(bytes.NewReader(data)).ForKind(kind)
			// A line yields at most one message, and blank lines yield none,
			// so len(data)+1 calls is more than enough.
			for i := 0; i <= len(data)+1; i++ {
				m, err := d.Next()
				if err != nil {
					if !errors.Is(err, io.EOF) {
						t.Fatalf("Next returned %v", err)
					}
					if m != nil {
						t.Fatal("message returned together with an error")
					}
					break
				}
				if m == nil {
					t.Fatal("nil message without error")
				}
				if MessageType(m) == "" {
					t.Fatalf("%T has no message type", m)
				}
				switch v := m.(type) {
				case ErrorEvent:
					if !v.Class.Valid() || (v.Ext != "" && Ext("x."+v.Ext) != v.Ext) || len(v.During) > 32 {
						t.Fatalf("bad ErrorEvent %+v", v)
					}
				case ExitError:
					if !v.Class.Valid() {
						t.Fatalf("bad ExitError %+v", v)
					}
				case Unparsed:
					if !v.Class.Valid() {
						t.Fatalf("bad Unparsed %+v", v)
					}
				case Unknown:
					if len(v.Type) > 64 {
						t.Fatalf("bad Unknown %+v", v)
					}
				}
				if i == len(data)+1 {
					t.Fatal("decoder did not terminate")
				}
			}
		}
		_, _ = DecodeSnapshots(bytes.NewReader(data))
		_, _ = DecodeStats(bytes.NewReader(data))
		_, _ = DecodeFind(bytes.NewReader(data))
		_, _ = DecodeForget(bytes.NewReader(data))
		_, _ = DecodeVersion(bytes.NewReader(data))
		_, _ = DecodeConfig(bytes.NewReader(data))
		_, _ = DecodeInit(bytes.NewReader(data))
		_ = ParsePruneText(bytes.NewReader(data))
		if c := Classify(string(data)); !c.Valid() {
			t.Fatalf("Classify gave %q", c)
		}
		_ = Ext(string(data))
	})
}
