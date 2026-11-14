package miniapp

import (
	"crypto/sha256"
	"sync"
	"time"
)

// replayCache remembers the initData already exchanged, so each is accepted once (plan §3.4.2 step 6). It is keyed by
// the initData's signature, which covers the canonical data-check-string: re-encoding or reordering the query string
// doesn't make a second entry. Entries are kept until their initData would be refused as too old under the largest
// max age the settings allow, so raising the setting can't revive a used one; at most limit entries are kept, the oldest
// dropped first.
type replayCache struct {
	mu    sync.Mutex
	limit int
	seen  map[[sha256.Size]byte]time.Time // signature → until
	order []replayEntry                   // insertion order, for dropping the oldest
	head  int                             // order[head:] is live
}

type replayEntry struct {
	h     [sha256.Size]byte
	until time.Time
}

func newReplayCache(limit int) *replayCache {
	return &replayCache{limit: limit, seen: map[[sha256.Size]byte]time.Time{}}
}

// add records h until until, and reports false when it was already recorded (a replay).
func (c *replayCache) add(h [sha256.Size]byte, until, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if exp, ok := c.seen[h]; ok && now.Before(exp) {
		return false
	}
	// Drop expired entries from the front, then the oldest while full.
	for c.head < len(c.order) {
		e := c.order[c.head]
		if exp, ok := c.seen[e.h]; ok && exp.Equal(e.until) {
			if now.Before(exp) && len(c.seen) < c.limit {
				break
			}
			delete(c.seen, e.h)
		}
		c.head++
	}
	if c.head > len(c.order)/2 {
		c.order = append(c.order[:0:0], c.order[c.head:]...)
		c.head = 0
	}
	c.seen[h] = until
	c.order = append(c.order, replayEntry{h: h, until: until})
	return true
}

func (c *replayCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seen)
}
