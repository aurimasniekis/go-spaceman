package sender_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics/metricstest"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/sender"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/tgtest"
)

var t0 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

type env struct {
	t    *testing.T
	fake *tgtest.Server
	clk  *clock.Fake
	s    *sender.Sender
	m    *metrics.Metrics
	stop func()
}

// newEnv builds a fake Bot API and a sender sharing a fake clock. The sender has one worker and an After that
// advances the clock at once, so virtual time moves only while the dispatcher waits and nothing is in flight: every
// call reaches the fake at the instant it was dispatched, which makes the rate-limit checks exact.
func newEnv(t *testing.T, mod func(*sender.Options)) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	fake := tgtest.New(t, tgtest.Options{Clock: clk})
	c, err := botapi.New(botapi.Options{BaseURL: fake.URL(), Token: fake.Token()})
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New()
	o := sender.Options{
		Client: c, Workers: 1, Clock: clk, Metrics: m,
		After: func(d time.Duration) <-chan time.Time {
			clk.Advance(d)
			ch := make(chan time.Time, 1)
			ch <- clk.Now()
			return ch
		},
	}
	if mod != nil {
		mod(&o)
	}
	s, err := sender.New(o)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, fake: fake, clk: clk, s: s, m: m}
	e.stop = func() {}
	t.Cleanup(func() { e.stop() })
	return e
}

func (e *env) start() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.s.Run(ctx) }()
	e.stop = func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				e.t.Errorf("Run: %v", err)
			}
		case <-time.After(10 * time.Second):
			e.t.Error("Run did not return")
		}
		e.stop = func() {}
	}
}

// results collects Done outcomes by name.
type results struct {
	mu  sync.Mutex
	got map[string]error
	ord []string
	ch  chan struct{}
}

func newResults() *results { return &results{got: map[string]error{}, ch: make(chan struct{}, 1)} }

func (r *results) done(name string) func(error) {
	return func(err error) {
		r.mu.Lock()
		if _, dup := r.got[name]; dup {
			panic("Done called twice for " + name)
		}
		r.got[name] = err
		r.ord = append(r.ord, name)
		r.mu.Unlock()
		select {
		case r.ch <- struct{}{}:
		default:
		}
	}
}

func (r *results) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func (r *results) err(name string) (error, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	err, ok := r.got[name]
	return err, ok
}

func (r *results) wait(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(60 * time.Second)
	for r.count() < n {
		select {
		case <-r.ch:
		case <-deadline:
			t.Fatalf("waited for %d results, have %d", n, r.count())
		}
	}
}

func text(chat int64, p sender.Priority, key, txt string, done func(error)) sender.Message {
	return sender.Message{
		ChatID: chat, Group: chat < 0, Priority: p, Key: key, Done: done,
		Send: func(ctx context.Context, c *botapi.Client) error {
			_, err := c.SendMessage(ctx, botapi.SendMessageParams{ChatID: chat, Text: txt})
			return err
		},
	}
}

func (e *env) enqueue(m sender.Message) {
	e.t.Helper()
	if err := e.s.Enqueue(m); err != nil {
		e.t.Fatalf("enqueue: %v", err)
	}
}

func texts(fake *tgtest.Server, chat int64) []string {
	var out []string
	for _, m := range fake.Messages(chat) {
		out = append(out, m.Text)
	}
	return out
}

func (e *env) metric(name string, labels ...string) float64 {
	return metricstest.Value(e.t, e.m, name, labels...)
}

func calls429(fake *tgtest.Server) int {
	n := 0
	for _, c := range fake.Calls() {
		if c.Status == http.StatusTooManyRequests {
			n++
		}
	}
	return n
}

