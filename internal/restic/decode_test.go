package restic

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const fixtureDir = "testdata/v0.19.1"

func fixtureBytes(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatalf("fixture %s: %v (regenerate with testdata/regen)", name, err)
	}
	return b
}

// decodeAll drains a Decoder.
func decodeAll(t testing.TB, data []byte, kind Kind) []Message {
	t.Helper()
	d := NewDecoder(bytes.NewReader(data))
	if kind != "" {
		d.ForKind(kind)
	}
	var out []Message
	for {
		m, err := d.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, m)
	}
}

func fixtureMessages(t testing.TB, name string, kind Kind) []Message {
	return decodeAll(t, fixtureBytes(t, name), kind)
}

// only returns the messages of type T.
func only[T Message](msgs []Message) []T {
	var out []T
	for _, m := range msgs {
		if v, ok := m.(T); ok {
			out = append(out, v)
		}
	}
	return out
}

func requireNone[T Message](t *testing.T, msgs []Message) {
	t.Helper()
	if got := only[T](msgs); len(got) != 0 {
		t.Fatalf("unexpected %T messages: %v", *new(T), got)
	}
}

func TestDecodeBackupOK(t *testing.T) {
	msgs := fixtureMessages(t, "backup_ok.stdout.jsonl", KindBackup)
	requireNone[Unparsed](t, msgs)
	requireNone[Unknown](t, msgs)
	statuses := only[BackupStatus](msgs)
	if len(statuses) == 0 {
		t.Fatal("no status lines decoded (fixture captured with RESTIC_PROGRESS_FPS=60)")
	}
	last := statuses[len(statuses)-1]
	if last.TotalFiles != 5 || last.FilesDone != 5 || last.PercentDone != 1 || last.TotalBytes == 0 || last.BytesDone != last.TotalBytes {
		t.Errorf("last status = %+v", last)
	}
	sums := only[BackupSummary](msgs)
	if len(sums) != 1 {
		t.Fatalf("got %d summaries", len(sums))
	}
	s := sums[0]
	if s.SnapshotID == "" || len(s.SnapshotID) != 64 {
		t.Errorf("snapshot id %q", s.SnapshotID)
	}
	if s.FilesNew != 5 || s.FilesChanged != 0 || s.FilesUnmodified != 0 || s.TotalFilesProcessed != 5 || s.DryRun {
		t.Errorf("summary counters = %+v", s)
	}
	if s.DirsNew == 0 || s.DataBlobs == 0 || s.TreeBlobs == 0 || s.DataAdded == 0 || s.DataAddedPacked == 0 || s.TotalBytesProcessed == 0 || s.TotalDuration <= 0 {
		t.Errorf("summary sizes = %+v", s)
	}
	if s.BackupStart.IsZero() || !s.BackupEnd.After(s.BackupStart) {
		t.Errorf("times start=%v end=%v", s.BackupStart, s.BackupEnd)
	}
	if len(decodeAll(t, fixtureBytes(t, "backup_ok.stderr.jsonl"), KindBackup)) != 0 {
		t.Error("a clean backup should leave stderr empty")
	}
	// The same stream decodes identically when the kind is not given.
	if got := fixtureMessages(t, "backup_ok.stdout.jsonl", ""); !reflect.DeepEqual(got, msgs) {
		t.Error("inference without ForKind disagrees with ForKind(KindBackup)")
	}
}

func TestDecodeBackupSkipped(t *testing.T) {
	sums := only[BackupSummary](fixtureMessages(t, "backup_skipped.stdout.jsonl", KindBackup))
	if len(sums) != 1 || sums[0].SnapshotID != "" || sums[0].FilesUnmodified != 1 || sums[0].FilesNew != 0 {
		t.Fatalf("summaries = %+v (SnapshotID must be empty when --skip-if-unchanged skipped)", sums)
	}
}

func TestDecodeBackupPartial(t *testing.T) {
	out := fixtureMessages(t, "backup_partial.stdout.jsonl", KindBackup)
	if s := only[BackupSummary](out); len(s) != 1 || s[0].SnapshotID == "" {
		t.Fatalf("a partial backup still creates a snapshot: %+v", s)
	}
	// restic 0.19.1 reports a missing root as a plain-text line, not a JSON error.
	errs := fixtureMessages(t, "backup_partial.stderr.jsonl", KindBackup)
	want := []Message{Unparsed{Class: ClassMissingSource}, ExitError{Code: 3, Class: ClassUnknown}}
	if !reflect.DeepEqual(errs, want) {
		t.Fatalf("stderr = %#v, want %#v", errs, want)
	}
}

func TestDecodeBackupLocked(t *testing.T) {
	out := fixtureMessages(t, "backup_locked.stdout.jsonl", KindBackup)
	var sawError bool
	for _, st := range only[BackupStatus](out) {
		if st.ErrorCount == 1 {
			sawError = true
		}
	}
	if !sawError {
		t.Error("no status line with error_count 1")
	}
	errs := fixtureMessages(t, "backup_locked.stderr.jsonl", KindBackup)
	want := []Message{
		ErrorEvent{During: "archival", Class: ClassLocked, Ext: "docx"},
		ExitError{Code: 3, Class: ClassUnknown},
	}
	if !reflect.DeepEqual(errs, want) {
		t.Fatalf("stderr = %#v, want %#v", errs, want)
	}
	counts := make(ErrorCounts)
	for _, e := range only[ErrorEvent](errs) {
		counts.Add(e)
	}
	if got := StderrClass(&ExitError{Code: 3, Class: ClassUnknown}, counts); got != ClassLocked {
		t.Errorf("StderrClass = %s, want locked", got)
	}
}

