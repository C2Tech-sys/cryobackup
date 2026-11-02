//go:build linux

package maintain

import (
	"os"
	"syscall"
)

// ownedByMe reports whether fi (an Lstat result) belongs to the process's
// effective user id. known is false when the owner cannot be read.
func ownedByMe(fi os.FileInfo) (mine, known bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false, false
	}
	return int(st.Uid) == os.Geteuid(), true
}

// Filesystem magic numbers of the in-memory filesystems (linux/magic.h).
const (
	tmpfsMagic = 0x01021994
	ramfsMagic = 0x858458f6
)

// onMemoryFS reports whether dir is on tmpfs or ramfs. known is false when the
// filesystem could not be asked.
func onMemoryFS(dir string) (inMemory, known bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return false, false
	}
	switch uint32(st.Type) { // the field's width differs per architecture; the magics fit 32 bits

	case tmpfsMagic, ramfsMagic:
		return true, true
	}
	return false, true
}
