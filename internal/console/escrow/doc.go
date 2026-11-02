// Package escrow holds the console master key and wraps what the console must
// be able to recover: per-device repository passwords (repo_key_escrow), the
// pending gateway password during delivery (gateway_logins.pending_secret_wrapped)
// and restore selections (restore_requests.selection_wrapped). Architecture
// §8.2, DH1, DH13, DH17, ticket P1-T5.
//
// # Master key
//
// A 32-byte random key in a file (master_key_path), base64, mode 0600, created
// by `cryo-console init` and never stored in the database. KeyID is
// hex(sha256(master))[:16] and is recorded with every wrapped row so a
// generation mismatch is detectable (banner C12) without revealing the key.
//
// # Wrap format
//
// Wrap derives a per-row key HKDF-SHA256(master, salt = nil,
// info = context + "/" + id + "/" + version), encrypts with AES-256-GCM using a
// fresh 12-byte random nonce that is prepended to the ciphertext, and binds the
// row with AAD "id|version". For a repository key the context is
// "cryobackup/escrow/v1", the id is the device id and wrap_alg is
// "aes-256-gcm+hkdf-sha256/v1". Tests carry a vector computed independently
// (Python hmac/hashlib HKDF and the cryptography package's AES-GCM) and prove
// that a flipped nonce, ciphertext or tag byte, another device id, another
// version, another context and another master key all give ErrDecrypt.
//
// # The device id
//
// The enroll handler generates the device ULID itself and passes it to
// store.AgentEnroll (EnrollInput.DeviceID), so the repository key and the
// gateway secret are wrapped under the real device id from the first write.
// There is no provisional id and no re-wrap step; Keys.Unwrap and
// Keys.GatewaySecret accept only the device-id wrap.
//
// # maintain
//
// Keys satisfies maintain.KeyUnwrapper (Unwrap(ctx, deviceID) (string, error))
// and its optional MasterKeyLoaded probe. It returns ErrCustomerHeld for
// customer-held custody (DH22).
package escrow