func TestLimitsValidate(t *testing.T) {
	if err := sender.DefaultLimits().Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []func(*sender.Limits){
		func(l *sender.Limits) { l.GlobalPerSecond = 31 },
		func(l *sender.Limits) { l.GlobalPerSecond = 0 },
		func(l *sender.Limits) { l.ChatPerSecond = 1.5 },
		func(l *sender.Limits) { l.ChatBurst = 4 },
		func(l *sender.Limits) { l.GroupPerMinute = 21 },
		func(l *sender.Limits) { l.QueueTotal = 0 },
		func(l *sender.Limits) { l.QueueTotal = sender.MaxQueueTotal + 1 },
		func(l *sender.Limits) { l.QueuePerChat = 0 },
		func(l *sender.Limits) { l.QueueTotal, l.QueuePerChat = 10, 20 },
	}
	for i, f := range bad {
		l := sender.DefaultLimits()
		f(&l)
		if l.Validate() == nil {
			t.Errorf("case %d accepted: %+v", i, l)
		}
	}
	e := newEnv(t, nil)
	l := sender.DefaultLimits()
	l.ChatBurst = 9
	if e.s.SetLimits(l) == nil {
		t.Error("SetLimits accepted an out-of-range burst")
	}
	l.ChatBurst = 1
	if err := e.s.SetLimits(l); err != nil {
		t.Error(err)
	}
}

func TestPriorityOrder(t *testing.T) {
	e := newEnv(t, nil)
	r := newResults()
	const chat = 1001
	for _, p := range []sender.Priority{sender.Digest, sender.Edit, sender.Notification, sender.NeedsYou, sender.Interactive} {
		name := strconv.Itoa(int(p))
		e.enqueue(text(chat, p, "", name, r.done(name)))
	}
	e.start()
	r.wait(t, 5)
	if got := strings.Join(texts(e.fake, chat), ","); got != "1,2,3,4,5" {
		t.Fatalf("order = %s", got)
	}
	if got := e.metric("spaceman_telegram_sends_total", "result", "sent"); got != 5 {
		t.Errorf("sent metric = %v", got)
	}
	if got := e.metric("spaceman_telegram_queue_wait_seconds", "class", "digest"); got != 1 {
		t.Errorf("wait observations = %v", got)
	}
}

func TestCoalescingSendsOnlyTheNewest(t *testing.T) {
	e := newEnv(t, nil)
	r := newResults()
	const chat = 1002
	e.enqueue(text(chat, sender.Notification, "card", "v1", r.done("v1")))
	e.enqueue(text(chat, sender.Notification, "", "other", r.done("other")))
	e.enqueue(text(chat, sender.Notification, "card", "v2", r.done("v2")))
	e.enqueue(text(chat, sender.Notification, "card", "v3", r.done("v3")))
	// Another chat's key is independent.
	e.enqueue(text(chat+1, sender.Notification, "card", "x1", r.done("x1")))
	// A class change moves the message to its new class.
	e.enqueue(text(chat, sender.Edit, "edit", "e1", r.done("e1")))
	e.enqueue(text(chat, sender.Digest, "", "digest", r.done("digest")))
	e.enqueue(text(chat, sender.NeedsYou, "edit", "e2", r.done("e2")))
	if got := e.s.Len(); got != 5 {
		t.Fatalf("Len = %d, want 5", got)
	}
	e.start()
	r.wait(t, 8)
	for _, n := range []string{"v1", "v2", "e1"} {
		if err, _ := r.err(n); !errors.Is(err, sender.ErrCoalesced) {
			t.Errorf("%s: err = %v, want ErrCoalesced", n, err)
		}
	}
	// v3 kept v1's place, ahead of "other".
	if got := strings.Join(texts(e.fake, chat), ","); got != "e2,v3,other,digest" {
		t.Fatalf("chat = %s", got)
	}
	if got := strings.Join(texts(e.fake, chat+1), ","); got != "x1" {
		t.Fatalf("other chat = %s", got)
	}
	if got := e.metric("spaceman_telegram_dropped_total", "reason", "coalesced"); got != 3 {
		t.Errorf("coalesced metric = %v", got)
	}
}

