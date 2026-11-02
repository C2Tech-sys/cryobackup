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

	"github.com/c2tech-sys/cryobackup/internal/agent"
	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/policy"
)

const (
	msiTestToken = "msi-enrollment-token-7f3a9c"
	// Two install runs' nonces.
	nonceA = "0123456789abcdef0123456789abcdef"
	nonceB = "fedcba9876543210fedcba9876543210"
)

// msiConsole is a console that enrolls msiTestToken and refuses anything else.
func msiConsole(t *testing.T) *httptest.Server {
	t.Helper()
	pol := policy.Default(policy.ClassDesktop)
	pol.Selection.Roots, pol.Selection.Presets = []string{`C:\fixture\tree`}, []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderServerTime, api.FormatServerTime(time.Now()))
		if r.URL.Path != api.RouteEnroll || r.Header.Get("Authorization") != "Bearer "+msiTestToken {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", api.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(api.EnrollResponse{
			DeviceID: "01JB0000000000000000000M51", CustomerID: "01JB0000000000000000000C01", DeviceToken: "dt", Custody: api.CustodyEscrowed,
			Gateway:      api.Gateway{URL: "https://gw.example", Username: "01JB0000000000000000000M51", Password: "gp"},
			RepoPassword: "rp", Policy: api.PolicyResponse{Policy: pol, PolicyID: "01JB0000000000000000000P01", Version: 2, ETag: `"e2"`}, ServerTime: time.Now(),
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// msiArgs are the test-only flags of an unelevated, cross-platform run.
func msiArgs(dir string) []string {
	a := []string{"--state-dir", dir, "--insecure-unprotected-state"}
	if runtime.GOOS != "windows" {
		a = append(a, "--insecure-plaintext-secrets")
	}
	return a
}

func msiDo(name, stdin string, args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = cmdMSI(name, args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

// configure, rollback and commit as one install run with the given nonce.
func configure(dir, nonce, lines string) (int, string, string) {
	return msiDo("msi-configure", lines+"nonce="+nonce+"\n", msiArgs(dir)...)
}

func rollback(dir, nonce string) (int, string) {
	code, out, _ := msiDo("msi-rollback", "nonce="+nonce+"\n", "--state-dir", dir)
	return code, out
}

func commit(dir string) int {
	code, _, _ := msiDo("msi-commit", "", "--state-dir", dir)
	return code
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// enrolledState is a state directory with an enrolled device in it, committed.
func enrolledState(t *testing.T) string {
	t.Helper()
	srv := msiConsole(t)
	dir := filepath.Join(t.TempDir(), "state")
	if code, _, errs := configure(dir, nonceA, "server="+srv.URL+"\ntoken="+msiTestToken+"\nlab=1\n"); code != exitOK {
		t.Fatalf("enroll: %d %q", code, errs)
	}
	if commit(dir) != exitOK {
		t.Fatal("commit")
	}
	return dir
}

// The subcommands are reachable through the command dispatch, with the
// request on the process's stdin.
func TestMSIDispatch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	in, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.WriteString("server=\ntoken=\nlab=\nrepair=\nnonce=" + nonceA + "\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = in
	t.Cleanup(func() { os.Stdin = old; _ = in.Close() })

	code, out, errs := do(append([]string{"msi-configure"}, msiArgs(dir)...)...)
	if code != exitOK || !strings.Contains(out, "NOT ENROLLED") {
		t.Fatalf("msi-configure through run: %d\nstdout %q\nstderr %q", code, out, errs)
	}
	if code, out, _ := do("msi-commit", "--state-dir", dir); code != exitOK || !strings.Contains(out, "commit") {
		t.Errorf("msi-commit through run: %d %q", code, out)
	}
	// stdin is at EOF now: a rollback with no request keeps the directory.
	if code, out, _ := do("msi-rollback", "--state-dir", dir); code != exitOK || !strings.Contains(out, "kept") || !exists(dir) {
		t.Errorf("msi-rollback through run: %d %q", code, out)
	}
}

func TestMSIRequestParsing(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"unknown key", "server=\nfoo=1\n", "unknown request key"},
		{"not key=value", "server\n", "not key=value"},
		{"lab not 0/1", "lab=yes\n", "CRYO_INSECURE_LAB: must be 1 or unset"},
		{"repair not 0/1", "repair=2\n", "CRYO_REPAIR_ACL: must be 1 or unset"},
		{"bad nonce", "nonce=XYZ\n", "nonce is malformed"},
		{"no nonce", "server=\ntoken=\n", "no run nonce"},
		{"too large", "server=" + strings.Repeat("x", msiRequestLimit) + "\n", "too large"},
	} {
		dir := filepath.Join(t.TempDir(), "state")
		code, _, errs := msiDo("msi-configure", tc.in, msiArgs(dir)...)
		if code != exitUsage || !strings.Contains(errs, tc.want) {
			t.Errorf("%s: %d %q, want exit 2 and %q", tc.name, code, errs, tc.want)
		}
		if exists(dir) {
			t.Errorf("%s: a refused request created the state directory", tc.name)
		}
	}
	bom := string([]byte{0xef, 0xbb, 0xbf}) // Windows PowerShell's pipeline writes one first
	req, err := parseMSIRequest(strings.NewReader(bom + "server=https://c.example\r\ntoken= t \r\nlab=0\r\nrepair=1\r\nnonce=" + nonceA + "\r\n"))
	if err != nil || req.server != "https://c.example" || req.token != "t" || req.lab || !req.repair || req.nonce != nonceA {
		t.Errorf("CRLF request: %+v %v", req, err)
	}
}

// The MSI's launch conditions are repeated by the agent: http without the lab
// property, and one of TOKEN/SERVER without the other, are refused before
// anything is created.
func TestMSIConfigureRefusesBadProperties(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"http without lab", "server=http://127.0.0.1:1\ntoken=" + msiTestToken + "\n", "CRYO_INSECURE_LAB=1"},
		{"token without server", "token=" + msiTestToken + "\n", "TOKEN was given without SERVER"},
		{"server without token", "server=https://c.example\n", "SERVER was given without TOKEN"},
		{"not a URL", "server=c.example\ntoken=x\n", "SERVER:"},
	} {
		dir := filepath.Join(t.TempDir(), "state")
		code, out, errs := configure(dir, nonceA, tc.in)
		if code != exitUsage || !strings.Contains(errs, tc.want) {
			t.Errorf("%s: %d %q, want exit 2 and %q", tc.name, code, errs, tc.want)
		}
		if strings.Contains(out+errs, msiTestToken) {
			t.Errorf("%s: the token was printed", tc.name)
		}
		if exists(dir) {
			t.Errorf("%s: the state directory was created", tc.name)
		}
	}
}

