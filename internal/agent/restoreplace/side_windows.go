//go:build windows

package restoreplace

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ErrSideCheck: the file checked after the side folder's reset does not carry
// only inherited entries for Administrators and SYSTEM.
var ErrSideCheck = errors.New("restoreplace: the side folder's check file is not reset to inherit")

// ResetInherit resets every entry below top (not top itself) to inherit from
// its parent, with nothing explicit, top-down (ADR 0027, "Side folder": the
// subtree inherits from the side folder, Administrators and SYSTEM only). It
// is the agent's own walk: every entry is opened without following it, a
// reparse point met on the way is deleted, never followed, and counted in
// links (one planted after RemoveReparsePoints ran is still never followed).
// The new DACL of each entry is computed here from its parent's DACL and set
// with SetKernelObjectSecurity through the entry's own handle, which
// propagates nothing to children: no API walks the tree on the agent's behalf
// (icacls /T follows junctions, p22-spike.md:435-441).
//
// It returns the path of one file it reset (for CheckInherited), "" when the
// tree holds no file.
func ResetInherit(top string) (checkFile string, links int, err error) {
	h, err := openNoFollow(top, holdAccess|windows.READ_CONTROL, shareRW)
	if err != nil {
		return "", 0, err
	}
	defer windows.CloseHandle(h)
	if ti, err := tagOf(h); err != nil || ti.reparse() || !ti.dir() {
		return "", 0, errNotRealDir
	}
	parent, err := daclOf(h)
	if err != nil {
		return "", 0, err
	}
	r := &resetter{}
	err = r.dir(h, top, parent)
	return r.check, r.links, err
}

type resetter struct {
	check string
	links int
}

const resetAccess = holdAccess | windows.READ_CONTROL | windows.WRITE_DAC | windows.DELETE

func (r *resetter) dir(h windows.Handle, path string, parentDACL *windows.ACL) error {
	ents, err := listDir(h)
	if err != nil {
		return err
	}
	for _, e := range ents {
		p := join(path, e.name)
		ch, err := openNoFollow(p, resetAccess, shareRW)
		if err != nil {
			ch, err = openNoFollow(p, windows.READ_CONTROL|windows.WRITE_DAC|windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, shareRW)
			if err != nil {
				return err
			}
		}
		err = r.entry(ch, p, parentDACL)
		_ = windows.CloseHandle(ch)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *resetter) entry(h windows.Handle, path string, parentDACL *windows.ACL) error {
	ti, err := tagOf(h)
	if err != nil {
		return err
	}
	if ti.reparse() {
		if err := deleteHandle(h); err != nil {
			return err
		}
		r.links++
		return nil
	}
	acl := inheritedFrom(parentDACL, ti.dir())
	sd, err := windows.NewSecurityDescriptor()
	if err != nil {
		return err
	}
	if err := sd.SetDACL(acl, true, false); err != nil {
		return err
	}
	// Auto-inherited and not protected: the entries are marked inherited and
	// a later inherit-aware tool treats them as such.
	if err := sd.SetControl(windows.SE_DACL_AUTO_INHERITED|windows.SE_DACL_PROTECTED, windows.SE_DACL_AUTO_INHERITED); err != nil {
		return err
	}
	if err := windows.SetKernelObjectSecurity(h, windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, sd); err != nil {
		return err
	}
	if !ti.dir() {
		if r.check == "" {
			r.check = path
		}
		return nil
	}
	return r.dir(h, path, acl)
}

// daclOf reads a DACL through the handle.
func daclOf(h windows.Handle) (*windows.ACL, error) {
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return nil, err
	}
	// Copy: sd is garbage-collected memory the ACL points into.
	return copyACL(dacl), nil
}

func copyACL(a *windows.ACL) *windows.ACL {
	if a == nil {
		return buildACL(nil)
	}
	var aces [][]byte
	for i := uint32(0); i < uint32(a.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(a, i, &ace) != nil {
			continue
		}
		aces = append(aces, append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(ace)), int(ace.Header.AceSize))...))
	}
	return buildACL(aces)
}

// Inheritance flags of an ACE header.
const (
	aceOI = 0x1 // OBJECT_INHERIT_ACE
	aceCI = 0x2 // CONTAINER_INHERIT_ACE
	aceNP = 0x4 // NO_PROPAGATE_INHERIT_ACE
	aceIO = 0x8 // INHERIT_ONLY_ACE
)

// File generic rights mapping (FILE_GENERIC_*), for an inherited ACE that
// becomes effective.
const (
	fileGenericRead    = 0x00120089
	fileGenericWrite   = 0x00120116
	fileGenericExecute = 0x001200A0
	fileAllAccess      = 0x001F01FF
)

func mapGeneric(m uint32) uint32 {
	out := m &^ (windows.GENERIC_READ | windows.GENERIC_WRITE | windows.GENERIC_EXECUTE | windows.GENERIC_ALL)
	if m&windows.GENERIC_READ != 0 {
		out |= fileGenericRead
	}
	if m&windows.GENERIC_WRITE != 0 {
		out |= fileGenericWrite
	}
	if m&windows.GENERIC_EXECUTE != 0 {
		out |= fileGenericExecute
	}
	if m&windows.GENERIC_ALL != 0 {
		out |= fileAllAccess
	}
	return out
}