func TestDecodeBackupDryRun(t *testing.T) {
	msgs := fixtureMessages(t, "backup_dry_run.stdout.jsonl", KindBackup)
	requireNone[Unparsed](t, msgs)
	vs := only[VerboseStatus](msgs)
	if len(vs) < 5 {
		t.Fatalf("only %d verbose_status lines", len(vs))
	}
	if vs[0].Action != "scan_finished" || vs[0].DataSize == 0 || vs[0].TotalFiles != 5 {
		t.Errorf("first verbose_status = %+v", vs[0])
	}
	actions := map[string]bool{}
	for _, v := range vs {
		actions[v.Action] = true
	}
	for _, a := range []string{"scan_finished", "unchanged"} {
		if !actions[a] {
			t.Errorf("no %q action in %v", a, actions)
		}
	}
	sums := only[BackupSummary](msgs)
	if len(sums) != 1 || !sums[0].DryRun {
		t.Fatalf("summary = %+v, want dry_run true", sums)
	}
}

func TestDecodeRestore(t *testing.T) {
	for _, kind := range []Kind{KindRestore, ""} {
		t.Run(fmt.Sprintf("kind=%q", kind), func(t *testing.T) {
			msgs := fixtureMessages(t, "restore.stdout.jsonl", kind)
			requireNone[Unparsed](t, msgs)
			requireNone[BackupSummary](t, msgs)
			requireNone[BackupStatus](t, msgs)
			sums := only[RestoreSummary](msgs)
			if len(sums) != 1 || sums[0].FilesRestored != 8 || sums[0].TotalFiles != 8 || sums[0].BytesRestored != sums[0].TotalBytes || sums[0].TotalBytes == 0 {
				t.Fatalf("summary = %+v", sums)
			}
			st := only[RestoreStatus](msgs)
			if len(st) == 0 || st[0].PercentDone != 1 || st[0].TotalFiles != 8 {
				t.Fatalf("status = %+v", st)
			}
		})
	}
	// the same line under a different kind
	st := fixtureMessages(t, "restore.stdout.jsonl", KindBackup)
	if len(only[BackupStatus](st)) == 0 || len(only[RestoreStatus](st)) != 0 {
		t.Error("ForKind(KindBackup) must pick the backup structs")
	}
}

func TestDecodeRestoreDryRun(t *testing.T) {
	msgs := fixtureMessages(t, "restore_dry_run.stdout.jsonl", KindRestore)
	vs := only[VerboseStatus](msgs)
	if len(vs) != 8 {
		t.Fatalf("got %d verbose_status lines, want 8", len(vs))
	}
	var total uint64
	for _, v := range vs {
		if v.Action != "restored" {
			t.Errorf("action %q", v.Action)
		}
		total += v.Size
	}
	if total != 2097213 {
		t.Errorf("sizes add up to %d", total)
	}
	if s := only[RestoreSummary](msgs); len(s) != 1 || s[0].FilesRestored != 8 {
		t.Fatalf("summary = %+v", s)
	}
}

func TestDecodeRestoreError(t *testing.T) {
	// restic prints the summary even though the run exits 1.
	if s := only[RestoreSummary](fixtureMessages(t, "restore_error.stdout.jsonl", KindRestore)); len(s) != 1 {
		t.Fatalf("summary = %+v", s)
	}
	errs := fixtureMessages(t, "restore_error.stderr.jsonl", KindRestore)
	want := []Message{
		ErrorEvent{During: "restore", Class: ClassPermission},
		ExitError{Code: 1, Class: ClassUnknown},
	}
	if !reflect.DeepEqual(errs, want) {
		t.Fatalf("stderr = %#v, want %#v", errs, want)
	}
}

func TestDecodeCheck(t *testing.T) {
	for _, kind := range []Kind{KindCheck, ""} {
		msgs := fixtureMessages(t, "check.stdout.jsonl", kind)
		want := []Message{CheckSummary{}}
		if !reflect.DeepEqual(msgs, want) {
			t.Errorf("kind %q: %#v, want %#v", kind, msgs, want)
		}
	}
	damaged := fixtureMessages(t, "check_damaged.stdout.jsonl", KindCheck)
	want := []Message{CheckSummary{NumErrors: 1, BrokenPacks: 1}}
	if !reflect.DeepEqual(damaged, want) {
		t.Fatalf("damaged summary = %#v, want %#v", damaged, want)
	}
	errs := fixtureMessages(t, "check_damaged.stderr.jsonl", KindCheck)
	events := only[ErrorEvent](errs)
	if len(events) != 4 {
		t.Fatalf("got %d error events, want 4: %v", len(events), errs)
	}
	for _, e := range events {
		if e.During != "check" || e.Class != ClassUnknown || e.Ext != "" {
			t.Errorf("event = %+v, want {check unknown \"\"}", e)
		}
	}
	if x := only[ExitError](errs); len(x) != 1 || x[0].Code != 1 {
		t.Errorf("exit error = %+v", x)
	}
	// Without the kind the phase is unknown rather than guessed.
	for _, e := range only[ErrorEvent](fixtureMessages(t, "check_damaged.stderr.jsonl", "")) {
		if e.During != "" {
			t.Errorf("During %q without ForKind", e.During)
		}
	}
}

func TestDecodeExitErrors(t *testing.T) {
	tests := []struct {
		file string
		want ExitError
	}{
		{"exit10_repo_missing.stderr.jsonl", ExitError{Code: 10, Class: ClassRepoMissing}},
		{"exit11_lock.stderr.jsonl", ExitError{Code: 11, Class: ClassLock}},
		{"exit12_wrong_password.stderr.jsonl", ExitError{Code: 12, Class: ClassWrongPassword}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			msgs := fixtureMessages(t, tt.file, "")
			if len(msgs) != 1 || msgs[0] != Message(tt.want) {
				t.Fatalf("got %#v, want [%#v]", msgs, tt.want)
			}
			// and the whole chain: stderr class -> Map
			r := Map(KindBackup, tt.want.Code, Evidence{Stderr: tt.want.Class})
			if r.Outcome == OutcomeSuccess {
				t.Errorf("Map says success for exit %d", tt.want.Code)
			}
		})
	}
}

