package escrow

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// WrapAlg is the value of repo_key_escrow.wrap_alg for keys wrapped here.
const WrapAlg = "aes-256-gcm+hkdf-sha256/v1"

// MasterKeyLen is the master key length in bytes.
const MasterKeyLen = 32

// Wrap contexts (HKDF info prefixes). The repository-key context is the one
// architecture §8.2 and ticket P1-T5 fix; the others follow the same pattern.
const (
	ContextRepoKey          = "cryobackup/escrow/v1"
	ContextGatewaySecret    = "cryobackup/gateway-secret/v1"
	ContextRestoreSelection = "cryobackup/restore/v1"
	contextDownload         = "cryobackup/download/v1"
)

// Errors. None of them carries key material or a path.
var (
	// ErrMasterKeyUnavailable means no master key is loaded (banner C12).
	ErrMasterKeyUnavailable = errors.New("escrow: master key unavailable")
	// ErrDecrypt means the blob is not valid under this key and context: wrong
	// master key, wrong device or version (AAD), a flipped bit or a truncated blob.
	ErrDecrypt = errors.New("escrow: cannot decrypt")
	// ErrCustomerHeld is returned for a device whose key the customer holds.
	ErrCustomerHeld = errors.New("escrow: repository key is held by the customer")
	// ErrKeyFile means the master key file is malformed (not 32 bytes of base64).
	ErrKeyFile = errors.New("escrow: master key file is malformed")
)

// MasterKey is the console master key. The zero value and nil are "no key";
// every method on them returns ErrMasterKeyUnavailable.
type MasterKey struct{ key []byte }

// NewMasterKey wraps 32 bytes.
func NewMasterKey(b []byte) (*MasterKey, error) {
	if len(b) != MasterKeyLen {
		return nil, ErrKeyFile
	}
	return &MasterKey{key: append([]byte(nil), b...)}, nil
}

// LoadMasterKey reads the master key file: 32 random bytes, base64 (standard,
// padded), one line.
func LoadMasterKey(path string) (*MasterKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, ErrMasterKeyUnavailable // the path is not echoed
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, ErrKeyFile
	}
	return NewMasterKey(raw)
}

// GenerateMasterKey creates path (exclusively, mode 0600) holding a fresh key
// read from r (nil means crypto/rand). It refuses to overwrite an existing file.
func GenerateMasterKey(path string, r io.Reader) (*MasterKey, error) {
	if r == nil {
		r = rand.Reader
	}
	b := make([]byte, MasterKeyLen)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, fmt.Errorf("escrow: reading random bytes: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, errors.New("escrow: master key file already exists")
		}
		return nil, errors.New("escrow: cannot create the master key file")
	}
	if _, err := f.WriteString(base64.StdEncoding.EncodeToString(b) + "\n"); err != nil {
		_ = f.Close()
		return nil, errors.New("escrow: cannot write the master key file")
	}
	if err := f.Close(); err != nil {
		return nil, errors.New("escrow: cannot write the master key file")
	}
	return NewMasterKey(b)
}

// Loaded reports whether the key is present.
func (m *MasterKey) Loaded() bool { return m != nil && len(m.key) == MasterKeyLen }

// KeyID is hex(sha256(master))[:16]: stored in repo_key_escrow.master_key_id to
// detect a generation mismatch (banner C12) without revealing the key.
func (m *MasterKey) KeyID() string {
	if !m.Loaded() {
		return ""
	}
	sum := sha256.Sum256(m.key)
	return hex.EncodeToString(sum[:])[:16]
}

// Bytes returns a copy of the key for the few derivations that take it (the
// gateway probe password). Callers must not keep it.
func (m *MasterKey) Bytes() []byte {
	if !m.Loaded() {
		return nil
	}
	return append([]byte(nil), m.key...)
}

func (m *MasterKey) derive(info string) ([]byte, error) {
	if !m.Loaded() {
		return nil, ErrMasterKeyUnavailable
	}
	return hkdf.Key(sha256.New, m.key, nil, info, 32)
}

// Wrap encrypts plaintext with AES-256-GCM under a per-row key
// HKDF-SHA256(master, salt = nil, info = context + "/" + id + "/" + version),
// a fresh 12-byte random nonce prepended to the ciphertext and AAD
// "id|version". For a repository key that is context ContextRepoKey and id the
// device id, as in architecture §8.2.
func (m *MasterKey) Wrap(context, id string, version int, plaintext []byte) ([]byte, error) {
	k, err := m.derive(infoFor(context, id, version))
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(k)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("escrow: reading random bytes: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, aadFor(id, version)), nil
}

// Unwrap reverses Wrap. A wrong key, id, version or context, a flipped bit and
// a short blob all give ErrDecrypt.
func (m *MasterKey) Unwrap(context, id string, version int, wrapped []byte) ([]byte, error) {
	k, err := m.derive(infoFor(context, id, version))
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(k)
	if err != nil {
		return nil, err
	}
	if len(wrapped) < gcm.NonceSize()+gcm.Overhead() {
		return nil, ErrDecrypt
	}
	nonce, ct := wrapped[:gcm.NonceSize()], wrapped[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, aadFor(id, version))
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// WrapRepoKey wraps a device's repository password for repo_key_escrow.
func (m *MasterKey) WrapRepoKey(deviceID string, keyVersion int, password []byte) ([]byte, error) {
	return m.Wrap(ContextRepoKey, deviceID, keyVersion, password)
}

// UnwrapRepoKey reverses WrapRepoKey.
func (m *MasterKey) UnwrapRepoKey(deviceID string, keyVersion int, wrapped []byte) ([]byte, error) {
	return m.Unwrap(ContextRepoKey, deviceID, keyVersion, wrapped)
}

// DownloadKey derives the HMAC key for single-use restore download tokens.
func (m *MasterKey) DownloadKey() ([]byte, error) { return m.derive(contextDownload) }

func infoFor(context, id string, version int) string {
	return context + "/" + id + "/" + strconv.Itoa(version)
}

func aadFor(id string, version int) []byte { return []byte(id + "|" + strconv.Itoa(version)) }

func newGCM(key []byte) (cipher.AEAD, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}
