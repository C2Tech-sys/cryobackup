package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
)

// The tests in this file (ticket P1-FIX10) cover the untrusted verdict, which
// lives in HKLM\SOFTWARE\CryoBackup\Agent on Windows (config.ReadVerdict):
// only an elevated process can write it, so an unelevated test process keeps
// it in memory (memVerdicts, installed by TestMain for every test of this
// package). The registry mechanics themselves are tested in package config.

// memVerdicts is the verdict store of this package's tests, keyed by state
// directory (config.VerdictID, as in production) so that tests stay
// independent of each other and of the machine.
var memVerdicts = struct {
	sync.Mutex
	m        map[string]config.Verdict
	writeErr error // forced WriteVerdict result
	clearErr error // forced ClearVerdict result
	readErr  error // forced ReadVerdict result
}{m: map[string]config.Verdict{}}

func useMemVerdicts() {
	readVerdict = func(root string) (config.Verdict, bool, error) {
		memVerdicts.Lock()
		defer memVerdicts.Unlock()
		if memVerdicts.readErr != nil {
			return config.Verdict{}, false, memVerdicts.readErr
		}
		v, ok := memVerdicts.m[config.VerdictID(root)]
		return v, ok, nil
	}
	writeVerdict = func(root string, v config.Verdict) error {
		memVerdicts.Lock()
		defer memVerdicts.Unlock()
		if memVerdicts.writeErr != nil {
			return memVerdicts.writeErr
		}
		if _, ok := memVerdicts.m[config.VerdictID(root)]; !ok { // the first verdict stands
			memVerdicts.m[config.VerdictID(root)] = v
		}
		return nil
	}
	clearVerdict = func(root string) error {
		memVerdicts.Lock()
		defer memVerdicts.Unlock()
		if memVerdicts.clearErr != nil {
			return memVerdicts.clearErr
		}
		delete(memVerdicts.m, config.VerdictID(root))
		return nil
	}
}

func verdictOf(root string) (config.Verdict, bool) {
	memVerdicts.Lock()
	defer memVerdicts.Unlock()
	v, ok := memVerdicts.m[config.VerdictID(root)]
	return v, ok
}

// setVerdict records a verdict as an earlier elevated start would have.
func setVerdict(t *testing.T, root, reason string) {
	t.Helper()
	memVerdicts.Lock()
	memVerdicts.m[config.VerdictID(root)] = config.Verdict{Since: fix9Stamp, Reason: reason}
	memVerdicts.Unlock()
	t.Cleanup(func() {
		memVerdicts.Lock()
		delete(memVerdicts.m, config.VerdictID(root))
		memVerdicts.Unlock()
	})
}

// forceVerdictErrs makes the store fail (nil leaves an operation working).
func forceVerdictErrs(t *testing.T, read, write, clear error) {
	t.Helper()
	memVerdicts.Lock()
	memVerdicts.readErr, memVerdicts.writeErr, memVerdicts.clearErr = read, write, clear
	memVerdicts.Unlock()
	t.Cleanup(func() {
		memVerdicts.Lock()
		memVerdicts.readErr, memVerdicts.writeErr, memVerdicts.clearErr = nil, nil, nil
		memVerdicts.Unlock()
	})
}

// verifiedTree is a state root whose ten layout directories all read back
// as restricted (the fake ACL's view).
func verifiedTree(t *testing.T, acl *fakeACL) config.Layout {
	t.Helper()
	l := config.Layout{Root: filepath.Join(t.TempDir(), "CryoBackup")}
	for _, d := range l.All() {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		acl.restricted[d] = true
	}
	return l
}

