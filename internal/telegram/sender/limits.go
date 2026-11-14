package sender

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// Upper bounds for Limits. The rates stay at or below Telegram's FAQ limits (about 30 messages/s overall, about 1/s
// per chat with short bursts, 20/min per group; docs/contracts/telegram.md); the queue bounds keep memory bounded.
const (
	MaxGlobalPerSecond = 30
	MaxChatPerSecond   = 1
	MaxChatBurst       = 3
	MaxGroupPerMinute  = 20
	MaxQueueTotal      = 50000
	MaxQueuePerChat    = 1000
)

// Limits are the sender's rate limits and queue bounds (plan §4.4). TG-3c fills them from telegram_settings
// (`send_limits`); the zero value means DefaultLimits.
type Limits struct {
	// GlobalPerSecond bounds all sends together; it is also the global burst (default 25, a margin below 30).
	GlobalPerSecond float64
	// ChatPerSecond and ChatBurst bound sends to one private chat (default 1/s with a burst of 3).
	ChatPerSecond float64
	ChatBurst     int
	// GroupPerMinute bounds sends to one group or supergroup; it is also the group burst (default 20).
	GroupPerMinute int
	// QueueTotal bounds the messages waiting over every chat (default 5000).
	QueueTotal int
	// QueuePerChat bounds the messages waiting for one chat (default 50).
	QueuePerChat int
}

// DefaultLimits are the plan §4.4 values.
func DefaultLimits() Limits {
	return Limits{GlobalPerSecond: 25, ChatPerSecond: 1, ChatBurst: 3, GroupPerMinute: 20, QueueTotal: 5000, QueuePerChat: 50}
}

// Validate reports a limit outside its bounds.
func (l Limits) Validate() error {
	var errs []error
	rate := func(name string, v, hi float64) {
		if math.IsNaN(v) || v <= 0 || v > hi {
			errs = append(errs, fmt.Errorf("%s must be above 0 and at most %g, got %g", name, hi, v))
		}
	}
	count := func(name string, v, hi int) {
		if v < 1 || v > hi {
			errs = append(errs, fmt.Errorf("%s must be between 1 and %d, got %d", name, hi, v))
		}
	}
	rate("global_per_second", l.GlobalPerSecond, MaxGlobalPerSecond)
	rate("chat_per_second", l.ChatPerSecond, MaxChatPerSecond)
	count("chat_burst", l.ChatBurst, MaxChatBurst)
	count("group_per_minute", l.GroupPerMinute, MaxGroupPerMinute)
	count("queue_total", l.QueueTotal, MaxQueueTotal)
	count("queue_per_chat", l.QueuePerChat, MaxQueuePerChat)
	if l.QueuePerChat > l.QueueTotal {
		errs = append(errs, errors.New("queue_per_chat must not exceed queue_total"))
	}
	return errors.Join(errs...)
}

// readyEpsilon is the extra fraction of a token the sender waits for, so float rounding never lets it send a hair
// before Telegram's bucket has refilled.
const readyEpsilon = 1e-6

// bucket is a token bucket refilled continuously at rate tokens per second up to capacity.
type bucket struct {
	rate, capacity, tokens float64
	last                   time.Time
}

func newBucket(rate, capacity float64, now time.Time) *bucket {
	return &bucket{rate: rate, capacity: capacity, tokens: capacity, last: now}
}

func (b *bucket) level(now time.Time) float64 {
	t := b.tokens
	if el := now.Sub(b.last).Seconds(); el > 0 {
		t = math.Min(b.capacity, t+el*b.rate)
	}
	return t
}

// readyAt is when one token is available (now if one is). It doesn't change the bucket.
func (b *bucket) readyAt(now time.Time) time.Time {
	t := b.level(now)
	if t >= 1+readyEpsilon {
		return now
	}
	ns := math.Ceil((1 + readyEpsilon - t) / b.rate * float64(time.Second))
	return now.Add(time.Duration(ns))
}

// take refills to now and spends one token.
func (b *bucket) take(now time.Time) {
	b.tokens = b.level(now) - 1
	if now.After(b.last) {
		b.last = now
	}
}

// settle moves the bucket's clock to now without refilling: the send taken at dispatch is treated as happening when
// its answer arrived. One send per chat is in flight at a time, so a chat's sends then conform to the bucket at the
// times Telegram saw them, however long each request took.
func (b *bucket) settle(now time.Time) {
	if now.After(b.last) {
		b.last = now
	}
}

func (b *bucket) full(now time.Time) bool { return b.level(now) >= b.capacity }

// set changes the rate and capacity, keeping the current level (clamped to the new capacity).
func (b *bucket) set(rate, capacity float64, now time.Time) {
	b.tokens = math.Min(capacity, b.level(now))
	if now.After(b.last) {
		b.last = now
	}
	b.rate, b.capacity = rate, capacity
}