func TestInFlightMessageIsNotReplaced(t *testing.T) {
	e := newEnv(t, func(o *sender.Options) { o.Workers = 2 })
	r := newResults()
	release := make(chan struct{})
	started := make(chan struct{})
	var sent []string
	var mu sync.Mutex
	send := func(name string, block bool) sender.SendFunc {
		return func(context.Context, *botapi.Client) error {
			if block {
				close(started)
				<-release
			}
			mu.Lock()
			sent = append(sent, name)
			mu.Unlock()
			return nil
		}
	}
	e.start()
	e.enqueue(sender.Message{ChatID: 7, Priority: sender.Edit, Key: "k", Send: send("v1", true), Done: r.done("v1")})
	<-started
	e.enqueue(sender.Message{ChatID: 7, Priority: sender.Edit, Key: "k", Send: send("v2", false), Done: r.done("v2")})
	close(release)
	r.wait(t, 2)
	for _, n := range []string{"v1", "v2"} {
		if err, _ := r.err(n); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	if got := strings.Join(sent, ","); got != "v1,v2" {
		t.Fatalf("sent = %s", got)
	}
}

func TestPerChatBoundDropsLowestClassAndCollapses(t *testing.T) {
	e := newEnv(t, nil)
	r := newResults()
	const chat = 1003
	for i := range 50 {
		e.enqueue(text(chat, sender.Edit, "", fmt.Sprintf("edit%02d", i), r.done(fmt.Sprintf("edit%02d", i))))
	}
	// A lower class than everything queued is refused.
	for i := range 10 {
		if err := e.s.Enqueue(text(chat, sender.Digest, "", "digest", nil)); !errors.Is(err, sender.ErrQueueFull) {
			t.Fatalf("digest %d: err = %v, want ErrQueueFull", i, err)
		}
	}
	// A higher class drops the oldest of the lowest class.
	for i := range 5 {
		e.enqueue(text(chat, sender.NeedsYou, "", fmt.Sprintf("need%d", i), r.done(fmt.Sprintf("need%d", i))))
	}
	if got := e.s.Len(); got != 51 { // 50 plus the overflow notice
		t.Fatalf("Len = %d, want 51", got)
	}
	for i := range 5 {
		if err, _ := r.err(fmt.Sprintf("edit%02d", i)); !errors.Is(err, sender.ErrQueueFull) {
			t.Errorf("edit%02d: err = %v, want ErrQueueFull", i, err)
		}
	}
	if got := e.metric("spaceman_telegram_overflow_notices_total"); got != 1 {
		t.Errorf("overflow notices = %v", got)
	}
	if got := e.metric("spaceman_telegram_dropped_total", "reason", "queue_full"); got != 15 {
		t.Errorf("queue_full drops = %v", got)
	}
	e.start()
	r.wait(t, 55)
	got := texts(e.fake, chat)
	if len(got) != 51 {
		t.Fatalf("sent %d messages, want 51", len(got))
	}
	want := []string{"need0", "need1", "need2", "need3", "need4", "15 updates — open the app", "edit05"}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("message %d = %q, want %q (all: %v)", i, got[i], w, got[:8])
		}
	}
	if got[50] != "edit49" {
		t.Errorf("last = %q", got[50])
	}
	if n := calls429(e.fake); n != 0 {
		t.Errorf("%d answers were 429", n)
	}
}

func TestTotalBoundDropsOldestOfLowestClass(t *testing.T) {
	l := sender.DefaultLimits()
	l.QueueTotal, l.QueuePerChat = 100, 50
	e := newEnv(t, func(o *sender.Options) { o.Limits = l })
	r := newResults()
	for chat := int64(1); chat <= 4; chat++ {
		for i := range 25 {
			name := fmt.Sprintf("c%d-%d", chat, i)
			e.enqueue(text(chat, sender.Edit, "", name, r.done(name)))
		}
	}
	for i := range 10 {
		e.enqueue(text(5, sender.Notification, "", fmt.Sprint(i), nil))
	}
	if err := e.s.Enqueue(text(6, sender.Digest, "", "d", nil)); !errors.Is(err, sender.ErrQueueFull) {
		t.Fatalf("digest: err = %v", err)
	}
	// Chat 1's ten oldest edits went; chats 1 and 6 each have one notice.
	if r.count() != 10 {
		t.Fatalf("%d dropped, want 10", r.count())
	}
	for i := range 10 {
		if err, _ := r.err(fmt.Sprintf("c1-%d", i)); !errors.Is(err, sender.ErrQueueFull) {
			t.Errorf("c1-%d: %v", i, err)
		}
	}
	if got := e.s.Len(); got != 102 {
		t.Fatalf("Len = %d, want 102", got)
	}
	if got := e.metric("spaceman_telegram_overflow_notices_total"); got != 2 {
		t.Errorf("overflow notices = %v", got)
	}
}