// Item 1: the verdict is not a file next to the root. An elevated refusal
// leaves nothing in the parent directory (ProgramData) but the root itself,
// and records the verdict in the store only an elevated process can write.
func TestPrepareStateRecordsTheVerdictOutsideProgramData(t *testing.T) {
	acl := stubElevatedACL(t)
	l := squat(t)
	_, err := PrepareState(l.Root, StateOptions{Strict: true})
	if !errors.Is(err, ErrStateUntrusted) || acl.restricts != 0 {
		t.Fatalf("an untrusted root: %v (restricts %d), want ErrStateUntrusted before anything is restricted", err, acl.restricts)
	}
	entries, _ := os.ReadDir(filepath.Dir(l.Root))
	if len(entries) != 1 || entries[0].Name() != "CryoBackup" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the parent directory holds %v, want the root alone (no file marker)", names)
	}
	v, ok := verdictOf(l.Root)
	if !ok || !v.Since.Equal(fix9Stamp) || !strings.Contains(v.Reason, "the state root") || strings.Contains(v.Reason, filepath.Dir(l.Root)) {
		t.Errorf("verdict %+v (recorded %v), want the stamp and a path-free reason", v, ok)
	}
	if strings.Contains(err.Error(), "untrusted-repair-required marker") {
		t.Errorf("the refusal still names a file marker: %v", err)
	}
}

// Item 1: a file at the old marker's path means nothing any more: no refusal,
// no event (1008 now reports a refused device token, ADR 0018), and it is neither read nor removed.
func TestPrepareStateIgnoresTheOldMarkerFile(t *testing.T) {
	acl := stubElevatedACL(t)
	l := verifiedTree(t, acl)
	old := filepath.Join(filepath.Dir(l.Root), "CryoBackup.untrusted-repair-required")
	if err := os.WriteFile(old, []byte("planted"), 0o600); err != nil {
		t.Fatal(err)
	}
	var events []string
	rep, err := PrepareState(l.Root, StateOptions{Strict: true, events: func(level string, id uint32, msg string) {
		events = append(events, fmt.Sprintf("%s %d %s", level, id, msg))
	}})
	if err != nil || !rep.Verified {
		t.Fatalf("a verified tree next to a stray marker file: %+v %v", rep, err)
	}
	if len(events) != 0 {
		t.Errorf("events %v, want none", events)
	}
	if b, err := os.ReadFile(old); err != nil || string(b) != "planted" {
		t.Errorf("the stray file was touched: %q %v", b, err)
	}
}

// Item 1: while the verdict exists every start refuses, elevated or not, lab
// flag or not, even when every descriptor reads back as expected; nothing is
// restricted or created.
func TestPrepareStateVerdictRefusesEveryStart(t *testing.T) {
	for _, tc := range []struct {
		name     string
		elevated bool
		o        StateOptions
	}{
		{"elevated service (strict)", true, StateOptions{Strict: true}},
		{"elevated console", true, StateOptions{}},
		{"elevated console with the lab flag", true, StateOptions{AllowUnprotected: true}},
		{"unelevated console", false, StateOptions{}},
		{"unelevated console with the lab flag", false, StateOptions{AllowUnprotected: true}},
		{"unelevated strict", false, StateOptions{Strict: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acl := stubElevatedACL(t)
			stateElevated = func() bool { return tc.elevated }
			l := verifiedTree(t, acl)
			setVerdict(t, l.Root, "secrets: owner "+squatterSID)
			_, err := PrepareState(l.Root, tc.o)
			if !errors.Is(err, ErrStateUntrusted) {
				t.Fatalf("%v, want ErrStateUntrusted", err)
			}
			if !strings.Contains(err.Error(), config.VerdictSource+" since 2026-10-22T15:30:00Z: secrets: owner "+squatterSID) {
				t.Errorf("the refusal does not quote the verdict: %v", err)
			}
			if acl.restricts != 0 {
				t.Errorf("the restrict hook ran %d times", acl.restricts)
			}
			if _, ok := verdictOf(l.Root); !ok {
				t.Error("a refused start cleared the verdict")
			}
		})
	}
	// No root at all: still refused, and none is created.
	stubElevatedACL(t)
	stateElevated = func() bool { return false }
	root := filepath.Join(t.TempDir(), "CryoBackup")
	setVerdict(t, root, "x")
	if _, err := PrepareState(root, StateOptions{AllowUnprotected: true}); !errors.Is(err, ErrStateUntrusted) {
		t.Fatalf("lab run without a root: %v, want ErrStateUntrusted", err)
	}
	if _, err := os.Lstat(root); err == nil {
		t.Error("the root was created although a verdict exists")
	}
}

