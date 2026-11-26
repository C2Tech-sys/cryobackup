//go:build windows

package restoreplace

// Test-only helpers (fix batch 3, T5): the agent removes and checks only
// relative to held handles (Root.RemoveChild, checkInheritedAt, chainOK);
// these by-path forms exist for the package's own tests and the SYSTEM lab
// tests, which build their trees by path.

import "golang.org/x/sys/windows"

// openForDelete opens a top folder by its path for the walk (the only open by
// path: the top is the agent's own folder under a held root).
func openForDelete(path string) (windows.Handle, error) {
	var first error
	for _, access := range deleteAccesses {
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
	h, err := openForDelete(path)
	if notFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)
	err = removeHeld(h, &links)
	return links, err
}

// checkName compares the real long name read through h with want, exactly.
func checkName(h windows.Handle, want string) bool {
	got, err := finalName(h)
	return err == nil && got == want
}
