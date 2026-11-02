package gateway

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// FileName is the deployed htpasswd file inside the sync directory.
const FileName = "htpasswd"

// fileMode is the mode of the deployed file: the console owns it, the gateway
// container reads it through a group.
const fileMode fs.FileMode = 0o640

// ErrNewerGeneration is returned by SyncIfOwner when the deployed file's
// generation header is newer than the caller's own view (architecture 7.2,
// "two consoles"). The caller raises an alarm instead of overwriting.
var ErrNewerGeneration = errors.New("gateway: deployed htpasswd generation is newer than this console's")

// Sync atomically replaces <dir>/htpasswd with content: it writes
// "<dir>/htpasswd.tmp-<random>" (created exclusively, mode 0640), fsyncs it,
// chmods it to 0640 regardless of umask, renames it over the target and, on
// POSIX systems, fsyncs the directory. content must be a well-formed rendering
// (Parse must accept it), so a nil or truncated slice can never be deployed.
//
// On POSIX the rename is atomic: a concurrent reader sees the old file or the
// new file, never a partial one. On Windows os.Rename is MoveFileEx with
// MOVEFILE_REPLACE_EXISTING, which replaces in one step but fails with a
// sharing violation while another process holds the target open without
// FILE_SHARE_DELETE; Sync retries briefly, then returns the error. A failed
// Sync leaves the previous file in place and removes its temp file; a crash
// can leave an orphan "htpasswd.tmp-*" that is harmless to the gateway.
func Sync(dir string, content []byte) error {
	if _, _, _, err := Parse(content); err != nil {
		return fmt.Errorf("gateway: refusing to deploy invalid content: %w", err)
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return fmt.Errorf("gateway: random temp name: %w", err)
	}
	tmp := filepath.Join(dir, FileName+".tmp-"+hex.EncodeToString(rnd[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return fmt.Errorf("gateway: creating temp file: %w", err)
	}
	cleanup := func() { _ = os.Remove(tmp) }
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("gateway: writing temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("gateway: fsync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("gateway: closing temp file: %w", err)
	}
	if err := os.Chmod(tmp, fileMode); err != nil {
		cleanup()
		return fmt.Errorf("gateway: chmod temp file: %w", err)
	}
	if err := rename(tmp, filepath.Join(dir, FileName)); err != nil {
		cleanup()
		return fmt.Errorf("gateway: renaming temp file: %w", err)
	}
	if runtime.GOOS != "windows" {
		if d, err := os.Open(dir); err == nil {
			err = d.Sync()
			_ = d.Close()
			if err != nil {
				return fmt.Errorf("gateway: fsync directory: %w", err)
			}
		}
	}
	return nil
}

// rename is os.Rename, retried on Windows where a reader that holds the target
// open makes MoveFileEx fail with a sharing violation.
func rename(from, to string) error {
	attempts := 1
	if runtime.GOOS == "windows" {
		attempts = 200
	}
	var err error
	for i := 0; i < attempts; i++ {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return err
}

// Current reads the header of the deployed file and returns its generation and
// the body hash it claims. It does not verify the hash against the body (Drifted
// compares the real body). When no file is deployed the error satisfies
// errors.Is(err, fs.ErrNotExist); a file with a missing or malformed header
// yields an error satisfying errors.Is(err, ErrFormat).
func Current(dir string) (generation int64, bodySHAHex string, err error) {
	b, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		return 0, "", err
	}
	gen, sha, _, err := splitHeader(b)
	if err != nil {
		return 0, "", err
	}
	return gen, sha, nil
}

// SyncIfOwner is Sync guarded by the two-consoles check of architecture 7.2: it
// returns an error wrapping ErrNewerGeneration, and writes nothing, when the
// deployed file's generation is greater than myGeneration. An absent file, an
// equal or older generation, or a deployed file with no valid header (which
// cannot belong to a newer console) is overwritten. content must have been
// rendered with myGeneration. The check and the write are not atomic with
// each other; the caller holds the htpasswd-sync lease.
func SyncIfOwner(dir string, content []byte, myGeneration int64) error {
	gen, _, _, err := Parse(content)
	if err != nil {
		return fmt.Errorf("gateway: refusing to deploy invalid content: %w", err)
	}
	if gen != myGeneration {
		return fmt.Errorf("gateway: content generation %d does not match owner generation %d", gen, myGeneration)
	}
	deployed, _, err := Current(dir)
	switch {
	case err == nil:
		if deployed > myGeneration {
			return fmt.Errorf("%w: deployed %d, this console %d", ErrNewerGeneration, deployed, myGeneration)
		}
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, ErrFormat):
	default:
		return err
	}
	return Sync(dir, content)
}

// Drifted reports whether the deployed file's body differs from expected's
// body (the reconcile check). It compares the real bytes, so a hand edit or a
// truncation is drift even if the header still claims the right hash, and the
// generation number alone is not drift. An absent or header-less deployed file
// is drift. expected must be a well-formed rendering.
func Drifted(dir string, expected []byte) (bool, error) {
	_, _, want, err := splitHeader(expected)
	if err != nil {
		return false, fmt.Errorf("gateway: invalid expected content: %w", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, FileName))
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	_, _, got, err := splitHeader(b)
	if err != nil {
		return true, nil
	}
	return !bytes.Equal(got, want), nil
}