// Item 1 (follow-up): one verdict per state directory. A refused lab
// --state-dir does not block another state directory (the service's default
// one), and repairing one leaves the other's verdict standing.
func TestPrepareStateVerdictIsPerStateDirectory(t *testing.T) {
	acl := stubElevatedACL(t)
	lab := squat(t)
	service := verifiedTree(t, acl)
	if _, err := PrepareState(lab.Root, StateOptions{AllowUnprotected: true}); !errors.Is(err, ErrStateUntrusted) {
		t.Fatalf("the squatted lab directory: %v, want ErrStateUntrusted", err)
	}
	if _, ok := verdictOf(lab.Root); !ok {
		t.Fatal("no verdict for the lab directory")
	}
	if rep, err := PrepareState(service.Root, StateOptions{Strict: true}); err != nil || !rep.Verified {
		t.Fatalf("the lab directory's verdict blocked another state directory: %+v %v", rep, err)
	}
	if _, ok := verdictOf(service.Root); ok {
		t.Error("a verdict was recorded for the verified directory")
	}
	// The same directory spelled differently is the same verdict.
	if _, err := PrepareState(strings.ToUpper(lab.Root)+string(filepath.Separator)+".", StateOptions{Strict: true}); !errors.Is(err, ErrStateUntrusted) && runtime.GOOS == "windows" {
		t.Errorf("the lab directory spelled differently: %v, want ErrStateUntrusted", err)
	}
	// Repairing the service directory (after its own refusal) leaves the lab
	// directory's verdict standing.
	setVerdict(t, service.Root, "service")
	if _, err := PrepareState(service.Root, StateOptions{Repair: true}); err != nil {
		t.Fatalf("--repair-acl on the service directory: %v", err)
	}
	if _, ok := verdictOf(service.Root); ok {
		t.Error("the service directory's verdict was not cleared")
	}
	if _, ok := verdictOf(lab.Root); !ok {
		t.Error("repairing one directory cleared another's verdict")
	}
}

// Item 1: a verdict that cannot be read is not "no verdict": refused, nothing
// created or restricted.
func TestPrepareStateUnreadableVerdictRefuses(t *testing.T) {
	for _, elevated := range []bool{true, false} {
		acl := stubElevatedACL(t)
		stateElevated = func() bool { return elevated }
		forceVerdictErrs(t, errors.New("Access is denied."), nil, nil)
		root := filepath.Join(t.TempDir(), "CryoBackup")
		_, err := PrepareState(root, StateOptions{AllowUnprotected: true, Repair: elevated})
		if !errors.Is(err, ErrStateUntrusted) || !strings.Contains(err.Error(), "cannot be read") {
			t.Errorf("elevated=%v: %v, want ErrStateUntrusted naming the unreadable verdict", elevated, err)
		}
		if _, err := os.Lstat(root); err == nil || acl.restricts != 0 {
			t.Errorf("elevated=%v: created (%v) or restricted (%d)", elevated, err, acl.restricts)
		}
	}
}

