package daemon

import (
	"context"
	"testing"
)

// StartLivePIDHelperForTest starts a child process that lives until test cleanup.
func StartLivePIDHelperForTest(t *testing.T) int {
	t.Helper()
	return startLivePIDHelper(t)
}

// ErrNotPortable exposes the portable-name sentinel to external tests.
var ErrNotPortable = errNotPortable

// EnsurePrivateDirForTest creates a private runtime directory for tests that
// need one without going through another package.
func EnsurePrivateDirForTest(path string) error {
	return ensurePrivateRuntimeDir(path)
}

// HoldLockForTest acquires path's daemon lock, blocking until it is available,
// and returns the release function. It lets tests hold a lock while another
// caller contends for it.
func HoldLockForTest(path string) (func(), error) {
	return acquireDaemonLock(context.Background(), path, "test hold daemon lock")
}
