package cli

import (
	"context"
	"errors"

	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/maintain"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// runMaintain is `cryo-console maintain`: the server-side maintenance worker and
// the admin-path socket (internal/console/maintain). maintain cannot import
// escrow (it is the other side of the cycle the factory breaks), so the console
// supplies the key unwrapper here, built from the same master key file and
// database the rest of the console uses.
func runMaintain(args []string, io IO) int {
	return maintain.Run(args, io.Stdout, io.Stderr, MaintainKeys)
}

// MaintainKeys is the maintain.KeyUnwrapperFactory of the console binary: it
// loads the master key file at masterKeyPath and returns an unwrapper over st.
// A missing or malformed key file is an error (maintain then refuses to start):
// a worker without keys could do nothing but fail every device.
func MaintainKeys(masterKeyPath string, st *store.Store) (maintain.KeyUnwrapper, error) {
	mk, err := escrow.LoadMasterKey(masterKeyPath)
	if err != nil {
		return nil, err
	}
	return maintainKeys{&escrow.Keys{Master: mk, Store: st}}, nil
}

// maintainKeys adapts escrow.Keys to maintain.KeyUnwrapper: the one difference is
// the custody sentinel, which maintain names itself. MasterKeyLoaded (the
// optional probe Ping reports) is promoted from the embedded *escrow.Keys.
type maintainKeys struct{ *escrow.Keys }

// Unwrap implements maintain.KeyUnwrapper.
func (m maintainKeys) Unwrap(ctx context.Context, deviceID string) (string, error) {
	pw, err := m.Keys.Unwrap(ctx, deviceID)
	if errors.Is(err, escrow.ErrCustomerHeld) {
		return "", maintain.ErrCustodyCustomerHeld
	}
	return pw, err
}