// No TOKEN: the state directory is prepared, the device is left unenrolled
// and the output says so; the marker lets this run's rollback remove what it
// created, and the commit removes the marker.
func TestMSIConfigureUnenrolledThenCommit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	code, out, errs := configure(dir, nonceA, "server=\ntoken=\nlab=\nrepair=\n")
	if code != exitOK || !strings.Contains(out, "state directory created") || !strings.Contains(out, "NOT ENROLLED") {
		t.Fatalf("configure: %d\nstdout %q\nstderr %q", code, out, errs)
	}
	marker := filepath.Join(dir, msiCreatedMarker)
	if b, err := os.ReadFile(marker); err != nil || string(b) != nonceA {
		t.Fatalf("marker = %q %v, want this run's nonce", b, err)
	}
	if commit(dir) != exitOK || exists(marker) {
		t.Fatalf("commit left the marker: %v", exists(marker))
	}
	// After the commit a rollback (of a later, failed upgrade) keeps the state.
	if code, out := rollback(dir, nonceA); code != exitOK || !strings.Contains(out, "kept") || !exists(dir) {
		t.Fatalf("rollback after commit: %d %q, dir kept %v", code, out, exists(dir))
	}
	// A second install (upgrade) finds the directory: no marker, state kept.
	code, out, _ = configure(dir, nonceB, "")
	if code != exitOK || !strings.Contains(out, "state directory exists") || exists(marker) {
		t.Fatalf("second configure: %d %q, marker %v", code, out, exists(marker))
	}
	if code, _ := rollback(dir, nonceB); code != exitOK || !exists(dir) {
		t.Fatal("a rollback removed a state directory that existed before the install")
	}
}

