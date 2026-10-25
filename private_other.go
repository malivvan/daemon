//go:build !unix && !windows

package daemon

import (
	"fmt"
	"os"
	"runtime"
)

// ensurePrivateRuntimeDir fails closed when ownership and link-safe directory
// handling are unavailable.
func ensurePrivateRuntimeDir(string) error {
	return fmt.Errorf("private directory creation is unsupported on %s", runtime.GOOS)
}

// validatePrivateRuntimeDir fails closed when ownership and link-safe
// directory validation are unavailable.
func validatePrivateRuntimeDir(string) error {
	return fmt.Errorf("private directory validation is unsupported on %s", runtime.GOOS)
}

// openRuntimeFile fails closed when the platform cannot bind an open file to a
// verified current-user identity without following links.
func openRuntimeFile(string) (*os.File, error) {
	return nil, fmt.Errorf("current-user file opening is unsupported on %s", runtime.GOOS)
}

// runtimeUID fails closed when the platform does not expose a stable
// filesystem user identity.
func runtimeUID() string {
	return "unknown"
}
