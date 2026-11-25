//go:build windows

package restoreplace

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The flip attack of the P22-D blind check (fix batch 2, H1), kept as a
// shipped regression test. An ordinary user who owns a folder on the
// destination path flips it, as fast as it can, between a plain folder and a
// junction to a folder outside the destination: in place (a reparse point set
// on and deleted from the very folder the agent holds) and by swapping names
// (a real folder and a junction renamed into the name in turn). Every
// placement must land inside the destination or be refused: 0 files placed
// and 0 folders created in the junction target. The same attack runs against
// the clean-up walk and the side-folder reset, which must never touch the
// target.

const flipTrials = 300

// mountPointBuffer is a REPARSE_DATA_BUFFER for IO_REPARSE_TAG_MOUNT_POINT
// that points at target (a junction).
func mountPointBuffer(target string) []byte {
	sub := utf16.Encode([]rune(`\??\` + target))
	prn := utf16.Encode([]rune(target))
	// MountPointReparseBuffer: SubstituteNameOffset, SubstituteNameLength,
	// PrintNameOffset, PrintNameLength (uint16 each), then PathBuffer with
	// both names, each NUL-terminated.
	pathBytes := (len(sub) + 1 + len(prn) + 1) * 2
	dataLen := 8 + pathBytes
	buf := make([]byte, 8+dataLen)
	*(*uint32)(unsafe.Pointer(&buf[0])) = windows.IO_REPARSE_TAG_MOUNT_POINT
	*(*uint16)(unsafe.Pointer(&buf[4])) = uint16(dataLen)
	*(*uint16)(unsafe.Pointer(&buf[8])) = 0
	*(*uint16)(unsafe.Pointer(&buf[10])) = uint16(len(sub) * 2)
	*(*uint16)(unsafe.Pointer(&buf[12])) = uint16((len(sub) + 1) * 2)
	*(*uint16)(unsafe.Pointer(&buf[14])) = uint16(len(prn) * 2)
	off := 16
	for _, c := range sub {
		*(*uint16)(unsafe.Pointer(&buf[off])) = c
		off += 2
	}
	off += 2
	for _, c := range prn {
		*(*uint16)(unsafe.Pointer(&buf[off])) = c
		off += 2
	}
	return buf
}

// flipInPlace sets or deletes a junction on the existing, empty folder dir.
func flipInPlace(dir, target string, set bool) {
	p, _ := windows.UTF16PtrFromString(dir)
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	var n uint32
	if set {
		b := mountPointBuffer(target)
		_ = windows.DeviceIoControl(h, windows.FSCTL_SET_REPARSE_POINT, &b[0], uint32(len(b)), nil, 0, &n, nil)
		return
	}
	hdr := make([]byte, 8)
	*(*uint32)(unsafe.Pointer(&hdr[0])) = windows.IO_REPARSE_TAG_MOUNT_POINT
	_ = windows.DeviceIoControl(h, windows.FSCTL_DELETE_REPARSE_POINT, &hdr[0], uint32(len(hdr)), nil, 0, &n, nil)
}

// flipper runs attack in a loop until stop is closed.
func flipper(stop chan struct{}, attack func(i int)) *sync.WaitGroup {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			attack(i)
		}
	}()
	return &wg
}

// entries is the number of entries anywhere below dir.
func entries(dir string) int {
	n := 0
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && p != dir {
			n++
		}
		return nil
	})
	return n
}

// flipRig is a destination with the attacked folder "a", a target outside
// the destination, and a fresh staging folder per trial.
type flipRig struct {
	base, dest, target string
}

func newFlipRig(t *testing.T) flipRig {
	base := t.TempDir()
	r := flipRig{base: base, dest: filepath.Join(base, "dest"), target: filepath.Join(base, "target")}
	mkdirs(t, filepath.Join(r.dest, "a"))
	mkdirs(t, r.target)
	return r
}

// TestFlipAttackPlacement: placements under a folder that is flipped in place
// between a plain folder and a junction (H1). Files and created folders must
// never reach the target.
func TestFlipAttackPlacement(t *testing.T) {
	for _, create := range []bool{false, true} {
		r := newFlipRig(t)
		a := filepath.Join(r.dest, "a")
		stop := make(chan struct{})
		var flips atomic.Int64
		wg := flipper(stop, func(i int) {
			flipInPlace(a, r.target, i%2 == 0)
			flips.Add(1)
		})
		placed, refused, other := 0, 0, 0
		for i := 0; i < flipTrials; i++ {
			stage := filepath.Join(r.base, "stage"+strconv.Itoa(i))
			rel := []string{"a", "f" + strconv.Itoa(i) + ".txt"}
			if create {
				rel = []string{"a", "new" + strconv.Itoa(i), "f.txt"}
			}
			write(t, filepath.Join(append([]string{stage}, rel...)...), "restored")
			c, err := Place(stage, r.dest, []Rel{Rel(rel)}, Keep)
			switch {
			case err != nil:
				other++
			case c.Restored > 0:
				placed++
			case c.Errors[ClassGuardRefused] > 0:
				refused++
			default:
				other++
			}
			if !create {
				// The placed file blocks the in-place flip (a junction needs
				// an empty folder); move it out of the way for the next trial.
				_ = os.Remove(filepath.Join(a, rel[1]))
			} else {
				_ = os.Remove(filepath.Join(a, rel[1], rel[2]))
				_ = os.Remove(filepath.Join(a, rel[1]))
			}
		}
		close(stop)
		wg.Wait()
		flipInPlace(a, r.target, false)
		escaped := entries(r.target)
		t.Logf("FLIP in place (create=%v): trials=%d flips=%d placed_in_destination=%d guard_refused=%d other=%d entries_in_target=%d",
			create, flipTrials, flips.Load(), placed, refused, other, escaped)
		if escaped != 0 {
			t.Errorf("create=%v: %d entries reached the junction target", create, escaped)
		}
	}
}

// TestFlipAttackSwap: the folder name is swapped between a real folder and a
// junction by renames while placements run.
func TestFlipAttackSwap(t *testing.T) {
	r := newFlipRig(t)
	a := filepath.Join(r.dest, "a")
	real := filepath.Join(r.dest, "a-real")
	link := filepath.Join(r.dest, "a-link")
	_ = os.Rename(a, real)
	mkdirs(t, link)
	flipInPlace(link, r.target, true)
	stop := make(chan struct{})
	wg := flipper(stop, func(i int) {
		if i%2 == 0 {
			if os.Rename(real, a) == nil {
				time.Sleep(time.Microsecond)
				_ = os.Rename(a, real)
			}
		} else if os.Rename(link, a) == nil {
			time.Sleep(time.Microsecond)
			_ = os.Rename(a, link)
		}
	})
	placed, refused := 0, 0
	for i := 0; i < flipTrials; i++ {
		stage := filepath.Join(r.base, "stage"+strconv.Itoa(i))
		rel := []string{"a", "new" + strconv.Itoa(i), "f.txt"}
		write(t, filepath.Join(append([]string{stage}, rel...)...), "restored")
		c, _ := Place(stage, r.dest, []Rel{Rel(rel)}, Keep)
		placed += int(c.Restored)
		refused += int(c.Errors[ClassGuardRefused])
	}
	close(stop)
	wg.Wait()
	escaped := entries(r.target)
	t.Logf("FLIP swap: trials=%d items_restored=%d guard_refused=%d entries_in_target=%d", flipTrials, placed, refused, escaped)
	if escaped != 0 {
		t.Errorf("%d entries reached the junction target", escaped)
	}
}

// TestFlipAttackCleanupAndReset: the clean-up walk and the side reset run over
// a tree whose folder is flipped in place; the target keeps its entries and
// its file's DACL.
func TestFlipAttackCleanupAndReset(t *testing.T) {
	for _, mode := range []string{"cleanup", "reset"} {
		base := t.TempDir()
		target := filepath.Join(base, "target")
		write(t, filepath.Join(target, "victim.txt"), "outside")
		victimSD := sddlOf(t, filepath.Join(target, "victim.txt"))
		o := Options{LabTrustee: me(t)}
		root, err := OpenRoot(filepath.Join(base, "CryoBackup-Restore"), o)
		if err != nil {
			t.Fatal(err)
		}
		removed := 0
		for i := 0; i < flipTrials/3; i++ {
			id := "01JB00000000000000000" + strconv.Itoa(10000+i)
			top := filepath.Join(root.Path(), id)
			if _, err := root.MakeChild(id); err != nil {
				t.Fatal(err)
			}
			mkdirs(t, filepath.Join(top, "C", "x"))
			write(t, filepath.Join(top, "C", "f.txt"), "f")
			flipDir := filepath.Join(top, "C", "x")
			stop := make(chan struct{})
			wg := flipper(stop, func(j int) { flipInPlace(flipDir, target, j%2 == 0) })
			if mode == "cleanup" {
				if _, err := root.RemoveChild(id); err == nil {
					removed++
				}
			} else {
				_, _, _ = ResetInherit(top, o)
			}
			close(stop)
			wg.Wait()
			flipInPlace(flipDir, target, false)
		}
		root.Close()
		got := entries(target)
		t.Logf("FLIP %s: trials=%d removed=%d entries_in_target=%d (want 1, the victim)", mode, flipTrials/3, removed, got)
		if got != 1 || read(t, filepath.Join(target, "victim.txt")) != "outside" || sddlOf(t, filepath.Join(target, "victim.txt")) != victimSD {
			t.Errorf("%s: the target changed: entries=%d", mode, got)
		}
	}
}

// M1: a hard link planted in the side folder to a file outside it is removed
// by the reset, never reset or re-owned: the outside file keeps its content,
// owner and DACL.
func TestResetRemovesHardLinks(t *testing.T) {
	base := t.TempDir()
	o := Options{LabTrustee: me(t)}
	root, err := OpenRoot(filepath.Join(base, "CryoBackup-Restore"), o)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	top, err := root.MakeChild("01JB0000000000000000000R01")
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside.txt")
	write(t, outside, "outside")
	setSDDL(t, outside, "D:P(A;;FA;;;"+me(t)+")(A;;FR;;;WD)")
	before := sddlOf(t, outside)
	beforeOwner, _ := windows.GetNamedSecurityInfo(outside, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	mkdirs(t, filepath.Join(top, "C"))
	link := filepath.Join(top, "C", "planted.txt")
	if err := os.Link(outside, link); err != nil {
		t.Fatalf("hard link: %v", err)
	}
	write(t, filepath.Join(top, "C", "real.txt"), "r")
	_, links, err := ResetInherit(top, o)
	if err != nil {
		t.Fatal(err)
	}
	afterOwner, _ := windows.GetNamedSecurityInfo(outside, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if links != 1 || exists(link) {
		t.Fatalf("links = %d, planted link exists = %v", links, exists(link))
	}
	if read(t, outside) != "outside" || sddlOf(t, outside) != before || afterOwner.String() != beforeOwner.String() {
		t.Fatalf("the outside file changed: %s -> %s", before, sddlOf(t, outside))
	}
	if !exists(filepath.Join(top, "C", "real.txt")) {
		t.Fatal("the real file was removed")
	}
}
