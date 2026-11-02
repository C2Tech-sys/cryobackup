package maintain

import (
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// M1-FIX4: free text is opaque in the log, so a run that cannot reach the
// storage names the configured storage host and the class in typed fields. An
// expired certificate or a DNS outage stays diagnosable (CLAUDE.md: the
// 18-day silent certificate outage of another product).
func TestStorageOutageLogsTheHostAndTheClass(t *testing.T) {
	for name, tc := range map[string]struct {
		stderr string
		class  restic.ErrorClass
	}{
		"certificate": {"inline:x509: certificate has expired or is not yet valid\n", restic.ClassTLS},
		"dns":         {"inline:dial tcp: lookup s3.eu-central-2.example.com: no such host\n", restic.ClassNetwork},
	} {
		t.Run(name, func(t *testing.T) {
			e := newTenv(t, []fakeRule{{Cmd: "cat", Exit: 1, Stderr: tc.stderr}}, func(c *Config) {
				c.StorageHost = "s3.eu-central-2.example.com"
			})
			_, err := e.w.CatConfig(e.ctx, dev(e.esc))
			wantCode(t, err, adminpath.CodeStorageUnreachable, tc.class)
			logs := e.logs.String()
			var line string
			for _, l := range strings.Split(logs, "\n") {
				if strings.Contains(l, `"msg":"restic could not reach the storage"`) {
					line = l
				}
			}
			if line == "" {
				t.Fatalf("no storage outage line in:\n%s", logs)
			}
			for _, want := range []string{`"storage_host":"s3.eu-central-2.example.com"`, `"class":"` + string(tc.class) + `"`, `"level":"WARN"`} {
				if !strings.Contains(line, want) {
					t.Errorf("missing %s in %s", want, line)
				}
			}
			if strings.Contains(logs, "x509") || strings.Contains(logs, "lookup") {
				t.Errorf("restic's own text reached the log:\n%s", logs)
			}
		})
	}
	t.Run("unconfigured", func(t *testing.T) {
		e := newTenv(t, []fakeRule{{Cmd: "cat", Exit: 1, Stderr: "inline:x509: certificate signed by unknown authority\n"}}, nil)
		_, _ = e.w.CatConfig(e.ctx, dev(e.esc))
		if !strings.Contains(e.logs.String(), `"storage_host":"unconfigured"`) {
			t.Errorf("no unconfigured storage host:\n%s", e.logs.String())
		}
	})
	t.Run("not a storage failure", func(t *testing.T) {
		e := newTenv(t, []fakeRule{{Cmd: "cat", Exit: 12, Stderr: "exit12_wrong_password_plain.stderr.txt"}}, nil)
		_, _ = e.w.CatConfig(e.ctx, dev(e.esc))
		if strings.Contains(e.logs.String(), "could not reach the storage") {
			t.Errorf("a wrong password was logged as a storage outage:\n%s", e.logs.String())
		}
	})
}

func TestStorageHostOf(t *testing.T) {
	for in, want := range map[string]string{
		"https://s3.example.com":      "s3.example.com",
		"https://s3.example.com:9000": "s3.example.com",
		"s3.example.invalid":          "s3.example.invalid",
		"minio:9000":                  "minio",
		"http://[2001:db8::7]:9000":   "2001:db8::7",
		"":                            "",
	} {
		if got := StorageHostOf(in); got != want {
			t.Errorf("StorageHostOf(%q) = %q, want %q", in, got, want)
		}
	}
}
