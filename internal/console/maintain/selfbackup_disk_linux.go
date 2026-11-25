//go:build linux

package maintain

import "syscall"

// freeDiskBytes is the space an unprivileged process may still write on the
// filesystem of dir (statfs f_bavail times f_bsize).
func freeDiskBytes(dir string) (uint64, bool, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, true, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true, nil
}
