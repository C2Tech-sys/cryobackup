package agent

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
	"github.com/c2tech-sys/cryobackup/internal/agent/secrets"
)

// The trust rule of the state directory, every combination that matters.
func TestDecideState(t *testing.T) {
	untrusted := config.ErrStateUntrusted
	for _, tc := range []struct {
		name          string
		f             stateFacts
		strict, lab   bool
		verified      bool
		wantErr       error // nil = proceeds
		warns         bool
		problemNeedle string
	}{
		{"fresh root, applied, reads back", stateFacts{applied: true}, true, false, true, nil, false, ""},
		{"existing verified root, applied, reads back", stateFacts{existed: true, applied: true}, true, false, true, nil, false, ""},
		{"service: nothing applied", stateFacts{}, true, false, false, ErrACLRefused, false, "could not be applied"},
		{"service: the lab flag does not help", stateFacts{}, true, true, false, ErrACLRefused, false, "could not be applied"},
		{"service: applied but reads back wrong", stateFacts{applied: true, post: untrusted}, true, false, false, ErrStateUntrusted, false, "do not read back"},
		{"service: a root that was open before this start", stateFacts{existed: true, pre: untrusted, applied: true}, true, false, false, ErrStateUntrusted, false, "not trusted"},
		{"console, no flag, nothing applied", stateFacts{}, false, false, false, ErrACLRefused, false, "could not be applied"},
		{"console, no flag, open before", stateFacts{existed: true, pre: untrusted, applied: true}, false, false, false, ErrStateUntrusted, false, "not trusted"},
		{"console, lab flag, nothing applied", stateFacts{}, false, true, false, nil, true, "could not be applied"},
		{"console, lab flag, unelevated, open before", stateFacts{existed: true, pre: untrusted}, false, true, false, nil, true, "not trusted"},
		// The lab flag waives the inability to restrict, nothing an elevated
		// process could judge.
		{"console, lab flag, applied but reads back wrong", stateFacts{applied: true, post: untrusted}, false, true, false, ErrStateUntrusted, false, "do not read back"},
		{"console, lab flag, elevated, open before", stateFacts{existed: true, pre: untrusted, applied: true}, false, true, false, ErrStateUntrusted, false, "not trusted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verified, problem, err := decideState(tc.f, tc.strict, tc.lab)
			if verified != tc.verified {
				t.Errorf("verified = %v", verified)
			}
			if tc.wantErr == nil && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				hint := "--repair-acl"
				if errors.Is(err, ErrACLRefused) {
					hint = "--insecure-unprotected-state"
				}
				if !strings.Contains(err.Error(), hint) {
					t.Errorf("the refusal does not say what to do (%s): %v", hint, err)
				}
			}
			if tc.warns != (err == nil && problem != "") {
				t.Errorf("warning = %q (err %v)", problem, err)
			}
			if !strings.Contains(problem, tc.problemNeedle) {
				t.Errorf("problem %q lacks %q", problem, tc.problemNeedle)
			}
		})
	}
}

// Item 3: every layout directory is checked, and one failing directory fails
// the state directory, at start (before and after Ensure) and while running.
func TestVerifyLayoutChecksEveryDirectory(t *testing.T) {
	l := config.Layout{Root: filepath.Join(t.TempDir(), "CryoBackup")}
	all := l.All()
	if len(all) != 10 {
		t.Fatalf("%d layout directories, want the root and nine children", len(all))
	}
	for i, bad := range all {
		verify := func(p string) error {
			if p == bad {
				return config.ErrStateUntrusted
			}
			return nil
		}
		err := verifyLayout(l, verify, false)
		if !errors.Is(err, config.ErrStateUntrusted) {
			t.Errorf("%s fails alone: %v, want ErrStateUntrusted", l.Rel(bad), err)
		}
		if err != nil && strings.Contains(err.Error(), l.Root) {
			t.Errorf("the error names the path: %v", err)
		}
		// Missing children are allowed before Ensure, a missing root never.
		missing := func(p string) error {
			if p == bad {
				return fs404{}
			}
			return nil
		}
		err = verifyLayout(l, missing, true)
		if (i == 0) != (err != nil) {
			t.Errorf("%s missing before Ensure: %v", l.Rel(bad), err)
		}
		if err := verifyLayout(l, missing, false); err == nil {
			t.Errorf("%s missing after Ensure was accepted", l.Rel(bad))
		}
	}
}