// Item 1: an owner that cannot be read, anywhere in the layout, is UNTRUSTED
// when elevated (never trusted): refused before anything is restricted, and
// the verdict is recorded. (config.TestOwnerRule has the rule's own rows.)
func TestPrepareStateElevatedUntrustedRows(t *testing.T) {
	unreadable := fmt.Errorf("%w: %w: the owner cannot be read (Access is denied.), and an elevated check never trusts what it cannot read", config.ErrStateUntrusted, config.ErrForeignOwner)
	for _, tc := range []struct {
		name string
		dir  func(config.Layout) string
		err  error
	}{
		{"the root's owner cannot be read", func(l config.Layout) string { return l.Root }, unreadable},
		{"secrets' owner cannot be read", func(l config.Layout) string { return l.SecretsDir() }, unreadable},
		{"logs\\runs' owner cannot be read", func(l config.Layout) string { return l.RunLogsDir() }, unreadable},
		{"journal's descriptor cannot be read", func(l config.Layout) string { return l.JournalDir() }, fmt.Errorf("%w: the descriptor cannot be read: Access is denied.", config.ErrStateUntrusted)},
		{"a foreign owner on cache\\restic", func(l config.Layout) string { return l.CacheDir() }, fmt.Errorf("%w: %w (owner %s)", config.ErrStateUntrusted, config.ErrForeignOwner, squatterSID)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acl := stubElevatedACL(t)
			l := verifiedTree(t, acl)
			acl.fail[tc.dir(l)] = tc.err
			_, err := PrepareState(l.Root, StateOptions{Strict: true})
			if !errors.Is(err, ErrStateUntrusted) {
				t.Fatalf("%v, want ErrStateUntrusted", err)
			}
			if acl.restricts != 0 {
				t.Errorf("the restrict hook ran %d times before the refusal", acl.restricts)
			}
			if _, ok := verdictOf(l.Root); !ok {
				t.Error("no verdict recorded")
			}
		})
	}
}

// Item 1: only a successful quarantine and a verified fresh root clear the
// verdict; a failed quarantine, or a verdict that cannot be cleared, refuses.
func TestPrepareStateRepairClearsTheVerdictOnlyOnSuccess(t *testing.T) {
	t.Run("the quarantine fails", func(t *testing.T) {
		stubElevatedACL(t)
		l := squat(t)
		q := quarantineState
		t.Cleanup(func() { quarantineState = q })
		quarantineState = func(string, time.Time) (config.Quarantine, error) {
			return config.Quarantine{}, config.ErrQuarantineExists
		}
		if _, err := PrepareState(l.Root, StateOptions{Repair: true}); !errors.Is(err, config.ErrQuarantineExists) {
			t.Fatalf("%v, want ErrQuarantineExists", err)
		}
		if _, ok := verdictOf(l.Root); !ok {
			t.Error("no verdict after a failed quarantine")
		}
	})
	t.Run("the verdict cannot be cleared", func(t *testing.T) {
		stubElevatedACL(t)
		l := squat(t)
		forceVerdictErrs(t, nil, nil, errors.New("Access is denied."))
		rep, err := PrepareState(l.Root, StateOptions{Repair: true})
		if !errors.Is(err, ErrStateUntrusted) || !strings.Contains(err.Error(), "could not be cleared") {
			t.Fatalf("%v, want ErrStateUntrusted naming the verdict", err)
		}
		if rep.Verified {
			t.Error("reported verified although the verdict stands")
		}
	})
	t.Run("success", func(t *testing.T) {
		stubElevatedACL(t)
		l := squat(t)
		if _, err := PrepareState(l.Root, StateOptions{Strict: true}); !errors.Is(err, ErrStateUntrusted) {
			t.Fatalf("first start: %v", err)
		}
		if _, ok := verdictOf(l.Root); !ok {
			t.Fatal("no verdict after the refusal")
		}
		rep, err := PrepareState(l.Root, StateOptions{Repair: true})
		if err != nil || rep.Quarantined == "" || !rep.Verified {
			t.Fatalf("--repair-acl: %+v %v", rep, err)
		}
		if _, ok := verdictOf(l.Root); ok {
			t.Error("the verdict survived a successful quarantine")
		}
	})
	t.Run("a verdict with no root left", func(t *testing.T) {
		stubElevatedACL(t)
		root := filepath.Join(t.TempDir(), "CryoBackup")
		setVerdict(t, root, "x")
		rep, err := PrepareState(root, StateOptions{Repair: true})
		if err != nil || rep.Quarantined != "" || !rep.Verified {
			t.Fatalf("--repair-acl without a root: %+v %v", rep, err)
		}
		if _, ok := verdictOf(root); ok {
			t.Error("the verdict was not cleared")
		}
	})
}
