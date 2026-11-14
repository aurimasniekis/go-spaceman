// Package sender is SpaceMan's outbound Telegram queue (ADR-0083, plan §4.4). Every message the bot sends, edits or
// deletes goes through one Sender, which keeps the bot inside Telegram's rate limits and keeps its own memory bounded
// (the ADR-0078 lessons):
//
//   - Token buckets: a global bucket (default 25/s, a margin below Telegram's 30/s), one per private chat (1/s, burst
//     3) and one per group (20/min). Edits, deletes and media count as sends, because Telegram doesn't document
//     otherwise. A message without a chat (a callback answer) takes only a global token.
//   - Priority classes (Interactive first, Digest last). The dispatcher always sends the most urgent message among the
//     chats whose buckets allow a send, oldest first within a class; one message per chat is in flight at a time, so
//     a chat's messages keep their order within a class.
//   - Coalescing: a message with the same (chat, Key) as one still queued replaces it. A message already in flight is
//     never replaced; the newer one is queued after it.
//   - Bounds: QueueTotal messages over every chat and QueuePerChat per chat. When a bound is reached, the lowest class
//     drops first (the oldest of it), and the chat that lost a message gets one overflow notice ("N updates — open
//     the app"), which is outside the bounds: at most one per chat.
//   - 429: retry_after pauses the chat, or every chat when the answer is global (no chat, or a second chat got a 429
//     within a second; Telegram doesn't say which limit it was). A message is retried at most 3 times, whatever the
//     reason; 5xx and transport failures back off 1, 2 and 4 seconds.
//   - 403: the chat is marked blocked, its queue is dropped and OnBlocked is called once (the caller marks the link
//     blocked). Enqueue refuses the chat until Unblock (the user wrote to the bot again).
//
// The sender writes nothing to the database. Callers that need to record what was sent (the telegram_messages
// mapping) collect it from their send functions and Done callbacks and write it in batches (plan §4.2 step 7).
package sender

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

// Priority is a message's class; a lower value is more urgent.
type Priority uint8

// Priority classes, most urgent first (plan §4.4).
const (
	// Interactive is a callback answer or a direct reply to the user's own action.
	Interactive Priority = iota + 1
	// NeedsYou is a new needs-you card or an escalation.
	NeedsYou
	// Notification is any other notification. Overflow notices use it.
	Notification
	// Edit is a card edit (final state, refresh).
	Edit
	// Digest is a digest.
	Digest
)

const numClasses = int(Digest)

var classNames = [numClasses]metrics.TelegramClass{
	metrics.TelegramInteractive, metrics.TelegramNeedsYou, metrics.TelegramNotification, metrics.TelegramEdit, metrics.TelegramDigest,
}

func (p Priority) valid() bool                  { return p >= Interactive && p <= Digest }
func (p Priority) idx() int                     { return int(p) - 1 }
func (p Priority) class() metrics.TelegramClass { return classNames[p.idx()] }

// MaxRetries is how often one message is retried at most, whatever the reason.
const MaxRetries = 3

// Errors returned by Enqueue or passed to Done.
var (
	// ErrBlocked: the chat blocked the bot (or the bot can't write to it).
	ErrBlocked = errors.New("telegram sender: chat blocked the bot")
	// ErrQueueFull: a queue bound was reached and the message was the lowest class, so it was dropped.
	ErrQueueFull = errors.New("telegram sender: queue full")
	// ErrCoalesced: a newer message with the same key replaced this one.
	ErrCoalesced = errors.New("telegram sender: replaced by a newer message")
	// ErrClosed: the sender stopped.
	ErrClosed = errors.New("telegram sender: stopped")
)

// SendFunc makes one Bot API call. It runs on a sender goroutine with the sender's context and may be called again
// on a retry.
type SendFunc func(ctx context.Context, c *botapi.Client) error

// Message is one queued Bot API call.
type Message struct {
	// ChatID is the chat the call writes to; 0 for a call without a chat (answerCallbackQuery), which takes only a
	// global token and is never limited to one call in flight.
	ChatID int64
	// Group is set for a group or supergroup (20/min bucket instead of the private-chat one).
	Group bool
	// Priority is the class (required).
	Priority Priority
	// Key coalesces: a queued message with the same chat and key is replaced by this one. Empty never coalesces.
	Key string
	// Send makes the call (required).
	Send SendFunc
	// Done, when set, is called exactly once if Enqueue accepted the message: with nil after Telegram accepted it,
	// otherwise with an error that matches ErrBlocked, ErrQueueFull, ErrCoalesced or ErrClosed, or wraps Telegram's
	// answer. It runs on a sender goroutine or the goroutine of a later Enqueue, never under the sender's lock; it
	// must not block for long.
	Done func(error)
}

