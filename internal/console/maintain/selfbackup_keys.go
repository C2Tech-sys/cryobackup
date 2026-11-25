package maintain

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// SelfBackupKeyInfo is the HKDF info that domain-separates the self-backup
// repository's password from every other key derived from the master key
// (ADR 0029, as cryobackup/probe-repo/v1 for the probe repository, ADR 0017).
const SelfBackupKeyInfo = "cryobackup/self-backup/v1"

// SelfBackupKeyer is what the self-backup needs from the master key. A
// KeyUnwrapper may implement it; otherwise Run builds one from the master key
// file (NewSelfBackupKeys).
type SelfBackupKeyer interface {
	// SelfBackupPassword is the repository password: HKDF-SHA256(master key,
	// salt nil, info SelfBackupKeyInfo), 32 bytes, lower-case hex. Never stored:
	// the same master key always gives the same password, and a new master key
	// cannot open the old repository.
	SelfBackupPassword() (string, error)
	// MasterKeyID is the loaded master key's id (escrow.MasterKey.KeyID), for
	// the manifest; "" when no key is loaded.
	MasterKeyID() string
	// VerifyEscrow unwraps the active escrowed key of deviceID from st (a
	// restored copy of the database, opened read-only) with the loaded master
	// key and forgets the result. A nil error proves the restored copy's
	// wrapped keys open with this master key.
	VerifyEscrow(ctx context.Context, st *store.Store, deviceID string) error
}

// SelfBackupPasswordFrom derives the self-backup password from the raw master
// key bytes (the bare-host restore has the key file and no database).
func SelfBackupPasswordFrom(masterKey []byte) (string, error) {
	if len(masterKey) != escrow.MasterKeyLen {
		return "", errSelfBackupKey
	}
	k, err := hkdf.Key(sha256.New, masterKey, nil, SelfBackupKeyInfo, 32)
	if err != nil {
		return "", errSelfBackupKey
	}
	return hex.EncodeToString(k), nil
}

// errSelfBackupKey: no master key is loaded, or it has the wrong length.
var errSelfBackupKey = errors.New("maintain: the self-backup key is not available")

// masterSelfKeys is the SelfBackupKeyer over a loaded master key.
type masterSelfKeys struct{ mk *escrow.MasterKey }

// NewSelfBackupKeys loads the master key file (the same file the KeyUnwrapper
// reads; escrow.LoadMasterKey, whose errors never name the path) and returns
// the SelfBackupKeyer over it.
func NewSelfBackupKeys(masterKeyPath string) (SelfBackupKeyer, error) {
	mk, err := escrow.LoadMasterKey(masterKeyPath)
	if err != nil {
		return nil, err
	}
	return masterSelfKeys{mk: mk}, nil
}

// NewSelfBackupKeysFrom is NewSelfBackupKeys over a key already loaded.
func NewSelfBackupKeysFrom(mk *escrow.MasterKey) SelfBackupKeyer { return masterSelfKeys{mk: mk} }

func (k masterSelfKeys) SelfBackupPassword() (string, error) {
	if !k.mk.Loaded() {
		return "", errSelfBackupKey
	}
	return SelfBackupPasswordFrom(k.mk.Bytes())
}

func (k masterSelfKeys) MasterKeyID() string { return k.mk.KeyID() }

func (k masterSelfKeys) VerifyEscrow(ctx context.Context, st *store.Store, deviceID string) error {
	keys := &escrow.Keys{Master: k.mk, Store: st}
	_, err := keys.Unwrap(ctx, deviceID)
	return err
}
