//go:build windows

package maintain

import (
	"syscall"
	"unsafe"
)

var procGetDiskFreeSpaceExW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// freeDiskBytes is the space the calling user may still write on the volume of
// dir (GetDiskFreeSpaceExW, lpFreeBytesAvailableToCaller).
func freeDiskBytes(dir string) (uint64, bool, error) {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, true, err
	}
	var avail, total, free uint64
	r, _, callErr := procGetDiskFreeSpaceExW.Call(uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&avail)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&free)))
	if r == 0 {
		return 0, true, callErr
	}
	return avail, true, nil
}
