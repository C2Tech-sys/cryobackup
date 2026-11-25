package maintain

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// hkdfOneBlock is HKDF-SHA256 for a 32-byte output written out by hand (RFC
// 5869: PRK = HMAC(salt, IKM) with a zero salt, OKM = HMAC(PRK, info || 0x01)),
// so the test does not check crypto/hkdf against itself.
func hkdfOneBlock(ikm []byte, info string) string {
	ext := hmac.New(sha256.New, make([]byte, sha256.Size))
	ext.Write(ikm)
	prk := ext.Sum(nil)
	exp := hmac.New(sha256.New, prk)
	exp.Write([]byte(info))
	exp.Write([]byte{1})
	return hex.EncodeToString(exp.Sum(nil))
}

// TestSelfBackupKeysFromMasterKeyFile: the password is HKDF(master key,
// cryobackup/self-backup/v1) as hex, separate from the probe repository's, the
// key id is the master key's, and VerifyEscrow unwraps a really wrapped row
// and refuses it under another master key.
func TestSelfBackupKeysFromMasterKeyFile(t *testing.T) {
	raw := make([]byte, escrow.MasterKeyLen)
	for i := range raw {
		raw[i] = byte(i*7 + 3)
	}
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(raw)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sk, err := NewSelfBackupKeys(path)
	if err != nil {
		t.Fatal(err)
	}
	pw, err := sk.SelfBackupPassword()
	if err != nil {
		t.Fatal(err)
	}
	if want := hkdfOneBlock(raw, "cryobackup/self-backup/v1"); pw != want {
		t.Fatalf("password = %s, want %s", pw, want)
	}
	if pw == hkdfOneBlock(raw, "cryobackup/probe-repo/v1") {
		t.Fatal("the self-backup password equals the probe repository's")
	}
	mk, _ := escrow.NewMasterKey(raw)
	if sk.MasterKeyID() != mk.KeyID() || sk.MasterKeyID() == "" {
		t.Fatalf("key id = %q, want %q", sk.MasterKeyID(), mk.KeyID())
	}
	if _, err := NewSelfBackupKeys(filepath.Join(t.TempDir(), "missing.key")); err == nil {
		t.Fatal("a missing key file loaded")
	}

	// A really wrapped escrow row in a fixtures store.
	e := newTenv(t, nil, nil)
	wrapped, err := mk.WrapRepoKey(e.esc, 1, []byte("repo-password"))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.Write(e.ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE repo_key_escrow SET wrapped_key = ?, master_key_id = ? WHERE device_id = ? AND retired_at IS NULL`, wrapped, mk.KeyID(), e.esc)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := sk.VerifyEscrow(e.ctx, e.st, e.esc); err != nil {
		t.Fatalf("VerifyEscrow with the right key: %v", err)
	}
	other := make([]byte, escrow.MasterKeyLen)
	omk, _ := escrow.NewMasterKey(other)
	if err := NewSelfBackupKeysFrom(omk).VerifyEscrow(e.ctx, e.st, e.esc); err == nil {
		t.Fatal("VerifyEscrow passed under another master key")
	}
	if _, err := SelfBackupPasswordFrom([]byte("short")); err == nil {
		t.Fatal("a short key derived a password")
	}
}
