package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"
)

var daemonLocks sync.Map

// daemonLockRetryDelay is the poll interval; the caller context bounds total wait.
const daemonLockRetryDelay = 50 * time.Millisecond

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