// inheritedFrom computes the DACL a child (a folder when dir) inherits from
// a parent DACL, under the Windows inheritance rules: object-inherit ACEs
// reach files, container-inherit ACEs reach folders (and keep propagating
// unless no-propagate is set), an object-inherit ACE reaches a folder as
// inherit-only so its files get it. Every resulting ACE is marked inherited.
// Only plain allow and deny ACEs are carried; ACEs for CREATOR OWNER and
// CREATOR GROUP are dropped (the side root has none, and the agent does not
// substitute owners).
func inheritedFrom(parent *windows.ACL, dir bool) *windows.ACL {
	creatorOwner, _ := windows.CreateWellKnownSid(windows.WinCreatorOwnerSid)
	creatorGroup, _ := windows.CreateWellKnownSid(windows.WinCreatorGroupSid)
	var aces [][]byte
	for i := uint32(0); parent != nil && i < uint32(parent.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(parent, i, &ace) != nil {
			continue
		}
		if t := ace.Header.AceType; t != windows.ACCESS_ALLOWED_ACE_TYPE && t != windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if (creatorOwner != nil && sid.Equals(creatorOwner)) || (creatorGroup != nil && sid.Equals(creatorGroup)) {
			continue
		}
		f := ace.Header.AceFlags
		var flags uint8
		mask := uint32(ace.Mask)
		switch {
		case !dir && f&aceOI != 0:
			flags = windows.INHERITED_ACE
			mask = mapGeneric(mask)
		case dir && f&aceCI != 0 && f&aceNP != 0:
			flags = windows.INHERITED_ACE
			mask = mapGeneric(mask)
		case dir && f&aceCI != 0:
			flags = windows.INHERITED_ACE | (f & (aceOI | aceCI))
			mask = mapGeneric(mask)
		case dir && f&aceOI != 0 && f&aceNP == 0:
			flags = windows.INHERITED_ACE | aceOI | aceIO
		default:
			continue
		}
		raw := append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(ace)), int(ace.Header.AceSize))...)
		raw[1] = flags
		*(*uint32)(unsafe.Pointer(&raw[4])) = mask
		aces = append(aces, raw)
	}
	return buildACL(aces)
}

// CheckInherited is the check after the reset (ADR 0027: "It then checks one
// file"): the file's DACL holds no explicit entry, and every entry that grants
// is for Administrators, SYSTEM or the lab trustee.
func CheckInherited(path string, o Options) error {
	lab, err := labSID(o)
	if err != nil {
		return err
	}
	h, err := openNoFollow(path, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, shareAll)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount == 0 {
		return ErrSideCheck
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(dacl, i, &ace) != nil || ace.Header.AceFlags&windows.INHERITED_ACE == 0 {
			return ErrSideCheck
		}
	}
	if !grantsOnly(dacl, lab) {
		return ErrSideCheck
	}
	return nil
}

// CountSide counts what a side-folder restore left under top (<side>\<restore
// id>\<L>, mirroring the drive below its root): every file at or below a
// selected path is restored, a selected folder itself counts once, and an
// entry no selected path explains is guard_refused (ADR 0027: files kept in
// the side folder are restored). Reparse points must have been removed
// already; one met here is counted as skipped. Sizes are read through each
// file's handle.
func CountSide(top string, sel []Rel) (Counts, error) {
	var c Counts
	h, err := openNoFollow(top, holdAccess, shareRW)
	if err != nil {
		return c, err
	}
	defer windows.CloseHandle(h)
	err = countSide(h, top, nil, sel, &c)
	return c, err
}

func countSide(h windows.Handle, path string, rel []string, sel []Rel, c *Counts) error {
	ents, err := listDir(h)
	if err != nil {
		return err
	}
	for _, e := range ents {
		childRel := append(append([]string(nil), rel...), e.name)
		p := join(path, e.name)
		r, selected := relate(childRel, sel)
		if r == relNone {
			c.Add(ClassGuardRefused, countItems(p))
			continue
		}
		ch, err := openNoFollow(p, holdAccess, shareRW)
		if err != nil {
			ch, err = openNoFollow(p, windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, shareRW)
			if err != nil {
				c.Add(ClassOther, 1)
				continue
			}
		}
		ti, terr := tagOf(ch)
		switch {
		case terr != nil:
			c.Add(ClassOther, 1)
		case ti.reparse():
			c.Skipped++
		case ti.dir():
			if selected {
				c.Restored++
			}
			err = countSide(ch, p, childRel, sel, c)
		case r == relUnder:
			c.Restored++
			if fi, ferr := fileInfo(ch); ferr == nil {
				c.BytesRestored += sizeOf(fi)
			}
		default:
			c.Add(ClassGuardRefused, 1)
		}
		_ = windows.CloseHandle(ch)
		if err != nil {
			return err
		}
	}
	return nil
}