// Options configure a Sender.
type Options struct {
	// Client calls the Bot API (required).
	Client *botapi.Client
	// Limits are the rate limits and bounds (zero value: DefaultLimits).
	Limits Limits
	// Workers is how many calls may be in flight together (default 8).
	Workers int
	// Clock drives the buckets (default: the system clock).
	Clock clock.Clock
	// After waits; tests pass one that advances a clock.Fake (default time.After).
	After func(time.Duration) <-chan time.Time
	// Metrics receives the telegram_* metrics (nil: none).
	Metrics *metrics.Metrics
	// Logger receives failures (default: none). Lines carry the class and Telegram's error, never a chat ID or text.
	Logger *slog.Logger
	// OnBlocked is called once when a chat answers 403, from a sender goroutine (the caller marks the link blocked).
	OnBlocked func(chatID int64)
	// Overflow builds a chat's overflow notice for n dropped messages, when the notice is sent (default: a plain
	// "N updates — open the app" text).
	Overflow func(chatID int64, n int) SendFunc
}

type item struct {
	msg      Message
	prio     Priority
	seq      uint64
	enq      time.Time
	attempts int
	notice   bool
	chat     *chatState
	chatEl   *list.Element // in chat.q[prio]
	globalEl *list.Element // in Sender.byClass[prio]
}

type coalesceKey struct {
	chat int64
	key  string
}

type chatState struct {
	id          int64
	group       bool
	q           [numClasses]list.List // of *item, oldest first
	n           int                   // queued items, the notice excluded
	notice      *item                 // the queued overflow notice, if any
	dropped     int                   // messages dropped since the last notice was sent
	b           *bucket               // nil for chat 0
	pausedUntil time.Time
	inflight    bool
	blocked     bool
}

func (c *chatState) queued() bool { return c.n > 0 || c.notice != nil }

// head is the most urgent, oldest queued item.
func (c *chatState) head() *item {
	var h *item
	for p := range numClasses {
		if e := c.q[p].Front(); e != nil {
			h = e.Value.(*item)
			break
		}
	}
	if c.notice != nil && (h == nil || before(c.notice, h)) {
		return c.notice
	}
	return h
}

func before(a, b *item) bool {
	if a.prio != b.prio {
		return a.prio < b.prio
	}
	return a.seq < b.seq
}

// Sender is the outbound queue. Create it with New, start it with Run.
type Sender struct {
	o       Options
	clk     clock.Clock
	after   func(time.Duration) <-chan time.Time
	workers int
	wake    chan struct{}

	mu        sync.Mutex
	limits    Limits
	global    *bucket
	globalP   time.Time // global pause from a 429
	chats     map[int64]*chatState
	active    map[int64]*chatState // chats with queued items
	byClass   [numClasses]list.List
	keys      map[coalesceKey]*item
	total     int // queued items over every chat, notices excluded
	depth     [numClasses]int
	seq       uint64
	inflight  int
	running   bool
	closed    bool
	last429ID int64
	last429At time.Time
	wg        sync.WaitGroup
}

// New returns a sender; call Run to start sending.
func New(o Options) (*Sender, error) {
	if o.Client == nil {
		return nil, errors.New("telegram sender: a client is required")
	}
	if o.Limits == (Limits{}) {
		o.Limits = DefaultLimits()
	}
	if err := o.Limits.Validate(); err != nil {
		return nil, fmt.Errorf("telegram sender: %w", err)
	}
	if o.Workers <= 0 {
		o.Workers = 8
	}
	if o.Clock == nil {
		o.Clock = clock.System{}
	}
	if o.After == nil {
		o.After = time.After
	}
	if o.Overflow == nil {
		o.Overflow = defaultOverflow
	}
	s := &Sender{
		o: o, clk: o.Clock, after: o.After, workers: o.Workers,
		wake:   make(chan struct{}, 1),
		limits: o.Limits,
		chats:  map[int64]*chatState{},
		active: map[int64]*chatState{},
		keys:   map[coalesceKey]*item{},
	}
	s.global = newBucket(o.Limits.GlobalPerSecond, o.Limits.GlobalPerSecond, s.clk.Now())
	return s, nil
}