// This run's rollback removes the directory this run created.
func TestMSIRollbackRemovesWhatThisRunCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if code, _, errs := configure(dir, nonceA, ""); code != exitOK {
		t.Fatalf("configure: %d %q", code, errs)
	}
	if code, out := rollback(dir, nonceB); code != exitOK || !exists(dir) {
		t.Fatalf("another run's rollback removed the directory: %d %q", code, out)
	}
	if code, out := rollback(dir, ""); code != exitOK || !exists(dir) {
		t.Fatalf("a rollback without a nonce removed the directory: %d %q", code, out)
	}
	code, out := rollback(dir, nonceA)
	if code != exitOK || !strings.Contains(out, "removed") || exists(dir) {
		t.Fatalf("rollback: %d %q, dir still there %v", code, out, exists(dir))
	}
}

// V-M1-T4 finding 1, the verifier's repro: run A created the directory and
// never committed (its marker is stale), the device was enrolled; run B's
// configure refuses its request early (exit 2) and run B's rollback must keep
// the enrolled device's state.
func TestMSIStaleMarkerAndEarlyRefusalKeepState(t *testing.T) {
	srv := msiConsole(t)
	dir := filepath.Join(t.TempDir(), "state")
	if code, _, errs := configure(dir, nonceA, "server="+srv.URL+"\ntoken="+msiTestToken+"\nlab=1\n"); code != exitOK {
		t.Fatalf("run A: %d %q", code, errs)
	}
	// No commit: the marker of run A stays.
	if code, _, _ := configure(dir, nonceB, "server=https://\ntoken=abc\n"); code != exitUsage {
		t.Fatalf("run B configure: %d, want 2", code)
	}
	if code, out := rollback(dir, nonceB); code != exitOK || !exists(filepath.Join(dir, "config", "agent.json")) {
		t.Fatalf("run B's rollback removed the enrolled device's state: %d %q", code, out)
	}
}

// Every early exit of configure, followed by that run's rollback, leaves an
// existing state directory (with a stale marker in it) untouched.
func TestMSIEveryEarlyExitThenRollbackKeepsState(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(dir string) int
	}{
		{"flag parse failure", func(dir string) int {
			c, _, _ := msiDo("msi-configure", "nonce="+nonceB+"\n", "--state-dir", dir, "--no-such-flag")
			return c
		}},
		{"request parse failure", func(dir string) int {
			c, _, _ := msiDo("msi-configure", "bogus=1\nnonce="+nonceB+"\n", msiArgs(dir)...)
			return c
		}},
		{"no nonce", func(dir string) int {
			c, _, _ := msiDo("msi-configure", "server=\n", msiArgs(dir)...)
			return c
		}},
		{"token without server", func(dir string) int { c, _, _ := configure(dir, nonceB, "token=abc\n"); return c }},
		{"server without token", func(dir string) int { c, _, _ := configure(dir, nonceB, "server=https://c.example\n"); return c }},
		{"https with no host", func(dir string) int { c, _, _ := configure(dir, nonceB, "server=https://\ntoken=abc\n"); return c }},
		{"http without lab", func(dir string) int {
			c, _, _ := configure(dir, nonceB, "server=http://127.0.0.1:1\ntoken=abc\n")
			return c
		}},
	} {
		dir := enrolledState(t)
		// A stale marker from a run whose commit never ran.
		if err := os.WriteFile(filepath.Join(dir, msiCreatedMarker), []byte(nonceA), 0o600); err != nil {
			t.Fatal(err)
		}
		if code := tc.run(dir); code == exitOK {
			t.Errorf("%s: configure succeeded", tc.name)
		}
		if _, out := rollback(dir, nonceB); !exists(filepath.Join(dir, "config", "agent.json")) {
			t.Errorf("%s: the rollback removed existing state (%q)", tc.name, out)
		}
	}

	// The late exit: an existing, unenrolled state directory (run A's stale
	// marker in it) whose enrollment the console refuses in run B.
	dir := filepath.Join(t.TempDir(), "state")
	if code, _, errs := configure(dir, nonceA, ""); code != exitOK {
		t.Fatalf("configure: %d %q", code, errs)
	}
	if code, _, _ := configure(dir, nonceB, "server=http://127.0.0.1:1\ntoken=abc\nlab=1\n"); code != exitNetwork {
		t.Fatalf("refused enrollment: %d, want 5", code)
	}
	if _, out := rollback(dir, nonceB); !exists(filepath.Join(dir, "config", "agent.json")) {
		t.Errorf("enrollment refused: the rollback removed existing state (%q)", out)
	}
}

