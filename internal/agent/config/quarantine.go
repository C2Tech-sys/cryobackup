package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Quarantine names, next to the state root (in its parent directory, never
// inside the tree they are about).
const (
	// quarantineInfix joins the root's name and the UTC stamp of a quarantine:
	// C:\ProgramData\CryoBackup.quarantine-20261022T153000Z-0a1b2c3d.
	quarantineInfix = ".quarantine-"
	// QuarantineStamp is the layout of the UTC stamp in a quarantine's name.
	QuarantineStamp = "20060102T150405Z"
	// QuarantineAttempts bounds QuarantineRoot's renames: each attempt draws a
	// fresh random suffix, and only when every one of them finds its target
	// taken is the result ErrQuarantineExists.
	QuarantineAttempts = 5
	// quarantineSuffixLen is the length of the random suffix (8 hex digits).
	quarantineSuffixLen = 8
)

// quarantineSuffix draws the random part of a quarantine's name (4 bytes from
// crypto/rand as 8 lower-case hex digits), so the name cannot be predicted and
// squatted ahead of a repair. This package's tests replace it.
var quarantineSuffix = func() (string, error) {
	var b [quarantineSuffixLen / 2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("config: draw a quarantine name: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// All is the root followed by Children: the ten layout directories that
// every check covers.
func (l Layout) All() []string { return append([]string{l.Root}, l.Children()...) }

// Rel names a layout directory relative to the root ("." for the root), for
// messages that must not carry the state directory's own path.
func (l Layout) Rel(d string) string { return l.rel(d) }

// QuarantinePath is where QuarantineRoot moves the root at t with the random
// suffix: <parent>\<name>.quarantine-<UTC stamp>-<suffix>, a sibling of the
// root so the move stays on the same volume.
func (l Layout) QuarantinePath(t time.Time, suffix string) string {
	return filepath.Join(filepath.Dir(l.Root), filepath.Base(l.Root)+quarantineInfix+t.UTC().Format(QuarantineStamp)+"-"+suffix)
}

// Quarantines lists the quarantined trees next to the root as
// "<UTC stamp>-<8 hex>", oldest first (names only; nothing inside them is
// read). A stamp without the suffix (the shape before ticket P1-FIX10) is
// listed as the stamp alone; names of any other shape are not quarantines and
// are left out.
func (l Layout) Quarantines() []string {
	entries, err := os.ReadDir(filepath.Dir(l.Root))
	if err != nil {
		return nil
	}
	prefix := filepath.Base(l.Root) + quarantineInfix
	var out []string
	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		if id, ok := parseQuarantineID(n[len(prefix):]); ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// parseQuarantineID accepts "<stamp>-<8 hex>" and the older "<stamp>".
func parseQuarantineID(s string) (string, bool) {
	stamp, suffix, hasSuffix := strings.Cut(s, "-")
	if _, err := time.Parse(QuarantineStamp, stamp); err != nil || len(stamp) != len(QuarantineStamp) {
		return "", false
	}
	if !hasSuffix {
		return s, true
	}
	if len(suffix) != quarantineSuffixLen {
		return "", false
	}
	if _, err := hex.DecodeString(suffix); err != nil || strings.ToLower(suffix) != suffix {
		return "", false
	}
	return s, true
}

// Quarantine is what QuarantineRoot did.
type Quarantine struct {
	// Path is where the untrusted tree now is.
	Path string
	// SecureErr is non-nil when the quarantine's owner (Administrators) and
	// protected admin-only DACL could not be set. Best effort: the tree is
	// already out of the agent's way, so this is logged, not fatal.
	SecureErr error
}

// QuarantineRoot moves an untrusted state root aside, whole, to
// QuarantinePath(now, <8 random hex>) (architecture §6.5, tickets P1-FIX9 and
// P1-FIX10). Nothing in the tree is opened, re-ACLed or trusted: the caller
// creates a fresh root afterwards, and the device must be enrolled again.
//
// The tree is scanned first, without following anything, and refused (nothing
// moved) when the root or anything below it is a reparse point
// (ErrReparsePoint) or when it cannot be scanned. The move is then a rename
// through a handle opened on the root itself (no reparse point followed) with
// ReplaceIfExists = FALSE, never a copy, so it stays on the root's volume and
// never replaces anything. A target that already exists (someone created that
// name first) is retried with a fresh random suffix, up to QuarantineAttempts
// renames in all; only then is the result ErrQuarantineExists, and nothing was
// moved. The quarantine then gets the owner Administrators and a protected
// admin-only DACL on its top directory, best effort (Quarantine.SecureErr).
// An elevated caller's privileges (SeBackupPrivilege to scan a tree whose DACL
// shuts it out, SeRestorePrivilege and SeTakeOwnershipPrivilege to move and
// secure it) are enabled for the call only. Errors carry no path.
func QuarantineRoot(root string, now time.Time) (Quarantine, error) {
	var q Quarantine
	l := Layout{Root: root}
	if root == "" {
		return q, errors.New("config: empty state directory")
	}
	err := withQuarantinePrivileges(func() error {
		if err := scanForReparse(l); err != nil {
			return err
		}
		for attempt := 0; attempt < QuarantineAttempts; attempt++ {
			suffix, err := quarantineSuffix()
			if err != nil {
				return err
			}
			target := l.QuarantinePath(now, suffix)
			secureErr, err := moveAside(root, target)
			if errors.Is(err, ErrQuarantineExists) {
				continue
			}
			if err != nil {
				return err
			}
			q.Path, q.SecureErr = target, secureErr
			return nil
		}
		return fmt.Errorf("%w (%d names tried)", ErrQuarantineExists, QuarantineAttempts)
	})
	if err != nil {
		return Quarantine{}, err
	}
	return q, nil
}

// scanForReparse walks the tree without following anything and refuses the
// first reparse point (the root included).
func scanForReparse(l Layout) error {
	return filepath.WalkDir(l.Root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return fmt.Errorf("config: the state directory cannot be scanned (%s): %w", l.rel(p), bareErr(werr))
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("config: the state directory cannot be scanned (%s): %w", l.rel(p), bareErr(err))
		}
		if isReparse(info) {
			if p == l.Root {
				return fmt.Errorf("%w: the state root", ErrReparsePoint)
			}
			return fmt.Errorf("%w: found inside the state directory; nothing was moved (remove it by hand and repair again)", ErrReparsePoint)
		}
		return nil
	})
}

// bareErr drops the path an *fs.PathError or *os.LinkError carries.
func bareErr(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err
	}
	return err
}
