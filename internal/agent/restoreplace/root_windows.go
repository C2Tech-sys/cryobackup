//go:build windows

package restoreplace

import (
	"errors"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// rootSDDL is the descriptor a created root gets: owner Administrators, a
// protected DACL that grants Administrators and SYSTEM only, inherited by
// everything below (ADR 0027, "The staging and side roots"; ruling C2).
const rootSDDL = "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

// Root is a staging or side root that passed the root rule, held open for the
// whole restore (ADR 0027: the agent holds the root's handle, as for an
// ancestor, so it can be neither renamed nor removed meanwhile).
type Root struct {
	path string
	h    windows.Handle
}

// Path is the root's path as given.
func (r *Root) Path() string { return r.path }

// Close releases the held handle. It is safe to call twice.
func (r *Root) Close() {
	if r != nil && r.h != windows.InvalidHandle && r.h != 0 {
		_ = windows.CloseHandle(r.h)
		r.h = windows.InvalidHandle
	}
}

// OpenRoot applies the root rule to path (<L>:\.cryobackup-restore or
// <SystemDrive>\CryoBackup-Restore): an absent root is created with rootSDDL;
// a present one must be a real directory (no name-surrogate tag) with the
// exact name, owned by Administrators or SYSTEM, with a protected DACL that
// grants only those two. Anything else is a *RootError and the root is left
// exactly as it is: the agent never repairs it.
func OpenRoot(path string, o Options) (*Root, error) {
	lab, err := labSID(o)
	if err != nil {
		return nil, err
	}
	h, err := openNoFollow(path, holdAccess|windows.READ_CONTROL, shareRW)
	if notFound(err) {
		if cerr := createRoot(path, lab); cerr != nil && !errors.Is(cerr, windows.ERROR_ALREADY_EXISTS) {
			return nil, &RootError{Check: CheckCreate}
		}
		h, err = openNoFollow(path, holdAccess|windows.READ_CONTROL, shareRW)
	}
	if err != nil {
		return nil, &RootError{Check: CheckOpen}
	}
	if err := checkRoot(h, path, lab); err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	return &Root{path: path, h: h}, nil
}

// VerifyRoot applies the root rule without creating anything: the start-up
// sweep's check (ADR 0027: a root that fails it is left alone and logged). An
// absent root is ErrNotExist.
func VerifyRoot(path string, o Options) (*Root, error) {
	lab, err := labSID(o)
	if err != nil {
		return nil, err
	}
	h, err := openNoFollow(path, holdAccess|windows.READ_CONTROL, shareRW)
	if notFound(err) {
		return nil, ErrNotExist
	}
	if err != nil {
		return nil, &RootError{Check: CheckOpen}
	}
	if err := checkRoot(h, path, lab); err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	return &Root{path: path, h: h}, nil
}

func labSID(o Options) (*windows.SID, error) {
	if o.LabTrustee == "" {
		return nil, nil
	}
	return windows.StringToSid(o.LabTrustee)
}

// createRoot creates the root with its descriptor already in place
// (CreateDirectory with SECURITY_ATTRIBUTES), so it is never open to anyone
// else, not even for a moment. CreateDirectory fails on any existing entry, a
// reparse point included, so nothing is adopted.
func createRoot(path string, lab *windows.SID) error {
	sddl := rootSDDL
	if lab != nil {
		// A non-elevated test cannot make Administrators the owner: the owner
		// is left to the caller, and the lab trustee is granted as well.
		sddl = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + lab.String() + ")"
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	p, err := u16(longPath(path))
	if err != nil {
		return err
	}
	sa := windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))
	return windows.CreateDirectory(p, &sa)
}

// checkRoot is the root rule on an open handle.
func checkRoot(h windows.Handle, path string, lab *windows.SID) error {
	ti, err := tagOf(h)
	if err != nil {
		return &RootError{Check: CheckOpen}
	}
	if ti.surrogate() {
		return &RootError{Check: CheckSurrogate}
	}
	if !ti.dir() {
		return &RootError{Check: CheckNotDirectory}
	}
	want := path[strings.LastIndexByte(strings.TrimRight(path, `\`), '\\')+1:]
	if got, err := finalName(h); err != nil || got != want {
		return &RootError{Check: CheckName}
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return &RootError{Check: CheckOpen}
	}
	owner, _, err := sd.Owner()
	if err != nil || !trusted(owner, lab) {
		return &RootError{Check: CheckOwner}
	}
	ctl, _, err := sd.Control()
	if err != nil || ctl&windows.SE_DACL_PROTECTED == 0 {
		return &RootError{Check: CheckNotProtected}
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return &RootError{Check: CheckNullDACL}
	}
	if !grantsOnly(dacl, lab) {
		return &RootError{Check: CheckGrantsOther}
	}
	return nil
}

// trusted reports Administrators, SYSTEM or the lab trustee.
func trusted(sid *windows.SID, lab *windows.SID) bool {
	if sid == nil {
		return false
	}
	return sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) ||
		(lab != nil && sid.Equals(lab))
}

// grantsOnly reports whether every ACE that grants is for a trusted SID. A
// deny ACE grants nothing and is allowed; any ACE type other than plain allow
// and deny (object, callback, compound) is refused.
func grantsOnly(dacl *windows.ACL, lab *windows.SID) bool {
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(dacl, i, &ace) != nil {
			return false
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			if !trusted((*windows.SID)(unsafe.Pointer(&ace.SidStart)), lab) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// MakeChild creates the folder names[len-1] fresh below the held root, after
// opening each earlier name relative to its held parent (never by a full
// path, fix batch 2): a restore's own <restore id> folder (names = id) or the
// side folder's <L> folder (names = id, L). ADR 0027: created fresh; if it
// already exists, the restore is refused, so anything at the name, a reparse
// point included, is ErrExists. The new folder inherits from its parent. It
// returns the folder's path (for restic's --target).
func (r *Root) MakeChild(names ...string) (string, error) {
	if len(names) == 0 {
		return "", ErrExists
	}
	parent := r.h
	path := r.path
	for i, n := range names {
		path = join(path, n)
		if i == len(names)-1 {
			h, err := createDirAt(parent, n, holdAccess, shareRW)
			if parent != r.h {
				_ = windows.CloseHandle(parent)
			}
			if existsErr(err) {
				return "", ErrExists
			}
			if err != nil {
				return "", err
			}
			_ = windows.CloseHandle(h)
			return path, nil
		}
		h, err := openAt(parent, n, holdAccess, shareRW)
		if parent != r.h {
			_ = windows.CloseHandle(parent)
		}
		if err != nil {
			return "", err
		}
		if ti, err := tagOf(h); err != nil || ti.reparse() || !ti.dir() {
			_ = windows.CloseHandle(h)
			return "", errNotRealDir
		}
		parent = h
	}
	return path, nil
}

// RemoveChild removes the entry name below the held root (and everything
// below it) with the clean-up walk, opened relative to the root's handle. A
// missing name is not an error. It returns the reparse points deleted.
func (r *Root) RemoveChild(name string) (int, error) {
	links := 0
	err := removeAt(r.h, name, &links)
	if notFound(err) {
		err = nil
	}
	return links, err
}

// Children lists the names directly under the held root, through its handle.
func (r *Root) Children() ([]string, error) {
	ents, err := listDir(r.h)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.name)
	}
	return out, nil
}