func TestDecodeLs(t *testing.T) {
	msgs := fixtureMessages(t, "ls.jsonl", "")
	requireNone[Unparsed](t, msgs)
	requireNone[Unknown](t, msgs)
	snaps := only[LsSnapshot](msgs)
	if len(snaps) != 1 {
		t.Fatalf("got %d snapshot records", len(snaps))
	}
	if _, ok := msgs[0].(LsSnapshot); !ok {
		t.Fatalf("first line is %T, want LsSnapshot", msgs[0])
	}
	s := snaps[0]
	if len(s.ID) != 64 || len(s.ShortID) != 8 || s.Hostname != "fixture-host" || s.Time.IsZero() ||
		!reflect.DeepEqual(s.Paths, []string{`C:\fixture\tree`}) || len(s.Tags) != 3 || s.Tags[0] != "cb.customer=fixture-customer" {
		t.Errorf("snapshot record = %+v", s)
	}
	nodes := only[Node](msgs)
	// ls <snap> <dir> is one level: the directory itself and its direct children.
	var paths []string
	for _, n := range nodes {
		paths = append(paths, n.Path)
	}
	want := []string{"/C/fixture/tree", "/C/fixture/tree/invoices", "/C/fixture/tree/media", "/C/fixture/tree/notes"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("node paths = %v, want %v (non-recursive)", paths, want)
	}
	for i, n := range nodes {
		if n.Type != "dir" || n.Mtime.IsZero() || n.Permissions[0] != 'd' || n.Mode == 0 {
			t.Errorf("node %d = %+v", i, n)
		}
	}
	if nodes[1].Name != "invoices" {
		t.Errorf("node name %q", nodes[1].Name)
	}

	roots := fixtureMessages(t, "ls_roots.jsonl", "")
	rn := only[Node](roots)
	if len(only[LsSnapshot](roots)) != 1 || len(rn) != 1 || rn[0].Path != "/C" || rn[0].Name != "C" {
		t.Fatalf("roots listing = %#v", roots)
	}
}

func TestDecodeLsFileNodes(t *testing.T) {
	// A listing one level further down has file nodes with sizes.
	line := `{"name":"readme.txt","type":"file","path":"/C/fixture/tree/notes/readme.txt","uid":0,"gid":0,"size":15,"mode":438,"permissions":"-rw-rw-rw-","mtime":"2026-10-22T12:04:31.2067022-05:00","atime":"2026-10-22T12:04:31.2067022-05:00","ctime":"2026-10-22T12:04:31.2067022-05:00","inode":77,"message_type":"node","struct_type":"node"}` + "\n"
	got := decodeAll(t, []byte(line), "")
	n, ok := got[0].(Node)
	if !ok || n.Size != 15 || n.Mode != 438 || n.Type != "file" || n.Inode != 77 || n.Mtime.Year() != 2026 {
		t.Fatalf("got %#v", got)
	}
	// older restic: struct_type only
	old := `{"struct_type":"node","name":"a","type":"file","path":"/a","size":1}` + "\n" +
		`{"struct_type":"snapshot","id":"abc","hostname":"h"}` + "\n"
	got = decodeAll(t, []byte(old), "")
	if len(got) != 2 || got[0].(Node).Name != "a" || got[1].(LsSnapshot).Hostname != "h" {
		t.Fatalf("struct_type fallback: %#v", got)
	}
}

func TestDecodeSnapshots(t *testing.T) {
	snaps, err := DecodeSnapshots(bytes.NewReader(fixtureBytes(t, "snapshots.json")))
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 5 {
		t.Fatalf("got %d snapshots, want 5", len(snaps))
	}
	var old, parented int
	for _, s := range snaps {
		if len(s.ID) != 64 || len(s.ShortID) != 8 || s.ID[:8] != s.ShortID || len(s.Tree) != 64 {
			t.Errorf("ids: %+v", s)
		}
		if s.Hostname != "fixture-host" || s.Username != "fixture-user" || s.ProgramVersion != "restic 0.19.1" {
			t.Errorf("identity: host %q user %q version %q", s.Hostname, s.Username, s.ProgramVersion)
		}
		if len(s.Paths) == 0 || s.Time.IsZero() || len(s.Tags) != 3 {
			t.Errorf("snapshot = %+v", s)
		}
		if s.Summary == nil || s.Summary.FilesNew+s.Summary.FilesUnmodified+s.Summary.FilesChanged == 0 {
			t.Errorf("summary = %+v", s.Summary)
		} else if s.Summary.SnapshotID != "" {
			t.Errorf("snapshot summary carries snapshot_id %q", s.Summary.SnapshotID)
		}
		if s.Time.Year() == 2020 {
			old++
		}
		if s.Parent != "" {
			parented++
		}
	}
	if old != 2 {
		t.Errorf("%d snapshots dated 2020, want 2 (made with --time)", old)
	}
	if parented == 0 {
		t.Error("no snapshot has a parent")
	}
	if !snaps[0].Time.Before(snaps[1].Time) {
		t.Error("snapshots are listed oldest first")
	}
}

func TestDecodeFind(t *testing.T) {
	got, err := DecodeFind(bytes.NewReader(fixtureBytes(t, "find.json")))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Hits != 1 || len(got[0].Snapshot) != 64 || len(got[0].Matches) != 1 {
		t.Fatalf("got %+v", got)
	}
	m := got[0].Matches[0]
	if m.Path != "/C/fixture/tree/notes/readme.txt" || m.Type != "file" || m.Size != 15 || m.Permissions != "-rw-rw-rw-" || m.Mtime.IsZero() {
		t.Errorf("match = %+v", m)
	}
	// A notice line in front of the document (stderr merged into stdout) is skipped.
	merged := append([]byte("Ignoring \"filters\": explicit snapshot ids are given\n"), fixtureBytes(t, "find.json")...)
	if again, err := DecodeFind(bytes.NewReader(merged)); err != nil || !reflect.DeepEqual(again, got) {
		t.Fatalf("with notice: %v %+v", err, again)
	}
}

