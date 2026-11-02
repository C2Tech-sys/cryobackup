//go:build !windows

package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// VerdictSource names the store in messages and "status".
const VerdictSource = "verdict file"

// verdictSuffix names the CI and lab stand-in for the HKLM verdict (there is
// no HKLM off Windows): <name>.untrusted-verdict next to the state root.
const verdictSuffix = ".untrusted-verdict"

// verdictOwnerOK decides whose verdict file is honoured: root's only. This
// package's tests accept their own uid.
var verdictOwnerOK = func(uid uint32) bool { return uid == 0 }

func verdictFile(root string) string {
	return filepath.Join(filepath.Dir(root), filepath.Base(root)+verdictSuffix)
}

// VerdictLocation names where root's verdict is kept, without root's path
// (for "status"): the stand-in file next to the state directory, and the
// directory's VerdictID.
func VerdictLocation(root string) string {
	return "the " + verdictSuffix[1:] + " file next to the state directory (id " + VerdictID(root) + ")"
}

// ReadVerdict reads the stand-in verdict file. It is honoured only when it is
// a regular file owned by uid 0; anything else at the path is ignored, so an
// unprivileged account cannot plant a verdict. One this process may not read
// still stands. A failure to inspect the path other than "does not exist" is
// an error (the caller refuses).
func ReadVerdict(root string) (Verdict, bool, error) {
	p := verdictFile(root)
	st, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return Verdict{}, false, nil
	}
	if err != nil {
		return Verdict{}, false, fmt.Errorf("config: inspect the untrusted verdict file: %w", bareErr(err))
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !st.Mode().IsRegular() || !ok || !verdictOwnerOK(sys.Uid) {
		return Verdict{}, false, nil
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return Verdict{Reason: "the verdict file cannot be read by this process"}, true, nil
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4*maxVerdictReason))
	if err != nil {
		return Verdict{Reason: "the verdict file cannot be read by this process"}, true, nil
	}
	return parseVerdict(string(b)), true, nil
}

// WriteVerdict creates the verdict file with mode 0600 (O_EXCL and
// O_NOFOLLOW: whatever already exists at the path is left as it is and counts
// only if ReadVerdict honours it). It is honoured only when the writer was
// root.
func WriteVerdict(root string, v Verdict) error {
	f, err := os.OpenFile(verdictFile(root), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("config: write the untrusted verdict file: %w", bareErr(err))
	}
	_, werr := f.WriteString(formatVerdict(v) + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("config: write the untrusted verdict file: %w", bareErr(werr))
	}
	return nil
}

// ClearVerdict removes the verdict file (the entry itself; a link is not
// followed). A missing file is not an error.
func ClearVerdict(root string) error {
	if err := os.Remove(verdictFile(root)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("config: clear the untrusted verdict file: %w", bareErr(err))
	}
	return nil
}