func defaultOverflow(chatID int64, n int) SendFunc {
	word := "updates"
	if n == 1 {
		word = "update"
	}
	text := fmt.Sprintf("%d %s — open the app", n, word)
	return func(ctx context.Context, c *botapi.Client) error {
		_, err := c.SendMessage(ctx, botapi.SendMessageParams{ChatID: chatID, Text: text})
		return err
	}
}

// SetLimits changes the limits while running. New rates apply at once; new bounds apply to later Enqueue calls
// (messages already queued above a lowered bound drain normally).
func (s *Sender) SetLimits(l Limits) error {
	if err := l.Validate(); err != nil {
		return fmt.Errorf("telegram sender: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clk.Now()
	s.limits = l
	s.global.set(l.GlobalPerSecond, l.GlobalPerSecond, now)
	for _, c := range s.chats {
		if c.b != nil {
			r, cp := s.chatRateLocked(c.group)
			c.b.set(r, cp, now)
		}
	}
	s.signal()
	return nil
}

func (s *Sender) chatRateLocked(group bool) (rate, capacity float64) {
	if group {
		return float64(s.limits.GroupPerMinute) / 60, float64(s.limits.GroupPerMinute)
	}
	return s.limits.ChatPerSecond, float64(s.limits.ChatBurst)
}

// Unblock lets a blocked chat receive messages again (the user wrote to the bot). Use SetBlocked at start to restore
// the blocked links.
func (s *Sender) Unblock(chatID int64) { s.SetBlocked(chatID, false) }

// SetBlocked marks a chat blocked or not, without calling OnBlocked. Blocking drops its queue.
func (s *Sender) SetBlocked(chatID int64, blocked bool) {
	if chatID == 0 {
		return
	}
	var done []func()
	s.mu.Lock()
	c := s.chats[chatID]
	switch {
	case blocked:
		if c == nil {
			c = s.chatLocked(chatID, false)
		}
		if !c.blocked {
			c.blocked = true
			done = s.dropChatLocked(c, ErrBlocked, metrics.TelegramDropBlocked)
		}
	case c != nil:
		c.blocked = false
	}
	s.mu.Unlock()
	run(done)
}

// Blocked reports whether chatID is marked blocked.
func (s *Sender) Blocked(chatID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.chats[chatID]
	return c != nil && c.blocked
}

// Len is the number of queued messages, overflow notices included (in-flight calls are not counted).
func (s *Sender) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, d := range s.depth {
		n += d
	}
	return n
}

// Enqueue queues m. It never blocks on the network. An error means m was not queued and Done won't be called.
func (s *Sender) Enqueue(m Message) error {
	if m.Send == nil {
		return errors.New("telegram sender: message without a send function")
	}
	if !m.Priority.valid() {
		return fmt.Errorf("telegram sender: invalid priority %d", m.Priority)
	}
	var done []func()
	err := func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return ErrClosed
		}
		c := s.chats[m.ChatID]
		if c != nil && c.blocked {
			return ErrBlocked
		}
		if c == nil {
			c = s.chatLocked(m.ChatID, m.Group)
		} else if c.group != m.Group && c.b != nil {
			c.group = m.Group
			r, cp := s.chatRateLocked(c.group)
			c.b.set(r, cp, s.clk.Now())
		}
		now := s.clk.Now()
		s.seq++
		it := &item{msg: m, prio: m.Priority, seq: s.seq, enq: now, chat: c}
		if m.Key != "" {
			k := coalesceKey{m.ChatID, m.Key}
			if old := s.keys[k]; old != nil {
				s.replaceLocked(old, it)
				s.keys[k] = it
				done = append(done, s.doneFn(old, ErrCoalesced))
				s.o.Metrics.TelegramDropped(old.prio.class(), metrics.TelegramDropCoalesced, 1)
				s.signal()
				return nil
			}
		}
		perChat := s.limits.QueuePerChat
		if m.ChatID == 0 {
			perChat = s.limits.QueueTotal
		}
		if c.n >= perChat {
			victim := lowest(func(p int) *list.List { return &c.q[p] })
			if victim == nil || victim.prio < it.prio {
				s.lostLocked(c, it.prio)
				return ErrQueueFull
			}
			done = append(done, s.dropLocked(victim, ErrQueueFull, metrics.TelegramDropQueueFull))
		}
		if s.total >= s.limits.QueueTotal {
			victim := lowest(func(p int) *list.List { return &s.byClass[p] })
			if victim == nil || victim.prio < it.prio {
				s.lostLocked(c, it.prio)
				return ErrQueueFull
			}
			done = append(done, s.dropLocked(victim, ErrQueueFull, metrics.TelegramDropQueueFull))
		}
		s.pushLocked(it, false)
		if m.Key != "" {
			s.keys[coalesceKey{m.ChatID, m.Key}] = it
		}
		s.signal()
		return nil
	}()
	run(done)
	return err
}

