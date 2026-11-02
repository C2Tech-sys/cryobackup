package logx

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// pathShapes returns path texts that embed needle in directory and file position.
func pathShapes(needle string) []string {
	return []string{
		`C:\Users\` + needle + `\Documents\` + needle + `.docx`,
		`d:/` + needle + `/notes.txt`,
		`\\fileserver\` + needle + `\share\` + needle,
		`//fileserver/` + needle + `/share`,
		`/home/` + needle + `/.ssh/id_rsa`,
		`/var/lib/` + needle,
		`%USERPROFILE%\` + needle,
		`~/` + needle + `/x`,
		`../` + needle + `/x`,
		`file:///C:/` + needle + `/x`,
		`open ` + needle + `: failed at C:\` + needle + `\a.txt (code 5)`,
		// Bare file names and quoted operands (M1-FIX4 item 4).
		needle + `.docx`,
		needle + `-bare-name.docx: The system cannot find the file specified.`,
		`ignoring error for ` + needle + ` notes.PDF: The process cannot access the file`,
		`backup of ` + needle + `.tar.gz failed`,
		`file "` + needle + `" blob 0 not found in index`,
		`directory "` + needle + ` dir" changed type, refusing to archive`,
		`strconv.Atoi: parsing "` + needle + `": invalid syntax`,
		`Get "https://files.example/share/` + needle + `.xlsx": EOF`,
		// URLs (foreman, M1-FIX4 item 4): userinfo, query, fragment, a client
		// path, restic's rest: and rclone: repository strings.
		`Get "https://` + needle + `:secret@files.example/x": EOF`,
		`Get "https://files.example/x?token=` + needle + `": EOF`,
		`Get "https://files.example/x#` + needle + `": EOF`,
		`Get "https://files.example/Clinic/` + needle + `": EOF`,
		`https://files.example/` + needle,
		`Fatal: unable to open repository at rest:https://gateway.example:9443/` + needle + `/: connection refused`,
		`rest:https://` + needle + `:***@gateway.example/dev/`,
		`rclone:store:bucket/` + needle,
		`Get "https://console.example/api/v1/restores/x/download?token=` + needle + `": EOF`,
		// Fail-closed round (foreman review): any extension, a space before the
		// dot, a trailing _something, every quote kind and an unterminated one,
		// authority tricks, percent-encoding, the colon backends.
		needle + `.mdf`, needle + `.kdbx`, needle + `.837`, needle + `.ai`, needle + `.one`, needle + `.html`,
		`open ` + needle + ` notes .md failed`,
		needle + `.docx_old`,
		`invalid character '` + needle + `' looking for beginning of value`,
		`open “` + needle + ` notes” failed`,
		`open ‘` + needle + ` notes’ failed`,
		`unterminated "` + needle + ` and the rest`,
		`an escaped \"` + needle + ` quote`,
		`https://a\"@evil:` + needle + `@host/x`,
		`https://h%40` + needle + `.example/x`,
		`https://files.example/%2F` + needle,
		`https://host:` + needle + `/`,
		`url=https://files.example/` + needle,
		`sftp:` + needle + `@host:repo`,
		`b2:bucket:` + needle,
		`s3:https://s3.example/` + needle + `/prefix`,
		`ftp://files.example/` + needle,
		// Final design (opaque free text): the re-verifier's leaks.
		needle + `, John's chart.pdf: Access is denied.`,
		`Get "https://` + needle + `\"user:pw@host/x": EOF`,
		needle + `.docx://host/x`,
		`repo=rclone:store:bucket/` + needle + ` failed`,
		`key:s3:bucket/` + needle,
		needle + `-Doe-1961.com opened`,
		`123456` + needle + `.7z copied`,
		needle + `．docx copied`,
		needle + "​.docx copied",
		needle + "․docx copied",
	}
}

func capture(f func(l *Logger)) string {
	var b bytes.Buffer
	f(New(&b, LevelDebug))
	return b.String()
}

