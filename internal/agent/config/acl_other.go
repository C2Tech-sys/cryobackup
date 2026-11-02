//go:build !windows

package config

import (
	"errors"
	"fmt"
	"os"
)

// Off Windows (CI and development) there is no SYSTEM/Administrators ACL: the
// state directory is never "protected", so writing secrets into it needs the
// lab flag, exactly as an unelevated Windows run does.

func canRestrict() bool { return false }

// CanRestrict is always false off Windows: nothing is ever restricted or
// verified there.
func CanRestrict() bool { return false }

func isReparse(st os.FileInfo) bool { return st.Mode()&os.ModeSymlink != 0 }

// RestrictDir has no meaning off Windows; it reports ErrACLNotApplied so
// Ensure treats it as "not applied".
func RestrictDir(path string) error {
	return fmt.Errorf("%w: no SYSTEM/Administrators ACL on this platform", ErrACLNotApplied)
}

// VerifyDir cannot verify anything off Windows: it reports ErrACLNotApplied,
// which callers treat as "not protected".
func VerifyDir(path string) error {
	return fmt.Errorf("%w: no SYSTEM/Administrators ACL on this platform", ErrACLNotApplied)
}

func checkRootOwner(root string) error { return nil }

// checkOwnerLab has no owner rule off Windows (CI only).
func checkOwnerLab(path string) error { return nil }

func createRestrictedDir(path string) error { return os.Mkdir(path, 0o700) }

func withQuarantinePrivileges(f func() error) error { return f() }

// moveAside renames dir to target (rename(2) moves the entry itself and never
// follows a link). An existing target is ErrQuarantineExists (QuarantineRoot
// then retries with a fresh name); off Windows (CI only) that check and the
// rename are not atomic. There is no ACL to secure the quarantine with.
func moveAside(dir, target string) (secureErr, err error) {
	st, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("config: inspect the state root: %w", bareErr(err))
	}
	if isReparse(st) {
		return nil, fmt.Errorf("%w: the state root", ErrReparsePoint)
	}
	if !st.IsDir() {
		return nil, errors.New("config: the state root is not a directory")
	}
	if _, err := os.Lstat(target); err == nil {
		return nil, ErrQuarantineExists
	}
	if err := os.Rename(dir, target); err != nil {
		return nil, fmt.Errorf("config: move the state root aside: %w", bareErr(err))
	}
	return fmt.Errorf("%w: no ACL to restrict the quarantine with on this platform", ErrACLNotApplied), nil
}

// ReadOwnerAndDACL has no meaning off Windows: it reports ErrACLNotApplied.
func ReadOwnerAndDACL(path string) (string, error) {
	return "", fmt.Errorf("%w: no SYSTEM/Administrators ACL on this platform", ErrACLNotApplied)
}