// TestStressNoRateLimitAtDefaults is the plan §5.4 TG-2 acceptance test: 10,000 messages over 20 private chats and
// 20 groups at the default limits, against the fake's FAQ limits, get no 429 and keep each chat's order. Every chat
// always has a backlog, so each chat's bucket is the bottleneck for that chat, and together (20/s + 6.7/s) they ask
// for more than the global 25/s.
func TestStressNoRateLimitAtDefaults(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	e := newEnv(t, nil)
	const total = 10000
	var chats []int64
	for i := range 20 {
		chats = append(chats, int64(5000+i))
	}
	for i := range 20 {
		chats = append(chats, int64(-7000-i))
	}
	var failed atomic.Int64
	outstanding := make([]atomic.Int64, len(chats))
	var finished atomic.Int64
	next := make([]int, len(chats))
	e.start()
	start := e.clk.Now()
	enq := 0
	for enq < total {
		progressed := false
		for ci, chat := range chats {
			// Groups get 100 messages each, private chats 400 each: 2000 + 8000.
			quota := 400
			if chat < 0 {
				quota = 100
			}
			if next[ci] >= quota || outstanding[ci].Load() >= 40 {
				continue
			}
			i := next[ci]
			next[ci]++
			outstanding[ci].Add(1)
			e.enqueue(text(chat, sender.Priority(1+i%5), "", fmt.Sprintf("%d", i), func(err error) {
				if err != nil {
					failed.Add(1)
				}
				outstanding[ci].Add(-1)
				finished.Add(1)
			}))
			enq++
			progressed = true
		}
		if !progressed {
			time.Sleep(time.Millisecond)
		}
	}
	deadline := time.Now().Add(5 * time.Minute)
	for finished.Load() < total {
		if time.Now().After(deadline) {
			t.Fatalf("finished %d of %d", finished.Load(), total)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := failed.Load(); n != 0 {
		t.Fatalf("%d messages failed", n)
	}
	if n := calls429(e.fake); n != 0 {
		t.Fatalf("%d answers were 429", n)
	}
	sent := 0
	for _, chat := range chats {
		got := texts(e.fake, chat)
		sent += len(got)
		// Classes interleave, but within a class each chat's messages keep their order.
		lastByClass := map[int]int{}
		for _, s := range got {
			i, _ := strconv.Atoi(s)
			if prev, ok := lastByClass[i%5]; ok && i < prev {
				t.Fatalf("chat %d: %d after %d", chat, i, prev)
			}
			lastByClass[i%5] = i
		}
	}
	if sent != total {
		t.Fatalf("delivered %d, want %d", sent, total)
	}
	if got := e.metric("spaceman_telegram_rate_limited_total"); got != 0 {
		t.Errorf("rate limited metric = %v", got)
	}
	// 25/s globally: 10,000 messages need at least ~399 s of virtual time.
	if el := e.clk.Now().Sub(start); el < 399*time.Second {
		t.Errorf("finished in %v of virtual time: faster than the global limit allows", el)
	}
}

// TestSlowCallDoesNotRefillChatBucket: a chat's bucket doesn't refill while its call is in flight, so a slow call
// followed by fast ones can't exceed the chat's burst at the times Telegram sees them.
func TestSlowCallDoesNotRefillChatBucket(t *testing.T) {
	e := newEnv(t, nil)
	r := newResults()
	const chat = 81
	for i := range 6 {
		name := fmt.Sprint(i)
		m := text(chat, sender.Notification, "", name, r.done(name))
		if i == 0 {
			inner := m.Send
			m.Send = func(ctx context.Context, c *botapi.Client) error {
				e.clk.Advance(2 * time.Second) // the request takes 2 s to reach Telegram
				return inner(ctx, c)
			}
		}
		e.enqueue(m)
	}
	e.start()
	r.wait(t, 6)
	if n := calls429(e.fake); n != 0 {
		t.Fatalf("%d answers were 429", n)
	}
}

func TestRetryAfterPausesOnlyThatChat(t *testing.T) {
	e := newEnv(t, nil)
	r := newResults()
	e.fake.FailNext("sendMessage", http.StatusTooManyRequests, "Too Many Requests: retry after 7", 7)
	e.enqueue(text(11, sender.Notification, "", "a", r.done("a")))
	e.enqueue(text(12, sender.Notification, "", "b", r.done("b")))
	e.start()
	r.wait(t, 2)
	for _, n := range []string{"a", "b"} {
		if err, _ := r.err(n); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
	}
	calls := e.fake.Calls("sendMessage")
	if len(calls) != 3 {
		t.Fatalf("%d calls, want 3", len(calls))
	}
	if calls[0].Params["chat_id"] != "11" || calls[0].Status != http.StatusTooManyRequests {
		t.Fatalf("first call = %+v", calls[0])
	}
	if calls[1].Params["chat_id"] != "12" || calls[1].At.Sub(calls[0].At) >= time.Second {
		t.Errorf("chat 12 was held back: %+v", calls[1])
	}
	if calls[2].Params["chat_id"] != "11" || calls[2].At.Sub(calls[0].At) < 7*time.Second {
		t.Errorf("retry came %v after the 429, want at least 7s", calls[2].At.Sub(calls[0].At))
	}
	if got := e.metric("spaceman_telegram_rate_limited_total", "scope", "chat"); got != 1 {
		t.Errorf("chat 429 metric = %v", got)
	}
}

func TestGlobalRetryAfterPausesEveryChat(t *testing.T) {
	e := newEnv(t, nil)
	r := newResults()
	e.fake.FailNext("sendMessage", http.StatusTooManyRequests, "Too Many Requests: retry after 2", 2)
	e.fake.FailNext("sendMessage", http.StatusTooManyRequests, "Too Many Requests: retry after 5", 5)
	for i, n := range []string{"a", "b", "c"} {
		e.enqueue(text(int64(21+i), sender.Notification, "", n, r.done(n)))
	}
	e.start()
	r.wait(t, 3)
	calls := e.fake.Calls("sendMessage")
	if len(calls) != 5 {
		t.Fatalf("%d calls, want 5", len(calls))
	}
	first := calls[0].At
	for _, c := range calls[2:] {
		if c.At.Sub(first) < 5*time.Second {
			t.Errorf("call to %s at +%v, before the global pause ended", c.Params["chat_id"], c.At.Sub(first))
		}
	}
	if got := e.metric("spaceman_telegram_rate_limited_total", "scope", "global"); got != 1 {
		t.Errorf("global 429 metric = %v", got)
	}
	if got := e.metric("spaceman_telegram_rate_limited_total", "scope", "chat"); got != 1 {
		t.Errorf("chat 429 metric = %v", got)
	}
}

func TestRetriesAreCapped(t *testing.T) {
	e := newEnv(t, nil)
	r := newResults()
	for range 4 {
		e.fake.FailNext("sendMessage", http.StatusTooManyRequests, "Too Many Requests: retry after 1", 1)
	}
	e.enqueue(text(31, sender.Notification, "", "x", r.done("x")))
	e.start()
	r.wait(t, 1)
	err, _ := r.err("x")
	if _, ok := botapi.RetryAfter(err); !ok {
		t.Fatalf("err = %v, want the 429", err)
	}
	if n := len(e.fake.Calls("sendMessage")); n != 1+sender.MaxRetries {
		t.Fatalf("%d calls, want %d", n, 1+sender.MaxRetries)
	}
	if got := e.metric("spaceman_telegram_sends_total", "result", "failed"); got != 1 {
		t.Errorf("failed metric = %v", got)
	}
}

func TestServerErrorsBackOffAndBadRequestsDoNotRetry(t *testing.T) {
	e := newEnv(t, nil)
	r := newResults()
	e.fake.FailNext("sendMessage", http.StatusBadGateway, "Bad Gateway", 0)
	e.fake.FailNext("sendMessage", http.StatusBadGateway, "Bad Gateway", 0)
	e.enqueue(text(41, sender.Notification, "", "x", r.done("x")))
	e.start()
	r.wait(t, 1)
	if err, _ := r.err("x"); err != nil {
		t.Fatalf("x: %v", err)
	}
	calls := e.fake.Calls("sendMessage")
	if len(calls) != 3 {
		t.Fatalf("%d calls, want 3", len(calls))
	}
	if d := calls[1].At.Sub(calls[0].At); d < time.Second {
		t.Errorf("first retry after %v, want 1s", d)
	}
	if d := calls[2].At.Sub(calls[1].At); d < 2*time.Second {
		t.Errorf("second retry after %v, want 2s", d)
	}

	e.fake.FailNext("editMessageText", http.StatusBadRequest, "Bad Request: message is not modified", 0)
	e.enqueue(sender.Message{ChatID: 41, Priority: sender.Edit, Done: r.done("edit"), Send: func(ctx context.Context, c *botapi.Client) error {
		_, err := c.EditMessageText(ctx, botapi.EditMessageTextParams{ChatID: 41, MessageID: 1, Text: "y"})
		return err
	}})
	r.wait(t, 2)
	var be *botapi.Error
	if err, _ := r.err("edit"); !errors.As(err, &be) || be.Code != http.StatusBadRequest {
		t.Fatalf("edit: err = %v", err)
	}
	if n := len(e.fake.Calls("editMessageText")); n != 1 {
		t.Errorf("a 400 was retried: %d calls", n)
	}
}

func TestBlockedChat(t *testing.T) {
	var blocked []int64
	var mu sync.Mutex
	e := newEnv(t, func(o *sender.Options) {
		o.OnBlocked = func(id int64) { mu.Lock(); blocked = append(blocked, id); mu.Unlock() }
	})
	r := newResults()
	const chat = 51
	e.fake.Block(chat)
	for _, n := range []string{"a", "b", "c"} {
		e.enqueue(text(chat, sender.Notification, "", n, r.done(n)))
	}
	e.enqueue(text(chat+1, sender.Notification, "", "other", r.done("other")))
	e.start()
	r.wait(t, 4)
	for _, n := range []string{"a", "b", "c"} {
		if err, _ := r.err(n); !errors.Is(err, sender.ErrBlocked) {
			t.Errorf("%s: err = %v, want ErrBlocked", n, err)
		}
	}
	if err, _ := r.err("a"); !botapi.IsForbidden(err) {
		t.Errorf("the sent message's error should wrap the 403: %v", err)
	}
	if err, _ := r.err("other"); err != nil {
		t.Errorf("other chat: %v", err)
	}
	if n := len(e.fake.Calls("sendMessage")); n != 2 {
		t.Errorf("%d calls, want 2 (one to the blocked chat)", n)
	}
	mu.Lock()
	if len(blocked) != 1 || blocked[0] != chat {
		t.Errorf("OnBlocked calls = %v", blocked)
	}
	mu.Unlock()
	if !e.s.Blocked(chat) {
		t.Error("chat not marked blocked")
	}
	if err := e.s.Enqueue(text(chat, sender.Interactive, "", "d", nil)); !errors.Is(err, sender.ErrBlocked) {
		t.Fatalf("enqueue to a blocked chat: %v", err)
	}
	// The user writes to the bot again.
	e.fake.Unblock(chat)
	e.s.Unblock(chat)
	e.enqueue(text(chat, sender.Interactive, "", "back", r.done("back")))
	r.wait(t, 5)
	if err, _ := r.err("back"); err != nil {
		t.Fatalf("after unblock: %v", err)
	}
	if got := e.metric("spaceman_telegram_sends_total", "result", "blocked"); got != 1 {
		t.Errorf("blocked metric = %v", got)
	}
	if got := e.metric("spaceman_telegram_dropped_total", "reason", "blocked"); got != 2 {
		t.Errorf("blocked drops = %v", got)
	}

	// SetBlocked restores a blocked link at start and drops its queue without calling OnBlocked.
	e.stop()
	e2 := newEnv(t, nil)
	r2 := newResults()
	e2.enqueue(text(61, sender.Notification, "", "q", r2.done("q")))
	e2.s.SetBlocked(61, true)
	if err, ok := r2.err("q"); !ok || !errors.Is(err, sender.ErrBlocked) {
		t.Fatalf("queued message: %v %v", err, ok)
	}
	if e2.s.Len() != 0 {
		t.Fatalf("Len = %d", e2.s.Len())
	}
}

func TestMessageWithoutChatTakesOnlyGlobalTokens(t *testing.T) {
	e := newEnv(t, nil)
	r := newResults()
	var n atomic.Int64
	for i := range 10 {
		name := fmt.Sprint(i)
		e.enqueue(sender.Message{Priority: sender.Interactive, Done: r.done(name), Send: func(context.Context, *botapi.Client) error {
			n.Add(1)
			return nil
		}})
	}
	start := e.clk.Now()
	e.start()
	r.wait(t, 10)
	// Ten in a row would take 7 s through a private chat's bucket; the global burst allows them at once.
	if el := e.clk.Now().Sub(start); el > 0 {
		t.Errorf("took %v of virtual time", el)
	}
}

func TestShutdownDropsQueue(t *testing.T) {
	e := newEnv(t, nil)
	r := newResults()
	for i := range 5 {
		name := fmt.Sprint(i)
		e.enqueue(text(-71, sender.Digest, "", name, r.done(name)))
	}
	e.s.SetBlocked(-72, true)
	// No start: stopping with a queue drops it.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if r.count() != 5 {
		t.Fatalf("%d results", r.count())
	}
	if err, _ := r.err("0"); !errors.Is(err, sender.ErrClosed) {
		t.Fatalf("err = %v", err)
	}
	if err := e.s.Enqueue(text(-71, sender.Digest, "", "late", nil)); !errors.Is(err, sender.ErrClosed) {
		t.Fatalf("enqueue after stop: %v", err)
	}
	if err := e.s.Run(context.Background()); err == nil {
		t.Fatal("a second Run started")
	}
	if got := e.metric("spaceman_telegram_dropped_total", "reason", "shutdown"); got != 5 {
		t.Errorf("shutdown drops = %v", got)
	}
}

func TestEnqueueValidates(t *testing.T) {
	e := newEnv(t, nil)
	if err := e.s.Enqueue(sender.Message{ChatID: 1, Priority: sender.Interactive}); err == nil {
		t.Error("message without Send accepted")
	}
	if err := e.s.Enqueue(text(1, 0, "", "x", nil)); err == nil {
		t.Error("priority 0 accepted")
	}
	if err := e.s.Enqueue(text(1, sender.Digest+1, "", "x", nil)); err == nil {
		t.Error("priority above Digest accepted")
	}
	if _, err := sender.New(sender.Options{}); err == nil {
		t.Error("New without a client")
	}
}

// TestConcurrentWorkersRealClock runs eight workers against the fake on the system clock: concurrent calls still
// stay inside the fake's limits.
func TestConcurrentWorkersRealClock(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about a second")
	}
	fake := tgtest.New(t, tgtest.Options{})
	c, err := botapi.New(botapi.Options{BaseURL: fake.URL(), Token: fake.Token()})
	if err != nil {
		t.Fatal(err)
	}
	s, err := sender.New(sender.Options{Client: c, Workers: 8})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	r := newResults()
	for chat := int64(1); chat <= 15; chat++ {
		for i := range 3 {
			name := fmt.Sprintf("%d-%d", chat, i)
			if err := s.Enqueue(text(chat, sender.Notification, "", name, r.done(name))); err != nil {
				t.Fatal(err)
			}
		}
	}
	r.wait(t, 45)
	r.mu.Lock()
	for n, err := range r.got {
		if err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	r.mu.Unlock()
	if n := calls429(fake); n != 0 {
		t.Fatalf("%d answers were 429", n)
	}
	for chat := int64(1); chat <= 15; chat++ {
		want := fmt.Sprintf("%d-0,%d-1,%d-2", chat, chat, chat)
		if got := strings.Join(texts(fake, chat), ","); got != want {
			t.Errorf("chat %d: %s", chat, got)
		}
	}
}
