//go:build windows

package daemon

import (
	"errors"
	"syscall"
)

// wsaECONNREFUSED is WSAECONNREFUSED, which Windows reports both for a socket
// file with no listener and for a missing path; neither maps to
// syscall.ECONNREFUSED.
const wsaECONNREFUSED syscall.Errno = 10061

func isStaleUnixSocketDialError(err error) bool {
	return errors.Is(err, wsaECONNREFUSED)
}
