//go:build !windows && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// detachChild has no platform-specific detach mechanism here.
func detachChild(*exec.Cmd) {}

// ProcessAlive cannot inspect processes on this platform.
func ProcessAlive(int) bool { return false }

func killProcess(int) error {
	return fmt.Errorf("killing processes is unsupported on %s", runtime.GOOS)
}

func publishFile(staging, target string) error {
	return os.Rename(staging, target)
}

func syncDir(string) error { return nil }

func lockFilePlatform(*os.File, bool) (bool, error) {
	return false, fmt.Errorf("file locking is unsupported on %s", runtime.GOOS)
}

func unlockFilePlatform(*os.File) error {
	return fmt.Errorf("file locking is unsupported on %s", runtime.GOOS)
}

func isStaleUnixSocketDialError(error) bool { return false }

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
func runtimeUID() string { return "unknown" }

// processIdentityScope is ProcessScopeLocal for a readable identity whatever
// pid: this build cannot inspect process creation, so only an identity already
// recorded by another implementation can be judged.
func processIdentityScope(_ int, identity ProcessIdentity) ProcessScope {
	if !processIdentityCompatible(identity) {
		return ProcessScopeUnknown
	}
	return ProcessScopeLocal
}

func runtimeProcessIdentities(int) (ProcessIdentity, ProcessIdentity) {
	return "", ""
}

// ReadProcessIdentity reports that this platform cannot inspect process
// creation identity. An unknown identity never authorizes destructive action.
func ReadProcessIdentity(int) (ProcessIdentity, bool) {
	return "", false
}
