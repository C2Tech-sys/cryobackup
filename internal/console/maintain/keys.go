package maintain

import (
	"context"
	"errors"
)

// KeyUnwrapper turns a device's escrowed repository key into the repository
// password, in memory, for the duration of one call (architecture DH1, §8.2).
// The real implementation belongs to internal/console/escrow (ticket P1-T5):
// it reads the active repo_key_escrow row, derives the per-row key from the
// master key file and decrypts. The maintain package knows nothing of the
// master key; it only calls Unwrap and drops the result when the restic child
// has been started.
//
// Unwrap returns ErrCustodyCustomerHeld for a device whose key the customer
// holds, an error wrapping store.ErrNotFound for an unknown device, and any
// other error for a failure it cannot name (a wrong master key, a tampered
// row, an unreadable key file). Callers never see the error text: the worker
// turns every other error into the admin-path code "internal".
type KeyUnwrapper interface {
	// Unwrap returns the repository password of deviceID.
	Unwrap(ctx context.Context, deviceID string) (repoPassword string, err error)
}

// ErrCustodyCustomerHeld is what a KeyUnwrapper returns for a customer-held
// device (architecture DH22). The worker also checks custody itself, before it
// asks for a key, so this is a second line of defence.
var ErrCustodyCustomerHeld = errors.New("maintain: repository key is held by the customer")

// masterKeyReporter is the optional interface a KeyUnwrapper may implement so
// Ping can report whether the master key is loaded.
type masterKeyReporter interface {
	MasterKeyLoaded() bool
}
