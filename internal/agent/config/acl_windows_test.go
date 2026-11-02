//go:build windows

package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const notElevatedSkip = "this process is not elevated: the SYSTEM owner and protected DACL are not applied here (P1-T8 runs it elevated on a VM)"

// The SDDL renders and reads back as the rule expects (Windows' own SDDL
// parser and printer, no privilege needed).
func TestStateSDDLRoundTrip(t *testing.T) {
	sd, err := windows.SecurityDescriptorFromString(SDDLStateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckStateSDDL(sd.String()); err != nil {
		t.Fatalf("%s does not pass its own check: %v", sd.String(), err)
	}
	owner, _, err := sd.Owner()
	if err != nil || !owner.IsWellKnown(windows.WinLocalSystemSid) {
		t.Errorf("owner is not SYSTEM: %v", err)
	}
	if _, _, _, err := stateSD(); err != nil {
		t.Errorf("stateSD: %v", err)
	}
}

func TestVerifyDir(t *testing.T) {
	dir := t.TempDir()
	if err := VerifyDir(dir); !errors.Is(err, ErrStateUntrusted) {
		t.Errorf("a directory with an inherited DACL: %v, want ErrStateUntrusted", err)
	}
	if err := VerifyDir(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing directory: %v, want fs.ErrNotExist", err)
	}
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	j := filepath.Join(dir, "junction")
	link(t, target, j)
	if err := VerifyDir(j); !errors.Is(err, ErrReparsePoint) {
		t.Errorf("a junction: %v, want ErrReparsePoint", err)
	}
}

func TestUnelevatedRefusalPaths(t *testing.T) {
	if canRestrict() {
		t.Skip("elevated: these are the paths of a process that is NOT elevated")
	}
	// RestrictDir changes nothing and says why.
	dir := t.TempDir()
	if err := RestrictDir(dir); !errors.Is(err, ErrACLNotApplied) {
		t.Fatalf("RestrictDir unelevated: %v, want ErrACLNotApplied", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "probe"), []byte("x"), 0o600); err != nil {
		t.Fatalf("the directory became unusable: %v", err)
	}

	// An existing root owned by this (non-administrator) user is a squat when
	// the caller can restrict: refused, not adopted.
	squat := Layout{Root: filepath.Join(t.TempDir(), "CryoBackup")}
	if err := os.Mkdir(squat.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	asElevated := func() bool { return true }
	hook := func(string) error { t.Error("the hook ran on a squatted root"); return nil }
	if _, err := squat.Ensure(EnsureOptions{Restrict: hook, elevated: asElevated}); !errors.Is(err, ErrForeignOwner) {
		t.Fatalf("a user-owned root: %v, want ErrForeignOwner", err)
	}
	if _, err := os.Stat(squat.ConfigDir()); err == nil {
		t.Error("the layout was created inside a squatted root")
	}

	// VerifyDir names the owner as the difference (ErrForeignOwner as well).
	if err := VerifyDir(squat.Root); !errors.Is(err, ErrStateUntrusted) || !errors.Is(err, ErrForeignOwner) {
		t.Errorf("VerifyDir on a user-owned root: %v, want ErrStateUntrusted and ErrForeignOwner", err)
	}

	// Creating the root with the SYSTEM owner needs SeRestorePrivilege, which
	// this token does not hold: a typed error, nothing created.
	fresh := Layout{Root: filepath.Join(t.TempDir(), "CryoBackup")}
	if _, err := fresh.Ensure(EnsureOptions{Restrict: hook, elevated: asElevated}); !errors.Is(err, ErrPrivilege) {
		t.Fatalf("creating a restricted root without the privileges: %v, want ErrPrivilege", err)
	}
	if _, err := os.Stat(fresh.Root); err == nil {
		t.Error("a root was created although its descriptor could not be set")
	}
}

// Elevated only: the real thing, end to end on a fresh root.
func TestEnsureElevatedAppliesOwnerAndDACLEverywhere(t *testing.T) {
	if !canRestrict() {
		t.Skip(notElevatedSkip)
	}
	l := Layout{Root: filepath.Join(t.TempDir(), "CryoBackup")}
	res, err := l.Ensure(EnsureOptions{Restrict: RestrictDir})
	if err != nil || !res.Applied || !res.Created {
		t.Fatalf("Ensure: %+v %v", res, err)
	}
	for _, d := range append([]string{l.Root}, l.Children()...) {
		if err := VerifyDir(d); err != nil {
			t.Errorf("%s: %v", l.rel(d), err)
		}
	}
	// A second start on the now-existing root is accepted (owner SYSTEM).
	if _, err := l.Ensure(EnsureOptions{Restrict: RestrictDir}); err != nil {
		t.Errorf("second Ensure: %v", err)
	}
}

// The lab owner rule reads real owners: this user's own directory passes, a
// directory another account owns (the Windows directory, owned by
// TrustedInstaller) does not. Read-only: nothing is created or changed.
func TestCheckOwnerLabReadsTheRealOwner(t *testing.T) {
	if err := checkOwnerLab(t.TempDir()); err != nil {
		t.Errorf("this user's own directory: %v", err)
	}
	sysRoot := os.Getenv("SystemRoot")
	owner, err := readOwner(sysRoot)
	if err != nil {
		t.Skipf("the Windows directory's owner cannot be read here (%v)", err)
	}
	if ownerTrusted(owner) {
		t.Skipf("the Windows directory is owned by %s here, not by another account", owner.String())
	}
	if err := checkOwnerLab(sysRoot); !errors.Is(err, ErrForeignOwner) {
		t.Errorf("a directory owned by %s: %v, want ErrForeignOwner", owner.String(), err)
	}
}

// The owner rule of every check (ticket P1-FIX10): an owner that cannot be
// read is UNTRUSTED when the verdict is an elevated one, never trusted; only
// an unelevated lab run lets it through.
func TestOwnerRule(t *testing.T) {
	wk := func(w windows.WELL_KNOWN_SID_TYPE) *windows.SID {
		s, err := windows.CreateWellKnownSid(w)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	sys, admins := wk(windows.WinLocalSystemSid), wk(windows.WinBuiltinAdministratorsSid)
	user, err := windows.StringToSid("S-1-5-21-1000000000-2000000000-3000000000-1001") // made up
	if err != nil {
		t.Fatal(err)
	}
	other, err := windows.StringToSid("S-1-5-21-1000000000-2000000000-3000000000-1002") // made up
	if err != nil {
		t.Fatal(err)
	}
	denied := windows.ERROR_ACCESS_DENIED
	for _, tc := range []struct {
		name     string
		owner    *windows.SID
		readErr  error
		elevated bool
		want     error // nil: trusted
	}{
		{"elevated, a reparse point", nil, ErrReparsePoint, true, ErrReparsePoint},
		{"elevated, owner unreadable (access denied)", nil, denied, true, ErrStateUntrusted},
		{"elevated, owner unreadable is foreign as well", nil, denied, true, ErrForeignOwner},
		{"elevated, no owner in the descriptor", nil, nil, true, ErrStateUntrusted},
		{"elevated, SYSTEM", sys, nil, true, nil},
		{"elevated, Administrators", admins, nil, true, nil},
		{"elevated, another account", other, nil, true, ErrForeignOwner},
		{"elevated lab check, this process's own account (self passed)", user, nil, true, nil},
		{"lab, a reparse point", nil, ErrReparsePoint, false, ErrReparsePoint},
		{"lab, owner unreadable (waived: the lab flag never verifies)", nil, denied, false, nil},
		{"lab, SYSTEM", sys, nil, false, nil},
		{"lab, this process's own account", user, nil, false, nil},
		{"lab, another account", other, nil, false, ErrForeignOwner},
	} {
		err := ownerRule(tc.owner, tc.readErr, tc.elevated, user)
		if tc.elevated && tc.owner != nil && !tc.owner.Equals(user) {
			// The verdict checks (checkRootOwner, VerifyDir) pass no self:
			// the same row without one must give the same answer.
			if e2 := ownerRule(tc.owner, tc.readErr, true, nil); (e2 == nil) != (err == nil) {
				t.Errorf("%s: with self %v, without %v", tc.name, err, e2)
			}
		}
		switch {
		case tc.want == nil && err != nil:
			t.Errorf("%s: %v, want trusted", tc.name, err)
		case tc.want != nil && !errors.Is(err, tc.want):
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	// checkRootOwner is the elevated rule: a root whose owner cannot be read
	// (it does not exist here) is untrusted, not let through.
	if err := checkRootOwner(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, ErrStateUntrusted) || !errors.Is(err, ErrForeignOwner) {
		t.Errorf("checkRootOwner with an unreadable owner: %v, want ErrStateUntrusted and ErrForeignOwner", err)
	}
}

// ReadOwnerAndDACL reads the object itself, never a junction's target.
func TestReadOwnerAndDACLDoesNotFollow(t *testing.T) {
	dir := t.TempDir()
	sddl, err := ReadOwnerAndDACL(dir)
	if err != nil || !strings.HasPrefix(sddl, "O:") || !strings.Contains(sddl, "D:") {
		t.Fatalf("ReadOwnerAndDACL: %q %v", sddl, err)
	}
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	j := filepath.Join(dir, "junction")
	link(t, target, j)
	if _, err := ReadOwnerAndDACL(j); !errors.Is(err, ErrReparsePoint) {
		t.Errorf("a junction: %v, want ErrReparsePoint", err)
	}
}

// Elevated only: a quarantine gets the owner Administrators and the protected
// admin-only DACL on its top directory, and nothing below it is touched.
func TestQuarantineRootElevatedSecuresTheQuarantine(t *testing.T) {
	if !canRestrict() {
		t.Skip(notElevatedSkip)
	}
	root := filepath.Join(t.TempDir(), "CryoBackup")
	planted := filepath.Join(root, "config", "agent.json")
	if err := os.MkdirAll(filepath.Dir(planted), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planted, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := readOwner(planted)
	if err != nil {
		t.Fatal(err)
	}
	q, err := QuarantineRoot(root, time.Now())
	if err != nil || q.SecureErr != nil {
		t.Fatalf("QuarantineRoot: %+v %v", q, err)
	}
	owner, err := readOwner(q.Path)
	if err != nil || !owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
		t.Errorf("quarantine owner = %v (%v), want Administrators", owner, err)
	}
	after, err := readOwner(filepath.Join(q.Path, "config", "agent.json"))
	if err != nil || !after.Equals(before) {
		t.Errorf("a file inside the quarantine changed owner: %v -> %v (%v)", before, after, err)
	}
}