func TestPathsAreRedactedEverywhere(t *testing.T) {
	const needle = "PatientSmithChart"
	for _, p := range pathShapes(needle) {
		p := p
		t.Run(p, func(t *testing.T) {
			out := capture(func(l *Logger) {
				l.Info("message "+p, ID("id", p), Class(p), Kind(p), Str("s", p), Hash("h", p), Err(errors.New(p)),
					slog.String("raw", p), slog.Any("any", p), slog.Any("anyerr", fmt.Errorf("wrap: %w", errors.New(p))),
					slog.Group("g", slog.String("inner", p)), slog.Any("stringer", stringer(p)))
				l.With(slog.String("with", p)).WithGroup("grp").Warn("x", slog.String("k", p))
			})
			if strings.Contains(out, needle) {
				t.Fatalf("path text survived:\n%s", out)
			}
			if !strings.Contains(out, "<path:") && !strings.Contains(out, "invalid") {
				t.Fatalf("expected a redaction marker:\n%s", out)
			}
		})
	}
}

type stringer string

func (s stringer) String() string { return string(s) }

func TestPathErrorsPrintOperationAndClassOnly(t *testing.T) {
	const needle = "SecretFolderName"
	cases := map[string]error{
		"pathError": &fs.PathError{Op: "open", Path: `C:\` + needle + `\f.txt`, Err: fs.ErrNotExist},
		"wrapped":   fmt.Errorf("reading config: %w", &fs.PathError{Op: "stat", Path: "/etc/" + needle, Err: fs.ErrPermission}),
		"link":      &os.LinkError{Op: "rename", Old: "/a/" + needle, New: "/b/" + needle, Err: fs.ErrExist},
		"exec":      &exec.Error{Name: "/opt/" + needle + "/restic", Err: exec.ErrNotFound},
		"joined":    errors.Join(errors.New("first"), &fs.PathError{Op: "read", Path: needle + `\x`, Err: errors.New("boom")}),
	}
	for name, err := range cases {
		out := capture(func(l *Logger) { l.Error("failed", Err(err), slog.Any("raw", err)) })
		if strings.Contains(out, needle) {
			t.Errorf("%s: path leaked: %s", name, out)
		}
		if !strings.Contains(out, "op:") || !strings.Contains(out, "class:") {
			t.Errorf("%s: expected op and class: %s", name, out)
		}
	}
}

func TestBenignValuesPassThrough(t *testing.T) {
	out := capture(func(l *Logger) {
		l.Info("request", ID("request_id", "01J9ZZ0000000000000000ABCD"), Class("not_found"), Kind("backup"),
			Count("n", 3), Int("status", 200), Dur("took", 1500*time.Millisecond), Bool("ok", true),
			Hash("sha", "deadbeef"), Str("route", "GET"), slog.String("note", "12.5GB read, 38% done (timeout)"))
	})
	for _, want := range []string{"01J9ZZ0000000000000000ABCD", "not_found", `"took":1500`, "deadbeef", "12.5GB read, 38% done (timeout)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
	if strings.Contains(out, "<path:") {
		t.Errorf("benign values were redacted: %s", out)
	}
}

func TestTypedStringsRejectFreeText(t *testing.T) {
	out := capture(func(l *Logger) {
		l.Info("x", Str("a", "has space"), ID("b", strings.Repeat("a", 65)), Hash("c", "xyz"))
	})
	if strings.Count(out, `"invalid"`) != 3 {
		t.Errorf("expected three invalid tokens: %s", out)
	}
}

func TestMarkerIsStableWithinProcess(t *testing.T) {
	p := `C:\Users\Someone\a.txt`
	if proc.String(p) != proc.String(p) || proc.String(p) == proc.String(p+"x") {
		t.Fatal("marker must be deterministic per value and differ across values")
	}
}

func TestInstallRoutesSlogDefault(t *testing.T) {
	var b bytes.Buffer
	prevSlog := slog.Default()
	prevDef := def.Load()
	t.Cleanup(func() {
		slog.SetDefault(prevSlog)
		def.Store(prevDef)
		log.SetFlags(log.LstdFlags)
		log.SetOutput(os.Stderr)
	})
	InstallTo(&b, LevelInfo)
	slog.Info("via slog default", "file", `C:\Users\Alice\secret.txt`)
	Default().Info("via logx default", Err(&fs.PathError{Op: "open", Path: "/srv/alice/x", Err: fs.ErrNotExist}))
	log.Print(`stdlib log C:\Users\Alice\secret.txt`)
	out := b.String()
	if strings.Contains(out, "Alice") || strings.Contains(out, "alice") {
		t.Fatalf("leak through the default logger: %s", out)
	}
	if strings.Count(out, "\n") != 3 {
		t.Fatalf("expected three JSON lines through the installed handler, got:\n%s", out)
	}
}