// lowest returns the oldest item of the least urgent non-empty class.
func lowest(q func(p int) *list.List) *item {
	for p := numClasses - 1; p >= 0; p-- {
		if e := q(p).Front(); e != nil {
			return e.Value.(*item)
		}
	}
	return nil
}

func (s *Sender) chatLocked(id int64, group bool) *chatState {
	c := &chatState{id: id, group: group}
	if id != 0 {
		r, cp := s.chatRateLocked(group)
		c.b = newBucket(r, cp, s.clk.Now())
	}
	s.chats[id] = c
	return c
}

// pushLocked queues it at the back of its class (front for a retry).
func (s *Sender) pushLocked(it *item, front bool) {
	c := it.chat
	p := it.prio.idx()
	if front {
		it.chatEl = c.q[p].PushFront(it)
		it.globalEl = s.byClass[p].PushFront(it)
	} else {
		it.chatEl = c.q[p].PushBack(it)
		it.globalEl = s.byClass[p].PushBack(it)
	}
	c.n++
	s.total++
	s.depth[p]++
	s.active[c.id] = c
}

// replaceLocked puts it in old's place: the same position when the class is the same, else at the back of its class.
func (s *Sender) replaceLocked(old, it *item) {
	c := old.chat
	if old.prio == it.prio {
		p := it.prio.idx()
		it.seq = old.seq
		it.chatEl = c.q[p].InsertAfter(it, old.chatEl)
		it.globalEl = s.byClass[p].InsertAfter(it, old.globalEl)
		c.q[p].Remove(old.chatEl)
		s.byClass[p].Remove(old.globalEl)
		return
	}
	s.unlinkLocked(old)
	s.pushLocked(it, false)
}

// unlinkLocked removes a queued item (not a notice) from every list.
func (s *Sender) unlinkLocked(it *item) {
	c := it.chat
	p := it.prio.idx()
	c.q[p].Remove(it.chatEl)
	s.byClass[p].Remove(it.globalEl)
	it.chatEl, it.globalEl = nil, nil
	c.n--
	s.total--
	s.depth[p]--
	if k := (coalesceKey{c.id, it.msg.Key}); it.msg.Key != "" && s.keys[k] == it {
		delete(s.keys, k)
	}
	if !c.queued() {
		delete(s.active, c.id)
	}
}

// dropLocked removes a queued item for a bound and records the loss on its chat.
func (s *Sender) dropLocked(it *item, err error, reason metrics.TelegramDropReason) func() {
	s.unlinkLocked(it)
	s.o.Metrics.TelegramDropped(it.prio.class(), reason, 1)
	s.lostLocked(it.chat, 0)
	return s.doneFn(it, err)
}

// lostLocked counts a message the chat won't get and makes sure it has an overflow notice queued. prio is the lost
// message's class when it was never queued (0: already counted).
func (s *Sender) lostLocked(c *chatState, prio Priority) {
	if prio != 0 {
		s.o.Metrics.TelegramDropped(prio.class(), metrics.TelegramDropQueueFull, 1)
	}
	if c.id == 0 {
		return
	}
	c.dropped++
	if c.notice == nil {
		s.seq++
		c.notice = &item{prio: Notification, seq: s.seq, enq: s.clk.Now(), notice: true, chat: c}
		s.depth[Notification.idx()]++
		s.active[c.id] = c
		s.o.Metrics.TelegramOverflowNotice()
	}
}