// fs404 is a "does not exist" answer of a verify hook.
type fs404 struct{}

func (fs404) Error() string        { return "missing" }
func (fs404) Is(target error) bool { return target == os.ErrNotExist }

// Item 3 at start: a single child that fails the check before or after Ensure
// refuses exactly like the root (Event Log 1004), and a pre-check failure
// refuses before anything is restricted.
func TestPrepareStateEveryLayoutDirectory(t *testing.T) {
	for _, phase := range []string{"existing tree, before Ensure", "fresh tree, after Ensure"} {
		for i := range 10 {
			acl := stubElevatedACL(t)
			l := config.Layout{Root: filepath.Join(t.TempDir(), "CryoBackup")}
			bad := l.All()[i]
			if phase == "existing tree, before Ensure" {
				for _, d := range l.All() {
					if err := os.MkdirAll(d, 0o700); err != nil {
						t.Fatal(err)
					}
					acl.restricted[d] = true
				}
			}
			acl.fail[bad] = config.ErrStateUntrusted
			var events []uint32
			_, err := PrepareState(l.Root, StateOptions{Strict: true, events: func(_ string, id uint32, _ string) { events = append(events, id) }})
			if !errors.Is(err, ErrStateUntrusted) {
				t.Errorf("%s, %s fails: %v, want ErrStateUntrusted", phase, l.Rel(bad), err)
			}
			if len(events) == 0 || events[len(events)-1] != eventStateRefused {
				t.Errorf("%s, %s fails: events %v, want %d", phase, l.Rel(bad), events, eventStateRefused)
			}
			if phase == "existing tree, before Ensure" && acl.restricts != 0 {
				t.Errorf("%s, %s fails: restricted %d directories before refusing", phase, l.Rel(bad), acl.restricts)
			}
		}
	}
}

// Item 3 while running: any one of the ten directories changing stops backups
// with last_error_class = permission (Event Log 1006).
func TestRecheckStateEveryLayoutDirectory(t *testing.T) {
	l := config.Layout{Root: filepath.Join(t.TempDir(), "CryoBackup")}
	for _, bad := range l.All() {
		a := &Agent{layout: l, log: slog.New(slog.NewTextHandler(io.Discard, nil)), stateVerified: true}
		a.o.verifyState = func(p string) error {
			if p == bad {
				return config.ErrStateUntrusted
			}
			return nil
		}
		a.recheckState()
		if !errors.Is(a.stateErr, config.ErrStateUntrusted) {
			t.Errorf("%s changed: stateErr = %v", l.Rel(bad), a.stateErr)
		}
	}
	a := &Agent{layout: l, log: slog.New(slog.NewTextHandler(io.Discard, nil)), stateVerified: true}
	a.o.verifyState = func(string) error { return nil }
	a.recheckState()
	if a.stateErr != nil {
		t.Errorf("all ten verify: stateErr = %v", a.stateErr)
	}
}

// Item 1: the report and the Event Log name the quarantine.
func TestPrepareStateQuarantineReportAndEvents(t *testing.T) {
	stubElevatedACL(t)
	l := squat(t)
	var lines []string
	rep, err := PrepareState(l.Root, StateOptions{Repair: true, events: func(level string, id uint32, msg string) {
		lines = append(lines, level+" "+itoa(int64(id))+" "+msg)
	}})
	if err != nil {
		t.Fatalf("PrepareState: %v", err)
	}
	if !isQuarantineOf(l, rep.Quarantined, fix9Stamp) || !rep.Verified {
		t.Errorf("report %+v", rep)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "warning 1007 ") || !strings.Contains(joined, rep.Quarantined) {
		t.Errorf("no Event Log 1007 naming the quarantine:\n%s", joined)
	}
	if !strings.Contains(readFile(t, l.AgentLog()), "enroll") {
		t.Error("the audit line does not say the device must be enrolled again")
	}
}

