//go:build windows

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Privileges enabled, on an impersonation token of the calling thread only, for
// the calls that set the owner to SYSTEM: SeRestorePrivilege lets an
// administrator assign an owner other than itself (and opens a directory whose
// DACL shuts it out, with backup semantics); SeTakeOwnershipPrivilege lets it
// take ownership of a directory someone else owns.
var ownerPrivileges = []string{"SeRestorePrivilege", "SeTakeOwnershipPrivilege"}

// quarantinePrivileges adds SeBackupPrivilege, which lets QuarantineRoot list
// an untrusted tree whose DACL shuts administrators out.
var quarantinePrivileges = []string{"SeBackupPrivilege", "SeRestorePrivilege", "SeTakeOwnershipPrivilege"}

// sddlQuarantine is put on a quarantine's top directory: owner Administrators,
// a protected DACL for Administrators only. The ACE is not inheritable and the
// descriptor is set with SetKernelObjectSecurity, so nothing is propagated
// into the untrusted tree (no file below it is touched).
const sddlQuarantine = "O:BAD:P(A;;FA;;;BA)"

// procAdjustTokenPrivileges is called directly: the x/sys wrapper drops
// ERROR_NOT_ALL_ASSIGNED, which AdjustTokenPrivileges reports with a non-zero
// return value when the token does not hold a privilege.
var procAdjustTokenPrivileges = windows.NewLazySystemDLL("advapi32.dll").NewProc("AdjustTokenPrivileges")

// canRestrict reports whether the process runs elevated (or as SYSTEM): the
// precondition for setting the SYSTEM owner and the protected DACL without
// locking the caller out of its own directory.
func canRestrict() bool { return windows.GetCurrentProcessToken().IsElevated() }

// CanRestrict reports whether this process can restrict and verify the state
// directory (elevated, or SYSTEM). Only then is a failed check of an existing
// state directory a verdict about the directory rather than about the caller.
func CanRestrict() bool { return canRestrict() }

// isReparse reports FILE_ATTRIBUTE_REPARSE_POINT on an os.Lstat result
// (junctions, symbolic links, mount points: anything the agent must not follow).
func isReparse(st os.FileInfo) bool {
	if d, ok := st.Sys().(*syscall.Win32FileAttributeData); ok {
		return d.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
	}
	return st.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0
}

// stateSD returns the parsed SDDLStateDir and its owner and DACL.
func stateSD() (*windows.SECURITY_DESCRIPTOR, *windows.SID, *windows.ACL, error) {
	sd, err := windows.SecurityDescriptorFromString(SDDLStateDir)
	if err != nil {
		return nil, nil, nil, err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return nil, nil, nil, err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return nil, nil, nil, err
	}
	return sd, owner, dacl, nil
}

// openNoFollow opens a file or directory itself (never the target of a
// reparse point) with backup semantics, and refuses it if it is a reparse
// point. The checks and the later security or rename call use the same
// handle, so the object cannot be swapped for a junction in between.
func openNoFollow(path string, access uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	h, err := windows.CreateFile(p, access|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		_ = windows.CloseHandle(h)
		return 0, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(h)
		return 0, ErrReparsePoint
	}
	return h, nil
}

// withOwnerPrivileges runs f with SeRestorePrivilege and
// SeTakeOwnershipPrivilege enabled (withPrivileges).
func withOwnerPrivileges(f func() error) error { return withPrivileges(ownerPrivileges, f) }

// withQuarantinePrivileges runs f with quarantinePrivileges enabled when the
// process is elevated, and as it is otherwise (an unelevated caller can only
// move a tree it may move anyway; securing the quarantine then fails, which
// is best effort).
func withQuarantinePrivileges(f func() error) error {
	if !canRestrict() {
		return f()
	}
	return withPrivileges(quarantinePrivileges, f)
}

