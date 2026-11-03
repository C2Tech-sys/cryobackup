//go:build !windows

package probe

import (
	"errors"
	"syscall"
)

// refused reports a connection refused.
func refused(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }
