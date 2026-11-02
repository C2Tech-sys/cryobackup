package escrow

import (
	"context"
	"errors"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// ErrMasterKeyMismatch means the escrow row was wrapped under another master
// key (its master_key_id differs from the loaded key's id), banner C12.
var ErrMasterKeyMismatch = errors.New("escrow: row was wrapped under a different master key")

// Keys joins the master key to the store. It implements the maintain worker's
// KeyUnwrapper (Unwrap) and its optional MasterKeyLoaded probe. Unwrap is
// strict: a row is only ever wrapped under its real device id (the enroll
// handler chooses the id before it wraps, store.EnrollInput.DeviceID).
type Keys struct {
	Master *MasterKey
	Store  *store.Store
}

// MasterKeyLoaded reports whether a master key is present (maintain's Ping
// reports it; banner C12 uses it).
func (k *Keys) MasterKeyLoaded() bool { return k != nil && k.Master.Loaded() }

// Unwrap returns the repository password of deviceID, in memory, for one call.
// It returns ErrCustomerHeld for customer-held custody, ErrMasterKeyUnavailable
// without a key, ErrMasterKeyMismatch for a row wrapped under another key and an
// error wrapping store.ErrNotFound for an unknown device.
func (k *Keys) Unwrap(ctx context.Context, deviceID string) (string, error) {
	if !k.MasterKeyLoaded() {
		return "", ErrMasterKeyUnavailable
	}
	esc, err := k.Store.ActiveEscrow(ctx, store.ScopeAll(), deviceID)
	if err != nil {
		return "", err
	}
	if esc.Custody != "escrowed" {
		return "", ErrCustomerHeld
	}
	if esc.MasterKeyID != k.Master.KeyID() {
		return "", ErrMasterKeyMismatch
	}
	pt, err := k.Master.UnwrapRepoKey(deviceID, esc.KeyVersion, esc.WrappedKey)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// GatewaySecret returns the still-undelivered gateway password of deviceID
// (policy pull, DH13), or store.ErrNotFound when none is pending.
func (k *Keys) GatewaySecret(ctx context.Context, deviceID string) (string, error) {
	if !k.MasterKeyLoaded() {
		return "", ErrMasterKeyUnavailable
	}
	blob, err := k.Store.AgentPendingGatewaySecret(ctx, deviceID)
	if err != nil {
		return "", err
	}
	pt, err := k.Master.Unwrap(ContextGatewaySecret, deviceID, 1, blob)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}