// withPrivileges runs f on a locked OS thread that impersonates the process
// with the named privileges enabled; the process token itself is not changed.
// A privilege the token does not hold is ErrPrivilege.
func withPrivileges(names []string, f func() error) error {
	runtime.LockOSThread()
	if err := windows.ImpersonateSelf(windows.SecurityImpersonation); err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("%w: impersonate: %v", ErrPrivilege, err)
	}
	defer func() {
		if windows.RevertToSelf() != nil {
			// The thread still carries the privileged token: leave it locked
			// so the runtime discards the thread with the goroutine.
			return
		}
		runtime.UnlockOSThread()
	}()
	var tok windows.Token
	if err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, false, &tok); err != nil {
		return fmt.Errorf("%w: open the thread token: %v", ErrPrivilege, err)
	}
	defer tok.Close()
	for _, name := range names {
		var luid windows.LUID
		n, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return err
		}
		if err := windows.LookupPrivilegeValue(nil, n, &luid); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrPrivilege, name, err)
		}
		tp := windows.Tokenprivileges{PrivilegeCount: 1}
		tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
		r1, _, e1 := procAdjustTokenPrivileges.Call(uintptr(tok), 0, uintptr(unsafe.Pointer(&tp)), 0, 0, 0)
		if r1 == 0 {
			return fmt.Errorf("%w: %s: %v", ErrPrivilege, name, e1)
		}
		if errors.Is(e1, windows.ERROR_NOT_ALL_ASSIGNED) {
			return fmt.Errorf("%w: the token does not hold %s", ErrPrivilege, name)
		}
	}
	return f()
}

// RestrictDir sets the owner SYSTEM and the protected DACL of SDDLStateDir
// (OWNER_SECURITY_INFORMATION | DACL_SECURITY_INFORMATION |
// PROTECTED_DACL_SECURITY_INFORMATION) on one directory, through a handle that
// refuses reparse points. Setting the owner to SYSTEM needs
// SeRestorePrivilege, which is enabled for the call only. A process that is
// not elevated would lock itself out, so it gets an error wrapping
// ErrACLNotApplied and nothing changes; an elevated process whose token lacks
// the privileges gets ErrPrivilege. It is only ever applied to a directory the
// agent just created or one that already verified (package agent refuses an
// untrusted tree before anything is applied), never to take a tree over.
func RestrictDir(path string) error {
	if !canRestrict() {
		return fmt.Errorf("%w: the process is not elevated", ErrACLNotApplied)
	}
	return withOwnerPrivileges(func() error { return applyStateSD(path) })
}

// applyStateSD does RestrictDir's work; the caller holds the privileges.
func applyStateSD(path string) error {
	_, owner, dacl, err := stateSD()
	if err != nil {
		return err
	}
	// READ_CONTROL as well: SetSecurityInfo reads the object's current
	// descriptor before it writes the new one, and without READ_CONTROL on the
	// handle it fails with ERROR_ACCESS_DENIED (seen on the first elevated CI
	// run, P1-FIX10).
	h, err := openNoFollow(path, windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER)
	if err != nil {
		if errors.Is(err, ErrReparsePoint) {
			return err
		}
		return fmt.Errorf("open for READ_CONTROL, WRITE_DAC and WRITE_OWNER: %w", err)
	}
	defer windows.CloseHandle(h)
	if err := windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner, nil, dacl, nil); err != nil {
		return fmt.Errorf("SetSecurityInfo (owner SYSTEM, protected DACL): %w", err)
	}
	return nil
}

// createRestrictedDir creates path with SDDLStateDir already in place
// (CreateDirectory with SECURITY_ATTRIBUTES), so the directory is never open
// to other users, not even for a moment. CreateDirectory fails on any existing
// entry, a reparse point included, so nothing is followed or adopted.
func createRestrictedDir(path string) error {
	sd, _, _, err := stateSD()
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	sa := windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))
	return withOwnerPrivileges(func() error { return windows.CreateDirectory(p, &sa) })
}

// ownerTrusted reports whether a SID may own the state root.
func ownerTrusted(sid *windows.SID) bool {
	return sid != nil && (sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid))
}

// readOwner reads the owner of path through a no-follow handle.
func readOwner(path string) (*windows.SID, error) {
	h, err := openNoFollow(path, windows.READ_CONTROL)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h)
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return nil, err
	}
	if owner == nil {
		return nil, errors.New("no owner")
	}
	return owner.Copy()
}

