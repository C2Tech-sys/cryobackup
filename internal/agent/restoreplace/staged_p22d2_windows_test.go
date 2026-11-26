//go:build windows

package restoreplace

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// P22-D2 (security review follow-up): placement refuses hard-linked and
// non-regular staged entries.

func ownerOf(t *testing.T, p string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}

// S1: a staged file that is a hard link to a file outside is never placed,
// and the outside file keeps its content, owner and DACL.
func TestPlaceRefusesHardLinkedStagedFile(t *testing.T) {
	r := newRig(t)
	outside := filepath.Join(r.base, "outside.txt")
	write(t, outside, "outside")
	setSDDL(t, outside, "D:P(A;;FA;;;"+me(t)+")(A;;FR;;;WD)")
	sdBefore, ownerBefore := sddlOf(t, outside), ownerOf(t, outside)
	mkdirs(t, filepath.Join(r.stage, "d"))
	if err := os.Link(outside, filepath.Join(r.stage, "d", "f.txt")); err != nil {
		t.Fatal(err)
	}
	mkdirs(t, filepath.Join(r.dest, "d"))
	setSDDL(t, filepath.Join(r.dest, "d"), "D:P(A;OICI;FA;;;"+me(t)+")(A;OICI;FR;;;BO)")
	c, err := Place(r.stage, r.dest, sel(`d\f.txt`), Keep)
	if err != nil {
		t.Fatal(err)
	}
	if c.Restored != 0 || c.Errors[ClassGuardRefused] != 1 || exists(filepath.Join(r.dest, "d", "f.txt")) {
		t.Fatalf("a hard-linked staged file was placed: counts=%+v dest exists=%v", c, exists(filepath.Join(r.dest, "d", "f.txt")))
	}
	if read(t, outside) != "outside" || sddlOf(t, outside) != sdBefore || ownerOf(t, outside) != ownerBefore {
		t.Fatalf("the outside file changed: %s -> %s", sdBefore, sddlOf(t, outside))
	}
}

// S1: a hard link added between the move and the descriptor rewrite is caught
// by the re-check through the handle: no rewrite, guard_refused, alarm.
func TestPlaceCatchesLinkAddedBeforeFix(t *testing.T) {
	r := newRig(t)
	write(t, filepath.Join(r.stage, "d", "f.txt"), "restored")
	mkdirs(t, filepath.Join(r.dest, "d"))
	setSDDL(t, filepath.Join(r.dest, "d"), "D:P(A;OICI;FA;;;"+me(t)+")(A;OICI;FR;;;BO)")
	outside := filepath.Join(r.base, "linked-outside.txt")
	var sdAtLink string
	testHookBeforeFix = func(dest string) {
		if err := os.Link(dest, outside); err == nil {
			sdAtLink = sddlOf(t, outside)
		}
	}
	defer func() { testHookBeforeFix = nil }()
	c, err := Place(r.stage, r.dest, sel(`d\f.txt`), Keep)
	if err != nil {
		t.Fatal(err)
	}
	if sdAtLink == "" {
		t.Fatal("the hook could not add the link")
	}
	if c.Restored != 0 || c.Errors[ClassGuardRefused] != 1 || c.Alarms != 1 {
		t.Fatalf("counts = %+v, want guard_refused 1 and an alarm", c)
	}
	if got := sddlOf(t, outside); got != sdAtLink {
		t.Fatalf("the descriptor rewrite reached the other link: %s -> %s", sdAtLink, got)
	}
}

// S2: a directory swapped in at a staged file's name is refused, never moved.
func TestPlaceRefusesDirectorySwappedInAtFileName(t *testing.T) {
	r := newRig(t)
	write(t, filepath.Join(r.stage, "d", "f.txt"), "restored")
	mkdirs(t, filepath.Join(r.dest, "d"))
	testHookBeforeFileOpen = func(staged string) {
		_ = os.Remove(staged)
		mkdirs(t, staged)
		write(t, filepath.Join(staged, "inner.txt"), "swapped")
	}
	defer func() { testHookBeforeFileOpen = nil }()
	c, err := Place(r.stage, r.dest, sel(`d\f.txt`), Keep)
	if err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(r.dest, "d", "f.txt")) || c.Restored != 0 || c.Errors[ClassGuardRefused] != 1 {
		t.Fatalf("a directory at a file name was placed or not refused: counts=%+v dest exists=%v", c, exists(filepath.Join(r.dest, "d", "f.txt")))
	}
}
