package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/secrets"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/policy"
)

func do(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestUsageAndVersion(t *testing.T) {
	if code, _, errs := do(); code != exitUsage || !strings.Contains(errs, "usage: cryo-agent") {
		t.Errorf("no args: %d %q", code, errs)
	}
	if code, _, _ := do("bogus"); code != exitUsage {
		t.Errorf("unknown command: %d", code)
	}
	if code, out, _ := do("version"); code != exitOK || !strings.HasPrefix(out, "cryo-agent ") {
		t.Errorf("version: %d %q", code, out)
	}
	if code, out, _ := do("help"); code != exitOK || !strings.Contains(out, "enroll") || !strings.Contains(out, "exit codes") {
		t.Errorf("help: %d", code)
	}
	for _, sub := range []string{"run", "install", "enroll", "status", "check-in"} {
		if code, _, _ := do(sub, "--no-such-flag"); code != exitUsage {
			t.Errorf("%s with a bad flag: %d, want 2", sub, code)
		}
	}
}

func TestEnrollUsageErrors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(enrollEnvVar, "")
	if code, _, errs := do("enroll", "--state-dir", dir); code != exitUsage || !strings.Contains(errs, "--server") {
		t.Errorf("no server: %d %q", code, errs)
	}
	if code, _, errs := do("enroll", "--state-dir", dir, "--server", "https://console.example"); code != exitUsage || !strings.Contains(errs, "token") {
		t.Errorf("no token: %d %q", code, errs)
	}
	if code, _, _ := do("enroll", "--state-dir", dir, "--server", "https://console.example", "--token-file", filepath.Join(dir, "missing")); code != exitConfig {
		t.Errorf("missing token file: %d", code)
	}
}

func TestExitCodesForStateAndNetwork(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	// Not enrolled: status says so (exit 0), run and check-in refuse (exit 4).
	if code, out, _ := do("status", "--state-dir", dir); code != exitOK || !strings.Contains(out, "enrolled:") || !strings.Contains(out, "no") {
		t.Errorf("status: %d %q", code, out)
	}
	const lab = "--insecure-unprotected-state"
	if code, _, errs := do("run", "--state-dir", dir, lab); code != exitConfig || !strings.Contains(errs, "not enrolled") || !strings.Contains(errs, "WARNING: "+lab) {
		t.Errorf("run: %d %q", code, errs)
	}
	if code, _, _ := do("check-in", "--state-dir", dir, lab); code != exitConfig {
		t.Errorf("check-in: %d", code)
	}
	if !(runtime.GOOS == "windows" && secrets.Elevated()) {
		// Unprivileged, without the lab flag: the unprotected state directory is refused (exit 4).
		if code, _, errs := do("run", "--state-dir", dir); code != exitConfig || !strings.Contains(errs, lab) {
			t.Errorf("run without the lab flag: %d %q", code, errs)
		}
		if code, _, errs := do("enroll", "--state-dir", dir, "--server", "http://127.0.0.1:1", "--insecure-tls-for-lab", "--token", "t"); code != exitConfig || !strings.Contains(errs, lab) {
			t.Errorf("enroll without the lab flag: %d %q (refused before the console is contacted)", code, errs)
		}
	}
	// An unreachable console is exit 5, and the token never appears in the output.
	const secretToken = "tok-do-not-print-0123456789"
	code, out, errs := do("enroll", "--state-dir", dir, "--server", "http://127.0.0.1:1", "--insecure-tls-for-lab", lab, "--token", secretToken)
	if code != exitNetwork {
		t.Errorf("unreachable console: %d, want 5 (stderr %q)", code, errs)
	}
	if strings.Contains(out+errs, secretToken) {
		t.Error("the enrollment token was printed")
	}
}

func TestEnrollThroughTheCommandLine(t *testing.T) {
	const tok = "enrollment-token-cli"
	pol := policy.Default(policy.ClassDesktop)
	pol.Selection.Roots, pol.Selection.Presets = []string{`C:\fixture\tree`}, []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderServerTime, api.FormatServerTime(time.Now()))
		if r.URL.Path != api.RouteEnroll || r.Header.Get("Authorization") != "Bearer "+tok {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", api.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(api.EnrollResponse{
			DeviceID: "01JB0000000000000000000001", CustomerID: "01JB0000000000000000000C01", DeviceToken: "dt", Custody: api.CustodyEscrowed,
			Gateway:      api.Gateway{URL: "https://gw.example", Username: "01JB0000000000000000000001", Password: "gp"},
			RepoPassword: "rp", Policy: api.PolicyResponse{Policy: pol, PolicyID: "01JB0000000000000000000P01", Version: 3, ETag: `"e3"`}, ServerTime: time.Now(),
		})
	}))
	defer srv.Close()

	dir := filepath.Join(t.TempDir(), "state")
	tokFile := filepath.Join(t.TempDir(), "token.txt")
	if err := os.WriteFile(tokFile, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"enroll", "--state-dir", dir, "--server", srv.URL, "--insecure-tls-for-lab", "--token-file", tokFile, "--insecure-unprotected-state"}
	if runtime.GOOS != "windows" {
		args = append(args, "--insecure-plaintext-secrets")
	}
	code, out, errs := do(args...)
	if code != exitOK || !strings.Contains(out, "enrolled: device 01JB0000000000000000000001") || !strings.Contains(out, "policy version 3") {
		t.Fatalf("enroll: %d\nstdout %q\nstderr %q", code, out, errs)
	}
	if !strings.Contains(errs, "WARNING: --insecure-unprotected-state") {
		t.Errorf("the lab flag was not warned about: %q", errs)
	}
	code, out, _ = do("status", "--state-dir", dir)
	if code != exitOK || !strings.Contains(out, "01JB0000000000000000000001") || !strings.Contains(out, "version 3") || !strings.Contains(out, "INSECURE LAB") {
		t.Errorf("status after enroll: %d\n%s", code, out)
	}
	if !(runtime.GOOS == "windows" && secrets.Elevated()) && !strings.Contains(out, "NOT VERIFIED") {
		t.Errorf("status does not say the state directory is unverified:\n%s", out)
	}
	// A second enroll is refused as a state error.
	if code, _, errs := do(args...); code != exitConfig || !strings.Contains(errs, "already enrolled") {
		t.Errorf("second enroll: %d %q", code, errs)
	}
}
