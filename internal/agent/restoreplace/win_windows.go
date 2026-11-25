//go:build windows

package restoreplace

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The share modes. A held folder is opened without FILE_SHARE_DELETE, so no
// other process can rename or remove it while the agent holds it (ADR 0027,
// p22-spike.md:525-531); probes that only read attributes share everything.
const (
	shareRW  = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
	shareAll = shareRW | windows.FILE_SHARE_DELETE
)

// holdAccess is the data right a held folder is opened with: a handle with
// attribute rights only takes no part in sharing and protects nothing
// (p22-spike.md:560-563).
const holdAccess = windows.FILE_LIST_DIRECTORY | windows.FILE_TRAVERSE | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE

// nameSurrogateBit is winnt.h's IsReparseTagNameSurrogate test.
const nameSurrogateBit = 0x20000000

// longPath returns p in the \\?\ form (ADR 0027: every path the agent opens is
// in that form, so Win32 neither strips a trailing dot or space nor maps a
// device name). p is a drive-absolute path such as C:\x.
func longPath(p string) string {
	if strings.HasPrefix(p, `\\?\`) {
		return p
	}
	return `\\?\` + p
}

// join appends one name to a directory path with a single backslash.
func join(dir, name string) string {
	if strings.HasSuffix(dir, `\`) {
		return dir + name
	}
	return dir + `\` + name
}

func u16(p string) (*uint16, error) { return windows.UTF16PtrFromString(p) }

// openNoFollow opens the named object itself, never what a reparse point
// names (FILE_FLAG_OPEN_REPARSE_POINT), with backup semantics so a folder can
// be opened and an enabled backup or restore privilege applies.
func openNoFollow(path string, access, share uint32) (windows.Handle, error) {
	p, err := u16(longPath(path))
	if err != nil {
		return windows.InvalidHandle, err
	}
	return windows.CreateFile(p, access, share, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
}

// tagInfo is FILE_ATTRIBUTE_TAG_INFO.
type tagInfo struct {
	Attrs uint32
	Tag   uint32
}

func (t tagInfo) reparse() bool { return t.Attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 }
func (t tagInfo) dir() bool     { return t.Attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0 }

// surrogate reports a reparse point whose tag is a name surrogate: a junction,
// a symbolic link, a mount point. A placeholder or a deduplicated file carries
// a tag that is not, and is an ordinary file or folder (ADR 0027).
func (t tagInfo) surrogate() bool { return t.reparse() && t.Tag&nameSurrogateBit != 0 }

// tagOf reads the attributes and reparse tag through the handle.
func tagOf(h windows.Handle) (tagInfo, error) {
	var ti tagInfo
	err := windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&ti)), uint32(unsafe.Sizeof(ti)))
	return ti, err
}

// fileInfo reads size, attributes and link count through the handle.
func fileInfo(h windows.Handle) (windows.ByHandleFileInformation, error) {
	var fi windows.ByHandleFileInformation
	err := windows.GetFileInformationByHandle(h, &fi)
	return fi, err
}

func sizeOf(fi windows.ByHandleFileInformation) uint64 {
	return uint64(fi.FileSizeHigh)<<32 | uint64(fi.FileSizeLow)
}

// finalName is the object's real long name, read through the handle: the
// last component of its normalized final path. A name opened through its 8.3
// short name comes back long, so the exact compare catches it (ADR 0027,
// "Names are compared exactly").
func finalName(h windows.Handle) (string, error) {
	buf := make([]uint16, 512)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)),
			fileNameNormalized|volumeNameNone)
		if err != nil {
			return "", err
		}
		if int(n) < len(buf) {
			s := windows.UTF16ToString(buf[:n])
			if i := strings.LastIndexByte(s, '\\'); i >= 0 {
				s = s[i+1:]
			}
			return s, nil
		}
		buf = make([]uint16, n+1)
	}
}

// dirEntry is one entry of a folder, read through the folder's handle.
type dirEntry struct {
	name  string
	attrs uint32
}

// listDir enumerates the folder through its own handle (no second open by
// name), skipping "." and "..".
func listDir(h windows.Handle) ([]dirEntry, error) {
	const (
		fileFullDirectoryInfo        = 14 // FileFullDirectoryInfo
		fileFullDirectoryRestartInfo = 15 // FileFullDirectoryRestartInfo
		offAttrs                     = 56
		offNameLen                   = 60
		offName                      = 68
	)
	// Backed by uint64s: the call needs an 8-byte aligned buffer (ERROR_NOACCESS otherwise).
	words := make([]uint64, (64<<10)/8)
	buf := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), len(words)*8)
	class := uint32(fileFullDirectoryRestartInfo)
	var out []dirEntry
	for {
		err := windows.GetFileInformationByHandleEx(h, class, &buf[0], uint32(len(buf)))
		class = fileFullDirectoryInfo
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		off := 0
		for {
			next := int(*(*uint32)(unsafe.Pointer(&buf[off])))
			attrs := *(*uint32)(unsafe.Pointer(&buf[off+offAttrs]))
			nlen := int(*(*uint32)(unsafe.Pointer(&buf[off+offNameLen]))) / 2
			name := make([]uint16, nlen)
			for i := 0; i < nlen; i++ {
				name[i] = *(*uint16)(unsafe.Pointer(&buf[off+offName+2*i]))
			}
			s := string(utf16.Decode(name))
			if s != "." && s != ".." {
				out = append(out, dirEntry{name: s, attrs: attrs})
			}
			if next == 0 {
				break
			}
			off += next
		}
	}
}

// deleteHandle marks exactly the opened object for deletion through its own
// handle: the clean-up walk's delete, which never follows a reparse point
// (p22-spike.md:548-553). It tries, in order: the POSIX-semantics delete; the
// same ignoring the read-only attribute of the agent's own staging copies
// (Windows then requires FILE_WRITE_ATTRIBUTES on the handle, which the walk
// asks for where it is granted); the plain disposition. The first that works
// is the delete.
func deleteHandle(h windows.Handle) error {
	var first error
	for _, flags := range []uint32{
		windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS,
		windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS | windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE,
	} {
		f := flags
		err := windows.SetFileInformationByHandle(h, windows.FileDispositionInfoEx, (*byte)(unsafe.Pointer(&f)), uint32(unsafe.Sizeof(f)))
		if err == nil {
			return nil
		}
		if first == nil {
			first = err
		}
	}
	var del uint32 = 1
	if err := windows.SetFileInformationByHandle(h, windows.FileDispositionInfo, (*byte)(unsafe.Pointer(&del)), uint32(unsafe.Sizeof(del))); err != nil {
		return first
	}
	return nil
}

// renameRelative renames the object behind h to name inside the folder behind
// root, through FileRenameInformationEx (ADR 0027: FileRenameInfoEx, the move
// made relative to the held parent handle; no path-based MoveFileEx, no
// os.Rename). Without replace an existing target makes it fail; with replace
// only FILE_RENAME_REPLACE_IF_EXISTS is set. It calls NtSetInformationFile
// directly: kernel32's SetFileInformationByHandle refuses a RootDirectory
// (ERROR_INVALID_PARAMETER), and the relative form is the point.
func renameRelative(h, root windows.Handle, name string, replace bool) error {
	n := utf16.Encode([]rune(name))
	// FILE_RENAME_INFORMATION (64-bit): Flags ULONG @0, RootDirectory HANDLE
	// @8, FileNameLength ULONG @16, FileName [1]WCHAR @20.
	const hdr = 20
	if unsafe.Sizeof(uintptr(0)) != 8 {
		return fmt.Errorf("restoreplace: rename layout not defined for %s", runtime.GOARCH)
	}
	size := hdr + 2*len(n) + 2
	words := make([]uint64, (size+7)/8) // 8-byte aligned, as the call requires
	buf := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), len(words)*8)
	var flags uint32
	if replace {
		flags = windows.FILE_RENAME_REPLACE_IF_EXISTS
	}
	*(*uint32)(unsafe.Pointer(&buf[0])) = flags
	*(*uintptr)(unsafe.Pointer(&buf[8])) = uintptr(root)
	*(*uint32)(unsafe.Pointer(&buf[16])) = uint32(2 * len(n))
	for i, c := range n {
		*(*uint16)(unsafe.Pointer(&buf[hdr+2*i])) = c
	}
	var iosb windows.IO_STATUS_BLOCK
	err := windows.NtSetInformationFile(h, &iosb, &buf[0], uint32(size), fileRenameInformationEx)
	var st windows.NTStatus
	if errors.As(err, &st) {
		return st.Errno()
	}
	return err
}

// fileRenameInformationEx is FILE_INFORMATION_CLASS FileRenameInformationEx.
const fileRenameInformationEx = 65

// classify maps a Windows error of a move to its countable class.
func classify(err error) Class {
	switch {
	case errors.Is(err, errCantResolveFilename):
		// Too many reparse levels on the way: a name surrogate the agent did
		// not make (fix batch 3, T3).
		return ClassGuardRefused
	case errors.Is(err, windows.ERROR_SHARING_VIOLATION), errors.Is(err, windows.ERROR_LOCK_VIOLATION),
		errors.Is(err, windows.ERROR_USER_MAPPED_FILE):
		return ClassTargetLocked
	case errors.Is(err, windows.ERROR_ACCESS_DENIED), errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD):
		return ClassAccessDenied
	}
	return ClassOther
}

// errCantResolveFilename is ERROR_CANT_RESOLVE_FILENAME (1921).
const errCantResolveFilename = windows.Errno(1921)

// existsErr reports the errors a no-replace rename gives when a file appeared
// at the target name.
func existsErr(err error) bool {
	return errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS)
}

// notFound reports the errors an open gives when nothing has the name.
func notFound(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

// procAdjustTokenPrivileges is called directly: the x/sys wrapper drops
// ERROR_NOT_ALL_ASSIGNED.
var procAdjustTokenPrivileges = windows.NewLazySystemDLL("advapi32.dll").NewProc("AdjustTokenPrivileges")

// WithPrivileges runs f with SeBackupPrivilege and SeRestorePrivilege enabled
// on an impersonation token of a locked OS thread, when the process holds
// them (SYSTEM, an elevated administrator): the agent's open flags with its
// backup and restore privileges, as ADR 0027 lab check 10 describes. A process
// that does not hold them (a non-elevated test) runs f as it is. The process
// token itself is never changed.
func WithPrivileges(f func() error) error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return f()
	}
	runtime.LockOSThread()
	if err := windows.ImpersonateSelf(windows.SecurityImpersonation); err != nil {
		runtime.UnlockOSThread()
		return f()
	}
	defer func() {
		if windows.RevertToSelf() != nil {
			return // leave the thread locked: the runtime discards it with the goroutine
		}
		runtime.UnlockOSThread()
	}()
	var tok windows.Token
	if err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, false, &tok); err == nil {
		for _, name := range []string{"SeBackupPrivilege", "SeRestorePrivilege"} {
			var luid windows.LUID
			if n, err := u16(name); err == nil && windows.LookupPrivilegeValue(nil, n, &luid) == nil {
				tp := windows.Tokenprivileges{PrivilegeCount: 1}
				tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
				_, _, _ = procAdjustTokenPrivileges.Call(uintptr(tok), 0, uintptr(unsafe.Pointer(&tp)), 0, 0, 0)
			}
		}
		tok.Close()
	}
	return f()
}

// FreeBytes is the free space available to the caller on the volume that
// holds dir (ADR 0027, "Free space").
func FreeBytes(dir string) (uint64, error) {
	p, err := u16(dir)
	if err != nil {
		return 0, err
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, err
	}
	return avail, nil
}

// GetFinalPathNameByHandle flags (fileapi.h).
const (
	fileNameNormalized = 0x0
	volumeNameNone     = 0x4
)

// FixedDrives lists the letters of the fixed drives present (the start-up
// sweep looks for a staging root on each).
func FixedDrives() []string {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil
	}
	var out []string
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		l := string(rune('A' + i))
		p, err := u16(l + `:\`)
		if err != nil {
			continue
		}
		if windows.GetDriveType(p) == windows.DRIVE_FIXED {
			out = append(out, l)
		}
	}
	return out
}

// NtCreateFile create options and dispositions used here (winternl.h).
const (
	ntOpenReparse = windows.FILE_OPEN_REPARSE_POINT
	ntBackup      = windows.FILE_OPEN_FOR_BACKUP_INTENT
	ntSync        = windows.FILE_SYNCHRONOUS_IO_NONALERT
	ntDirectory   = windows.FILE_DIRECTORY_FILE
	ntDispOpen    = windows.FILE_OPEN
	ntDispCreate  = windows.FILE_CREATE
	ntBaseOptions = ntOpenReparse | ntBackup | ntSync
	objCaseInsens = windows.OBJ_CASE_INSENSITIVE
)

// ntAt opens (or, with disposition FILE_CREATE, creates) the single name
// relative to the folder handle dir (OBJECT_ATTRIBUTES.RootDirectory), never
// by a full path and never following a reparse point on the name
// (FILE_OPEN_REPARSE_POINT). The held parent is the folder the name is
// looked up in, whatever its path has become meanwhile (fix batch 2, H1:
// ADR 0027 "No placement step reopens a file or folder by name"). A created
// object gets no security descriptor of its own: it inherits from dir. NT
// status codes come back as their Win32 errors, so notFound and existsErr
// work on them.
func ntAt(dir windows.Handle, name string, access, share, disposition, options uint32) (windows.Handle, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `\/`) {
		return windows.InvalidHandle, windows.ERROR_INVALID_NAME
	}
	us, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: dir, ObjectName: us, Attributes: objCaseInsens}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var iosb windows.IO_STATUS_BLOCK
	var h windows.Handle
	err = windows.NtCreateFile(&h, access|windows.SYNCHRONIZE, &oa, &iosb, nil, 0, share, disposition, options|ntBaseOptions, 0, 0)
	if err != nil {
		var st windows.NTStatus
		if errors.As(err, &st) {
			return windows.InvalidHandle, st.Errno()
		}
		return windows.InvalidHandle, err
	}
	return h, nil
}

// openAt opens an existing name relative to dir (ntAt with FILE_OPEN).
func openAt(dir windows.Handle, name string, access, share uint32) (windows.Handle, error) {
	return ntAt(dir, name, access, share, ntDispOpen, 0)
}

// createDirAt creates the folder name relative to dir and returns it open with
// access (one call: no window between the creation and the open).
func createDirAt(dir windows.Handle, name string, access, share uint32) (windows.Handle, error) {
	return ntAt(dir, name, access, share, ntDispCreate, ntDirectory)
}

// finalPath is the object's full normalized path below its volume, read
// through the handle ("\Users\x\Documents"): long names, as the object is
// reached now.
func finalPath(h windows.Handle) (string, error) {
	buf := make([]uint16, 512)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), fileNameNormalized|volumeNameNone)
		if err != nil {
			return "", err
		}
		if int(n) < len(buf) {
			return windows.UTF16ToString(buf[:n]), nil
		}
		buf = make([]uint16, n+1)
	}
}

// chainOK is the defence in depth of fix batch 2: the child's final path,
// read through its own handle, must be exactly the parent's final path plus
// the expected name (case-sensitive, as the name rule). An 8.3 or otherwise
// inexact match, or a child reached through anything but the held parent,
// fails it.
func chainOK(child windows.Handle, parentPath, name string) bool {
	got, err := finalPath(child)
	return err == nil && got == strings.TrimRight(parentPath, `\`)+`\`+name
}
