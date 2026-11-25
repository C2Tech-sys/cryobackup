//go:build windows

package restoreplace

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

var errNotRealDir = errors.New("restoreplace: not a real folder")

// testHookBeforeMove, when a test sets it, runs between the destination
// checks and the parent re-read of each file move, with the destination path:
// the window a race would use.
var testHookBeforeMove func(dest string)

// Place moves the files restic restored into staging to their original
// location under driveRoot (ADR 0027, "Original location: restore into
// staging, then place", step 4). staging is the restore's fresh staging folder
// (<L>:\.cryobackup-restore\<restore id>), whose tree mirrors the drive below
// the drive root; driveRoot is "<L>:\"; sel are the selected paths below the
// drive root. Reparse points must already have been removed from staging
// (RemoveReparsePoints); one met here is skipped and left for the clean-up
// walk.
//
// The rules, each where it is applied:
//
//   - the destination is checked from the drive root down, every component
//     opened by the agent itself without following it, its tag read through
//     the handle: a name surrogate anywhere refuses the items below it
//     (guard_refused), a reparse tag that is not a name surrogate is an
//     ordinary folder (openDestDir);
//   - every checked folder is held, with a data right and without
//     FILE_SHARE_DELETE, until the items below it are placed (the recursion
//     keeps the handle of every ancestor open);
//   - a folder that does not exist is created by the agent and inherits from
//     its parent, then opened and held like any other; a folder that exists is
//     never changed (openDestDir);
//   - names are compared exactly: the real long name read through the handle
//     must equal the expected name (checkName);
//   - one handle per placed file: opened once, renamed through it relative to
//     the held parent with FileRenameInfoEx, descriptor applied through it
//     (placeFile);
//   - immediately before each move the parent's tag is read again through its
//     held handle (placeFile);
//   - a file that does not exist is moved in with a no-replace rename; a lost
//     race is skipped and counted, never overwritten (foreman ruling,
//     2026-11-25: counted in files_skipped);
//   - an existing file is skipped in keep mode; in replace mode only a checked
//     regular file is replaced, a read-only or system one is access_denied;
//   - a file where a folder is, or a folder where a file is, is guard_refused
//     in any mode;
//   - the descriptor of a moved file: unless the snapshot's DACL is
//     protected, its explicit entries are applied again with
//     UNPROTECTED_DACL_SECURITY_INFORMATION through the file's handle
//     (fixDescriptor).
//
// The returned error is a failure of the walk itself (staging or the drive
// root could not be read); everything per item is in Counts.
func Place(staging, driveRoot string, sel []Rel, mode Mode) (Counts, error) {
	var c Counts
	sh, err := openNoFollow(staging, holdAccess, shareRW)
	if err != nil {
		return c, err
	}
	defer windows.CloseHandle(sh)
	if ti, err := tagOf(sh); err != nil || ti.reparse() || !ti.dir() {
		return c, errNotRealDir
	}
	dh, err := openNoFollow(driveRoot, holdAccess, shareRW)
	if err != nil {
		return c, err
	}
	defer windows.CloseHandle(dh)
	if ti, err := tagOf(dh); err != nil || ti.surrogate() || !ti.dir() {
		return c, errNotRealDir
	}
	p := &placer{sel: sel, mode: mode, c: &c}
	return c, p.dir(sh, staging, dh, driveRoot, nil)
}

type placer struct {
	sel  []Rel
	mode Mode
	c    *Counts
}

// dir places the entries of one staged folder (held as sh) into the
// destination folder held as dh. rel is the folder's path below the drive
// root.
func (p *placer) dir(sh windows.Handle, sPath string, dh windows.Handle, dPath string, rel []string) error {
	ents, err := listDir(sh)
	if err != nil {
		return err
	}
	for _, e := range ents {
		childRel := append(append([]string(nil), rel...), e.name)
		r, selected := relate(childRel, p.sel)
		sChild, dChild := join(sPath, e.name), join(dPath, e.name)
		if r == relNone {
			// restic restored something no selected path explains: the agent
			// writes only at the original location of a selected item.
			p.c.Add(ClassGuardRefused, countItems(sChild))
			continue
		}
		if e.attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0 && e.attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
			if err := p.subdir(sChild, dh, dChild, childRel, selected); err != nil {
				return err
			}
			continue
		}
		if r != relUnder {
			// A file above every selected path cannot be a selected item.
			p.c.Add(ClassGuardRefused, 1)
			continue
		}
		p.placeFile(sChild, dh, dChild, e.name)
	}
	return nil
}