func TestMSIConfigureEnrolls(t *testing.T) {
	srv := msiConsole(t)
	dir := filepath.Join(t.TempDir(), "state")
	in := "server=" + srv.URL + "\ntoken=" + msiTestToken + "\nlab=1\nrepair=\n"
	code, out, errs := configure(dir, nonceA, in)
	if code != exitOK || !strings.Contains(out, "enrolled: device 01JB0000000000000000000M51") || !strings.Contains(out, "policy version 2") {
		t.Fatalf("configure with TOKEN: %d\nstdout %q\nstderr %q", code, out, errs)
	}
	if !strings.Contains(errs, "CRYO_INSECURE_LAB=1") {
		t.Errorf("the lab property was not warned about: %q", errs)
	}
	if strings.Contains(out+errs, msiTestToken) {
		t.Error("the token was printed")
	}
	// The status command sees the enrollment.
	if code, st, _ := do("status", "--state-dir", dir); code != exitOK || !strings.Contains(st, "01JB0000000000000000000M51") {
		t.Errorf("status after the MSI enrollment: %d\n%s", code, st)
	}
	// An upgrade or re-run with TOKEN on an enrolled device keeps the identity.
	code, out, errs = configure(dir, nonceB, in)
	if code != exitOK || !strings.Contains(out, "TOKEN ignored") || !strings.Contains(out, "01JB0000000000000000000M51") {
		t.Errorf("re-run with TOKEN: %d %q %q", code, out, errs)
	}
	code, out, _ = configure(dir, nonceB, "")
	if code != exitOK || !strings.Contains(out, "enrolled: device 01JB0000000000000000000M51") {
		t.Errorf("upgrade without TOKEN: %d %q", code, out)
	}
}

// An enrollment the console refuses fails the install with the error class
// in the output, never the token, and removes the state directory the install
// created.
func TestMSIConfigureEnrollFailure(t *testing.T) {
	srv := msiConsole(t)
	const wrong = "wrong-token-1b2c3d"
	dir := filepath.Join(t.TempDir(), "state")
	code, out, errs := configure(dir, nonceA, "server="+srv.URL+"\ntoken="+wrong+"\nlab=1\n")
	if code != exitNetwork || !strings.Contains(errs, "error class: auth") {
		t.Fatalf("refused enrollment: %d\nstdout %q\nstderr %q, want exit 5 and the auth class", code, out, errs)
	}
	if strings.Contains(out+errs, wrong) {
		t.Error("the token was printed")
	}
	if exists(dir) {
		t.Error("a failed enrollment left the state directory this install created")
	}

	// Unreachable console: the network class.
	dir2 := filepath.Join(t.TempDir(), "state")
	srv.Close()
	code, _, errs = configure(dir2, nonceA, "server="+srv.URL+"\ntoken="+wrong+"\nlab=1\n")
	if code != exitNetwork || !strings.Contains(errs, "error class: network") || exists(dir2) {
		t.Errorf("unreachable console: %d %q, dir left %v", code, errs, exists(dir2))
	}
}

