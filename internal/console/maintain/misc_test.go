package maintain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
	"github.com/c2tech-sys/cryobackup/internal/restic"
)

// The worker's attributes are logx's typed constructors: every key is the one the
// worker documents, a value that is not a short token becomes "invalid", and
// logx's handler rewrites anything path-shaped that still reaches it.
func TestWorkerAttributesAreTypedAndPathFree(t *testing.T) {
	var buf bytes.Buffer
	lg := logx.New(&buf, logx.LevelDebug)
	lg.Info("line",
		attrDevice("01ABCDEF"), attrRequest("req-1"), attrKind("unlock"), attrExit(11), attrClass("lock"),
		attrCount(3), attrElapsed(1500*time.Millisecond), attrOutcome("deferred"), attrMethod("ls"))
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"device_id": "01ABCDEF", "request_id": "req-1", "kind": "unlock", "exit_code": float64(11),
		"class": "lock", "count": float64(3), "elapsed_ms": float64(1500), "outcome": "deferred", "method": "ls"} {
		if rec[k] != want {
			t.Errorf("attribute %s = %v, want %v", k, rec[k], want)
		}
	}

	// A path put under any of the string keys cannot leave the process.
	buf.Reset()
	lg.Info("line",
		attrDevice(`C:\fixture\PHI-sentinel\x.docx`), attrRequest("/var/lib/patient/record.docx"), attrKind("../../etc"),
		attrClass("a path /with/segments"), attrOutcome(`\\server\share\PHI-sentinel`), attrMethod("C:/PHI-sentinel"))
	out := buf.String()
	for _, leak := range []string{"PHI-sentinel", "segments", "etc", "patient", "fixture", "server"} {
		if strings.Contains(out, leak) {
			t.Fatalf("%q survived under a typed key: %s", leak, out)
		}
	}
	if !strings.Contains(out, "invalid") {
		t.Fatalf("expected \"invalid\" placeholders: %s", out)
	}
}

func TestRunnerDecodesAndNeverInherits(t *testing.T) {
	e := newTenv(t, []fakeRule{{
		Cmd: "snapshots", Exit: 1, Stdout: "snapshots.json",
		Stderr: "inline:" + `{"message_type":"exit_error","code":1,"message":"Fatal: x509: certificate has expired"}` + "\n",
	}}, nil)
	r := e.w.run
	env := []string{"RCLONE_CONFIG=" + filepath.Join(e.state, "rclone.conf"), "PATH=" + os.Getenv("PATH")}
	var docs int
	res, err := r.run(e.ctx, 30*time.Second, []string{"snapshots", "--json"}, env, func(rd io.Reader) error {
		s, derr := restic.DecodeSnapshots(rd)
		docs = len(s)
		return derr
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Exit != 1 || docs != 5 || res.StdoutErr != nil || res.TimedOut || res.Stderr != restic.ClassTLS || res.ExitError == nil {
		t.Fatalf("result = %+v, snapshots = %d", res, docs)
	}
	if _, err := r.run(e.ctx, time.Second, []string{"snapshots"}, nil, nil); err == nil {
		t.Fatal("an empty environment was accepted (it would inherit the parent's)")
	}
	if _, err := (&runner{program: filepath.Join(e.state, "missing")}).run(e.ctx, time.Second, nil, env, nil); err == nil {
		t.Fatal("a missing program started")
	}
}

func TestRunnerDrainsUnreadOutput(t *testing.T) {
	big := strings.Repeat("x", 4<<20)
	e := newTenv(t, []fakeRule{{Cmd: "snapshots", Stdout: "inline:" + big, Stderr: "inline:" + big}}, nil)
	env := []string{"RCLONE_CONFIG=" + filepath.Join(e.state, "rclone.conf"), "PATH=" + os.Getenv("PATH")}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The consumer returns at once without reading; the child must not block on a full pipe.
		_, _ = e.w.run.run(e.ctx, 20*time.Second, []string{"snapshots"}, env, func(io.Reader) error { return errors.New("stop") })
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("runner deadlocked on unread output")
	}
}

func TestRunnerCancelByCaller(t *testing.T) {
	e := newTenv(t, []fakeRule{{Cmd: "snapshots", SleepMS: 20000}}, nil)
	env := []string{"RCLONE_CONFIG=" + filepath.Join(e.state, "rclone.conf"), "PATH=" + os.Getenv("PATH")}
	ctx, cancel := context.WithCancel(e.ctx)
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	res, err := e.w.run.run(ctx, time.Minute, []string{"snapshots"}, env, nil)
	if err != nil || !res.Canceled || res.TimedOut {
		t.Fatalf("result = %+v, %v", res, err)
	}
}

func TestRunMainUsage(t *testing.T) {
	dir := t.TempDir()
	keys := func(string, *store.Store) (KeyUnwrapper, error) { return &fakeKeys{pw: "x"}, nil }
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("good.json", `{"data_dir":`+jsonStr(filepath.Join(dir, "data"))+`,"db_path":`+jsonStr(filepath.Join(dir, "c.db"))+
		`,"master_key_path":`+jsonStr(filepath.Join(dir, "master.key"))+`,"artifacts_dir":`+jsonStr(filepath.Join(dir, "art"))+
		`,"adminpath_socket":"x.sock","unknown_key":{"a":1}}`)
	for name, tc := range map[string]struct {
		args []string
		keys KeyUnwrapperFactory
		code int
	}{
		"no mode":          {[]string{"--config", good}, keys, ExitUsage},
		"both modes":       {[]string{"--config", good, "--loop", "--once"}, keys, ExitUsage},
		"stray argument":   {[]string{"--config", good, "--once", "extra"}, keys, ExitUsage},
		"unknown flag":     {[]string{"--nope"}, keys, ExitUsage},
		"missing config":   {[]string{"--config", filepath.Join(dir, "absent.json"), "--once"}, keys, ExitUsage},
		"invalid json":     {[]string{"--config", write("bad.json", "{not json"), "--once"}, keys, ExitUsage},
		"no db path":       {[]string{"--config", write("nodb.json", `{"master_key_path":"k","artifacts_dir":"a"}`), "--once"}, keys, ExitUsage},
		"loop needs sock":  {[]string{"--config", write("nosock.json", `{"db_path":"d","master_key_path":"k","artifacts_dir":"a","data_dir":"x"}`), "--loop"}, keys, ExitUsage},
		"no key unwrapper": {[]string{"--config", good, "--once"}, nil, ExitUsage},
	} {
		var out, errb bytes.Buffer
		if got := Run(tc.args, &out, &errb, tc.keys); got != tc.code {
			t.Errorf("%s: exit %d, want %d (stderr %q)", name, got, tc.code, errb.String())
		}
	}
}

func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }

