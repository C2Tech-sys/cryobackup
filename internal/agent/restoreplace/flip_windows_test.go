//go:build windows

package restoreplace

import (
	"os"
	"path/filepath"
	"sync"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Helpers of the flip attack (fix batch 2, H1) used by the SYSTEM lab test
// TestLabSystemFlipProtected; the shipped regression tests are the verifier's
// own harness in attack_windows_test.go (fix batch 3, T1).

const flipTrials = 300

// mountPointBuffer is a REPARSE_DATA_BUFFER for IO_REPARSE_TAG_MOUNT_POINT
// that points at target (a junction).
func mountPointBuffer(target string) []byte {
	sub := utf16.Encode([]rune(`\??\` + target))
	prn := utf16.Encode([]rune(target))
	// MountPointReparseBuffer: SubstituteNameOffset, SubstituteNameLength,
	// PrintNameOffset, PrintNameLength (uint16 each), then PathBuffer with
	// both names, each NUL-terminated.
	pathBytes := (len(sub) + 1 + len(prn) + 1) * 2
	dataLen := 8 + pathBytes
	buf := make([]byte, 8+dataLen)
	*(*uint32)(unsafe.Pointer(&buf[0])) = windows.IO_REPARSE_TAG_MOUNT_POINT
	*(*uint16)(unsafe.Pointer(&buf[4])) = uint16(dataLen)
	*(*uint16)(unsafe.Pointer(&buf[8])) = 0
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

// flipInPlace sets or deletes a junction on the existing, empty folder dir.
func flipInPlace(dir, target string, set bool) {
	p, _ := windows.UTF16PtrFromString(dir)
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	var n uint32
	if set {
		b := mountPointBuffer(target)
		_ = windows.DeviceIoControl(h, windows.FSCTL_SET_REPARSE_POINT, &b[0], uint32(len(b)), nil, 0, &n, nil)
		return
	}
	hdr := make([]byte, 8)
	*(*uint32)(unsafe.Pointer(&hdr[0])) = windows.IO_REPARSE_TAG_MOUNT_POINT
	_ = windows.DeviceIoControl(h, windows.FSCTL_DELETE_REPARSE_POINT, &hdr[0], uint32(len(hdr)), nil, 0, &n, nil)
}

// flipper runs attack in a loop until stop is closed.
func flipper(stop chan struct{}, attack func(i int)) *sync.WaitGroup {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			attack(i)
		}
	}()
	return &wg
}

// entries is the number of entries anywhere below dir.
func entries(dir string) int {
	n := 0
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && p != dir {
			n++
		}
		return nil
	})
	return n
}
