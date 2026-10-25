package daemon

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"
)

// fileLock is an exclusive cross-process lock on a file. The file is created
// when missing and never removed, so every holder locks the same filesystem
// object, and the caller's directory permissions protect it.
type fileLock struct {
	path string

	mu   sync.Mutex
	file *os.File
}

func newFileLock(path string) *fileLock {
	return &fileLock{path: path}
}

// tryLock takes the lock without waiting. It returns true when this instance
// already holds it.
func (l *fileLock) tryLock() (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		return true, nil
	}
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return false, err
	}
	locked, err := lockFilePlatform(file, true)
	if err != nil {
		_ = file.Close()
		return false, err
	}
	if !locked {
		_ = file.Close()
		return false, nil
	}
	l.file = file
	return true, nil
}

// tryLockContext retries tryLock until it succeeds, fails, or ctx is done.
func (l *fileLock) tryLockContext(ctx context.Context, retryDelay time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	for {
		ok, err := l.tryLock()
		if ok || err != nil {
			return ok, err
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(retryDelay):
		}
	}
}

// unlock releases the lock. It is a no-op when this instance is unlocked.
func (l *fileLock) unlock() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	unlockErr := unlockFilePlatform(l.file)
	closeErr := l.file.Close()
	l.file = nil
	return errors.Join(unlockErr, closeErr)
}