// TestRunMainOnce drives --once end to end with the fake restic and an empty
// database: the pass runs, finds nothing due and exits 0.
func TestRunMainOnce(t *testing.T) {
	dir := t.TempDir()
	exe, sum := fakeResticPath(t)
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	sc, _ := json.Marshal(fakeScenario{Rules: []fakeRule{{Cmd: "version", Stdout: `inline:{"message_type":"version","version":"0.19.1"}`}}})
	if err := os.WriteFile(filepath.Join(state, "fake.json"), sc, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.json")
	body := `{"db_path":` + jsonStr(filepath.Join(dir, "c.db")) + `,"master_key_path":` + jsonStr(filepath.Join(dir, "master.key")) +
		`,"artifacts_dir":` + jsonStr(filepath.Join(dir, "art")) + `}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRYO_MAINTAIN_RESTIC", exe)
	t.Setenv("CRYO_MAINTAIN_RESTIC_SHA256", sum)
	t.Setenv("RCLONE_CONFIG", filepath.Join(state, "rclone.conf")) // the fake restic finds its scenario next to it
	t.Setenv("CRYO_MAINTAIN_SCRATCH_DIR", filepath.Join(dir, "scratch"))
	t.Setenv("CRYO_MAINTAIN_STATE_DIR", filepath.Join(dir, "old-state")) // ADR 0008: ignored, and said so
	t.Setenv("CRYO_MAINTAIN_BUCKET", "bucket")
	t.Setenv("CRYO_ARTIFACTS_DIR", filepath.Join(dir, "art2"))
	keys := func(path string, _ *store.Store) (KeyUnwrapper, error) {
		if path != filepath.Join(dir, "master.key") {
			t.Errorf("master key path = %q", path)
		}
		return &fakeKeys{pw: "x"}, nil
	}
	var out, errb bytes.Buffer
	if code := Run([]string{"--config", cfg, "--once"}, &out, &errb, keys); code != ExitOK {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "considered=0") {
		t.Fatalf("stdout = %q", out.String())
	}
	// ADR 0008: the worker has no artifacts and no persistent state directory.
	for _, d := range []string{"art", "art2", "old-state"} {
		if _, err := os.Stat(filepath.Join(dir, d)); !os.IsNotExist(err) {
			t.Errorf("the worker created %s (%v)", d, err)
		}
	}
	if !strings.Contains(errb.String(), "CRYO_MAINTAIN_STATE_DIR is ignored") {
		t.Errorf("a set CRYO_MAINTAIN_STATE_DIR is not reported: %q", errb.String())
	}
	if fi, err := os.Stat(filepath.Join(dir, "scratch", "tmp")); err != nil || !fi.IsDir() {
		t.Errorf("CRYO_MAINTAIN_SCRATCH_DIR not used: %v", err)
	}

	// The checksum may also come from a file (the image build writes one): first word.
	t.Setenv("CRYO_MAINTAIN_RESTIC_SHA256", "")
	sumFile := filepath.Join(dir, "restic.sha256")
	if err := os.WriteFile(sumFile, []byte(sum+"  /usr/local/bin/restic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRYO_MAINTAIN_RESTIC_SHA256_FILE", sumFile)
	out.Reset()
	errb.Reset()
	if code := Run([]string{"--config", cfg, "--once"}, &out, &errb, keys); code != ExitOK {
		t.Fatalf("checksum from a file: exit %d: %s", code, errb.String())
	}
	if err := os.WriteFile(sumFile, []byte(strings.Repeat("2", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"--config", cfg, "--once"}, &out, &errb, keys); code != ExitUsage {
		t.Fatalf("wrong checksum in a file: exit %d", code)
	}
	t.Setenv("CRYO_MAINTAIN_RESTIC_SHA256_FILE", filepath.Join(dir, "absent.sha256"))
	if code := Run([]string{"--config", cfg, "--once"}, &out, &errb, keys); code != ExitUsage {
		t.Fatalf("unreadable checksum file: exit %d", code)
	}
	t.Setenv("CRYO_MAINTAIN_RESTIC_SHA256_FILE", "")

	// A wrong restic hash refuses to start with the usage exit code.
	t.Setenv("CRYO_MAINTAIN_RESTIC_SHA256", strings.Repeat("1", 64))
	out.Reset()
	errb.Reset()
	if code := Run([]string{"--config", cfg, "--once"}, &out, &errb, keys); code != ExitUsage {
		t.Fatalf("hash mismatch: exit %d: %s", code, errb.String())
	}
}
