//go:build windows

package restoreplace

import (
	"golang.org/x/sys/windows"
)

// The clean-up walk (ADR 0027, "The clean-up walk"): every removal the agent
// makes - staging at the end of a restore, the start-up sweep, the links in the
// side folder, a side folder that did not succeed - is this walk. Each entry is
// opened relative to its held parent handle with FILE_OPEN_REPARSE_POINT and
// backup intent (ntAt; never by a full path, fix batch 2, M1), a reparse point
// is deleted itself through that handle and never descended into
// (p22-spike.md:548-553). os.RemoveAll is not used. Each folder is held, with a
// data right and without FILE_SHARE_DELETE, while its entries are removed, so
// no ancestor can be renamed under the walk; the folder is then deleted
// through the same handle (POSIX semantics where the volume has them).

// walkDirAccess is how the walk opens a folder it will empty and delete.
const walkDirAccess = holdAccess | windows.DELETE

// deleteAccesses are the opens the walk tries for an entry, most rights first:
// with FILE_WRITE_ATTRIBUTES where granted (needed to delete a read-only
// copy), else without; with list rights (a folder), else the rights a file
// needs.
var deleteAccesses = []uint32{
	walkDirAccess | windows.FILE_WRITE_ATTRIBUTES,
	walkDirAccess,
	windows.DELETE | windows.FILE_READ_ATTRIBUTES | windows.FILE_WRITE_ATTRIBUTES,
	windows.DELETE | windows.FILE_READ_ATTRIBUTES,
}

// openForDeleteAt opens name relative to the folder held as parent for the
// walk.
func openForDeleteAt(parent windows.Handle, name string) (windows.Handle, error) {
	var first error
	for _, access := range deleteAccesses {
		h, err := openAt(parent, name, access, shareRW)
		if err == nil {
			return h, nil
		}
		if first == nil || notFound(err) {
			first = err
		}
	}
	return windows.InvalidHandle, first
}

// removeAt removes the entry name of the folder held as parent.
func removeAt(parent windows.Handle, name string, links *int) error {
	h, err := openForDeleteAt(parent, name)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return removeHeld(h, links)
}

// removeHeld removes the open entry h: a reparse point as itself, a real
// folder after its entries, a file.
func removeHeld(h windows.Handle, links *int) error {
	ti, err := tagOf(h)
	if err != nil {
		return err
	}
	if ti.reparse() {
		*links++
		return deleteHandle(h)
	}
	if ti.dir() {
		ents, err := listDir(h)
		if err != nil {
			return err
		}
		var first error
		for _, e := range ents {
			if err := removeAt(h, e.name, links); err != nil && !notFound(err) && first == nil {
				first = err
			}
		}
		if first != nil {
			return first
		}
	}
	return deleteHandle(h)
}

// RemoveReparsePoints deletes every reparse point below path (not path itself)
// with the clean-up walk, never following or descending into one, and returns
// how many it deleted (ADR 0027: restic restores a junction in the snapshot as
// a symbolic link; each is deleted in staging or the side folder, never moved
// into place, and counted as skipped).
func RemoveReparsePoints(path string) (int, error) {
	h, err := openNoFollow(path, holdAccess, shareRW)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)
	ti, err := tagOf(h)
	if err != nil {
		return 0, err
	}
	if ti.reparse() || !ti.dir() {
		return 0, errNotRealDir
	}
	n := 0
	err = stripLinks(h, &n)
	return n, err
}

// stripLinks deletes the reparse points among the entries of the held folder
// and recurses into its real subfolders, every entry opened relative to it.
func stripLinks(h windows.Handle, n *int) error {
	ents, err := listDir(h)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 && e.attrs&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			continue // a plain file: nothing to do (its handle-read tag is checked by every later step)
		}
		ch, err := openAt(h, e.name, holdAccess|windows.DELETE, shareRW)
		if err != nil {
			ch, err = openAt(h, e.name, windows.DELETE|windows.FILE_READ_ATTRIBUTES, shareRW)
			if err != nil {
				return err
			}
		}
		ti, err := tagOf(ch)
		switch {
		case err != nil:
		case ti.reparse():
			if err = deleteHandle(ch); err == nil {
				*n++
			}
		case ti.dir():
			err = stripLinks(ch, n)
		}
		_ = windows.CloseHandle(ch)
		if err != nil {
			return err
		}
	}
	return nil
}
