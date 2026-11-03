package probe

import (
	"errors"
	"syscall"
)

// wsaeconnrefused is WSAECONNREFUSED (winerror.h), which the syscall package does not name.
const wsaeconnrefused = syscall.Errno(10061)

// refused reports a connection refused (Windows reports WSAECONNREFUSED).
func refused(err error) bool {
	return errors.Is(err, wsaeconnrefused) || errors.Is(err, syscall.ECONNREFUSED)
}