// A failed enrollment into a directory that existed before the install keeps
// that directory (it may hold another install's state).
func TestMSIConfigureEnrollFailureKeepsExistingState(t *testing.T) {
	srv := msiConsole(t)
	dir := filepath.Join(t.TempDir(), "state")
	if code, _, errs := configure(dir, nonceA, ""); code != exitOK {
		t.Fatalf("configure: %d %q", code, errs)
	}
	if commit(dir) != exitOK {
		t.Fatal("commit")
	}
	code, _, _ := configure(dir, nonceB, "server="+srv.URL+"\ntoken=nope\nlab=1\n")
	if code != exitNetwork || !exists(dir) {
		t.Fatalf("enroll failure on existing state: %d, dir kept %v", code, exists(dir))
	}
	if _, out := rollback(dir, nonceB); !exists(dir) {
		t.Fatalf("the failed run's rollback removed pre-existing state: %q", out)
	}
}

func TestEnrollErrorClass(t *testing.T) {
	if got := enrollErrorClass(os.ErrPermission); got != "state" {
		t.Errorf("local error class = %q, want state", got)
	}
}

// stubQuarantine makes msi-configure's state preparation behave like an
// elevated --repair-acl on an untrusted directory: the existing tree is moved
// aside whole and a fresh one is prepared in its place (M1-FIX3 item 4; the
// real quarantine needs elevation and is exercised by smoke step 15b).
func stubQuarantine(t *testing.T) (quarantine string) {
	t.Helper()
	old := msiPrepareState
	t.Cleanup(func() { msiPrepareState = old })
	quarantine = filepath.Join(t.TempDir(), "state.quarantine-test")
	msiPrepareState = func(dir string, o agent.StateOptions) (agent.StateReport, error) {
		if !o.Repair {
			return old(dir, o)
		}
		if err := os.Rename(dir, quarantine); err != nil {
			return agent.StateReport{}, err
		}
		o.Repair = false
		st, err := old(dir, o)
		st.Quarantined = quarantine
		return st, err
	}
	return quarantine
}

// After a repair quarantine the state directory in place is the fresh one this
// run created: configure marks it with this run's nonce, so this run's rollback
// removes it (and only it: the quarantine stays).
func TestMSIConfigureRepairMarksTheFreshDirectory(t *testing.T) {
	dir := enrolledState(t)
	q := stubQuarantine(t)
	code, out, errs := configure(dir, nonceB, "repair=1\n")
	if code != exitOK || !strings.Contains(out, "state directory created") {
		t.Fatalf("configure: %d\nstdout %q\nstderr %q", code, out, errs)
	}
	if b, err := os.ReadFile(filepath.Join(dir, msiCreatedMarker)); err != nil || string(b) != nonceB {
		t.Fatalf("marker = %q %v, want run B's nonce in the fresh directory", b, err)
	}
	if !exists(filepath.Join(q, "config", "agent.json")) {
		t.Fatal("the quarantine does not hold the old state")
	}
	if code, out := rollback(dir, nonceB); code != exitOK || exists(dir) {
		t.Fatalf("rollback kept the fresh directory this run created: %d %q", code, out)
	}
	if !exists(filepath.Join(q, "config", "agent.json")) {
		t.Fatal("the rollback touched the quarantine")
	}
}

// A failure after the quarantine removes the fresh directory this run created
// at once and keeps the quarantine.
func TestMSIConfigureRepairThenFailureRemovesTheFreshDirectory(t *testing.T) {
	dir := enrolledState(t)
	q := stubQuarantine(t)
	srv := msiConsole(t)
	code, _, errs := configure(dir, nonceB, "repair=1\nserver="+srv.URL+"\ntoken=not-the-token\nlab=1\n")
	if code == exitOK {
		t.Fatalf("configure with a refused token succeeded: %q", errs)
	}
	if exists(dir) {
		t.Error("the fresh state directory this run created after the quarantine was left behind")
	}
	if !strings.Contains(errs, "quarantine") || !exists(filepath.Join(q, "config", "agent.json")) {
		t.Errorf("the quarantine was not kept and reported: %q", errs)
	}
}
