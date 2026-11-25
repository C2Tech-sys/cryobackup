//go:build windows

package restoreplace

import (
	"golang.org/x/sys/windows"
)

// The clean-up walk (ADR 0027, "The clean-up walk"): every removal the agent
// makes - staging at the end of a restore, the start-up sweep, the links in the
// side folder - is this walk. It opens each entry with
// FILE_FLAG_OPEN_REPARSE_POINT | FILE_FLAG_BACKUP_SEMANTICS, deletes a reparse
// point itself through that handle, and never descends through one
// (p22-spike.md:548-553). os.RemoveAll is not used. Each folder is held, with
// a data right and without FILE_SHARE_DELETE, while its entries are removed,
// so no ancestor can be renamed under the walk; the folder is then deleted
// through the same handle.

// walkDirAccess is how the walk opens a folder it will empty and delete.
const walkDirAccess = holdAccess | windows.DELETE

// openForDelete opens an entry for the walk without following it: with
// FILE_WRITE_ATTRIBUTES where that is granted (needed to delete a read-only
// copy), else without; with list rights (a folder), else with the rights a
// file needs.
func openForDelete(path string) (windows.Handle, error) {
	var first error
	for _, access := range []uint32{
		walkDirAccess | windows.FILE_WRITE_ATTRIBUTES,
		walkDirAccess,
		windows.DELETE | windows.FILE_READ_ATTRIBUTES | windows.FILE_WRITE_ATTRIBUTES | windows.SYNCHRONIZE,
		windows.DELETE | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
	} {
		h, err := openNoFollow(path, access, shareRW)
		if err == nil {
			return h, nil
		}
		if first == nil || notFound(err) {
			first = err
		}
	}
	return windows.InvalidHandle, first
}

// RemoveTree removes path and everything below it with the clean-up walk and
// returns how many reparse points it deleted (each as itself, unfollowed).
// path itself may be a reparse point: it is then deleted as a link. A missing
// path is not an error.
func RemoveTree(path string) (links int, err error) {
	err = removeEntry(path, &links)
	if notFound(err) {
		err = nil
	}
	return links, err
}

// removeEntry removes one entry (and, for a real folder, its contents).
func removeEntry(path string, links *int) error {
	h, err := openForDelete(path)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
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
			if err := removeEntry(join(path, e.name), links); err != nil && !notFound(err) && first == nil {
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
	err = stripLinks(h, path, &n)
	return n, err
}

// stripLinks deletes the reparse points among the entries of the held folder
// and recurses into its real subfolders.
func stripLinks(h windows.Handle, path string, n *int) error {
	ents, err := listDir(h)
	if err != nil {
		return err
	}
	for _, e := range ents {
		p := join(path, e.name)
		if e.attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 && e.attrs&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			continue // a plain file: nothing to do (its handle-read tag is checked by every later step)
		}
		ch, err := openNoFollow(p, holdAccess|windows.DELETE, shareRW)
		if err != nil {
			ch, err = openNoFollow(p, windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, shareRW)
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
			err = stripLinks(ch, p, n)
		}
		_ = windows.CloseHandle(ch)
		if err != nil {
			return err
		}
	}
	return nil
}