// ownerRule is the owner rule of every check (checkRootOwner, checkOwnerLab,
// VerifyDir), pure so that every row is tested (ticket P1-FIX10). owner and
// readErr are what readOwner (or the descriptor read) returned; elevated is
// whether the verdict is an elevated one (the caller can restrict, or the
// check decides "verified"); self is the account this process runs as (nil
// when unknown).
//
//   - a reparse point: ErrReparsePoint;
//   - SYSTEM or BUILTIN\Administrators: trusted;
//   - an owner that cannot be read, or none: UNTRUSTED when elevated, never
//     trusted (ErrStateUntrusted and ErrForeignOwner); an unelevated lab run
//     lets it through, because the lab flag waives only what that process
//     cannot see or set and never yields a verified state directory;
//   - the account this process runs as (self; only the lab check passes it):
//     trusted;
//   - anyone else: ErrForeignOwner.
func ownerRule(owner *windows.SID, readErr error, elevated bool, self *windows.SID) error {
	switch {
	case errors.Is(readErr, ErrReparsePoint):
		return ErrReparsePoint
	case readErr != nil || owner == nil:
		if !elevated {
			return nil
		}
		why := "the descriptor has no owner"
		if readErr != nil {
			why = readErr.Error()
		}
		return fmt.Errorf("%w: %w: the owner cannot be read (%s), and an elevated check never trusts what it cannot read", ErrStateUntrusted, ErrForeignOwner, why)
	case ownerTrusted(owner):
		return nil
	case self != nil && owner.Equals(self):
		return nil
	case elevated:
		return fmt.Errorf("%w (owner %s)", ErrForeignOwner, owner.String())
	default:
		return fmt.Errorf("%w (owner %s): another account created it, and --insecure-unprotected-state never adopts one", ErrForeignOwner, owner.String())
	}
}

// checkRootOwner is the owner rule of an elevated Ensure on an existing root
// (ownerRule, elevated): SYSTEM or BUILTIN\Administrators, read through a
// no-follow handle; an owner that cannot be read is untrusted.
func checkRootOwner(root string) error {
	owner, err := readOwner(root)
	if err := ownerRule(owner, err, true, nil); err != nil {
		if errors.Is(err, ErrReparsePoint) {
			return fmt.Errorf("%w: the state root", ErrReparsePoint)
		}
		return err
	}
	return nil
}

// checkOwnerLab is the owner rule of an Ensure that does not restrict (an
// unelevated lab run): an existing layout directory whose owner can be read
// must be owned by SYSTEM, BUILTIN\Administrators or the user this process
// runs as. An owner that cannot be read is let through only when this process
// is not elevated; an elevated process never trusts what it cannot read
// (ownerRule).
func checkOwnerLab(path string) error {
	owner, err := readOwner(path)
	var self *windows.SID
	if tu, terr := windows.GetCurrentProcessToken().GetTokenUser(); terr == nil {
		self = tu.User.Sid
	}
	return ownerRule(owner, err, canRestrict(), self)
}

// fileRenameInfo is FILE_RENAME_INFO (SetFileInformationByHandle,
// FileRenameInfo): Flags is the ReplaceIfExists BOOLEAN of the union.
type fileRenameInfo struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

