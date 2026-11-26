//go:build windows

// The blind verifier's attack harness of P22-D fix batch 2 (H1, M1), shipped
// as regression tests (fix batch 3, T1). Each attack was run against the code
// before the fix (ff2af2c) and broke it: the in-place flip with a created
// folder put 45 entries in the junction target, the flip of an agent-created
// folder 6, the delete-then-flip deleted 8556 outside files through the
// clean-up walk and changed 1145 outside DACLs through the side reset, and a
// planted hard link stayed in the side folder (log
// C:\dev\cryo-wt\_view\p22\d-fix4\old-ff2af2c.log on the build PC). The
// attack on a plain file under the flipped folder, which the old code already
// survived, is not kept.
//
// Attacker model: an ordinary user who controls a folder on the path of the
// agent (an empty folder they own, or one the agent created that inherits
// their rights, or a restored side-folder folder whose snapshot DACL grants
// them). Every test counts entries created, changed or deleted in an outside
// target, and each must count 0.

package restoreplace

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	v2Trials  = 300
	v2Threads = 4
	v2Names   = 150 // files per folder in the delete-then-flip attacks
	v2WalkTr  = 100 // trials of the delete-then-flip attacks
)

func v2MountBuf(target string) []byte {
	sub := utf16.Encode([]rune(`\??\` + target))
	prn := utf16.Encode([]rune(target))
	pathBytes := (len(sub) + 1 + len(prn) + 1) * 2
	dataLen := 8 + pathBytes
	buf := make([]byte, 8+dataLen)
	*(*uint32)(unsafe.Pointer(&buf[0])) = windows.IO_REPARSE_TAG_MOUNT_POINT
	*(*uint16)(unsafe.Pointer(&buf[4])) = uint16(dataLen)
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

// v2Flip sets (set) or deletes a junction to target on the folder dir, in
// place. It reports success.
func v2Flip(dir, target string, set bool) bool {
	p, _ := windows.UTF16PtrFromString(dir)
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, 7, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var n uint32
	if set {
		b := v2MountBuf(target)
		return windows.DeviceIoControl(h, windows.FSCTL_SET_REPARSE_POINT, &b[0], uint32(len(b)), nil, 0, &n, nil) == nil
	}
	hdr := make([]byte, 8)
	*(*uint32)(unsafe.Pointer(&hdr[0])) = windows.IO_REPARSE_TAG_MOUNT_POINT
	return windows.DeviceIoControl(h, windows.FSCTL_DELETE_REPARSE_POINT, &hdr[0], uint32(len(hdr)), nil, 0, &n, nil) == nil
}

// v2OpenAt is the relative open of the attacker (never follows, never by path).
func v2OpenAt(dir windows.Handle, name string, access, disp, opts uint32) (windows.Handle, error) {
	us, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: dir, ObjectName: us, Attributes: windows.OBJ_CASE_INSENSITIVE}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var iosb windows.IO_STATUS_BLOCK
	var h windows.Handle
	err = windows.NtCreateFile(&h, access|windows.SYNCHRONIZE, &oa, &iosb, nil, 0, 7, disp,
		opts|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	return h, err
}

func v2ExistsAt(dir windows.Handle, name string) bool {
	h, err := v2OpenAt(dir, name, windows.FILE_READ_ATTRIBUTES, windows.FILE_OPEN, 0)
	if err != nil {
		return false
	}
	windows.CloseHandle(h)
	return true
}

func v2DeleteAt(dir windows.Handle, name string) {
	h, err := v2OpenAt(dir, name, windows.DELETE|windows.FILE_READ_ATTRIBUTES, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE)
	if err != nil {
		return
	}
	f := uint32(windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS)
	_ = windows.SetFileInformationByHandle(h, windows.FileDispositionInfoEx, (*byte)(unsafe.Pointer(&f)), 4)
	windows.CloseHandle(h)
}

func v2OpenDir(p string) windows.Handle {
	u, _ := windows.UTF16PtrFromString(p)
	h, err := windows.CreateFile(u, windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES, 7, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return windows.InvalidHandle
	}
	return h
}

func v2SDAt(dir windows.Handle, name string) string {
	h, err := v2OpenAt(dir, name, windows.READ_CONTROL, windows.FILE_OPEN, 0)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return ""
	}
	return sd.String()
}

// v2Count is the number of entries anywhere below dir (plain folders only).
func v2Count(dir string) (int, []string) {
	n := 0
	var names []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && p != dir {
			n++
			if len(names) < 5 {
				r, _ := filepath.Rel(dir, p)
				names = append(names, r)
			}
		}
		return nil
	})
	return n, names
}

func v2Clear(dir string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

func vname(k int) string { return "n" + strconv.Itoa(1000 + k)[1:] }

// v2PlaceCase: what is flipped and where the staged item goes.
type v2PlaceCase struct {
	name    string
	rel     func(a string) []string
	flipDir func(dest, a string) string
	setOnly int // how many of the threads only set (never delete) the junction
}

func v2RunPlace(t *testing.T, pc v2PlaceCase) {
	base := t.TempDir()
	dest := filepath.Join(base, "dest")
	target := filepath.Join(base, "target")
	mkdirs(t, dest)
	mkdirs(t, target)
	var cur atomic.Int64
	cur.Store(-1)
	var stop atomic.Bool
	var flips atomic.Int64
	var wg sync.WaitGroup
	for th := 0; th < v2Threads; th++ {
		wg.Add(1)
		go func(th int) {
			defer wg.Done()
			k := th
			for !stop.Load() {
				i := cur.Load()
				if i < 0 {
					runtime.Gosched()
					continue
				}
				d := pc.flipDir(dest, "a"+strconv.FormatInt(i, 10))
				set := k%2 == 0 || th < pc.setOnly
				if v2Flip(d, target, set) {
					flips.Add(1)
				}
				k++
			}
		}(th)
	}
	placed, refused, other, escTrials, escTotal := 0, 0, 0, 0, 0
	var sample []string
	for i := 0; i < v2Trials; i++ {
		a := "a" + strconv.Itoa(i)
		mkdirs(t, filepath.Join(dest, a))
		rel := pc.rel(a)
		stage := filepath.Join(base, "stage", strconv.Itoa(i))
		write(t, filepath.Join(append([]string{stage}, rel...)...), "restored")
		cur.Store(int64(i))
		c, err := Place(stage, dest, []Rel{Rel(rel)}, Keep)
		cur.Store(-1)
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
		time.Sleep(200 * time.Microsecond) // let an in-flight flip finish before counting
		if n, names := v2Count(target); n > 0 {
			escTrials++
			escTotal += n
			if sample == nil {
				sample = names
			}
			v2Clear(target)
		}
	}
	stop.Store(true)
	wg.Wait()
	for i := 0; i < v2Trials; i++ {
		a := "a" + strconv.Itoa(i)
		v2Flip(pc.flipDir(dest, a), target, false)
		v2Flip(filepath.Join(dest, a), target, false)
	}
	n, _ := v2Count(target)
	escTotal += n
	t.Logf("V2 %s: trials=%d threads=%d flips=%d placed=%d guard_refused=%d other=%d escaped_trials=%d target_entries_total=%d sample=%v",
		pc.name, v2Trials, v2Threads, flips.Load(), placed, refused, other, escTrials, escTotal, sample)
	if escTotal != 0 {
		t.Errorf("FINDING %s: %d entries reached the junction target in %d of %d trials", pc.name, escTotal, escTrials, v2Trials)
	}
}

func TestV2FlipPlaceCreate(t *testing.T) {
	v2RunPlace(t, v2PlaceCase{name: "place-create(flip a)",
		rel:     func(a string) []string { return []string{a, "new", "f.txt"} },
		flipDir: func(dest, a string) string { return filepath.Join(dest, a) }})
}

// The folder the agent itself creates (inheriting the rights of the user) is
// flipped as soon as it appears, while the agent creates a folder and places a
// file below it.
func TestV2FlipAgentCreated(t *testing.T) {
	v2RunPlace(t, v2PlaceCase{name: "place-under-agent-created(flip a/new)",
		rel:     func(a string) []string { return []string{a, "new", "sub", "f.txt"} },
		flipDir: func(dest, a string) string { return filepath.Join(dest, a, "new") },
		setOnly: 3})
}

// delete-then-flip: once the walk has listed folder x and started on its
// entries, the attacker empties x (relative to its own handle) and turns it
// into a junction to target, which holds files of the same names. A walk that
// reopens the remaining names by full path goes through the junction.
func v2RunWalk(t *testing.T, mode string, walk func(top string) error) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	mkdirs(t, target)
	victimSDDL := "D:P(A;;FA;;;" + me(t) + ")(A;;FR;;;WD)"
	for k := 0; k < v2Names; k++ {
		write(t, filepath.Join(target, vname(k)), "victim")
		setSDDL(t, filepath.Join(target, vname(k)), victimSDDL)
	}
	wantSD := sddlOf(t, filepath.Join(target, vname(0)))
	markSDDL := "D:P(A;;FA;;;" + me(t) + ")"
	deletedTotal, changedTotal, escTrials, flipped, walkErrs := 0, 0, 0, 0, 0
	for i := 0; i < v2WalkTr; i++ {
		top := filepath.Join(base, mode, strconv.Itoa(i))
		x := filepath.Join(top, "C", "x")
		mkdirs(t, x)
		for k := 0; k < v2Names; k++ {
			write(t, filepath.Join(x, vname(k)), "s")
		}
		setSDDL(t, filepath.Join(x, vname(0)), markSDDL)
		mark := sddlOf(t, filepath.Join(x, vname(0)))
		hx := v2OpenDir(x)
		var phase atomic.Int32
		var ok atomic.Bool
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { // the deleter
			defer wg.Done()
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if !v2ExistsAt(hx, vname(0)) || (mode == "reset" && v2SDAt(hx, vname(0)) != mark) {
					break
				}
			}
			for k := v2Names - 1; k >= 0; k-- {
				v2DeleteAt(hx, vname(k))
			}
			phase.Store(1)
		}()
		for th := 0; th < v2Threads; th++ {
			wg.Add(1)
			go func() { // flippers: only ever set, once x is empty
				defer wg.Done()
				deadline := time.Now().Add(3 * time.Second)
				for phase.Load() == 0 && time.Now().Before(deadline) {
					runtime.Gosched()
				}
				for !ok.Load() && time.Now().Before(deadline) {
					if v2Flip(x, target, true) {
						ok.Store(true)
					}
				}
			}()
		}
		if err := walk(top); err != nil {
			walkErrs++
		}
		wg.Wait()
		windows.CloseHandle(hx)
		if ok.Load() {
			flipped++
		}
		deleted, changed := 0, 0
		for k := 0; k < v2Names; k++ {
			p := filepath.Join(target, vname(k))
			if !exists(p) {
				deleted++
				write(t, p, "victim")
				setSDDL(t, p, victimSDDL)
				continue
			}
			if read(t, p) != "victim" || sddlOf(t, p) != wantSD {
				changed++
				setSDDL(t, p, victimSDDL)
				_ = os.WriteFile(p, []byte("victim"), 0o644)
			}
		}
		if deleted+changed > 0 {
			escTrials++
		}
		deletedTotal += deleted
		changedTotal += changed
		v2Flip(x, target, false)
		_ = os.RemoveAll(top)
	}
	if n, _ := v2Count(target); n != v2Names {
		t.Errorf("target holds %d entries, want %d", n, v2Names)
	}
	t.Logf("V2 walk %s: trials=%d names=%d flipped_trials=%d walk_errors=%d escaped_trials=%d victims_deleted=%d victims_changed=%d",
		mode, v2WalkTr, v2Names, flipped, walkErrs, escTrials, deletedTotal, changedTotal)
	if deletedTotal+changedTotal != 0 {
		t.Errorf("FINDING walk %s: %d outside files deleted, %d changed, in %d of %d trials", mode, deletedTotal, changedTotal, escTrials, v2WalkTr)
	}
}

func TestV2WalkCleanup(t *testing.T) {
	v2RunWalk(t, "cleanup", func(top string) error { _, err := RemoveTree(top); return err })
}

func TestV2WalkReset(t *testing.T) {
	o := Options{LabTrustee: me(t)}
	v2RunWalk(t, "reset", func(top string) error { _, _, err := ResetInherit(top, o); return err })
}

// M1: a hard link planted in the side folder to an outside file.
func TestV2SideHardLink(t *testing.T) {
	base := t.TempDir()
	top := filepath.Join(base, "side", "01JB0000000000000000000R01")
	mkdirs(t, filepath.Join(top, "C", "Users", "u"))
	outside := filepath.Join(base, "outside.txt")
	write(t, outside, "outside")
	setSDDL(t, outside, "D:P(A;;FA;;;"+me(t)+")(A;;FR;;;WD)")
	before := sddlOf(t, outside)
	o1, _ := windows.GetNamedSecurityInfo(outside, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	link := filepath.Join(top, "C", "Users", "u", "planted.txt")
	if err := os.Link(outside, link); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(top, "C", "Users", "u", "real.txt"), "r")
	check, links, err := ResetInherit(top, Options{LabTrustee: me(t)})
	o2, _ := windows.GetNamedSecurityInfo(outside, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	after := sddlOf(t, outside)
	t.Logf("V2 side hard link: err=%v links=%d check=%q link_exists=%v outside_content=%q sddl_before=%s sddl_after=%s owner_before=%s owner_after=%s",
		err, links, filepath.Base(check), exists(link), read(t, outside), before, after, o1.String(), o2.String())
	if after != before || read(t, outside) != "outside" || o1.String() != o2.String() {
		t.Errorf("FINDING: the outside file changed through a planted hard link")
	}
	if exists(link) {
		t.Errorf("FINDING: the planted hard link is still in the side folder")
	}
}

// Replace mode over a destination file that is a hard link to an outside file.
func TestV2ReplaceOverHardLink(t *testing.T) {
	base := t.TempDir()
	dest := filepath.Join(base, "dest")
	mkdirs(t, filepath.Join(dest, "a"))
	outside := filepath.Join(base, "outside.txt")
	write(t, outside, "outside-content")
	setSDDL(t, outside, "D:P(A;;FA;;;"+me(t)+")(A;;FR;;;WD)")
	before := sddlOf(t, outside)
	if err := os.Link(outside, filepath.Join(dest, "a", "h.txt")); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(base, "stage")
	write(t, filepath.Join(stage, "a", "h.txt"), "restored")
	c, err := Place(stage, dest, []Rel{Rel([]string{"a", "h.txt"})}, Replace)
	t.Logf("V2 replace over hard link: err=%v counts=%+v dest=%q outside=%q sddl_same=%v",
		err, c, read(t, filepath.Join(dest, "a", "h.txt")), read(t, outside), sddlOf(t, outside) == before)
	if read(t, outside) != "outside-content" || sddlOf(t, outside) != before {
		t.Errorf("FINDING: the outside file changed")
	}
	if c.Restored != 1 || read(t, filepath.Join(dest, "a", "h.txt")) != "restored" {
		t.Errorf("replace over a hard link no longer restores (behaviour change)")
	}
}
