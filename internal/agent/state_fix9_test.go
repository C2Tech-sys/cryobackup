package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
)

// The tests in this file (ticket P1-FIX9) drive PrepareState through its
// hooks as an ELEVATED process would see it, without elevation: stateElevated
// says yes, restrictStateDir records the directories it would restrict, and
// verifyStateDir answers like VerifyDir would (a squatter's directory reads
// back with a foreign owner until it is restricted). Everything else, the
// quarantine rename included, is real.

var fix9Stamp = time.Date(2026, 10, 22, 15, 30, 0, 0, time.UTC)

// fakeACL stands in for the Windows descriptor calls.
type fakeACL struct {
	restricted map[string]bool  // directories "restricted" (verify then passes)
	fail       map[string]error // forced verify results, per directory
	restricts  int              // calls of the restrict hook
}

const squatterSID = "S-1-5-21-1000000000-2000000000-3000000000-1001" // a made-up user SID

func (f *fakeACL) verify(p string) error {
	if err, ok := f.fail[p]; ok {
		return err
	}
	if _, err := os.Lstat(p); err != nil {
		return fmt.Errorf("fake verify: %w", fs.ErrNotExist)
	}
	if f.restricted[p] {
		return nil
	}
	return fmt.Errorf("%w: %w (owner %s)", config.ErrStateUntrusted, config.ErrForeignOwner, squatterSID)
}

func (f *fakeACL) restrict(p string) error {
	f.restricts++
	f.restricted[p] = true
	return nil
}

// stubElevatedACL installs a fakeACL as an elevated process's view.
func stubElevatedACL(t *testing.T) *fakeACL {
	t.Helper()
	f := &fakeACL{restricted: map[string]bool{}, fail: map[string]error{}}
	el, ve, re, now := stateElevated, verifyStateDir, restrictStateDir, stateNow
	t.Cleanup(func() { stateElevated, verifyStateDir, restrictStateDir, stateNow = el, ve, re, now })
	stateElevated = func() bool { return true }
	verifyStateDir = f.verify
	restrictStateDir = f.restrict
	stateNow = func() time.Time { return fix9Stamp }
	return f
}

// squat leaves a state directory as a squatter would: their own agent.json
// (pointing at their console) and a device token.
func squat(t *testing.T) config.Layout {
	t.Helper()
	l := config.Layout{Root: filepath.Join(t.TempDir(), "CryoBackup")}
	for _, d := range []string{l.ConfigDir(), l.SecretsDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	planted := `{"server_url":"https://attacker.example","device_id":"01J00000000000000000000000"}`
	if err := os.WriteFile(l.ConfigFile(), []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.SecretsDir(), "device.token"), []byte("planted"), 0o600); err != nil {
		t.Fatal(err)
	}
	return l
}

// isQuarantineOf reports whether q is a quarantine of l made at `at`:
// <parent>\<name>.quarantine-<UTC stamp>-<8 hex>.
func isQuarantineOf(l config.Layout, q string, at time.Time) bool {
	prefix := filepath.Join(filepath.Dir(l.Root), filepath.Base(l.Root)+".quarantine-"+at.UTC().Format("20060102T150405Z")+"-")
	return strings.HasPrefix(q, prefix) && len(q) == len(prefix)+8
}