// dropChatLocked drops everything queued for c.
func (s *Sender) dropChatLocked(c *chatState, err error, reason metrics.TelegramDropReason) []func() {
	var done []func()
	for p := range numClasses {
		for e := c.q[p].Front(); e != nil; {
			next := e.Next()
			it := e.Value.(*item)
			s.unlinkLocked(it)
			s.o.Metrics.TelegramDropped(it.prio.class(), reason, 1)
			done = append(done, s.doneFn(it, err))
			e = next
		}
	}
	if c.notice != nil {
		s.o.Metrics.TelegramDropped(Notification.class(), reason, 1)
		s.depth[Notification.idx()]--
		c.notice = nil
	}
	c.dropped = 0
	delete(s.active, c.id)
	return done
}

func (s *Sender) doneFn(it *item, err error) func() {
	if it.msg.Done == nil {
		return nil
	}
	f := it.msg.Done
	return func() { f(err) }
}

func run(fs []func()) {
	for _, f := range fs {
		if f != nil {
			f()
		}
	}
}

func (s *Sender) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run sends until ctx ends, then waits for the calls in flight and drops what is still queued (Done gets
// ErrClosed). A Sender runs once.
func (s *Sender) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.running || s.closed {
		s.mu.Unlock()
		return errors.New("telegram sender: already started")
	}
	s.running = true
	sweepAt := s.clk.Now().Add(time.Minute)
	s.mu.Unlock()
	for {
		if ctx.Err() != nil {
			return s.shutdown()
		}
		s.mu.Lock()
		now := s.clk.Now()
		var next time.Time
		for s.inflight < s.workers {
			it, at := s.pickLocked(now)
			if it == nil {
				next = at
				break
			}
			s.dispatchLocked(ctx, it, now)
		}
		if !now.Before(sweepAt) {
			s.sweepLocked(now)
			sweepAt = now.Add(time.Minute)
		}
		var timer <-chan time.Time
		if !next.IsZero() && s.inflight < s.workers {
			timer = s.after(next.Sub(now))
		}
		s.publishDepthLocked()
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return s.shutdown()
		case <-s.wake:
		case <-timer:
		}
	}
}

func (s *Sender) shutdown() error {
	var done []func()
	s.mu.Lock()
	s.closed = true
	for _, c := range s.active {
		done = append(done, s.dropChatLocked(c, ErrClosed, metrics.TelegramDropShutdown)...)
	}
	s.publishDepthLocked()
	s.mu.Unlock()
	run(done)
	s.wg.Wait()
	return nil
}

func (s *Sender) publishDepthLocked() {
	if s.o.Metrics == nil {
		return
	}
	d := make(map[metrics.TelegramClass]int, numClasses)
	for p, n := range s.depth {
		d[classNames[p]] = n
	}
	s.o.Metrics.SetTelegramQueueDepth(d)
}

// pickLocked returns the most urgent sendable item, or nil and when the next one may become sendable (zero: none
// queued, or every queued chat has a call in flight).
func (s *Sender) pickLocked(now time.Time) (*item, time.Time) {
	var best *item
	var next time.Time
	for _, c := range s.active {
		if c.inflight {
			continue
		}
		r := c.pausedUntil
		if c.b != nil {
			if br := c.b.readyAt(now); br.After(r) {
				r = br
			}
		}
		if r.After(now) {
			if next.IsZero() || r.Before(next) {
				next = r
			}
			continue
		}
		if h := c.head(); h != nil && (best == nil || before(h, best)) {
			best = h
		}
	}
	g := s.global.readyAt(now)
	if s.globalP.After(g) {
		g = s.globalP
	}
	if best != nil {
		if g.After(now) {
			return nil, g
		}
		return best, time.Time{}
	}
	if !next.IsZero() && g.After(next) {
		next = g
	}
	return nil, next
}

func (s *Sender) dispatchLocked(ctx context.Context, it *item, now time.Time) {
	c := it.chat
	if it.notice {
		it.msg = Message{ChatID: c.id, Group: c.group, Priority: Notification, Send: s.o.Overflow(c.id, c.dropped)}
		c.notice = nil
		c.dropped = 0
		s.depth[Notification.idx()]--
		if !c.queued() {
			delete(s.active, c.id)
		}
	} else {
		s.unlinkLocked(it)
	}
	if it.attempts == 0 {
		s.o.Metrics.TelegramQueueWait(it.prio.class(), now.Sub(it.enq))
	}
	it.attempts++
	s.global.take(now)
	if c.b != nil {
		c.b.take(now)
		c.inflight = true
	}
	s.inflight++
	s.wg.Add(1)
	go s.attempt(ctx, it)
}

