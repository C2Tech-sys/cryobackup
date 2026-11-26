// Package restoreplace is the Windows-only file-system half of a restore to the
// original device (ADR 0027): the staging and side roots, the agent's own
// clean-up walk, the placement of staged files at their original location, the
// side folder's inherit reset, and the free-space figure. restic writes the
// files; this package only creates folders, moves files, sets descriptors and
// deletes what the agent itself made. It never reads a repository.
//
// Every rule here is a rule of ADR 0027 ("Original location: restore into
// staging, then place", "Side folder", "Rules shared by both device
// destinations"); the function comments name the rule they implement.
//
// Nothing in this package logs. Errors carry no path and no file name: a
// RootError names the failed check, a placement error is counted by Class, and
// any other error is a fixed phrase wrapping a Windows error number. The caller
// decides what goes to the device log and what (counts only) leaves the device.
//
// On other platforms every entry point returns ErrUnsupported: the agent refuses
// a device restore there rather than guessing at another file system.
package restoreplace

import (
	"errors"
	"sort"
)

// ErrUnsupported is returned by every entry point on a platform other than
// Windows.
var ErrUnsupported = errors.New("restoreplace: device restore is only implemented on Windows")

// ErrExists is returned by CreateFresh when something already has the name
// (ADR 0027: each restore's own folder is created fresh; if it already exists,
// the restore is refused).
var ErrExists = errors.New("restoreplace: the folder already exists")

// ErrNotExist is returned by VerifyRoot when the root is absent.
var ErrNotExist = errors.New("restoreplace: the root does not exist")

// Class is the countable class of an item that was not placed. The values are
// the countable api.RestoreResultClass words, so the caller copies them
// without a table (the agent's tests pin that they match).
type Class string

// Placement classes (ADR 0027 "Errors are classified on the device").
const (
	// ClassGuardRefused: a name surrogate on the destination path, a file
	// where a folder is (or the reverse), a name that matched only inexactly,
	// or an entry in staging that no selected path explains.
	ClassGuardRefused Class = "guard_refused"
	// ClassAccessDenied: Windows refused the move, or replace mode met a
	// read-only or system file (the agent never clears those attributes).
	ClassAccessDenied Class = "access_denied"
	// ClassTargetLocked: another program held the file open.
	ClassTargetLocked Class = "target_locked"
	// ClassOther: any other failure, counted so that nothing vanishes.
	ClassOther Class = "other"
)

// Mode is what placement does with a file that already exists.
type Mode int

const (
	// Keep leaves an existing file alone and counts the staged one as
	// skipped (ADR 0027 destination 1, the default).
	Keep Mode = iota
	// Replace replaces an existing regular file the agent checked; read-only
	// and system files are counted under access_denied (destination 3).
	Replace
)

// Counts is what a placement (or a side-folder count) did, by item. Restored
// and Skipped count files, plus each selected folder itself (restored when the
// agent created it, skipped when it already existed); folders above a selected
// item and below a selected folder are not counted. Errors counts items by
// Class. No field can hold a path.
type Counts struct {
	Restored      int64
	Skipped       int64
	BytesRestored uint64
	BytesSkipped  uint64
	Errors        map[Class]int64
	// Alarms counts moved files that failed a check after the move: their
	// final path, read through their handle, was not the intended one (ADR
	// 0027's fallback check, fix batch 3 T4), or they had gained another link
	// before their descriptor was rewritten (P22-D2 S1). Each is also counted
	// under guard_refused, and the caller raises the alarm.
	Alarms int64
}

// Add counts n items under class c.
func (c *Counts) Add(cl Class, n int64) {
	if n <= 0 {
		return
	}
	if c.Errors == nil {
		c.Errors = map[Class]int64{}
	}
	c.Errors[cl] += n
}

// Merge adds o into c.
func (c *Counts) Merge(o Counts) {
	c.Restored += o.Restored
	c.Skipped += o.Skipped
	c.BytesRestored += o.BytesRestored
	c.BytesSkipped += o.BytesSkipped
	c.Alarms += o.Alarms
	for k, v := range o.Errors {
		c.Add(k, v)
	}
}

// ErrorTotal is the number of items counted under any class.
func (c Counts) ErrorTotal() int64 {
	var n int64
	for _, v := range c.Errors {
		n += v
	}
	return n
}

// Classes returns the classes with a count, in a stable order.
func (c Counts) Classes() []Class {
	out := make([]Class, 0, len(c.Errors))
	for k, v := range c.Errors {
		if v > 0 {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// RootError says that a staging or side root failed the root rule of ADR 0027
// ("The staging and side roots"). Check is a fixed word naming the failed
// check, for the device log line the ADR asks for; it carries no path. The
// agent never repairs such a root: an operator removes it.
type RootError struct {
	Check string
}

func (e *RootError) Error() string { return "restoreplace: the root fails the root rule: " + e.Check }

// Root checks, as RootError.Check.
const (
	CheckOpen         = "cannot_open"
	CheckNotDirectory = "not_a_directory"
	CheckSurrogate    = "name_surrogate"
	CheckName         = "name_mismatch"
	CheckOwner        = "owner_not_administrators_or_system"
	CheckNotProtected = "dacl_not_protected"
	CheckGrantsOther  = "dacl_grants_other_trustee"
	CheckNullDACL     = "dacl_missing"
	CheckCreate       = "cannot_create"
)

// Options tunes the root rule for tests that run without elevation. Production
// passes the zero value.
type Options struct {
	// LabTrustee is a SID string (S-1-5-21-...) that the root rule accepts as
	// owner and grantee in addition to Administrators and SYSTEM, and that a
	// created root grants as well. It exists only so a non-elevated test can
	// create and use a root; the agent sets it only from a test hook.
	LabTrustee string
}

// Rel is one selected path below a drive root, split into exact components
// ("/C/Users/x/a.txt" with drive "C" is {"Users", "x", "a.txt"}).
type Rel []string

// relation is how a path (as components below the drive root) stands to the
// selection: at or below a selected path, a strict ancestor of one, or
// neither.
type relation int

const (
	relNone relation = iota
	relAncestor
	relUnder
)

// relate compares exactly: byte for byte, no case folding (ADR 0027: names
// stay byte-exact). selected reports whether p equals a selected path.
func relate(p []string, sel []Rel) (rel relation, selected bool) {
	rel = relNone
	for _, s := range sel {
		n := len(s)
		if len(p) < n {
			if prefix(s, p) {
				if rel == relNone {
					rel = relAncestor
				}
			}
			continue
		}
		if prefix(p, s) {
			rel = relUnder
			if len(p) == n {
				selected = true
			}
		}
	}
	return rel, selected
}

// prefix reports whether the first len(pre) components of p equal pre.
func prefix(p, pre []string) bool {
	if len(pre) > len(p) {
		return false
	}
	for i := range pre {
		if p[i] != pre[i] {
			return false
		}
	}
	return true
}