// subdir handles one staged folder: open (or create) and hold its destination
// counterpart, then recurse.
func (p *placer) subdir(sChild string, dh windows.Handle, dChild string, rel []string, selected bool) error {
	sh, err := openNoFollow(sChild, holdAccess, shareRW)
	if err != nil {
		p.c.Add(ClassOther, countItems(sChild))
		return nil
	}
	defer windows.CloseHandle(sh)
	if ti, err := tagOf(sh); err != nil || ti.reparse() || !ti.dir() {
		p.c.Skipped++ // a link restic made: left for the clean-up walk
		return nil
	}
	ch, created, cls := openDestDir(dh, dChild, rel[len(rel)-1])
	if cls != "" {
		p.c.Add(cls, countItems(sChild))
		return nil
	}
	defer windows.CloseHandle(ch)
	if selected {
		// The selected folder itself is an item: restored when the agent made
		// it, skipped when it was already there.
		if created {
			p.c.Restored++
		} else {
			p.c.Skipped++
		}
	}
	return p.dir(sh, sChild, ch, dChild, rel)
}

// openDestDir opens the destination folder dPath (whose parent is held as
// dh) without following it and holds it, creating it first when it does not
// exist. It returns a class when the items below must not be placed.
func openDestDir(dh windows.Handle, dPath, name string) (h windows.Handle, created bool, cls Class) {
	if ti, err := tagOf(dh); err != nil || ti.surrogate() {
		return windows.InvalidHandle, false, ClassGuardRefused
	}
	h, err := openNoFollow(dPath, holdAccess, shareRW)
	if notFound(err) {
		p, perr := u16(longPath(dPath))
		if perr != nil {
			return windows.InvalidHandle, false, ClassOther
		}
		// No security attributes: the new folder inherits from its parent
		// (ADR 0027: recreated folders get their parent's permissions). If
		// something appeared at the name meanwhile, creation fails and the
		// existing item is checked like any other.
		cerr := windows.CreateDirectory(p, nil)
		if cerr == nil {
			created = true
		} else if !errors.Is(cerr, windows.ERROR_ALREADY_EXISTS) {
			return windows.InvalidHandle, false, classify(cerr)
		}
		h, err = openNoFollow(dPath, holdAccess, shareRW)
	}
	if err != nil {
		return windows.InvalidHandle, false, classify(err)
	}
	ti, err := tagOf(h)
	switch {
	case err != nil:
		cls = ClassOther
	case ti.surrogate():
		cls = ClassGuardRefused // a junction, a symbolic link or a mount point on the path
	case !ti.dir():
		cls = ClassGuardRefused // a file where a folder is
	case !checkName(h, name):
		cls = ClassGuardRefused // an 8.3 or otherwise inexact match
	}
	if cls != "" {
		_ = windows.CloseHandle(h)
		return windows.InvalidHandle, false, cls
	}
	return h, created, ""
}

// checkName compares the real long name read through h with want, exactly.
func checkName(h windows.Handle, want string) bool {
	got, err := finalName(h)
	return err == nil && got == want
}

// stagedAccess is how a staged file is opened, once: DELETE to rename it,
// READ_CONTROL and WRITE_DAC for its descriptor.
const stagedAccess = windows.DELETE | windows.SYNCHRONIZE | windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL | windows.WRITE_DAC

// placeFile places one staged file at dPath, inside the destination folder
// held as dh.
func (p *placer) placeFile(sPath string, dh windows.Handle, dPath, name string) {
	fh, err := openNoFollow(sPath, stagedAccess, windows.FILE_SHARE_READ)
	if err != nil {
		p.c.Add(ClassOther, 1)
		return
	}
	defer windows.CloseHandle(fh)
	ti, err := tagOf(fh)
	if err != nil {
		p.c.Add(ClassOther, 1)
		return
	}
	if ti.reparse() {
		p.c.Skipped++ // a link restic made: never moved into place
		return
	}
	fi, err := fileInfo(fh)
	if err != nil {
		p.c.Add(ClassOther, 1)
		return
	}
	size := sizeOf(fi)

	// What is at the destination name now.
	replace := false
	if xh, err := openNoFollow(dPath, windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, shareAll); err == nil {
		xt, terr := tagOf(xh)
		okName := checkName(xh, name)
		_ = windows.CloseHandle(xh)
		switch {
		case terr != nil:
			p.c.Add(ClassOther, 1)
			return
		case xt.surrogate(), xt.dir(), !okName:
			// A link, a folder where a file is, or an inexact name match.
			p.c.Add(ClassGuardRefused, 1)
			return
		case p.mode == Keep:
			p.c.Skipped++
			p.c.BytesSkipped += size
			return
		case xt.Attrs&(windows.FILE_ATTRIBUTE_READONLY|windows.FILE_ATTRIBUTE_SYSTEM) != 0:
			// Checked by the agent, never cleared: Windows itself would
			// replace a system file (p22-spike.md:532-540).
			p.c.Add(ClassAccessDenied, 1)
			return
		}
		// A file another program holds open cannot be replaced; probing with
		// DELETE says so as a sharing violation (target_locked) rather than the
		// access denied the rename itself would report.
		if lh, lerr := openNoFollow(dPath, windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, shareAll); lerr != nil {
			p.c.Add(classify(lerr), 1)
			return
		} else {
			_ = windows.CloseHandle(lh)
		}
		replace = true // a checked regular file with no name-surrogate tag
	} else if !notFound(err) {
		p.c.Add(classify(err), 1)
		return
	}

	if testHookBeforeMove != nil {
		testHookBeforeMove(dPath)
	}
	// The parent's tag again, through its held handle, immediately before the
	// move: the hold is not credited with stopping a held folder from being
	// turned into a reparse point in place.
	if pt, err := tagOf(dh); err != nil || pt.surrogate() {
		p.c.Add(ClassGuardRefused, 1)
		return
	}
	if err := renameRelative(fh, dh, name, replace); err != nil {
		if !replace && existsErr(err) {
			p.c.Skipped++ // something appeared at the name: never overwritten
			p.c.BytesSkipped += size
			return
		}
		p.c.Add(classify(err), 1)
		return
	}
	p.c.Restored++
	p.c.BytesRestored += size
	if err := fixDescriptor(fh); err != nil {
		// The file is in place with the entries staging handed down, which
		// grant Administrators and SYSTEM only: nothing is opened up, but
		// the descriptor is not what the ADR promises, so it is counted.
		p.c.Restored--
		p.c.BytesRestored -= size
		p.c.Add(ClassOther, 1)
	}
}