func (s *Sender) attempt(ctx context.Context, it *item) {
	defer s.wg.Done()
	err := it.msg.Send(ctx, s.o.Client)
	s.mu.Lock()
	done := s.finishLocked(ctx, it, err)
	s.mu.Unlock()
	run(done)
	s.signal()
}

// finishLocked handles a call's outcome.
func (s *Sender) finishLocked(ctx context.Context, it *item, err error) []func() {
	c := it.chat
	now := s.clk.Now()
	s.inflight--
	if c.b != nil {
		c.inflight = false
		c.b.settle(now)
	}
	class := it.prio.class()
	switch {
	case err == nil:
		s.o.Metrics.TelegramSend(class, metrics.TelegramSent)
		return []func(){s.doneFn(it, nil)}
	case ctx.Err() != nil:
		s.o.Metrics.TelegramDropped(class, metrics.TelegramDropShutdown, 1)
		return []func(){s.doneFn(it, fmt.Errorf("%w: %w", ErrClosed, err))}
	case botapi.IsForbidden(err):
		s.o.Metrics.TelegramSend(class, metrics.TelegramBlocked)
		done := []func(){s.doneFn(it, fmt.Errorf("%w: %w", ErrBlocked, err))}
		if c.id != 0 && !c.blocked {
			c.blocked = true
			done = append(done, s.dropChatLocked(c, ErrBlocked, metrics.TelegramDropBlocked)...)
			if f := s.o.OnBlocked; f != nil {
				id := c.id
				done = append(done, func() { f(id) })
			}
		}
		return done
	}
	var wait time.Duration
	if ra, ok := botapi.RetryAfter(err); ok {
		wait = max(ra, time.Second)
		scope := metrics.TelegramScopeChat
		if c.id == 0 || (s.last429ID != c.id && !s.last429At.IsZero() && now.Sub(s.last429At) < time.Second) {
			scope = metrics.TelegramScopeGlobal
		}
		s.last429ID, s.last429At = c.id, now
		s.o.Metrics.TelegramRateLimited(scope)
		if scope == metrics.TelegramScopeGlobal {
			s.globalP = later(s.globalP, now.Add(wait))
		} else {
			c.pausedUntil = later(c.pausedUntil, now.Add(wait))
		}
	} else if retryable(err) {
		wait = time.Second << (it.attempts - 1)
		c.pausedUntil = later(c.pausedUntil, now.Add(wait))
	}
	if wait == 0 || it.attempts > MaxRetries || c.blocked || s.closed {
		s.o.Metrics.TelegramSend(class, metrics.TelegramFailed)
		if s.o.Logger != nil {
			s.o.Logger.Warn("telegram send failed", "class", string(class), "attempts", it.attempts, "error", err.Error())
		}
		return []func(){s.doneFn(it, err)}
	}
	if it.notice {
		// The notice's count was taken at dispatch; requeue it as a plain message of its class.
		it.notice = false
	}
	if k := (coalesceKey{c.id, it.msg.Key}); it.msg.Key != "" {
		if _, newer := s.keys[k]; newer {
			s.o.Metrics.TelegramDropped(class, metrics.TelegramDropCoalesced, 1)
			return []func(){s.doneFn(it, ErrCoalesced)}
		}
		s.keys[k] = it
	}
	s.pushLocked(it, true)
	return nil
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// retryable reports a failure that a later attempt may not hit: a transport failure or a 5xx answer.
func retryable(err error) bool {
	var te *botapi.TransportError
	if errors.As(err, &te) {
		return true
	}
	var e *botapi.Error
	return errors.As(err, &e) && e.Code >= http.StatusInternalServerError
}

// sweepLocked forgets idle chats whose state a fresh one would reproduce (bucket full, no pause, nothing queued).
func (s *Sender) sweepLocked(now time.Time) {
	for id, c := range s.chats {
		if id == 0 || c.queued() || c.inflight || c.blocked || c.dropped > 0 || c.pausedUntil.After(now) || (c.b != nil && !c.b.full(now)) {
			continue
		}
		delete(s.chats, id)
	}
}
