//go:build !windows

package probe

import "golang.org/x/sys/unix"

// DiskSpace returns the free and total bytes of the volume holding dir (banner
// C11: the console database's volume). Free is what an unprivileged process may
// use (f_bavail), the figure that runs out first.
func DiskSpace(dir string) (free, total int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	bs := int64(st.Bsize) //nolint:unconvert // Bsize is int64 on linux, uint32 on darwin
	return int64(st.Bavail) * bs, int64(st.Blocks) * bs, nil
}