// Item 1: a quarantine that cannot be done safely changes nothing and the
// verdict stays.
func TestPrepareStateQuarantineRefusals(t *testing.T) {
	t.Run("the predictable name is squatted: not a refusal any more (P1-FIX10)", func(t *testing.T) {
		stubElevatedACL(t)
		l := squat(t)
		squatted := filepath.Join(filepath.Dir(l.Root), filepath.Base(l.Root)+".quarantine-"+fix9Stamp.Format(config.QuarantineStamp))
		if err := os.Mkdir(squatted, 0o700); err != nil {
			t.Fatal(err)
		}
		rep, err := PrepareState(l.Root, StateOptions{Repair: true})
		if err != nil || !isQuarantineOf(l, rep.Quarantined, fix9Stamp) {
			t.Fatalf("a squatted stamp name: %+v %v", rep, err)
		}
		if entries, err := os.ReadDir(squatted); err != nil || len(entries) != 0 {
			t.Errorf("the squatted directory was used: %d entries, %v", len(entries), err)
		}
	})
	t.Run("every name tried is taken", func(t *testing.T) {
		stubElevatedACL(t)
		l := squat(t)
		q := quarantineState
		t.Cleanup(func() { quarantineState = q })
		quarantineState = func(string, time.Time) (config.Quarantine, error) {
			return config.Quarantine{}, fmt.Errorf("%w (%d names tried)", config.ErrQuarantineExists, config.QuarantineAttempts)
		}
		_, err := PrepareState(l.Root, StateOptions{Repair: true})
		if !errors.Is(err, ErrStateUntrusted) || !errors.Is(err, config.ErrQuarantineExists) {
			t.Fatalf("%v, want ErrStateUntrusted wrapping ErrQuarantineExists", err)
		}
		if _, err := os.Stat(l.ConfigFile()); err != nil {
			t.Errorf("the tree was changed: %v", err)
		}
		if _, ok := verdictOf(l.Root); !ok {
			t.Error("no verdict after a failed quarantine")
		}
	})
	t.Run("a reparse point inside", func(t *testing.T) {
		acl := stubElevatedACL(t)
		l := squat(t)
		target := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		junction := filepath.Join(l.SecretsDir(), "j")
		if runtime.GOOS == "windows" {
			if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, target).CombinedOutput(); err != nil {
				t.Skipf("cannot create a junction here (%v: %s)", err, out)
			}
		} else if err := os.Symlink(target, junction); err != nil {
			t.Skipf("cannot create a symbolic link: %v", err)
		}
		_, err := PrepareState(l.Root, StateOptions{Repair: true})
		if !errors.Is(err, config.ErrReparsePoint) {
			t.Fatalf("%v, want ErrReparsePoint", err)
		}
		if _, err := os.Stat(l.ConfigFile()); err != nil || acl.restricts != 0 {
			t.Errorf("the tree was changed (%v) or restricted (%d)", err, acl.restricts)
		}
	})
	t.Run("not elevated", func(t *testing.T) {
		if runtime.GOOS == "windows" && secrets.Elevated() {
			t.Skip("elevated: this covers the unelevated refusal of --repair-acl")
		}
		l := squat(t)
		_, err := PrepareState(l.Root, StateOptions{Repair: true, AllowUnprotected: true})
		if !errors.Is(err, ErrACLRefused) || !strings.Contains(err.Error(), "elevated") {
			t.Fatalf("--repair-acl unelevated: %v, want ErrACLRefused naming elevation", err)
		}
		if _, err := os.Stat(l.ConfigFile()); err != nil {
			t.Errorf("the tree was changed: %v", err)
		}
	})
	t.Run("a verdict refuses an unelevated lab run too", func(t *testing.T) {
		if runtime.GOOS == "windows" && secrets.Elevated() {
			t.Skip("elevated: this covers the unelevated view of the verdict")
		}
		l := config.Layout{Root: filepath.Join(t.TempDir(), "CryoBackup")}
		setVerdict(t, l.Root, "x") // as recorded by an elevated start
		if _, err := PrepareState(l.Root, StateOptions{AllowUnprotected: true}); !errors.Is(err, ErrStateUntrusted) {
			t.Fatalf("lab run with a verdict: %v, want ErrStateUntrusted", err)
		}
		if _, err := os.Lstat(l.Root); err == nil {
			t.Error("the root was created although a verdict exists")
		}
	})
}

