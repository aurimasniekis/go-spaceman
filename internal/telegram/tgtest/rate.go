package tgtest

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

// bucket is a token bucket refilled continuously at rate tokens per second up to capacity.
type bucket struct {
	rate, capacity, tokens float64
	last                   time.Time
}

func newBucket(rate, capacity float64, now time.Time) *bucket {
	return &bucket{rate: rate, capacity: capacity, tokens: capacity, last: now}
}

func (b *bucket) refill(now time.Time) {
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = math.Min(b.capacity, b.tokens+el*b.rate)
	}
	b.last = now
}

// wait is how long until one token is available (0 if one is).
func (b *bucket) wait() time.Duration {
	if b.tokens >= 1 {
		return 0
	}
	return time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
}

// rateLimitLocked takes one send from the global bucket and chat's bucket, or answers 429 with retry_after (whole
// seconds, rounded up, at least 1) without taking anything.
func (s *Server) rateLimitLocked(chat botapi.Chat) *apiError {
	l := s.opts.Limits
	if l.Disabled {
		return nil
	}
	now := s.clk.Now()
	cb, ok := s.chatBuckets[chat.ID]
	if !ok {
		if chat.Type == botapi.ChatPrivate {
			cb = newBucket(l.ChatPerSecond, float64(l.ChatBurst), now)
		} else {
			cb = newBucket(float64(l.GroupPerMinute)/60, float64(l.GroupPerMinute), now)
		}
		s.chatBuckets[chat.ID] = cb
	}
	cb.refill(now)
	s.globalBucket.refill(now)
	w := max(cb.wait(), s.globalBucket.wait())
	if w > 0 {
		secs := int(math.Ceil(w.Seconds()))
		secs = max(secs, 1)
		return &apiError{
			code:       http.StatusTooManyRequests,
			desc:       "Too Many Requests: retry after " + strconv.Itoa(secs),
			retryAfter: secs,
		}
	}
	cb.tokens--
	s.globalBucket.tokens--
	return nil
}
