//go:build windows

package daemon

import (
	"errors"
	"os"
	"syscall"
)

// errorLockViolation is returned by LockFileEx when the lock is held and
// LOCKFILE_FAIL_IMMEDIATELY is set.
const errorLockViolation syscall.Errno = 33

// lockFilePlatform takes an exclusive LockFileEx lock on file. With
// nonblocking it reports false instead of waiting when another holder has the
// lock.
func lockFilePlatform(file *os.File, nonblocking bool) (bool, error) {
	err := lockFileExclusive(syscall.Handle(file.Fd()), nonblocking)
	if err == nil {
		return true, nil
	}
	if nonblocking && (errors.Is(err, errorLockViolation) || errors.Is(err, syscall.ERROR_IO_PENDING)) {
		return false, nil
	}
	return false, err
}

func unlockFilePlatform(file *os.File) error {
	return unlockFileEx(syscall.Handle(file.Fd()))
}