// PrepareState as an unelevated process sees it (the elevated half is P1-T8's).
func TestPrepareStateUnelevated(t *testing.T) {
	if runtime.GOOS == "windows" && secrets.Elevated() {
		t.Skip("elevated: PrepareState restricts the directory here; this test covers the unelevated refusals")
	}
	dir := filepath.Join(t.TempDir(), "state")
	if _, err := PrepareState(dir, StateOptions{Strict: true}); !errors.Is(err, ErrACLRefused) {
		t.Fatalf("strict (service, install): %v, want ErrACLRefused", err)
	}
	if _, err := PrepareState(dir, StateOptions{}); !errors.Is(err, ErrACLRefused) {
		t.Fatalf("console without the flag: %v, want ErrACLRefused", err)
	}
	if _, err := os.Lstat(dir); err == nil {
		t.Error("a refused start created the state directory")
	}
	rep, err := PrepareState(dir, StateOptions{AllowUnprotected: true})
	if err != nil || rep.Verified || rep.Applied || rep.Warning == "" {
		t.Fatalf("lab flag: %+v %v", rep, err)
	}
	if strings.Contains(rep.Warning, dir) {
		t.Errorf("the warning names the path: %s", rep.Warning)
	}
	for _, d := range append([]string{dir}, config.Layout{Root: dir}.Children()...) {
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			t.Errorf("layout directory missing: %s", filepath.Base(d))
		}
	}
	// A second lab start on this user's own existing tree still works.
	if _, err := PrepareState(dir, StateOptions{AllowUnprotected: true}); err != nil {
		t.Fatalf("second lab start: %v", err)
	}
	if _, ok := verdictOf(dir); ok {
		t.Error("an unelevated run recorded the untrusted verdict (only an elevated start may)")
	}
}

// Item 4 (V7 N1): even with the lab flag, an unelevated run refuses a root
// another account owns when its owner can be read, and creates nothing in it.
// The probe is the Windows directory (owned by TrustedInstaller): read-only.
func TestPrepareStateLabRefusesAForeignOwnedRoot(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("owners are only read on Windows")
	}
	if secrets.Elevated() {
		t.Skip("elevated: this covers the unelevated lab path, and must not run where it could write")
	}
	sysRoot := os.Getenv("SystemRoot")
	if sysRoot == "" {
		t.Skip("no SystemRoot")
	}
	l := config.Layout{Root: sysRoot}
	_, err := PrepareState(sysRoot, StateOptions{AllowUnprotected: true})
	if !errors.Is(err, config.ErrForeignOwner) {
		t.Fatalf("a foreign-owned root with the lab flag: %v, want ErrForeignOwner", err)
	}
	for _, d := range []string{l.ConfigDir(), l.SecretsDir(), l.JournalDir()} {
		if _, err := os.Lstat(d); err == nil {
			t.Errorf("%s exists inside the foreign-owned root", l.Rel(d))
		}
	}
}

// A reparse-point root is refused whatever the flags say.
func TestPrepareStateRefusesAReparsePointRoot(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "state")
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", root, target).CombinedOutput(); err != nil {
			t.Skipf("cannot create a junction here (%v: %s)", err, out)
		}
	} else if err := os.Symlink(target, root); err != nil {
		t.Skipf("cannot create a symbolic link: %v", err)
	}
	_, err := PrepareState(root, StateOptions{AllowUnprotected: true})
	if !errors.Is(err, ErrACLRefused) || !errors.Is(err, config.ErrReparsePoint) {
		t.Fatalf("a reparse-point root: %v, want ErrACLRefused wrapping ErrReparsePoint", err)
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Errorf("the link target was written to: %d entries", len(entries))
	}
}
