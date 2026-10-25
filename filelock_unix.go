//go:build !windows

package daemon

import (
	"errors"
	"os"
	"syscall"
)

// lockFilePlatform takes an exclusive flock lock on file. With nonblocking it
// reports false instead of waiting when another holder has the lock.
func lockFilePlatform(file *os.File, nonblocking bool) (bool, error) {
	how := syscall.LOCK_EX
	if nonblocking {
		how |= syscall.LOCK_NB
	}
	err := syscall.Flock(int(file.Fd()), how)
	if err == nil {
		return true, nil
	}
	if nonblocking && (errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)) {
		return false, nil
	}
	return false, err
}

func unlockFilePlatform(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
