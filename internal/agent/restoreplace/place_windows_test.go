//go:build windows

package restoreplace

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// These tests run as an ordinary, non-elevated user on NTFS (the checks that
// need SYSTEM are the lab checks of ADR 0027, docs/lab/p22-agent-system.md).
// Each builds a staging tree and a destination tree in its own temp folder;
// the destination folder stands in for the drive root.

func mkdirs(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, p, body string) {
	t.Helper()
	mkdirs(t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

func junction(t *testing.T, link, target string) {
	t.Helper()
	out, err := exec.Command("cmd.exe", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Fatalf("mklink /J: %v: %s", err, out)
	}
}

func sddlOf(t *testing.T, p string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}

func setSDDL(t *testing.T, p, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	ctl, _, _ := sd.Control()
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION)
	if ctl&windows.SE_DACL_PROTECTED != 0 {
		info |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	} else {
		info |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	if err := windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, info, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

func me(t *testing.T) string {
	t.Helper()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return u.User.Sid.String()
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// rig is a staging folder and a destination "drive root" side by side.
type rig struct{ base, stage, dest string }

func newRig(t *testing.T) rig {
	t.Helper()
	base := t.TempDir()
	r := rig{base: base, stage: filepath.Join(base, "stage"), dest: filepath.Join(base, "dest")}
	mkdirs(t, r.stage)
	mkdirs(t, r.dest)
	return r
}

func sel(paths ...string) []Rel {
	var out []Rel
	for _, p := range paths {
		out = append(out, Rel(strings.Split(p, `\`)))
	}
	return out
}

func TestPlaceKeepAndReplace(t *testing.T) {
	r := newRig(t)
	write(t, filepath.Join(r.stage, "docs", "new.txt"), "restored-new")
	write(t, filepath.Join(r.stage, "docs", "old.txt"), "restored-old")
	write(t, filepath.Join(r.stage, "docs", "deep", "gone", "g.txt"), "restored-g")
	write(t, filepath.Join(r.dest, "docs", "old.txt"), "existing")

	c, err := Place(r.stage, r.dest, sel(`docs`), Keep)
	if err != nil {
		t.Fatal(err)
	}
	// Restored: new.txt and g.txt; skipped: old.txt and the selected folder
	// docs itself (it existed).
	if c.Restored != 2 || c.Skipped != 2 || c.ErrorTotal() != 0 {
		t.Fatalf("keep counts = %+v", c)
	}
	if read(t, filepath.Join(r.dest, "docs", "old.txt")) != "existing" || read(t, filepath.Join(r.dest, "docs", "new.txt")) != "restored-new" ||
		read(t, filepath.Join(r.dest, "docs", "deep", "gone", "g.txt")) != "restored-g" {
		t.Fatal("keep mode placed the wrong content")
	}
	if c.BytesRestored != uint64(len("restored-new")+len("restored-g")) || c.BytesSkipped != uint64(len("restored-old")) {
		t.Fatalf("bytes = %d / %d", c.BytesRestored, c.BytesSkipped)
	}

	// Replace mode over the remaining staged file.
	c, err = Place(r.stage, r.dest, sel(`docs`), Replace)
	if err != nil {
		t.Fatal(err)
	}
	if c.Restored != 1 || read(t, filepath.Join(r.dest, "docs", "old.txt")) != "restored-old" {
		t.Fatalf("replace counts = %+v, content %q", c, read(t, filepath.Join(r.dest, "docs", "old.txt")))
	}
}

// A junction anywhere on the destination path refuses the items below it, and
// nothing is written into the junction's target.
func TestPlaceRefusesJunctionOnDestinationPath(t *testing.T) {
	r := newRig(t)
	outside := filepath.Join(r.base, "outside")
	mkdirs(t, outside)
	write(t, filepath.Join(outside, "victim.txt"), "outside-content")
	mkdirs(t, filepath.Join(r.dest, "Users"))
	junction(t, filepath.Join(r.dest, "Users", "bob"), outside)
	write(t, filepath.Join(r.stage, "Users", "bob", "victim.txt"), "restored")
	write(t, filepath.Join(r.stage, "Users", "bob", "other.txt"), "restored")

	c, err := Place(r.stage, r.dest, sel(`Users\bob`), Replace)
	if err != nil {
		t.Fatal(err)
	}
	if c.Errors[ClassGuardRefused] != 2 || c.Restored != 0 {
		t.Fatalf("counts = %+v, want 2 guard_refused", c)
	}
	if read(t, filepath.Join(outside, "victim.txt")) != "outside-content" || exists(filepath.Join(outside, "other.txt")) {
		t.Fatal("placement wrote through the junction")
	}
}

// While a file is moved, its destination parent and grandparent are held: a
// rename of either fails, and the move still lands.
func TestPlaceHoldsParentsDuringMove(t *testing.T) {
	r := newRig(t)
	mkdirs(t, filepath.Join(r.dest, "a", "b"))
	write(t, filepath.Join(r.stage, "a", "b", "f.txt"), "restored")
	var renameParent, renameGrand error
	testHookBeforeMove = func(dest string) {
		parent := filepath.Dir(dest)
		renameParent = os.Rename(parent, parent+"-swapped")
		grand := filepath.Dir(parent)
		renameGrand = os.Rename(grand, grand+"-swapped")
	}
	defer func() { testHookBeforeMove = nil }()
	c, err := Place(r.stage, r.dest, sel(`a\b\f.txt`), Keep)
	if err != nil {
		t.Fatal(err)
	}
	if renameParent == nil || renameGrand == nil {
		t.Fatalf("a held folder was renamed: parent=%v grandparent=%v", renameParent, renameGrand)
	}
	if c.Restored != 1 || read(t, filepath.Join(r.dest, "a", "b", "f.txt")) != "restored" {
		t.Fatalf("counts = %+v", c)
	}
	// Released afterwards: the same rename now works.
	if err := os.Rename(filepath.Join(r.dest, "a"), filepath.Join(r.dest, "a2")); err != nil {
		t.Fatalf("rename after placement: %v", err)
	}
}

// A file that appears at the destination name between the check and the move
// is never overwritten: the no-replace rename fails and the item is skipped.
func TestPlaceNoReplaceRaceIsSkipped(t *testing.T) {
	r := newRig(t)
	mkdirs(t, filepath.Join(r.dest, "d"))
	write(t, filepath.Join(r.stage, "d", "f.txt"), "restored")
	testHookBeforeMove = func(dest string) {
		_ = os.WriteFile(dest, []byte("appeared"), 0o644)
	}
	defer func() { testHookBeforeMove = nil }()
	c, err := Place(r.stage, r.dest, sel(`d\f.txt`), Keep)
	if err != nil {
		t.Fatal(err)
	}
	if c.Skipped != 1 || c.Restored != 0 || c.ErrorTotal() != 0 {
		t.Fatalf("counts = %+v, want one skipped", c)
	}
	if read(t, filepath.Join(r.dest, "d", "f.txt")) != "appeared" {
		t.Fatal("the file that appeared was overwritten")
	}
}

// A destination folder reached only through its 8.3 short name is refused:
// the real long name read through the handle does not equal the selected name.
func TestPlaceRefusesShortNameMatch(t *testing.T) {
	r := newRig(t)
	long := filepath.Join(r.dest, "LongFolderNameForTest")
	mkdirs(t, long)
	buf := make([]uint16, 512)
	p, _ := windows.UTF16PtrFromString(long)
	n, err := windows.GetShortPathName(p, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 {
		t.Skipf("no short name: %v", err)
	}
	short := filepath.Base(windows.UTF16ToString(buf[:n]))
	if short == filepath.Base(long) {
		t.Skip("8.3 names are not generated on this volume")
	}
	write(t, filepath.Join(r.stage, short, "f.txt"), "restored")
	c, err := Place(r.stage, r.dest, sel(short), Keep)
	if err != nil {
		t.Fatal(err)
	}
	if c.Errors[ClassGuardRefused] != 1 || c.Restored != 0 {
		t.Fatalf("counts = %+v, want 1 guard_refused", c)
	}
	if exists(filepath.Join(long, "f.txt")) {
		t.Fatal("placed through a short name")
	}
}

// A name with a trailing dot is never mapped to the name without it: the
// \\?\ form keeps the dot, and a handle a Win32 open stripped fails the exact
// compare.
func TestTrailingDotIsRefusedNotMapped(t *testing.T) {
	r := newRig(t)
	write(t, filepath.Join(r.dest, "name"), "existing")
	// The Win32 form strips the trailing dot and opens "name": the exact
	// compare refuses it.
	p, _ := windows.UTF16PtrFromString(filepath.Join(r.dest, "name."))
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES, shareAll, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatalf("Win32 open of the dotted name: %v", err)
	}
	ok := checkName(h, "name.")
	windows.CloseHandle(h)
	if ok {
		t.Fatal("a stripped trailing dot passed the exact compare")
	}
	// Placement of a staged "name." in replace mode never touches "name".
	sp := longPath(filepath.Join(r.stage, "name."))
	sp16, _ := windows.UTF16PtrFromString(sp)
	fh, err := windows.CreateFile(sp16, windows.GENERIC_WRITE, 0, nil, windows.CREATE_NEW, 0, 0)
	if err != nil {
		t.Fatalf("create staged dotted name: %v", err)
	}
	windows.CloseHandle(fh)
	c, err := Place(r.stage, r.dest, []Rel{{"name."}}, Replace)
	if err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(r.dest, "name")) != "existing" {
		t.Fatal("the undotted name was replaced")
	}
	if c.Restored+c.Errors[ClassGuardRefused] != 1 {
		t.Fatalf("counts = %+v", c)
	}
}

// A file where a folder is, or a folder where a file is: guard_refused in any
// mode, nothing changed.
func TestPlaceTypeMismatch(t *testing.T) {
	r := newRig(t)
	mkdirs(t, filepath.Join(r.dest, "x", "isdir"))
	write(t, filepath.Join(r.dest, "x", "isfile"), "existing")
	write(t, filepath.Join(r.stage, "x", "isdir"), "restored-file")
	write(t, filepath.Join(r.stage, "x", "isfile", "a.txt"), "restored-a")
	write(t, filepath.Join(r.stage, "x", "isfile", "b.txt"), "restored-b")
	c, err := Place(r.stage, r.dest, sel(`x`), Replace)
	if err != nil {
		t.Fatal(err)
	}
	if c.Errors[ClassGuardRefused] != 3 {
		t.Fatalf("counts = %+v, want 3 guard_refused", c)
	}
	if read(t, filepath.Join(r.dest, "x", "isfile")) != "existing" {
		t.Fatal("the file was changed")
	}
	if st, err := os.Lstat(filepath.Join(r.dest, "x", "isdir")); err != nil || !st.IsDir() {
		t.Fatal("the folder was changed")
	}
}

// Replace mode never replaces a read-only or a system file, and never clears
// the attribute.
func TestPlaceReplaceSkipsReadOnlyAndSystem(t *testing.T) {
	r := newRig(t)
	for name, attr := range map[string]uint32{"ro.txt": windows.FILE_ATTRIBUTE_READONLY, "sys.txt": windows.FILE_ATTRIBUTE_SYSTEM} {
		d := filepath.Join(r.dest, "d", name)
		write(t, d, "existing")
		p, _ := windows.UTF16PtrFromString(d)
		if err := windows.SetFileAttributes(p, attr); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = windows.SetFileAttributes(p, windows.FILE_ATTRIBUTE_NORMAL) })
		write(t, filepath.Join(r.stage, "d", name), "restored")
	}
	c, err := Place(r.stage, r.dest, sel(`d`), Replace)
	if err != nil {
		t.Fatal(err)
	}
	if c.Errors[ClassAccessDenied] != 2 || c.Restored != 0 {
		t.Fatalf("counts = %+v, want 2 access_denied", c)
	}
	for name, attr := range map[string]uint32{"ro.txt": windows.FILE_ATTRIBUTE_READONLY, "sys.txt": windows.FILE_ATTRIBUTE_SYSTEM} {
		d := filepath.Join(r.dest, "d", name)
		p, _ := windows.UTF16PtrFromString(d)
		a, _ := windows.GetFileAttributes(p)
		if read(t, d) != "existing" || a&attr == 0 {
			t.Errorf("%s: content %q attrs %#x", name, read(t, d), a)
		}
	}
}

// A file held open by another program cannot be replaced: target_locked, the
// rest is placed.
func TestPlaceReplaceLockedFile(t *testing.T) {
	r := newRig(t)
	d := filepath.Join(r.dest, "d", "locked.txt")
	write(t, d, "existing")
	write(t, filepath.Join(r.stage, "d", "locked.txt"), "restored")
	write(t, filepath.Join(r.stage, "d", "free.txt"), "restored")
	p, _ := windows.UTF16PtrFromString(d)
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	c, err := Place(r.stage, r.dest, sel(`d`), Replace)
	if err != nil {
		t.Fatal(err)
	}
	if c.Errors[ClassTargetLocked] != 1 || c.Restored != 1 {
		t.Fatalf("counts = %+v, want 1 target_locked and 1 restored", c)
	}
}

// The moved file's descriptor: staging's inherited entry is gone, the new
// parent's inheritable entry is present; a protected descriptor is unchanged.
func TestPlaceDescriptorRule(t *testing.T) {
	r := newRig(t)
	user := me(t)
	// Staging hands down Guests read; the destination folder hands down
	// Backup Operators read.
	setSDDL(t, r.stage, "D:P(A;OICI;FA;;;"+user+")(A;OICI;FA;;;SY)(A;OICI;FR;;;BG)")
	mkdirs(t, filepath.Join(r.dest, "d"))
	setSDDL(t, filepath.Join(r.dest, "d"), "D:P(A;OICI;FA;;;"+user+")(A;OICI;FA;;;SY)(A;OICI;FR;;;BO)")
	write(t, filepath.Join(r.stage, "d", "plain.txt"), "p")
	write(t, filepath.Join(r.stage, "d", "prot.txt"), "q")
	setSDDL(t, filepath.Join(r.stage, "d", "prot.txt"), "D:P(A;;FA;;;"+user+")(A;;FR;;;WD)")
	protBefore := sddlOf(t, filepath.Join(r.stage, "d", "prot.txt"))

	c, err := Place(r.stage, r.dest, sel(`d`), Keep)
	if err != nil {
		t.Fatal(err)
	}
	if c.Restored != 2 {
		t.Fatalf("counts = %+v", c)
	}
	plain := sddlOf(t, filepath.Join(r.dest, "d", "plain.txt"))
	if strings.Contains(plain, ";;;BG)") || !strings.Contains(plain, "ID;FR;;;BO)") {
		t.Fatalf("plain.txt descriptor = %s: want staging's BG entry gone and the parent's BO entry inherited", plain)
	}
	if got := sddlOf(t, filepath.Join(r.dest, "d", "prot.txt")); got != protBefore {
		t.Fatalf("protected descriptor changed: %s -> %s", protBefore, got)
	}
}

// Something in staging that no selected path explains is never placed.
func TestPlaceRefusesUnselected(t *testing.T) {
	r := newRig(t)
	write(t, filepath.Join(r.stage, "sel", "a.txt"), "a")
	write(t, filepath.Join(r.stage, "other", "b.txt"), "b")
	c, err := Place(r.stage, r.dest, sel(`sel\a.txt`), Keep)
	if err != nil {
		t.Fatal(err)
	}
	if c.Restored != 1 || c.Errors[ClassGuardRefused] != 1 || exists(filepath.Join(r.dest, "other")) {
		t.Fatalf("counts = %+v", c)
	}
}

// The clean-up walk deletes junctions as links and leaves their target, its
// file and that file's DACL untouched.
func TestRemoveTreeLeavesJunctionTarget(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	tree := filepath.Join(base, "tree")
	write(t, filepath.Join(outside, "victim.txt"), "outside-content")
	write(t, filepath.Join(tree, "x", "r.txt"), "r")
	mkdirs(t, filepath.Join(tree, "x", "deep"))
	junction(t, filepath.Join(tree, "x", "junc"), outside)
	junction(t, filepath.Join(tree, "x", "deep", "junc2"), outside)
	ro := filepath.Join(tree, "x", "ro.txt")
	write(t, ro, "read-only staged copy")
	p, _ := windows.UTF16PtrFromString(ro)
	_ = windows.SetFileAttributes(p, windows.FILE_ATTRIBUTE_READONLY)
	before := sddlOf(t, filepath.Join(outside, "victim.txt"))

	links, err := RemoveTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	if links != 2 || exists(tree) {
		t.Fatalf("links = %d, tree exists = %v", links, exists(tree))
	}
	if read(t, filepath.Join(outside, "victim.txt")) != "outside-content" || sddlOf(t, filepath.Join(outside, "victim.txt")) != before {
		t.Fatal("the junction target was changed")
	}
	// A missing tree is not an error.
	if _, err := RemoveTree(tree); err != nil {
		t.Fatalf("second RemoveTree: %v", err)
	}
}

func TestRemoveReparsePoints(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	write(t, filepath.Join(outside, "victim.txt"), "v")
	tree := filepath.Join(base, "tree")
	write(t, filepath.Join(tree, "a", "f.txt"), "f")
	junction(t, filepath.Join(tree, "a", "j"), outside)
	n, err := RemoveReparsePoints(tree)
	if err != nil || n != 1 {
		t.Fatalf("n = %d err = %v", n, err)
	}
	if exists(filepath.Join(tree, "a", "j")) || !exists(filepath.Join(tree, "a", "f.txt")) || !exists(filepath.Join(outside, "victim.txt")) {
		t.Fatal("wrong entries removed")
	}
}

// The side-folder reset never follows a reparse point: a junction inside the
// side tree pointing outside is deleted, the outside file's DACL is unchanged,
// and every entry inside inherits only from the side folder.
func TestResetInheritNeverFollows(t *testing.T) {
	base := t.TempDir()
	user := me(t)
	o := Options{LabTrustee: user}
	root, err := OpenRoot(filepath.Join(base, "CryoBackup-Restore"), o)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	side := filepath.Join(root.Path(), "01JB0000000000000000000R01")
	if _, err := root.MakeChild(filepath.Base(side)); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	write(t, filepath.Join(outside, "victim.txt"), "v")
	setSDDL(t, filepath.Join(outside, "victim.txt"), "D:P(A;;FA;;;"+user+")(A;;FR;;;WD)")
	victimBefore := sddlOf(t, filepath.Join(outside, "victim.txt"))
	vol := filepath.Join(side, "C")
	write(t, filepath.Join(vol, "Users", "bob", "doc.txt"), "doc")
	// The snapshot's explicit entries, as restic restores them.
	setSDDL(t, filepath.Join(vol, "Users", "bob"), "D:P(A;OICI;FA;;;"+user+")(A;OICI;FA;;;WD)")
	setSDDL(t, filepath.Join(vol, "Users", "bob", "doc.txt"), "D:P(A;;FA;;;"+user+")(A;;FA;;;WD)")
	// A file whose snapshot DACL grants no WRITE_OWNER (its owner only has
	// the implicit READ_CONTROL and WRITE_DAC): the owner is set through the
	// checked second open once the new DACL grants it.
	narrow := filepath.Join(vol, "Users", "bob", "narrow.txt")
	write(t, narrow, "n")
	setSDDL(t, narrow, "D:P(A;;FR;;;WD)")
	junction(t, filepath.Join(vol, "Users", "bob", "junc"), outside)

	check, links, err := ResetInherit(side, o)
	if err != nil {
		t.Fatal(err)
	}
	if links != 1 || exists(filepath.Join(vol, "Users", "bob", "junc")) {
		t.Fatalf("links = %d", links)
	}
	if got := sddlOf(t, filepath.Join(outside, "victim.txt")); got != victimBefore {
		t.Fatalf("the outside file changed: %s -> %s", victimBefore, got)
	}
	if check == "" {
		t.Fatal("no check file")
	}
	// ResetInherit checked one file itself (relative to its held parent); err
	// above is nil only if it passed.
	// D1: every entry got the side owner (here the lab trustee, as a
	// non-elevated test cannot name Administrators) ...
	for _, p := range []string{filepath.Join(vol, "Users"), filepath.Join(vol, "Users", "bob"), filepath.Join(vol, "Users", "bob", "doc.txt"), narrow} {
		sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		if own, _, _ := sd.Owner(); own == nil || own.String() != user {
			t.Errorf("owner of %s = %v, want the side owner", filepath.Base(p), own)
		}
	}
	// ... and the check refuses a file whose owner is not the production side
	// owner (Administrators): this user owns it.
	ba, _ := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	ph, err := openNoFollow(filepath.Dir(check), holdAccess, shareRW)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(ph)
	if err := checkInheritedAt(ph, filepath.Base(check), ba, nil); !errors.Is(err, ErrSideCheck) {
		t.Fatalf("check with the production owner rule = %v, want ErrSideCheck", err)
	}
	doc := sddlOf(t, filepath.Join(vol, "Users", "bob", "doc.txt"))
	if strings.Contains(doc, ";;;WD)") || strings.Contains(doc, "D:P") {
		t.Fatalf("doc.txt still carries the snapshot's entries: %s", doc)
	}
	// Counting the side folder: one file below the selected folder.
	c, err := CountSide(vol, sel(`Users\bob`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Restored != 3 || c.BytesRestored != 4 { // the selected folder itself, doc.txt and narrow.txt
		t.Fatalf("side counts = %+v", c)
	}
}

// The root rule: a created root verifies; one that grants another trustee, is
// not protected, or is a junction is refused and left exactly as it was.
func TestRootRule(t *testing.T) {
	base := t.TempDir()
	user := me(t)
	o := Options{LabTrustee: user}

	good := filepath.Join(base, ".cryobackup-restore")
	r, err := OpenRoot(good, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.MakeChild("id1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.MakeChild("id1"); !errors.Is(err, ErrExists) {
		t.Fatalf("second MakeChild = %v, want ErrExists", err)
	}
	// The held root cannot be renamed.
	if err := os.Rename(good, good+"-x"); err == nil {
		t.Fatal("the held root was renamed")
	}
	r.Close()
	if vr, err := VerifyRoot(good, o); err != nil {
		t.Fatalf("verify of a created root: %v", err)
	} else {
		vr.Close()
	}
	if _, err := VerifyRoot(filepath.Join(base, "absent"), o); !errors.Is(err, ErrNotExist) {
		t.Fatalf("absent root = %v", err)
	}

	for _, tc := range []struct {
		name, sddl, check string
	}{
		{"grants-everyone", "D:P(A;OICI;FA;;;" + user + ")(A;OICI;FR;;;WD)", CheckGrantsOther},
		{"not-protected", "D:(A;OICI;FA;;;" + user + ")", CheckNotProtected},
	} {
		p := filepath.Join(base, tc.name, ".cryobackup-restore")
		mkdirs(t, p)
		setSDDL(t, p, tc.sddl)
		before := sddlOf(t, p)
		_, err := OpenRoot(p, o)
		var re *RootError
		if !errors.As(err, &re) || re.Check != tc.check {
			t.Errorf("%s: err = %v, want check %s", tc.name, err, tc.check)
		}
		if after := sddlOf(t, p); after != before {
			t.Errorf("%s: the root was repaired: %s -> %s", tc.name, before, after)
		}
	}
	// A junction as the root.
	target := filepath.Join(base, "target")
	mkdirs(t, target)
	jp := filepath.Join(base, "j", ".cryobackup-restore")
	mkdirs(t, filepath.Dir(jp))
	junction(t, jp, target)
	var re *RootError
	if _, err := OpenRoot(jp, o); !errors.As(err, &re) || re.Check != CheckSurrogate {
		t.Fatalf("junction root = %v", err)
	}
	// Without the lab trustee (production), a root owned by this user is refused.
	if _, err := VerifyRoot(good, Options{}); !errors.As(err, &re) || re.Check != CheckOwner {
		t.Fatalf("user-owned root without the lab trustee = %v", err)
	}
}

// T3: a too-deep reparse chain (ERROR_CANT_RESOLVE_FILENAME) is a guard
// refusal, like any other name-surrogate failure.
func TestClassifyCantResolveIsGuardRefused(t *testing.T) {
	if got := classify(errCantResolveFilename); got != ClassGuardRefused {
		t.Fatalf("classify(1921) = %s", got)
	}
}

// T4: a placed file whose final path after the move is not the intended one
// is counted under guard_refused and raises an alarm count; its descriptor is
// not opened up.
func TestPlacePostMoveMismatchAlarms(t *testing.T) {
	r := newRig(t)
	write(t, filepath.Join(r.stage, "d", "f.txt"), "restored")
	testHookPostMove = func(string) string { return `\somewhere\else\f.txt` }
	defer func() { testHookPostMove = nil }()
	c, err := Place(r.stage, r.dest, sel(`d\f.txt`), Keep)
	if err != nil {
		t.Fatal(err)
	}
	if c.Alarms != 1 || c.Errors[ClassGuardRefused] != 1 || c.Restored != 0 {
		t.Fatalf("counts = %+v", c)
	}
}
