//go:build windows

package probe

import "golang.org/x/sys/windows"

// DiskSpace returns the free and total bytes of the volume holding dir (banner
// C11: the console database's volume). Free is what this process may use.
func DiskSpace(dir string) (free, total int64, err error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, 0, err
	}
	var avail, tot, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &tot, &totalFree); err != nil {
		return 0, 0, err
	}
	return int64(avail), int64(tot), nil
}