func TestDecodeStats(t *testing.T) {
	raw, err := DecodeStats(bytes.NewReader(fixtureBytes(t, "stats_raw_data.json")))
	if err != nil {
		t.Fatal(err)
	}
	if raw.TotalSize == 0 || raw.TotalUncompressedSize == 0 || raw.TotalBlobCount == 0 || raw.SnapshotsCount != 5 || raw.CompressionRatio <= 0 || raw.CompressionProgress != 100 {
		t.Errorf("raw-data = %+v", raw)
	}
	rs, err := DecodeStats(bytes.NewReader(fixtureBytes(t, "stats_restore_size.json")))
	if err != nil {
		t.Fatal(err)
	}
	if rs.TotalSize == 0 || rs.TotalFileCount == 0 || rs.SnapshotsCount != 5 || rs.TotalBlobCount != 0 {
		t.Errorf("restore-size = %+v", rs)
	}
}

func TestDecodeForget(t *testing.T) {
	for _, file := range []string{"forget_dry_run.json", "forget.json"} {
		t.Run(file, func(t *testing.T) {
			groups, err := DecodeForget(bytes.NewReader(fixtureBytes(t, file)))
			if err != nil {
				t.Fatal(err)
			}
			if len(groups) != 1 {
				t.Fatalf("got %d groups", len(groups))
			}
			g := groups[0]
			if g.Host != "fixture-host" || !reflect.DeepEqual(g.Paths, []string{`C:\fixture\tree`}) || g.Tags != nil {
				t.Errorf("group = host %q paths %v tags %v", g.Host, g.Paths, g.Tags)
			}
			if len(g.Keep) != 3 || len(g.Remove) != 2 || len(g.Reasons) != 3 {
				t.Fatalf("keep %d remove %d reasons %d, want 3/2/3", len(g.Keep), len(g.Remove), len(g.Reasons))
			}
			for _, s := range g.Remove {
				if s.Time.Year() != 2020 || len(s.ID) != 64 {
					t.Errorf("removed snapshot %+v", s)
				}
			}
			var last int
			for _, r := range g.Reasons {
				if len(r.Matches) == 0 || r.Snapshot.ID == "" {
					t.Errorf("reason = %+v", r)
				}
				for _, m := range r.Matches {
					if m == "last snapshot" {
						last++
					}
				}
			}
			if last != 1 {
				t.Errorf("%d snapshots matched keep-last", last)
			}
		})
	}
}

func TestDecodeSmallDocuments(t *testing.T) {
	v, err := DecodeVersion(bytes.NewReader(fixtureBytes(t, "version.json")))
	if err != nil || v.Version != "0.19.1" || v.GoVersion == "" || v.GoOS == "" || v.GoArch == "" {
		t.Errorf("version = %+v, %v", v, err)
	}
	c, err := DecodeConfig(bytes.NewReader(fixtureBytes(t, "cat_config.json")))
	if err != nil || c.Version != 2 || len(c.ID) != 64 || c.ChunkerPolynomial == "" {
		t.Errorf("config = %+v, %v", c, err)
	}
	i, err := DecodeInit(bytes.NewReader(fixtureBytes(t, "init.json")))
	if err != nil || len(i.ID) != 64 || i.Repository != `C:\fixture\repo` || i.ID != c.ID {
		t.Errorf("init = %+v, %v", i, err)
	}
}

func TestDocumentDecodersRejectGarbageWithoutEchoing(t *testing.T) {
	decoders := map[string]func(io.Reader) error{
		"snapshots": func(r io.Reader) error { _, err := DecodeSnapshots(r); return err },
		"stats":     func(r io.Reader) error { _, err := DecodeStats(r); return err },
		"find":      func(r io.Reader) error { _, err := DecodeFind(r); return err },
		"forget":    func(r io.Reader) error { _, err := DecodeForget(r); return err },
		"version":   func(r io.Reader) error { _, err := DecodeVersion(r); return err },
		"config":    func(r io.Reader) error { _, err := DecodeConfig(r); return err },
		"init":      func(r io.Reader) error { _, err := DecodeInit(r); return err },
	}
	inputs := []string{
		"", "   \n", "not json at all C:\\fixture\\PHI-sentinel\\x.docx", `{"id": `, `[{"id":"C:\fixture\PHI-sentinel"`,
		"Fatal: wrong password or no key found\n", `"just a string"`, "12345",
	}
	for name, dec := range decoders {
		for _, in := range inputs {
			err := dec(strings.NewReader(in))
			// version/config/init/stats accept "{...}" only; a bare string or number is malformed for all.
			if err == nil {
				t.Errorf("%s accepted %q", name, in)
				continue
			}
			if !errors.Is(err, ErrMalformed) {
				t.Errorf("%s: error %v is not ErrMalformed", name, err)
			}
			if strings.Contains(err.Error(), "PHI-sentinel") || strings.Contains(err.Error(), "fixture") {
				t.Errorf("%s: error echoes input: %v", name, err)
			}
		}
	}
	// null and the empty array both mean "no groups".
	for _, in := range []string{"null\n", "[]\n"} {
		g, err := DecodeForget(strings.NewReader(in))
		if err != nil || len(g) != 0 {
			t.Errorf("DecodeForget(%q) = %v, %v", in, g, err)
		}
	}
	// A type mismatch inside a well-formed document is malformed, not a partial result.
	if _, err := DecodeStats(strings.NewReader(`{"total_size":"big"}`)); !errors.Is(err, ErrMalformed) {
		t.Errorf("type mismatch: %v", err)
	}
	// Failing reader: the reader's error comes back.
	if _, err := DecodeVersion(&failingReader{}); err == nil || errors.Is(err, ErrMalformed) {
		t.Errorf("reader failure: %v", err)
	}
}

type failingReader struct{ sent bool }

func (f *failingReader) Read(p []byte) (int, error) {
	if !f.sent {
		f.sent = true
		return copy(p, `{"version":`), nil
	}
	return 0, errors.New("disk on fire")
}