// moveAside renames dir to target through a handle opened on dir itself
// (FILE_FLAG_OPEN_REPARSE_POINT, refused if dir is a reparse point or not a
// directory) with FILE_RENAME_INFO.ReplaceIfExists = FALSE: an existing target
// is ERROR_ALREADY_EXISTS, and a rename never copies (another volume is
// ERROR_NOT_SAME_DEVICE). Through the same handle, after the move, it then
// sets sddlQuarantine with SetKernelObjectSecurity (no propagation into the
// tree). secureErr reports that last step only; err means nothing was moved.
func moveAside(dir, target string) (secureErr, err error) {
	// A name that is already taken is ErrQuarantineExists whatever else is going
	// on. Without this check the answer depends on the tree: a scanner or indexer
	// holding a handle inside it makes the rename fail with "Access is denied"
	// before the name is looked at, and the retry with a fresh name never happens.
	// Lstat does not follow a link; the rename below still refuses to replace.
	if _, lerr := os.Lstat(target); lerr == nil {
		return nil, ErrQuarantineExists
	}
	h, err := openNoFollow(dir, windows.DELETE|windows.SYNCHRONIZE|windows.WRITE_DAC|windows.WRITE_OWNER)
	if err != nil && !errors.Is(err, ErrReparsePoint) {
		secureErr = fmt.Errorf("open for WRITE_DAC and WRITE_OWNER: %w", err)
		h, err = openNoFollow(dir, windows.DELETE|windows.SYNCHRONIZE)
	}
	if errors.Is(err, ErrReparsePoint) {
		return nil, fmt.Errorf("%w: the state root", ErrReparsePoint)
	}
	if err != nil {
		return nil, fmt.Errorf("config: open the state root to move it: %w", err)
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return nil, fmt.Errorf("config: inspect the state root: %w", err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return nil, errors.New("config: the state root is not a directory")
	}

	name, err := windows.UTF16FromString(target)
	if err != nil {
		return nil, err
	}
	var ri fileRenameInfo
	size := int(unsafe.Offsetof(ri.FileName)) + len(name)*2
	buf := make([]uint64, (size+7)/8) // 8-byte aligned for the HANDLE field
	p := (*fileRenameInfo)(unsafe.Pointer(&buf[0]))
	p.Flags = 0 // ReplaceIfExists = FALSE
	p.FileNameLength = uint32((len(name) - 1) * 2)
	copy(unsafe.Slice(&p.FileName[0], len(name)), name)
	if err := windows.SetFileInformationByHandle(h, windows.FileRenameInfo, (*byte)(unsafe.Pointer(&buf[0])), uint32(size)); err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
			return nil, ErrQuarantineExists
		}
		return nil, fmt.Errorf("config: move the state root aside: %w", err)
	}

	if secureErr != nil {
		return secureErr, nil
	}
	sd, err := windows.SecurityDescriptorFromString(sddlQuarantine)
	if err != nil {
		return err, nil
	}
	if err := windows.SetKernelObjectSecurity(h, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION, sd); err != nil {
		return fmt.Errorf("set the owner Administrators and the admin-only DACL: %w", err), nil
	}
	return nil, nil
}

// readSD reads the owner and DACL of path through a no-follow handle
// (GetSecurityInfo on a handle opened with FILE_FLAG_OPEN_REPARSE_POINT and
// refused if it is a reparse point): the object read is the one at path, never
// a link's target.
func readSD(path string) (*windows.SECURITY_DESCRIPTOR, error) {
	h, err := openNoFollow(path, windows.READ_CONTROL)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h)
	return windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
}

// ReadOwnerAndDACL returns the SDDL of path's owner and DACL, read through a
// no-follow handle (readSD); a reparse point is ErrReparsePoint. It replaces
// the path-based read that package secrets had (V7 N2).
func ReadOwnerAndDACL(path string) (string, error) {
	sd, err := readSD(path)
	if err != nil {
		return "", err
	}
	return sd.String(), nil
}

// VerifyDir reads the owner and DACL of an existing directory through a
// no-follow handle (readSD) and checks them: the owner with ownerRule as an
// elevated verdict (an owner that cannot be read, or none, is untrusted,
// never trusted), then the descriptor with CheckStateSDDL. It returns
// fs.ErrNotExist (wrapped) when the directory does not exist,
// ErrReparsePoint for a reparse point and ErrStateUntrusted for any other
// difference, including a descriptor this process may not read; an owner
// other than SYSTEM or Administrators, or one that cannot be read, also wraps
// ErrForeignOwner.
func VerifyDir(path string) error {
	sd, err := readSD(path)
	switch {
	case errors.Is(err, ErrReparsePoint):
		return err
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND), errors.Is(err, windows.ERROR_PATH_NOT_FOUND):
		return fmt.Errorf("config: %w", fs.ErrNotExist)
	case err != nil:
		return fmt.Errorf("%w: the descriptor cannot be read: %v", ErrStateUntrusted, err)
	}
	owner, _, oerr := sd.Owner()
	if err := ownerRule(owner, oerr, true, nil); err != nil {
		if errors.Is(err, ErrStateUntrusted) {
			return err
		}
		return fmt.Errorf("%w: %w", ErrStateUntrusted, err)
	}
	return CheckStateSDDL(sd.String())
}
