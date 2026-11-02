package logx

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// M1-FIX4 item 4, final design: free text is opaque by default (redact.go).
// Every string below once leaked through a pattern list; each must now be
// replaced as a whole, through every way a value reaches the handler.
var leakStrings = []string{
	// the re-verifier's leaks
	"Smith, John's chart.pdf: Access is denied.",
	`Get "https://ZQ7\"user:pw@host/x": EOF`,
	"Smith.docx://host/x",
	"https://Smith.docx/x",
	"repo=rclone:store:bucket/ZQ7 failed",
	"repository: rclone:store:ZQ7",
	"key:s3:bucket/ZQ7",
	"Jane-Doe-1961.com opened",
	"123456.7z copied",
	"notes．docx copied",       // fullwidth full stop
	"notes\u200b.docx copied", // zero-width space before the dot
	"notes\u2024docx copied",  // one-dot leader
	// earlier rounds
	"ZQ7-bare-name.docx: The system cannot find the file specified.",
	"ZQ7-Smith, John (DOB 1961).mdf: Access is denied.",
	"open ZQ7 notes .md failed",
	"ZQ7-report.docx_old copied",
	`file "ZQ7 Jane" blob 0 not found in index`,
	`invalid character 'ZQ7' looking for beginning of value`,
	"open “ZQ7 notes” failed",
	"open ‘ZQ7 notes’ failed",
	`unterminated "ZQ7 and the rest`,
	`https://a\"@evil:ZQ7@host/x`,
	"https://h%40ZQ7.example/x",
	"https://[::1]:8443/ZQ7",
	"Fatal: unable to open repository at rest:https://gateway.example:9443/ZQ7/: connection refused",
	"sftp:ZQ7@host:repo",
	"b2:bucket:ZQ7",
	`C:\Users\ZQ7\Documents\x.docx`,
	`\\fileserver\clinic$\ZQ7`,
	"/home/ZQ7/.ssh/id_rsa",
	"%USERPROFILE%\\ZQ7",
	"ZQ7#1 ZQ7?x ZQ7&y ZQ7=z ZQ7@h ZQ7/x",
	"Zoë Müller", // non-ASCII letters
	"copied Smith notes.pdf",
	"Jane.Doe chart.docx",
}

func TestFreeTextLeaksAreRedactedWhole(t *testing.T) {
	for _, s := range leakStrings {
		got := proc.String(s)
		if !markerOnly(got) {
			t.Errorf("String(%q) = %q, want one marker for the whole value", s, got)
		}
		out := capture(func(l *Logger) {
			l.Info(s, slog.String("raw", s), Err(errors.New(s)), Str("s", s), slog.Any("any", s),
				slog.Group("g", slog.String("k", s)))
		})
		for _, frag := range []string{"Smith", "ZQ7", "Jane", "123456", "notes", "Zo", "docx"} {
			if strings.Contains(s, frag) && strings.Contains(out, frag) {
				t.Errorf("%q: %q survived:\n%s", s, frag, out)
			}
		}
	}
}

func markerOnly(s string) bool {
	return len(s) == len("<path:00000000>") && strings.HasPrefix(s, "<path:") && strings.HasSuffix(s, ">")
}

// The safe set survives as it is.
func TestSafeFreeTextSurvives(t *testing.T) {
	for _, s := range []string{
		"exit status 3",
		"context deadline exceeded",
		"error 0x80070005 from the service",
		"restic 0.19.1 built with go1.27 agent v1.2.3",
		"retrying after 1.2s - 38.5% done - 12GiB read",
		"connection refused (timeout)",
		"heartbeat: store failed",
		"op - step -> next | x = y :",
		"[done] 3, 4; 5:",
		"",
	} {
		if got := proc.String(s); got != s {
			t.Errorf("String(%q) = %q, want it unchanged", s, got)
		}
	}
	// Accepted over-redaction: a dotted address, i/o, a host in free text.
	for _, s := range []string{"dial tcp: i/o timeout", "dial tcp 10.0.0.9:443: connect: connection refused", "lookup console.example.com: no such host", "restic 0.19.1.2.3"} {
		if got := proc.String(s); !markerOnly(got) {
			t.Errorf("String(%q) = %q, want the whole value marked (accepted over-redaction)", s, got)
		}
	}
}

// Diagnosability comes from typed fields: they are written as built.
func TestTypedFieldsCarryWhatFreeTextCannot(t *testing.T) {
	out := capture(func(l *Logger) {
		l.Warn("storage unreachable", Host("storage_host", "s3.eu-central-2.example.com"), Class("tls"), Code("exit_code", 1),
			Host("ip", "10.0.0.9"), Host("ip6", "[2001:db8::7]"), ID("request_id", "01J9ZZ0000000000000000ABCD"),
			Str("v", "0.19.1"), Class("not_found"), Err(errors.New("context deadline exceeded")))
	})
	for _, want := range []string{`"msg":"storage unreachable"`, `"storage_host":"s3.eu-central-2.example.com"`, `"class":"tls"`, `"exit_code":1`,
		`"ip":"10.0.0.9"`, `"ip6":"2001:db8::7"`, `"request_id":"01J9ZZ0000000000000000ABCD"`, `"v":"0.19.1"`,
		`"class":"not_found"`, `"err":"context deadline exceeded"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
	for host, want := range map[string]string{
		"console.example.com": "console.example.com", "DC01.corp.local": "dc01.corp.local", "::1": "::1",
		"Smith, John.docx": "invalid", "host:443": "invalid", "user@host": "invalid", "a..b": "invalid", "-a.b": "invalid",
		"notes．docx": "invalid", "": "invalid", strings.Repeat("a", 254): "invalid", "h/x": "invalid",
	} {
		out := capture(func(l *Logger) { l.Info("x", Host("h", host)) })
		if !strings.Contains(out, `"h":"`+want+`"`) {
			t.Errorf("Host(%q): want %q in %s", host, want, out)
		}
	}
	// Typed tokens refuse file names and look-alikes too.
	out = capture(func(l *Logger) {
		l.Info("x", Str("a", "ZQ7.docx"), Str("b", "123456.7z"), ID("c", "x.y"), Str("d", "_path..._"))
	})
	for _, want := range []string{`"a":"invalid"`, `"b":"invalid"`, `"c":"invalid"`, `"d":"_path..._"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
}
