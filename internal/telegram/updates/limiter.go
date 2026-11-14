package updates

import (
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
)

// Kind is what a linked user's request costs against the inbound limits (plan §3.5).
type Kind int

// Inbound limit kinds.
const (
	KindAction    Kind = iota // any message or button tap (the dispatcher checks it)
	KindInboxItem             // creating an inbox item (checked by the inbox capture)
	KindDownload              // downloading an attachment (checked by the download path)
	kinds
)

// InboundLimits are the per-link inbound abuse limits. The global updates-per-second limit is the Intake's rate
// (Intake.SetRate).
type InboundLimits struct {
	ActionsPerMinute  int
	ActionsBurst      int
	InboxItemsPerHour int
	DownloadsPerHour  int
}

// DefaultInboundLimits are the plan §3.5 values.
func DefaultInboundLimits() InboundLimits {
	return InboundLimits{ActionsPerMinute: 30, ActionsBurst: 10, InboxItemsPerHour: 60, DownloadsPerHour: 50}
}

// limiterIdle is how long a link's buckets are kept without use. A bucket left alone that long is full again, so
// forgetting it changes nothing.
const limiterIdle = 2 * time.Hour

// Limiter holds each link's inbound token buckets in memory. It writes nothing and survives no restart.
type Limiter struct {
	clock clock.Clock

	mu        sync.Mutex
	limits    InboundLimits
	links     map[string]*linkBuckets
	lastSweep time.Time
}

type linkBuckets struct {
	b    [kinds]*rate.Limiter
	used time.Time
	// warned: per kind, the link was told to slow down and hasn't had an allowed action of that kind since, so it isn't
	// told again.
	warned [kinds]bool
}

// NewLimiter returns a limiter with limits (zero fields take the defaults).
func NewLimiter(l InboundLimits, clk clock.Clock) *Limiter {
	if clk == nil {
		clk = clock.System{}
	}
	return &Limiter{clock: clk, limits: withDefaults(l), links: map[string]*linkBuckets{}}
}

func withDefaults(l InboundLimits) InboundLimits {
	d := DefaultInboundLimits()
	if l.ActionsPerMinute <= 0 {
		l.ActionsPerMinute = d.ActionsPerMinute
	}
	if l.ActionsBurst <= 0 {
		l.ActionsBurst = d.ActionsBurst
	}
	if l.InboxItemsPerHour <= 0 {
		l.InboxItemsPerHour = d.InboxItemsPerHour
	}
	if l.DownloadsPerHour <= 0 {
		l.DownloadsPerHour = d.DownloadsPerHour
	}
	return l
}

// rateOf is a kind's refill rate and burst.
func (l InboundLimits) rateOf(k Kind) (rate.Limit, int) {
	switch k {
	case KindInboxItem:
		return rate.Limit(float64(l.InboxItemsPerHour) / 3600), l.InboxItemsPerHour
	case KindDownload:
		return rate.Limit(float64(l.DownloadsPerHour) / 3600), l.DownloadsPerHour
	default:
		return rate.Limit(float64(l.ActionsPerMinute) / 60), l.ActionsBurst
	}
}

// SetLimits changes the limits at once, for every link (current bucket levels are kept, capped at the new burst).
func (lm *Limiter) SetLimits(l InboundLimits) {
	l = withDefaults(l)
	now := lm.clock.Now()
	lm.mu.Lock()
	defer lm.mu.Unlock()
	lm.limits = l
	for _, lb := range lm.links {
		for k := range kinds {
			r, b := l.rateOf(k)
			lb.b[k].SetLimitAt(now, r)
			lb.b[k].SetBurstAt(now, b)
		}
	}
}

// Limits returns the current limits.
func (lm *Limiter) Limits() InboundLimits {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	return lm.limits
}

// Allow takes one unit of kind for linkID, reporting whether it is within the limits.
func (lm *Limiter) Allow(linkID string, k Kind) bool {
	ok, _ := lm.allow(linkID, k)
	return ok
}

// allow is Allow that also reports whether the caller should tell the user to slow down: true only for the first
// refusal after an allowed action.
func (lm *Limiter) allow(linkID string, k Kind) (ok, warn bool) {
	now := lm.clock.Now()
	lm.mu.Lock()
	defer lm.mu.Unlock()
	lm.sweepLocked(now)
	lb := lm.links[linkID]
	if lb == nil {
		lb = &linkBuckets{}
		for k := range kinds {
			r, b := lm.limits.rateOf(k)
			lb.b[k] = rate.NewLimiter(r, b)
		}
		lm.links[linkID] = lb
	}
	lb.used = now
	if lb.b[k].AllowN(now, 1) {
		lb.warned[k] = false
		return true, false
	}
	warn = !lb.warned[k]
	lb.warned[k] = true
	return false, warn
}

// Forget drops a link's buckets (on unlink).
func (lm *Limiter) Forget(linkID string) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	delete(lm.links, linkID)
}

func (lm *Limiter) sweepLocked(now time.Time) {
	if now.Sub(lm.lastSweep) < limiterIdle/4 {
		return
	}
	lm.lastSweep = now
	for id, lb := range lm.links {
		if now.Sub(lb.used) > limiterIdle {
			delete(lm.links, id)
		}
	}
}
