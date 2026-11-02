//go:build windows

package secrets

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
)

func newProtector(insecure bool) (protector, error) {
	if insecure {
		return nil, ErrPlaintextRefused
	}
	return dpapi{}, nil
}

// dpapi protects values with the machine-scope DPAPI key: any process running
// as an administrator or SYSTEM on this machine can unprotect them, no one off
// the machine can. The directory ACL is what keeps unprivileged users out.
type dpapi struct{}

func (dpapi) protect(plain []byte) ([]byte, error) {
	in := windows.DataBlob{Size: uint32(len(plain)), Data: &plain[0]}
	var out windows.DataBlob
	desc, _ := windows.UTF16PtrFromString("CryoBackup")
	if err := windows.CryptProtectData(&in, desc, nil, 0, nil,
		windows.CRYPTPROTECT_LOCAL_MACHINE|windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	// Copy before the deferred LocalFree: the blob lives in system memory.
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

func (dpapi) unprotect(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, errors.New("empty blob")
	}
	in := windows.DataBlob{Size: uint32(len(blob)), Data: &blob[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

// Elevated reports whether the process runs elevated (or as SYSTEM): the
// precondition for changing an ACL to SYSTEM and Administrators only without
// locking the caller out.
func Elevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// RestrictToSystemAndAdmins sets the owner SYSTEM and a protected DACL
// granting SYSTEM and Administrators full control (inherited by children) on
// path: config.SDDLStateDir, applied by config.RestrictDir with
// SeRestorePrivilege and SeTakeOwnershipPrivilege enabled for the call. A
// process that is not elevated would lock itself out, so it gets an error
// wrapping config.ErrACLNotApplied and nothing changes; an elevated process
// whose token lacks the privileges gets config.ErrPrivilege.
func RestrictToSystemAndAdmins(path string) error { return config.RestrictDir(path) }
