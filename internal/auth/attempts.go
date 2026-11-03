package auth

import "sync"

// attemptLocks serializes credential attempts per throttle key, so the throttle check, the verification and the
// recorded failure happen as one step for each account or MFA factor. Without it, a burst of parallel attempts all
// read "not throttled" before any failure committed, and a holder of a password could try many TOTP codes per
// throttle window (security review SR-M01). One central owns the database (SM-INV-001), so an in-process lock is
// enough.
type attemptLocks struct {
	mu   sync.Mutex
	keys map[string]*attemptLock
}

type attemptLock struct {
	mu   sync.Mutex
	refs int
}

// lock takes the lock of key and returns its release.
func (l *attemptLocks) lock(key string) func() {
	l.mu.Lock()
	if l.keys == nil {
		l.keys = map[string]*attemptLock{}
	}
	e := l.keys[key]
	if e == nil {
		e = &attemptLock{}
		l.keys[key] = e
	}
	e.refs++
	l.mu.Unlock()
	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		l.mu.Lock()
		if e.refs--; e.refs == 0 {
			delete(l.keys, key)
		}
		l.mu.Unlock()
	}
}
