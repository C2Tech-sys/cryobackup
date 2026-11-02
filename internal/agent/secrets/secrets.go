package secrets

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
)

// File names of the secrets (architecture §6.5). Each file holds one
// protected value.
const (
	NameDeviceToken = "device.token"
	NameGatewayCred = "gateway.cred"
	NameRepoKey     = "repo.key"
)

var knownNames = map[string]bool{
	NameDeviceToken: true, NameGatewayCred: true, NameRepoKey: true,
}

// Errors.
var (
	// ErrNotFound is returned by Get when the secret does not exist.
	ErrNotFound = errors.New("secrets: not found")
	// ErrUnsupported is returned by New on a platform without DPAPI when the
	// insecure fallback was not requested.
	ErrUnsupported = errors.New("secrets: DPAPI is not available on this platform; the plaintext fallback needs --insecure-plaintext-secrets")
	// ErrPlaintextRefused is returned by New on Windows when the insecure
	// fallback is requested: DPAPI is always used there.
	ErrPlaintextRefused = errors.New("secrets: the plaintext fallback is refused on Windows")
	// ErrInvalidName is returned for a name outside the fixed set.
	ErrInvalidName = errors.New("secrets: unknown secret name")
	// ErrEmpty is returned by Put for an empty value.
	ErrEmpty = errors.New("secrets: empty secret")
	// ErrCorrupt is returned by Get when the file cannot be unprotected (wrong
	// machine, wrong protector, damaged file). The error carries no content.
	ErrCorrupt = errors.New("secrets: stored secret cannot be unprotected")
	// ErrUnprotectedDir is returned by Put when the secrets directory does not
	// carry the owner SYSTEM and the protected SYSTEM + Administrators DACL
	// (config.SDDLStateDir) and the lab flag --insecure-unprotected-state was
	// not given. Machine-scope DPAPI is only as strong as that ACL.
	ErrUnprotectedDir = errors.New("secrets: refusing to write a secret into a directory that is not restricted to SYSTEM and Administrators (lab only: --insecure-unprotected-state)")
)

// Options configures New.
type Options struct {
	// Dir is the secrets directory (config.Layout.SecretsDir).
	Dir string
	// InsecurePlaintext selects the plaintext fallback. It is honoured only on
	// platforms without DPAPI (Linux CI tests) and refused on Windows.
	InsecurePlaintext bool
	// AllowUnprotected lets Put write into a directory whose owner and DACL
	// are not config.SDDLStateDir (an unelevated lab run, or a platform
	// without ACLs). It is the lab flag --insecure-unprotected-state; without
	// it Put refuses with ErrUnprotectedDir.
	AllowUnprotected bool
}

type protector interface {
	protect(plain []byte) ([]byte, error)
	unprotect(blob []byte) ([]byte, error)
}

// Store keeps the device token, gateway password and repository password,
// protected at rest. Values exist in memory only while a caller holds them.
type Store struct {
	dir              string
	p                protector
	allowUnprotected bool
}

// New returns a Store over opts.Dir.
func New(o Options) (*Store, error) {
	if o.Dir == "" {
		return nil, errors.New("secrets: empty directory")
	}
	p, err := newProtector(o.InsecurePlaintext)
	if err != nil {
		return nil, err
	}
	return &Store{dir: o.Dir, p: p, allowUnprotected: o.AllowUnprotected}, nil
}

// verifyDir is config.VerifyDir (the owner and DACL read back from the
// directory itself, not what the caller believes it applied).
var verifyDir = config.VerifyDir

// checkDir refuses an unprotected secrets directory unless the lab flag was
// given. It runs before every write.
func (s *Store) checkDir() error {
	if s.allowUnprotected {
		return os.MkdirAll(s.dir, 0o700)
	}
	if err := verifyDir(s.dir); err != nil {
		return fmt.Errorf("%w (%v)", ErrUnprotectedDir, err)
	}
	return nil
}

// Insecure reports whether the store is the plaintext fallback.
func (s *Store) Insecure() bool {
	_, ok := s.p.(plainProtector)
	return ok
}

func (s *Store) path(name string) (string, error) {
	if !knownNames[name] {
		return "", ErrInvalidName
	}
	return filepath.Join(s.dir, name), nil
}

// Put protects and stores a secret, replacing the previous value atomically.
func (s *Store) Put(name string, value []byte) error {
	path, err := s.path(name)
	if err != nil {
		return err
	}
	if len(value) == 0 {
		return ErrEmpty
	}
	if err := s.checkDir(); err != nil {
		return err
	}
	blob, err := s.p.protect(value)
	if err != nil {
		return fmt.Errorf("secrets: protect %s: %w", name, err)
	}
	return config.WriteFileAtomic(path, blob, 0o600)
}

// Get returns the secret. The caller owns the returned slice and should drop it
// as soon as the child has been started.
func (s *Store) Get(name string) ([]byte, error) {
	path, err := s.path(name)
	if err != nil {
		return nil, err
	}
	blob, err := os.ReadFile(path) //nolint:gosec // path from the agent's own state directory or configuration
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("secrets: read %s: %w", name, err)
	}
	plain, err := s.p.unprotect(blob)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrCorrupt, name)
	}
	return plain, nil
}

// Has reports whether a secret exists (without decrypting it).
func (s *Store) Has(name string) bool {
	path, err := s.path(name)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// Delete removes a secret; a missing secret is not an error.
func (s *Store) Delete(name string) error {
	path, err := s.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// plainHeader marks a file written by the plaintext fallback so that it cannot
// be mistaken for a DPAPI blob (and the other way round).
const plainHeader = "CRYO-PLAINTEXT-SECRET-v1\n"

// plainProtector stores the value as is, behind a header. It exists for Linux
// CI tests (--insecure-plaintext-secrets) and is never selected on Windows.
type plainProtector struct{}

func (plainProtector) protect(plain []byte) ([]byte, error) {
	return append([]byte(plainHeader), plain...), nil
}

func (plainProtector) unprotect(blob []byte) ([]byte, error) {
	if !bytes.HasPrefix(blob, []byte(plainHeader)) {
		return nil, errors.New("not a plaintext secret file")
	}
	return bytes.Clone(blob[len(plainHeader):]), nil
}

// CleanString trims the newline a hand-edited secret file might carry.
func CleanString(b []byte) string { return strings.TrimSpace(string(b)) }