func TestParsePruneTextFixtures(t *testing.T) {
	// The numbers differ from capture to capture, so the expectation is read
	// out of the fixture text by an independent, deliberately dumb extraction.
	line := regexp.MustCompile(`(?m)^(to repack|this removes|to delete|total prune|remaining):\s+(\d+) blobs / ([0-9.]+) (B|KiB|MiB|GiB)\s*$`)
	unit := map[string]float64{"B": 1, "KiB": 1024, "MiB": 1 << 20, "GiB": 1 << 30}
	for _, file := range []string{"prune_dry_run.stdout.txt", "prune.stdout.txt"} {
		text := fixtureBytes(t, file)
		var want PruneStats
		matches := line.FindAllStringSubmatch(string(text), -1)
		if len(matches) != 5 {
			t.Fatalf("%s: %d summary lines, want 5", file, len(matches))
		}
		for _, m := range matches {
			blobs, _ := strconv.ParseUint(m[2], 10, 64)
			f, _ := strconv.ParseFloat(m[3], 64)
			bytes := uint64(f*unit[m[4]] + 0.5)
			switch m[1] {
			case "to repack":
				want.RepackBlobs, want.RepackBytes = blobs, bytes
			case "this removes":
				want.RemoveBlobs, want.RemoveBytes = blobs, bytes
			case "to delete":
				want.DeleteBlobs, want.DeleteBytes = blobs, bytes
			case "total prune":
				want.TotalPruneBlobs, want.TotalPruneBytes = blobs, bytes
			case "remaining":
				want.RemainingBlobs, want.RemainingBytes = blobs, bytes
			}
		}
		got := ParsePruneText(bytes.NewReader(text))
		if got != want {
			t.Errorf("%s: got %+v, want %+v", file, got, want)
		}
		if got.DeleteBlobs == 0 || got.RemainingBlobs == 0 || got.RemainingBytes < 2<<20 || got.UnusedPercentAfter != 0 {
			t.Errorf("%s: implausible numbers %+v", file, got)
		}
	}
}

