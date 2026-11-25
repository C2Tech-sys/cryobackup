//go:build windows

package restoreplace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// The SYSTEM part of ADR 0027's lab checks (docs/lab/p22-agent-system.md).
// These tests run only when CRYO_P22_LAB_SYSTEM=1 and the process is
// LocalSystem (a one-shot scheduled task on the lab VM runs the compiled test
// binary); everywhere else they skip. They use the production options (no lab
// trustee) and the agent's privileges (WithPrivileges).

func labSystem(t *testing.T) string {
	t.Helper()
	if os.Getenv("CRYO_P22_LAB_SYSTEM") != "1" {
		t.Skip("lab only: set CRYO_P22_LAB_SYSTEM=1 and run as SYSTEM (docs/lab/p22-agent-system.md)")
	}
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || !u.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		t.Skip("lab only: not running as SYSTEM")
	}
	root := os.Getenv("CRYO_P22_LAB_DIR")
	if root == "" {
		t.Fatal("CRYO_P22_LAB_DIR is not set")
	}
	dir := filepath.Join(root, t.Name())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Check 6: the roots are created with the protected Administrators + SYSTEM
// descriptor and verify; a root owned by an ordinary user is refused and left.
func TestLabSystemRootRule(t *testing.T) {
	dir := labSystem(t)
	p := filepath.Join(dir, ".cryobackup-restore")
	r, err := OpenRoot(p, Options{})
	if err != nil {
		t.Fatalf("OpenRoot as SYSTEM: %v", err)
	}
	r.Close()
	sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("LAB 6 created root descriptor: %s", sd.String())
	if s := sd.String(); !strings.HasPrefix(s, "O:BA") || !strings.Contains(s, "D:P") {
		t.Errorf("created root descriptor = %s", s)
	}
	if user := os.Getenv("CRYO_P22_LAB_USER_SID"); user != "" {
		bad := filepath.Join(dir, "user-owned", ".cryobackup-restore")
		if err := os.MkdirAll(bad, 0o755); err != nil {
			t.Fatal(err)
		}
		sid, err := windows.StringToSid(user)
		if err != nil {
			t.Fatal(err)
		}
		_ = WithPrivileges(func() error {
			return windows.SetNamedSecurityInfo(bad, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, sid, nil, nil, nil)
		})
		var re *RootError
		if _, err := OpenRoot(bad, Options{}); !errors.As(err, &re) {
			t.Errorf("a user-owned root was accepted: %v", err)
		} else {
			t.Logf("LAB 6 user-owned root refused: check=%s", re.Check)
		}
	}
}

// Check 11 (and 5): a real symbolic link is a name surrogate; placement through
// it is refused; the clean-up walk deletes it without touching its target.
func TestLabSystemSymlink(t *testing.T) {
	dir := labSystem(t)
	target := filepath.Join(dir, "target")
	write(t, filepath.Join(target, "victim.txt"), "outside")
	dest := filepath.Join(dir, "dest")
	mkdirs(t, dest)
	link := filepath.Join(dest, "linked")
	lp, _ := windows.UTF16PtrFromString(link)
	tp, _ := windows.UTF16PtrFromString(target)
	if err := windows.CreateSymbolicLink(lp, tp, windows.SYMBOLIC_LINK_FLAG_DIRECTORY); err != nil {
		t.Fatalf("CreateSymbolicLink as SYSTEM: %v", err)
	}
	h, err := openNoFollow(link, windows.FILE_READ_ATTRIBUTES, shareAll)
	if err != nil {
		t.Fatal(err)
	}
	ti, _ := tagOf(h)
	windows.CloseHandle(h)
	t.Logf("LAB 11 symbolic link: attrs=%#x tag=%#x surrogate=%v", ti.Attrs, ti.Tag, ti.surrogate())
	if !ti.surrogate() {
		t.Error("a symbolic link is not a name surrogate")
	}
	stage := filepath.Join(dir, "stage")
	write(t, filepath.Join(stage, "linked", "victim.txt"), "restored")
	var c Counts
	_ = WithPrivileges(func() error {
		var err error
		c, err = Place(stage, dest, []Rel{{"linked"}}, Replace)
		return err
	})
	if c.Errors[ClassGuardRefused] != 1 || read(t, filepath.Join(target, "victim.txt")) != "outside" {
		t.Errorf("placement through a symbolic link: %+v", c)
	}
	links, err := RemoveTree(dest)
	if err != nil || links != 1 || read(t, filepath.Join(target, "victim.txt")) != "outside" {
		t.Errorf("clean-up walk: links=%d err=%v", links, err)
	}
}

// Check 7: the descriptor rule after a move, as SYSTEM with the agent's
// privileges.
func TestLabSystemDescriptorRule(t *testing.T) {
	dir := labSystem(t)
	stage := filepath.Join(dir, "stage")
	dest := filepath.Join(dir, "dest")
	mkdirs(t, stage)
	mkdirs(t, filepath.Join(dest, "d"))
	setSDDL(t, stage, "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FR;;;BG)")
	setSDDL(t, filepath.Join(dest, "d"), "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FR;;;BO)")
	write(t, filepath.Join(stage, "d", "plain.txt"), "p")
	write(t, filepath.Join(stage, "d", "prot.txt"), "q")
	setSDDL(t, filepath.Join(stage, "d", "prot.txt"), "D:P(A;;FA;;;SY)(A;;FR;;;WD)")
	protBefore := sddlOf(t, filepath.Join(stage, "d", "prot.txt"))
	var c Counts
	err := WithPrivileges(func() error {
		var err error
		c, err = Place(stage, dest, []Rel{{"d"}}, Keep)
		return err
	})
	if err != nil || c.Restored != 2 {
		t.Fatalf("place: %+v %v", c, err)
	}
	plain := sddlOf(t, filepath.Join(dest, "d", "plain.txt"))
	prot := sddlOf(t, filepath.Join(dest, "d", "prot.txt"))
	t.Logf("LAB 7 plain.txt: %s", plain)
	t.Logf("LAB 7 prot.txt: %s (before %s)", prot, protBefore)
	if strings.Contains(plain, ";;;BG)") || !strings.Contains(plain, "ID;FR;;;BO)") || prot != protBefore {
		t.Error("the descriptor rule does not hold as SYSTEM")
	}
}