// fixDescriptor applies a moved file's explicit entries again with
// UNPROTECTED_DACL_SECURITY_INFORMATION through its own handle: a move keeps
// the inherited entries staging handed down and does not pick up the new
// parent's (p22-spike.md:496-507), so staging's are dropped and the new
// parent's picked up. A protected descriptor is kept as restored.
func fixDescriptor(h windows.Handle) error {
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	ctl, _, err := sd.Control()
	if err != nil {
		return err
	}
	if ctl&windows.SE_DACL_PROTECTED != 0 {
		return nil
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	explicit, err := explicitOnly(dacl)
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, explicit, nil)
}

// explicitOnly builds a new ACL holding the ACEs of dacl that are not marked
// inherited, in their order, byte for byte.
func explicitOnly(dacl *windows.ACL) (*windows.ACL, error) {
	var aces [][]byte
	if dacl != nil {
		for i := uint32(0); i < uint32(dacl.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(dacl, i, &ace); err != nil {
				return nil, err
			}
			if ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
				continue
			}
			raw := unsafe.Slice((*byte)(unsafe.Pointer(ace)), int(ace.Header.AceSize))
			aces = append(aces, append([]byte(nil), raw...))
		}
	}
	return buildACL(aces), nil
}

// buildACL lays out an ACL (revision ACL_REVISION_DS) from raw ACE bytes.
func buildACL(aces [][]byte) *windows.ACL {
	size := 8
	for _, a := range aces {
		size += len(a)
	}
	// Backed by uint32s so the ACL is 4-byte aligned, as Windows requires.
	words := make([]uint32, (size+3)/4)
	buf := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), len(words)*4)
	buf[0] = 4 // ACL_REVISION_DS
	*(*uint16)(unsafe.Pointer(&buf[2])) = uint16(size)
	*(*uint16)(unsafe.Pointer(&buf[4])) = uint16(len(aces))
	off := 8
	for _, a := range aces {
		copy(buf[off:], a)
		off += len(a)
	}
	return (*windows.ACL)(unsafe.Pointer(&buf[0]))
}

// countItems is how many items a refused staged entry stands for: the files
// below a folder (at least one, the folder itself), or one for a file. It is
// read with the walk's own no-follow opens and never descends a reparse point.
func countItems(path string) int64 {
	h, err := openNoFollow(path, holdAccess, shareRW)
	if err != nil {
		return 1
	}
	defer windows.CloseHandle(h)
	ti, err := tagOf(h)
	if err != nil || ti.reparse() || !ti.dir() {
		return 1
	}
	var n int64
	countFiles(h, path, &n)
	if n == 0 {
		return 1
	}
	return n
}

func countFiles(h windows.Handle, path string, n *int64) {
	ents, err := listDir(h)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			*n++
			continue
		}
		if e.attrs&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			*n++
			continue
		}
		p := join(path, e.name)
		ch, err := openNoFollow(p, holdAccess, shareRW)
		if err != nil {
			continue
		}
		if ti, err := tagOf(ch); err == nil && !ti.reparse() && ti.dir() {
			countFiles(ch, p, n)
		}
		_ = windows.CloseHandle(ch)
	}
}
