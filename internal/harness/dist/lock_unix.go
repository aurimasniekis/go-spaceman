//go:build unix

package dist

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// lockPoll is how often lockFile retries a lock another process holds.
const lockPoll = 50 * time.Millisecond

// lockFile takes an exclusive flock on path, waiting until ctx ends. It keeps a second agent on the same cache
// directory from installing or removing the same version at once.
func lockFile(ctx context.Context, path string) (func(), error) {
	for {
		unlock, ok, err := tryLockFile(path)
		if err != nil || ok {
			return unlock, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("claude code: waiting for %s: %w", path, ctx.Err())
		case <-time.After(lockPoll):
		}
	}
}

// tryLockFile takes an exclusive flock on path without waiting; ok is false when another process holds it.
func tryLockFile(path string) (unlock func(), ok bool, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // inside the cache directory
	if err != nil {
		return nil, false, fmt.Errorf("claude code: open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // fd fits int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("claude code: lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // fd fits int
		_ = f.Close()
	}, true, nil
}