// Item 2 (HIGH, fail closed): an elevated start on an untrusted root refuses
// BEFORE any descriptor is modified, and a refused start never becomes a
// verified one on a later start.
func TestPrepareStateRefusesAnUntrustedRootBeforeTouchingIt(t *testing.T) {
	acl := stubElevatedACL(t)
	l := squat(t)

	for _, tc := range []struct {
		name string
		o    StateOptions
	}{
		{"service and install (strict)", StateOptions{Strict: true}},
		{"console without flags", StateOptions{}},
		{"console with the lab flag (it waives only the inability to restrict)", StateOptions{AllowUnprotected: true}},
	} {
		_, err := PrepareState(l.Root, tc.o)
		if !errors.Is(err, ErrStateUntrusted) {
			t.Errorf("%s: %v, want ErrStateUntrusted", tc.name, err)
		}
		if acl.restricts != 0 {
			t.Errorf("%s: the restrict hook ran %d times before the refusal", tc.name, acl.restricts)
		}
	}
	for _, d := range l.Children() {
		if d == l.ConfigDir() || d == l.SecretsDir() {
			continue
		}
		if _, err := os.Lstat(d); err == nil {
			t.Errorf("%s was created inside the untrusted root", filepath.Base(d))
		}
	}

	// A retry, even after someone made every descriptor read back as
	// expected, still refuses: the recorded verdict holds.
	for _, d := range append([]string{l.Root}, l.Children()...) {
		acl.restricted[d] = true
	}
	if _, err := PrepareState(l.Root, StateOptions{Strict: true}); !errors.Is(err, ErrStateUntrusted) {
		t.Errorf("a later start on the same tree: %v, want ErrStateUntrusted (a refused start became a verified one)", err)
	}
	if _, ok := verdictOf(l.Root); !ok {
		t.Error("no untrusted verdict recorded")
	}
	if q := l.Quarantines(); len(q) != 0 {
		t.Errorf("a start without --repair-acl quarantined the tree: %v", q)
	}
	if b, err := os.ReadFile(l.ConfigFile()); err != nil || !strings.Contains(string(b), "attacker.example") {
		t.Errorf("the refused tree was changed: %v", err)
	}
}

// Item 1 (HIGH): --repair-acl never adopts what is in an untrusted tree. The
// whole tree is moved aside, a fresh root is created and verified, and the
// device is not enrolled until it is enrolled again.
func TestPrepareStateRepairQuarantinesAndRecreates(t *testing.T) {
	acl := stubElevatedACL(t)
	l := squat(t)

	rep, err := PrepareState(l.Root, StateOptions{Repair: true})
	if err != nil {
		t.Errorf("--repair-acl on a squatted root: %v", err)
	}
	if b, err := os.ReadFile(l.ConfigFile()); err == nil {
		t.Errorf("the planted agent.json is still in the state directory: %s", b)
	}
	if _, err := os.Stat(filepath.Join(l.SecretsDir(), "device.token")); err == nil {
		t.Error("the planted device token is still in the state directory")
	}
	q := rep.Quarantined
	if !isQuarantineOf(l, q, fix9Stamp) {
		t.Errorf("quarantine %s is not <name>.quarantine-<UTC stamp>-<8 hex> next to the root", q)
	}
	if b, err := os.ReadFile(filepath.Join(q, "config", "agent.json")); err != nil || !strings.Contains(string(b), "attacker.example") {
		t.Errorf("the planted tree was not moved, whole, to %s: %v", filepath.Base(q), err)
	}
	for _, d := range append([]string{l.Root}, l.Children()...) {
		if st, err := os.Lstat(d); err != nil || !st.IsDir() {
			t.Errorf("fresh layout directory missing: %s", filepath.Base(d))
			continue
		}
		if !acl.restricted[d] || acl.verify(d) != nil {
			t.Errorf("fresh layout directory not restricted and verified: %s", filepath.Base(d))
		}
	}
	if _, ok := verdictOf(l.Root); ok {
		t.Error("the untrusted verdict survived a successful quarantine")
	}
	if b, err := os.ReadFile(l.AgentLog()); err != nil || !strings.Contains(string(b), filepath.Base(q)) {
		t.Errorf("no audit line naming the quarantine in the fresh agent.log: %v", err)
	}

	// Not enrolled until an elevated enroll writes agent.json and the secrets.
	if _, err := New(context.Background(), fakeOptions(t, l.Root)); !errors.Is(err, ErrNotEnrolled) {
		t.Errorf("New after a quarantine: %v, want ErrNotEnrolled", err)
	}
	// The fresh root is trusted from now on: a strict start verifies.
	if rep, err := PrepareState(l.Root, StateOptions{Strict: true}); err != nil || !rep.Verified {
		t.Errorf("the fresh root after the quarantine: %+v %v", rep, err)
	}
	// And the device can be enrolled into it again.
	fc := newFakeConsole(t)
	if _, err := Enroll(context.Background(), EnrollOptions{
		StateDir: l.Root, ServerURL: fc.URL(), Token: testEnrollToken, InsecureLab: true,
		AllowUnprotectedState: true, InsecurePlaintextSecrets: plainSecrets(),
	}); err != nil {
		t.Errorf("re-enrolment into the fresh root: %v", err)
	}
}