func TestParsePruneTextTable(t *testing.T) {
	tests := []struct {
		name string
		text string
		want PruneStats
	}{
		{"empty", "", PruneStats{}},
		{"garbage", "hello\nworld: 1\n\x00\xff\n", PruneStats{}},
		{
			"all lines",
			"to repack:            12 blobs / 1.500 MiB\nthis removes:          3 blobs / 2 KiB\nto delete:            40 blobs / 1.000 GiB\ntotal prune:          43 blobs / 1.002 GiB\nremaining:           999 blobs / 2.5 TiB\nunused size after prune: 12.5 MiB (3.50% of remaining size)\n",
			PruneStats{
				RepackBlobs: 12, RepackBytes: 1572864, RemoveBlobs: 3, RemoveBytes: 2048,
				DeleteBlobs: 40, DeleteBytes: 1 << 30, TotalPruneBlobs: 43, TotalPruneBytes: 1075889308,
				RemainingBlobs: 999, RemainingBytes: 2748779069440, UnusedBytesAfter: 13107200, UnusedPercentAfter: 3.5,
			},
		},
		{"only one line", "to delete: 5 blobs / 10 B\n", PruneStats{DeleteBlobs: 5, DeleteBytes: 10}},
		{"indented and upper case", "   TO DELETE:   5 blobs / 10 B\r\n", PruneStats{DeleteBlobs: 5, DeleteBytes: 10}},
		{"progress with carriage returns", "[0:00] 10%\rto delete: 7 blobs / 1 KiB\n", PruneStats{DeleteBlobs: 7, DeleteBytes: 1024}},
		{"unknown unit", "to delete: 5 blobs / 10 parsecs\n", PruneStats{DeleteBlobs: 5}},
		{"missing slash", "to delete: 5 blobs\n", PruneStats{}},
		{"bad number", "to delete: many blobs / 10 B\nremaining: 4 blobs / 1 B\n", PruneStats{RemainingBlobs: 4, RemainingBytes: 1}},
		{"thousands separator", "remaining: 1,234 blobs / 1 KiB\n", PruneStats{RemainingBlobs: 1234, RemainingBytes: 1024}},
		{"unused without percent", "unused size after prune: 2 KiB\n", PruneStats{UnusedBytesAfter: 2048}},
		{"negative size", "to delete: 5 blobs / -3 B\n", PruneStats{DeleteBlobs: 5}},
		{"later line wins", "to delete: 1 blobs / 1 B\nto delete: 2 blobs / 2 B\n", PruneStats{DeleteBlobs: 2, DeleteBytes: 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParsePruneText(strings.NewReader(tt.text)); got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
	// A very long line must not make the parser fail or hang.
	long := strings.Repeat("x", 3<<20) + "\nto delete: 1 blobs / 1 B\n"
	if got := ParsePruneText(strings.NewReader(long)); got != (PruneStats{}) && got != (PruneStats{DeleteBlobs: 1, DeleteBytes: 1}) {
		t.Errorf("long line: %+v", got)
	}
}

func TestDecoderUnknownAndUnparsed(t *testing.T) {
	long := strings.Repeat("a", 65)
	input := strings.Join([]string{
		`{"message_type":"brand_new_thing","x":1}`,
		`{"message_type":"Brand-New_2"}`,
		`{"message_type":"C:\\secret place"}`,
		`{"message_type":"` + long + `"}`,
		`{"x":1}`,
		`plain text progress 50%`,
		`Fatal: wrong password or no key found`,
		`{broken`,
		`[1,2,3]`,
		`"a string"`,
		`{"message_type":"status","files_done":"not a number"}`,
		`{"message_type":"summary","files_new":"x"}`,
		``,
		`   `,
		`{"message_type":"exit_error","code":1,"message":"Fatal: x"}`,
	}, "\n") + "\n"
	got := decodeAll(t, []byte(input), "")
	want := []Message{
		Unknown{Type: "brand_new_thing"},
		Unknown{Type: "brand-new_2"},
		Unknown{Type: "?"},
		Unknown{Type: "?"},
		Unknown{Type: ""},
		Unparsed{Class: ClassUnknown},
		Unparsed{Class: ClassWrongPassword},
		Unparsed{Class: ClassUnknown},
		Unparsed{Class: ClassUnknown},
		Unparsed{Class: ClassUnknown},
		Unparsed{Class: ClassUnknown},
		Unparsed{Class: ClassUnknown},
		ExitError{Code: 1, Class: ClassUnknown},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got\n%#v\nwant\n%#v", got, want)
	}
	for _, m := range got {
		if MessageType(m) == "" {
			t.Errorf("%T has an empty message type", m)
		}
	}
}

func TestDecoderLineHandling(t *testing.T) {
	t.Run("CRLF", func(t *testing.T) {
		got := decodeAll(t, []byte("{\"message_type\":\"exit_error\",\"code\":12,\"message\":\"Fatal: wrong password or no key found\"}\r\n{\"x\":1}\r\n"), "")
		want := []Message{ExitError{Code: 12, Class: ClassWrongPassword}, Unknown{}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("complete last line without newline", func(t *testing.T) {
		got := decodeAll(t, []byte("{\"message_type\":\"exit_error\",\"code\":10,\"message\":\"Fatal: repository does not exist\"}"), "")
		if !reflect.DeepEqual(got, []Message{ExitError{Code: 10, Class: ClassRepoMissing}}) {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("truncated last line without newline", func(t *testing.T) {
		in := "{\"message_type\":\"exit_error\",\"code\":3,\"message\":\"x\"}\n{\"message_type\":\"summary\",\"files_new\":1,\"files_chang"
		got := decodeAll(t, []byte(in), KindBackup)
		want := []Message{ExitError{Code: 3, Class: ClassUnknown}, Unparsed{Class: ClassUnknown}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v want %#v", got, want)
		}
	})
	t.Run("truncated error line still classifies", func(t *testing.T) {
		in := `{"message_type":"error","error":{"message":"open x.docx: Access is denied`
		got := decodeAll(t, []byte(in), "")
		if !reflect.DeepEqual(got, []Message{Unparsed{Class: ClassPermission}}) {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("line over 64 KiB still parses", func(t *testing.T) {
		files := strings.Repeat(`"C:\\fixture\\PHI-sentinel\\patient-record.docx",`, 3000)
		line := `{"message_type":"status","percent_done":0.25,"total_files":4,"files_done":1,"current_files":[` + files + `"x"]}`
		if len(line) < 100<<10 {
			t.Fatalf("test line is only %d bytes", len(line))
		}
		got := decodeAll(t, []byte(line+"\n"+`{"message_type":"exit_error","code":1,"message":"y"}`+"\n"), "")
		if len(got) != 2 {
			t.Fatalf("got %d messages", len(got))
		}
		if st, ok := got[0].(BackupStatus); !ok || st.PercentDone != 0.25 || st.TotalFiles != 4 {
			t.Fatalf("first = %#v", got[0])
		}
	})
	t.Run("line over 16 MiB is Unparsed and the stream continues", func(t *testing.T) {
		var b bytes.Buffer
		b.WriteString(`{"message_type":"status","current_files":["`)
		b.WriteString(strings.Repeat("a", maxLine+1024))
		b.WriteString("\"]}\n")
		b.WriteString(`{"message_type":"exit_error","code":11,"message":"unable to create lock"}` + "\n")
		got := decodeAll(t, b.Bytes(), "")
		want := []Message{Unparsed{Class: ClassUnknown}, ExitError{Code: 11, Class: ClassLock}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("over-long last line without newline", func(t *testing.T) {
		got := decodeAll(t, []byte(strings.Repeat("z", maxLine+10)), "")
		if !reflect.DeepEqual(got, []Message{Unparsed{Class: ClassUnknown}}) {
			t.Fatalf("got %#v", got)
		}
	})
	t.Run("EOF is sticky and nothing is returned for empty input", func(t *testing.T) {
		d := NewDecoder(strings.NewReader(""))
		for i := 0; i < 3; i++ {
			if m, err := d.Next(); m != nil || err != io.EOF {
				t.Fatalf("Next = %v, %v", m, err)
			}
		}
	})
	t.Run("reader failure", func(t *testing.T) {
		d := NewDecoder(io.MultiReader(strings.NewReader("{\"message_type\":\"exit_error\",\"code\":1,\"message\":\"x\"}\n{\"mess"), errReader{}))
		if m, err := d.Next(); err != nil || m == nil {
			t.Fatalf("first: %v %v", m, err)
		}
		// The cut-off line is delivered first (as Unparsed), then the failure.
		if m, err := d.Next(); err != nil || !reflect.DeepEqual(m, Unparsed{Class: ClassUnknown}) {
			t.Fatalf("second: %v %v", m, err)
		}
		if m, err := d.Next(); err == nil || errors.Is(err, io.EOF) || m != nil {
			t.Fatalf("third: %v %v", m, err)
		}
	})
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("pipe broke") }

func TestDecoderForKindDispatch(t *testing.T) {
	restoreStatus := `{"message_type":"status","percent_done":0.5,"total_files":3,"files_restored":1,"total_bytes":9,"bytes_restored":4}`
	restoreSummary := `{"message_type":"summary","total_files":3,"files_restored":3,"total_bytes":9,"bytes_restored":9}`
	backupStatus := `{"message_type":"status","percent_done":0.5,"total_files":3,"files_done":1,"total_bytes":9,"bytes_done":4}`
	backupSummary := `{"message_type":"summary","files_new":3,"snapshot_id":"abcdef0123456789"}`
	checkSummary := `{"message_type":"summary","num_errors":2,"broken_packs":["a","b"],"suggest_prune":true}`
	tests := []struct {
		name string
		line string
		kind Kind
		want Message
	}{
		{"restore status unset", restoreStatus, "", RestoreStatus{PercentDone: 0.5, TotalFiles: 3, FilesRestored: 1, TotalBytes: 9, BytesRestored: 4}},
		{"restore status restore", restoreStatus, KindRestore, RestoreStatus{PercentDone: 0.5, TotalFiles: 3, FilesRestored: 1, TotalBytes: 9, BytesRestored: 4}},
		{"restore status as backup", restoreStatus, KindBackup, BackupStatus{PercentDone: 0.5, TotalFiles: 3, TotalBytes: 9}},
		{"restore status as check", restoreStatus, KindCheck, Unknown{Type: "status"}},
		{"restore status as prune (other kinds infer)", restoreStatus, KindPrune, RestoreStatus{PercentDone: 0.5, TotalFiles: 3, FilesRestored: 1, TotalBytes: 9, BytesRestored: 4}},
		{"backup status unset", backupStatus, "", BackupStatus{PercentDone: 0.5, TotalFiles: 3, FilesDone: 1, TotalBytes: 9, BytesDone: 4}},
		{"backup status backup", backupStatus, KindBackup, BackupStatus{PercentDone: 0.5, TotalFiles: 3, FilesDone: 1, TotalBytes: 9, BytesDone: 4}},
		{"restore summary unset", restoreSummary, "", RestoreSummary{TotalFiles: 3, FilesRestored: 3, TotalBytes: 9, BytesRestored: 9}},
		{"restore summary restore", restoreSummary, KindRestore, RestoreSummary{TotalFiles: 3, FilesRestored: 3, TotalBytes: 9, BytesRestored: 9}},
		{"backup summary unset", backupSummary, "", BackupSummary{FilesNew: 3, SnapshotID: "abcdef0123456789"}},
		{"backup summary backup", backupSummary, KindBackup, BackupSummary{FilesNew: 3, SnapshotID: "abcdef0123456789"}},
		{"check summary unset", checkSummary, "", CheckSummary{NumErrors: 2, BrokenPacks: 2, SuggestPrune: true}},
		{"check summary check", checkSummary, KindCheck, CheckSummary{NumErrors: 2, BrokenPacks: 2, SuggestPrune: true}},
		{"check summary as backup", checkSummary, KindBackup, BackupSummary{}},
		{"check summary broken_packs number", `{"message_type":"summary","num_errors":1,"broken_packs":3}`, KindCheck, CheckSummary{NumErrors: 1, BrokenPacks: 3}},
		{"check summary broken_packs null", `{"message_type":"summary","num_errors":0,"broken_packs":null}`, KindCheck, CheckSummary{}},
		{"check summary broken_packs junk", `{"message_type":"summary","num_errors":0,"broken_packs":"x"}`, KindCheck, CheckSummary{}},
		{"check summary repair flags", `{"message_type":"summary","num_errors":0,"suggest_repair_index":true}`, KindCheck, CheckSummary{SuggestRepairIndex: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decodeAll(t, []byte(tt.line+"\n"), tt.kind)
			if len(got) != 1 || !reflect.DeepEqual(got[0], tt.want) {
				t.Fatalf("got %#v, want %#v", got, tt.want)
			}
		})
	}
	d := NewDecoder(strings.NewReader(""))
	if d.ForKind(KindRestore) != d {
		t.Error("ForKind must return its receiver")
	}
}

func TestDecoderErrorShapes(t *testing.T) {
	tests := []struct {
		name string
		line string
		kind Kind
		want ErrorEvent
	}{
		{
			"backup style", `{"message_type":"error","error":{"message":"open C:\\x\\a.docx: Access is denied."},"during":"archival","item":"C:\\x\\a.docx"}`,
			KindBackup, ErrorEvent{During: "archival", Class: ClassPermission, Ext: "docx"},
		},
		{
			"item with disallowed extension", `{"message_type":"error","error":{"message":"x"},"during":"scan","item":"C:\\x\\a.exe"}`,
			"", ErrorEvent{During: "scan", Class: ClassUnknown},
		},
		{
			"posix item", `{"message_type":"error","error":{"message":"permission denied"},"during":"archival","item":"/home/u/Report.PDF"}`,
			"", ErrorEvent{During: "archival", Class: ClassPermission, Ext: "pdf"},
		},
		{
			"check style top-level message", `{"message_type":"error","message":"pack x damaged: unexpected EOF"}`,
			KindCheck, ErrorEvent{During: "check", Class: ClassNetwork},
		},
		{
			"check style without kind", `{"message_type":"error","message":"Access is denied"}`,
			"", ErrorEvent{Class: ClassPermission},
		},
		{
			"error object wins over top-level message", `{"message_type":"error","message":"Access is denied","error":{"message":"connection refused"}}`,
			"", ErrorEvent{Class: ClassNetwork},
		},
		{"no text at all", `{"message_type":"error"}`, "", ErrorEvent{Class: ClassUnknown}},
		{
			"during that is not a word", `{"message_type":"error","error":{"message":"x"},"during":"C:\\Secret Folder"}`,
			"", ErrorEvent{Class: ClassUnknown},
		},
		{
			"during upper-cased", `{"message_type":"error","error":{"message":"x"},"during":"Archival"}`,
			"", ErrorEvent{During: "archival", Class: ClassUnknown},
		},
		{
			"restore error", `{"message_type":"error","error":{"message":"failed to restore timestamp: Access is denied."},"during":"restore","item":"\\C\\x"}`,
			KindRestore, ErrorEvent{During: "restore", Class: ClassPermission},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decodeAll(t, []byte(tt.line+"\n"), tt.kind)
			if len(got) != 1 || !reflect.DeepEqual(got[0], tt.want) {
				t.Fatalf("got %#v, want %#v", got, tt.want)
			}
		})
	}
	exit := decodeAll(t, []byte(`{"message_type":"exit_error","message":"Fatal: wrong password or no key found"}`+"\n"), "")
	if !reflect.DeepEqual(exit, []Message{ExitError{Code: 0, Class: ClassWrongPassword}}) {
		t.Fatalf("exit_error without code: %#v", exit)
	}
}

func TestMessageTypeNames(t *testing.T) {
	tests := map[string]Message{
		"status": BackupStatus{}, "summary": BackupSummary{}, "verbose_status": VerboseStatus{},
		"error": ErrorEvent{}, "exit_error": ExitError{}, "snapshot": LsSnapshot{}, "node": Node{},
		"unknown": Unknown{}, "unparsed": Unparsed{},
	}
	for want, m := range tests {
		if got := MessageType(m); got != want {
			t.Errorf("MessageType(%T) = %q, want %q", m, got, want)
		}
	}
	if MessageType(RestoreStatus{}) != "status" || MessageType(RestoreSummary{}) != "summary" || MessageType(CheckSummary{}) != "summary" {
		t.Error("shared message types")
	}
}

func TestExtOfFixtureItems(t *testing.T) {
	// The sentinel test relies on this: the extension survives, the name does not.
	if got := Ext(`C:\fixture\PHI-sentinel\patient-record.docx`); got != "docx" {
		t.Fatalf("Ext = %q", got)
	}
}

func TestFixturesComplete(t *testing.T) {
	// Every fixture the ticket lists exists, is not empty (apart from the one
	// stream that is empty by nature), and MANIFEST.md mentions it.
	required := strings.Fields(`backup_ok.stdout.jsonl backup_ok.stderr.jsonl backup_partial.stdout.jsonl backup_partial.stderr.jsonl
		backup_dry_run.stdout.jsonl restore.stdout.jsonl restore_dry_run.stdout.jsonl snapshots.json ls.jsonl find.json
		stats_raw_data.json stats_restore_size.json forget_dry_run.json forget.json check.stdout.jsonl prune.stdout.txt
		cat_config.json version.json init.json exit10_repo_missing.stderr.jsonl exit11_lock.stderr.jsonl exit12_wrong_password.stderr.jsonl`)
	manifest := string(fixtureBytes(t, "MANIFEST.md"))
	for _, name := range required {
		b := fixtureBytes(t, name)
		if len(b) == 0 && name != "backup_ok.stderr.jsonl" {
			t.Errorf("%s is empty", name)
		}
		if !strings.Contains(manifest, "`"+name+"`") {
			t.Errorf("MANIFEST.md does not list %s", name)
		}
	}
	if !strings.Contains(manifest, "0.19.1") {
		t.Error("MANIFEST.md does not name the restic version")
	}
}

func TestDecodePlainTextStderr(t *testing.T) {
	// Commands run without --json (cat config, prune) report errors as plain
	// text; every line is Unparsed and only its class survives.
	plain := fixtureMessages(t, "exit12_wrong_password_plain.stderr.txt", "")
	if !reflect.DeepEqual(plain, []Message{Unparsed{Class: ClassWrongPassword}}) {
		t.Fatalf("exit 12 plain = %#v", plain)
	}
	lock := fixtureMessages(t, "exit11_prune_lock.stderr.txt", "")
	if len(lock) != 4 {
		t.Fatalf("exit 11 plain has %d lines: %#v", len(lock), lock)
	}
	var locks int
	for i, m := range lock {
		u, ok := m.(Unparsed)
		if !ok {
			t.Fatalf("line %d is %T", i, m)
		}
		if u.Class == ClassLock {
			locks++
		}
	}
	if locks != 1 || lock[0].(Unparsed).Class != ClassLock {
		t.Errorf("classes = %#v, want the first line (and only it) to be lock", lock)
	}
	// and the whole chain for a prune that hit the lock
	if r := Map(KindPrune, 11, Evidence{Stderr: ClassLock}); r.Outcome != OutcomeDeferred || r.DeferredReason != DeferredLock {
		t.Errorf("Map = %+v", r)
	}
}

func TestVerboseActionIsReducedToResticVocabulary(t *testing.T) {
	tests := []struct{ in, want string }{
		{"new", "new"}, {"unchanged", "unchanged"}, {"modified", "modified"}, {"scan_finished", "scan_finished"},
		{"restored", "restored"}, {"updated", "updated"}, {"deleted", "deleted"},
		{"", ""},
		{"New", "?"}, {"newer", "?"}, {" new", "?"}, {"new\n", "?"},
		{`C:\fixture\PHI-sentinel\patient-record.docx`, "?"}, {"/C/fixture/x.docx", "?"}, {"patient-record", "?"},
	}
	for _, tt := range tests {
		line := fmt.Sprintf(`{"message_type":"verbose_status","action":%q,"item":"x"}`, tt.in)
		got := decodeAll(t, []byte(line+"\n"), KindBackup)
		if len(got) != 1 || !reflect.DeepEqual(got[0], VerboseStatus{Action: tt.want}) {
			t.Errorf("action %q: got %#v, want Action %q", tt.in, got, tt.want)
		}
	}
	// no action field at all stays empty
	if got := decodeAll(t, []byte(`{"message_type":"verbose_status","item":"x"}`+"\n"), ""); !reflect.DeepEqual(got, []Message{VerboseStatus{}}) {
		t.Errorf("no action: %#v", got)
	}
}

func TestBackupSummarySnapshotIDIsHexOrEmpty(t *testing.T) {
	full := strings.Repeat("0123456789abcdef", 4)
	tests := []struct{ name, in, want string }{
		{"64 hex", full, full},
		{"8 hex", "0123abcd", "0123abcd"},
		{"7 hex", "0123abc", ""},
		{"65 hex", full + "0", ""},
		{"upper case", "0123ABCD", ""},
		{"non hex", "0123abcg", ""},
		{"empty", "", ""},
		{"path", `C:\fixture\PHI-sentinel\patient-record.docx`, ""},
		{"hex then path", "0123abcd/patient", ""},
		{"trailing newline", "0123abcd\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line := fmt.Sprintf(`{"message_type":"summary","files_new":1,"snapshot_id":%q}`, tt.in)
			got := decodeAll(t, []byte(line+"\n"), KindBackup)
			if len(got) != 1 || !reflect.DeepEqual(got[0], BackupSummary{FilesNew: 1, SnapshotID: tt.want}) {
				t.Errorf("got %#v, want SnapshotID %q", got, tt.want)
			}
		})
	}
}
