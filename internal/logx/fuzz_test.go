package logx

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
)

// needleFrom makes a letters-and-digits token from arbitrary fuzz input, long
// enough to be unmistakable in the output.
func needleFrom(s string) string {
	var b strings.Builder
	b.WriteString("zq")
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
		if b.Len() > 30 {
			break
		}
	}
	b.WriteString("xjv")
	return b.String()
}

// FuzzNoPathSurvives feeds Windows, UNC and POSIX paths built around the fuzzed
// token through every attribute type and fails if the token appears in the
// output (architecture §8.4 item 2).
func FuzzNoPathSurvives(f *testing.F) {
	for _, s := range []string{"", "a", "Documents", "Patient Smith", "../../x", `C:\x`, "é", "a b c",
		// M1-FIX4: bare names and quoted operands (pathShapes puts the needle
		// into name.ext and "quoted" positions as well).
		"ZQ7-bare-name", "Smith, John (DOB 1961)", `"quoted"`, "report.final", "x.docx", "résumé",
		// URL shapes (pathShapes puts the needle into each): userinfo, a query
		// token, a fragment, a client path, an API path, rest: and rclone:.
		"https://user:pass@files.example/x", "?token=abc", "#fragment", "https://files.example/Clinic/Jane",
		"/api/v1/agent/heartbeat", "rest:https://gateway.example:9443/dev/", "rclone:store:bucket/prefix",
		// Fail-closed round: the extensions a denylist missed, quote kinds,
		// authority tricks, percent-encoding, the colon backends.
		"x.mdf", "x.ldf", "x.edb", "x.kdbx", "x.nsf", "x.dbf", "x.one", "x.md", "x.tmp", "x.htm", "x.html", "x.nii",
		"x.ccda", "x.cda", "x.edi", "x.x12", "x.837", "x.tib", "x.vbk", "x.gpg", "x.rdp", "x.ai", "x.indd", "x.mpp",
		"x.jfif", "x.avif", "x.3gp", "notes .md", "x.docx_old", "'single'", "“curly”", "‘curly’", `"unterminated`,
		`https://a\"@evil:pw@host/x`, "https://h%40evil.example/", "https://[::1]:8443", "sftp:u@host:repo",
		"b2:bucket:x", "s3:https://s3.example/b", "ftp://h/x",
		// Final design: the re-verifier's leaks.
		"Smith, John's chart.pdf", `https://a\"user:pw@host/x`, "Smith.docx://host", "repo=rclone:store:x", "key:s3:bucket/x",
		"Jane-Doe-1961.com", "123456.7z", "notes．docx", "notes​.docx", "notes․docx",
		"copied Smith notes.pdf", "Jane.Doe chart.docx", "Zoë Müller", "repository: rclone:store:x", "ZQ7#1 ZQ7?x ZQ7&y ZQ7=z ZQ7@h"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		needle := needleFrom(s)
		for _, p := range pathShapes(needle) {
			var b bytes.Buffer
			l := New(&b, LevelDebug)
			l.Info("m "+p, ID("id", p), Class(p), Kind(p), Str("s", p), Hash("h", p), Err(errors.New(p)),
				Err(&fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}),
				slog.String("raw", p), slog.Any("any", p), slog.Any("e", fmt.Errorf("x: %w", errors.New(p))),
				slog.Group("g", slog.String("k", p), slog.Any("e", &fs.PathError{Op: "stat", Path: p, Err: fs.ErrPermission})))
			if strings.Contains(b.String(), needle) {
				t.Fatalf("path text survived for shape %q:\n%s", p, b.String())
			}
		}
	})
}
