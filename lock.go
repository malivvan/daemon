package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var daemonLocks sync.Map

// daemonLockRetryDelay is the poll interval; the caller context bounds total wait.
const daemonLockRetryDelay = 50 * time.Millisecond

// localSemaphore is a context-aware binary semaphore. It serializes holders in
// this process, so local contention is observable before the file lock is
// tried, and it grants waiters in arrival order.
type localSemaphore struct {
	tokens chan struct{}
}

func newLocalSemaphore() *localSemaphore {
	s := &localSemaphore{tokens: make(chan struct{}, 1)}
	s.tokens <- struct{}{}
	return s
}

// TryAcquire takes the semaphore without waiting.
func (s *localSemaphore) TryAcquire() bool {
	select {
	case <-s.tokens:
		return true
	default:
		return false
	}
}

// Acquire takes the semaphore, waiting until it is free or ctx is done.
func (s *localSemaphore) Acquire(ctx context.Context) error {
	select {
	case <-s.tokens:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release returns the semaphore. The caller must hold it.
func (s *localSemaphore) Release() {
	s.tokens <- struct{}{}
}

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

type daemonLock struct {
	local *localSemaphore
	file  *fileLock
}

// AcquireStartLock serializes discovery, replacement, and daemon launch for a
// runtime store. The caller must invoke the returned release function.
func (s RuntimeStore) AcquireStartLock(ctx context.Context) (func(), error) {
	path, err := s.LockPath()
	if err != nil {
		return nil, err
	}
	return acquireDaemonLock(ctx, path, "acquire daemon start lock")
}

// TryAcquireStartLock attempts to acquire the same lock as AcquireStartLock
// without waiting for a holder. On success, acquired is true and the caller
// must call release exactly once, after any application-owned state cleanup.
// With contention, including another goroutine in this process, it returns
// nil, false, nil. An error means the lock state could not be determined.
//
// For a probe, release immediately after acquisition. The result is advisory
// once released; acquire again before making a launch decision. This method
// does not identify the holder, inspect startup snapshots, retry errors, or
// remove lock files. Callers that distinguish their own startup from other
// holders must track their retained release function themselves.
//
// The context is checked before acquisition; filesystem operations themselves
// are synchronous and cannot be interrupted by cancellation.
func (s RuntimeStore) TryAcquireStartLock(ctx context.Context) (release func(), acquired bool, err error) {
	const action = "try acquire daemon start lock"
	if err := ctx.Err(); err != nil {
		return nil, false, fmt.Errorf("%s: %w", action, err)
	}
	path, err := s.LockPath()
	if err != nil {
		return nil, false, err
	}
	lock, err := getDaemonLock(path, action)
	if err != nil {
		return nil, false, err
	}
	if !lock.local.TryAcquire() {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		lock.local.Release()
		return nil, false, fmt.Errorf("%s: %w", action, err)
	}
	locked, err := lock.file.tryLock()
	if err != nil || !locked {
		lock.local.Release()
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", action, err)
		}
		return nil, false, nil
	}
	return lock.release, true, nil
}

// AcquireOwnerLock grants exclusive writable ownership for the lifetime of a
// daemon. The caller must retain the lock until server teardown is complete.
func (s RuntimeStore) AcquireOwnerLock(ctx context.Context) (func(), error) {
	prefix, err := s.validatePrefix()
	if err != nil {
		return nil, err
	}
	if err := s.prepareDir(); err != nil {
		return nil, err
	}
	path := filepath.Join(s.Dir, prefix+".lock.owner")
	return acquireDaemonLock(ctx, path, "acquire daemon owner lock")
}

func getDaemonLock(lockPath, action string) (*daemonLock, error) {
	if lockPath == "" {
		return nil, fmt.Errorf("%s: empty daemon lock path", action)
	}
	if !filepath.IsAbs(lockPath) {
		return nil, fmt.Errorf("%s: daemon lock path %q must be absolute", action, lockPath)
	}
	if err := ensurePrivateRuntimeDir(filepath.Dir(lockPath)); err != nil {
		return nil, fmt.Errorf("prepare daemon lock dir: %w", err)
	}
	value, _ := daemonLocks.LoadOrStore(lockPath, &daemonLock{
		local: newLocalSemaphore(),
		file:  newFileLock(lockPath),
	})
	return value.(*daemonLock), nil
}

func acquireDaemonLock(ctx context.Context, lockPath, action string) (func(), error) {
	lock, err := getDaemonLock(lockPath, action)
	if err != nil {
		return nil, err
	}
	if err := lock.local.Acquire(ctx); err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	locked, err := lock.file.tryLockContext(ctx, daemonLockRetryDelay)
	if err != nil {
		lock.local.Release()
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	if !locked {
		lock.local.Release()
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%s: %w", action, err)
		}
		return nil, errors.New(action + ": lock not acquired")
	}
	return lock.release, nil
}

func (lock *daemonLock) release() {
	_ = lock.file.unlock()
	lock.local.Release()
}
