package daemon

import "context"

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
