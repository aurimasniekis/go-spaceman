package notify_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/notify"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/sender"
)

// A change committed between Prime and Run (central's listener and recover passes run in between) is delivered when
// Run starts, without waiting for another change; history before Prime is not replayed.
func TestPrimedStartDeliversChangesMadeBeforeRun(t *testing.T) {
	e := newEnv(t, 1)
	e.rd.set(action("hum_old", 1, false))
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_old", changefeed.Created, 1)
	e.n = notify.New(e.options(nil))
	if err := e.n.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.rd.set(action("hum_mid", 1, false))
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_mid", changefeed.Created, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.n.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case <-e.n.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("Ready was not closed after Prime and Run")
	}
	calls := e.waitCalls("sendMessage", 1)
	time.Sleep(100 * time.Millisecond)
	if n := len(e.fake.Calls("sendMessage")); n != 1 || !strings.Contains(calls[0].Params["text"], "hum_mid") {
		t.Errorf("sends = %d, first %q: want only the change made after Prime", n, calls[0].Params["text"])
	}
}

// A failed Prime leaves the lazy start in Run.
func TestFailedPrimeFallsBackToTheLazyStart(t *testing.T) {
	e := newEnv(t, 1)
	f := &latestFeed{feed: e.feed, entered: make(chan struct{}, 1), gate: make(chan struct{})}
	boom := errors.New("feed down")
	f.err.Store(&boom)
	close(f.gate)
	opts := e.options(nil)
	opts.Feed = f
	e.n = notify.New(opts)
	if err := e.n.Prime(context.Background()); err == nil {
		t.Fatal("Prime succeeded with a failing feed")
	}
	f.err.Store(nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.n.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case <-e.n.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("Ready was not closed by the lazy start")
	}
}

// memCursors is an in-memory notify.Cursors.
type memCursors struct {
	mu  sync.Mutex
	seq int64
	at  time.Time
	ok  bool
}

func (m *memCursors) Load(context.Context) (int64, time.Time, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seq, m.at, m.ok, nil
}

func (m *memCursors) Save(_ context.Context, seq int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq, m.at, m.ok = seq, at, true
	return nil
}

// restart stops a first notifier (which saves its position), makes a change while nothing runs, waits down, then
// starts a second notifier over the same saved position and reports how many cards it sent.
func restartAfter(t *testing.T, down time.Duration, wantSend bool) (sent int, text string) {
	t.Helper()
	e := newEnv(t, 1)
	cur := &memCursors{}
	opts := e.options(nil)
	opts.Cursors = cur

	first := notify.New(opts)
	if err := first.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan struct{})
	go func() { _ = first.Run(ctx1); close(done1) }()
	<-first.Ready()
	cancel1()
	<-done1
	if !cur.ok {
		t.Fatal("the position was not saved at shutdown")
	}

	e.rd.set(action("hum_down", 1, false))
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_down", changefeed.Created, 1)
	e.clk.Advance(down)

	e.n = notify.New(opts)
	if err := e.n.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { _ = e.n.Run(ctx2); close(done2) }()
	defer func() { cancel2(); <-done2 }()
	<-e.n.Ready()
	if wantSend {
		e.waitCalls("sendMessage", 1)
		time.Sleep(100 * time.Millisecond) // and no more than one
	} else {
		time.Sleep(300 * time.Millisecond)
	}
	calls := e.fake.Calls("sendMessage")
	if len(calls) > 0 {
		text = calls[0].Params["text"]
	}
	return len(calls), text
}

// A restart within the replay bound delivers what changed while central was down.
func TestRestartResumesFromTheSavedPosition(t *testing.T) {
	n, text := restartAfter(t, time.Minute, true)
	if n != 1 || !strings.Contains(text, "hum_down") {
		t.Errorf("sends = %d, first %q; want the change made while down", n, text)
	}
}

// A saved position older than the bound is not replayed: the start is the newest change.
func TestStaleSavedPositionStartsAtTheNewestChange(t *testing.T) {
	if n, text := restartAfter(t, notify.DefaultMaxReplay+time.Minute, false); n != 0 {
		t.Errorf("sends = %d, first %q; a stale position must not replay", n, text)
	}
}

// startWith runs a notifier over cur (primed), with one change made after Prime, and reports the sends.
func startWith(t *testing.T, cur *memCursors, setup func(e *env)) *env {
	t.Helper()
	e := newEnv(t, 1)
	e.rd.set(action("hum_old", 1, false))
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_old", changefeed.Created, 1)
	if setup != nil {
		setup(e)
	}
	opts := e.options(nil)
	opts.Cursors = cur
	e.n = notify.New(opts)
	if err := e.n.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.rd.set(action("hum_new", 1, false))
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_new", changefeed.Created, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.n.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	<-e.n.Ready()
	return e
}

// A saved position beyond the newest change comes from a restore from backup: it is dropped, and the start is the
// newest change (the change made after Prime is still delivered, the old one is not).
func TestSavedPositionBeyondTheNewestChangeIsDropped(t *testing.T) {
	cur := &memCursors{seq: 9999, ok: true}
	e := startWith(t, cur, func(e *env) { cur.at = e.clk.Now() })
	calls := e.waitCalls("sendMessage", 1)
	time.Sleep(100 * time.Millisecond)
	if n := len(e.fake.Calls("sendMessage")); n != 1 || !strings.Contains(calls[0].Params["text"], "hum_new") {
		t.Errorf("sends = %d, first %q; want only hum_new", n, calls[0].Params["text"])
	}
}

// A save time in the future (the clock was set back) is not trusted: the start is the newest change.
func TestSavedPositionWithNegativeAgeIsDropped(t *testing.T) {
	cur := &memCursors{seq: 0, ok: true}
	e := startWith(t, cur, func(e *env) { cur.at = e.clk.Now().Add(time.Hour) })
	calls := e.waitCalls("sendMessage", 1)
	time.Sleep(100 * time.Millisecond)
	if n := len(e.fake.Calls("sendMessage")); n != 1 || !strings.Contains(calls[0].Params["text"], "hum_new") {
		t.Errorf("sends = %d, first %q; want only hum_new (hum_old is history)", n, calls[0].Params["text"])
	}
}

// With nothing changing, the position is saved again once the last save is old, so a long-running notifier's save
// time keeps a restart within the bound.
func TestIdlePositionIsSavedPeriodically(t *testing.T) {
	cur := &memCursors{}
	e := startWith(t, cur, func(e *env) { e.flushEvery = 5 * time.Millisecond })
	e.waitCalls("sendMessage", 1)
	read := func() time.Time { cur.mu.Lock(); defer cur.mu.Unlock(); return cur.at }
	wait := func(what string, ok func(time.Time) bool) time.Time {
		deadline := time.Now().Add(10 * time.Second)
		for !ok(read()) {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
			time.Sleep(5 * time.Millisecond)
		}
		return read()
	}
	first := wait("the moved position was never saved", func(at time.Time) bool { return !at.IsZero() })
	e.clk.Advance(2 * time.Minute)
	wait("the idle position was never saved again", func(at time.Time) bool { return at.After(first) })
}

// The bot's sender comes up after the notifier started (they start side by side): the window waits for it and is
// delivered once, not consumed and lost.
func TestWindowWaitsForALateSender(t *testing.T) {
	e := newEnv(t, 1)
	real := e.snd
	var up atomic.Bool
	e.retry = 10 * time.Millisecond
	opts := e.options(nil)
	opts.Sender = func() *sender.Sender {
		if up.Load() {
			return real
		}
		return nil
	}
	e.n = notify.New(opts)
	if err := e.n.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.rd.set(action("hum_boot", 1, false))
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_boot", changefeed.Created, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.n.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	<-e.n.Ready()
	e.ring()
	e.endWindow()
	time.Sleep(150 * time.Millisecond)
	if n := len(e.fake.Calls("sendMessage")); n != 0 {
		t.Fatalf("sends = %d before the sender exists", n)
	}
	up.Store(true)
	calls := e.waitCalls("sendMessage", 1)
	time.Sleep(150 * time.Millisecond)
	if n := len(e.fake.Calls("sendMessage")); n != 1 || !strings.Contains(calls[0].Params["text"], "hum_boot") {
		t.Errorf("sends = %d, first %q; want hum_boot exactly once", n, calls[0].Params["text"])
	}
}

// Done is closed after Run returned.
func TestDoneIsClosedAfterRun(t *testing.T) {
	e := newEnv(t, 1)
	e.n = notify.New(e.options(nil))
	if e.n.Started() {
		t.Fatal("Started before Run")
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = e.n.Run(ctx) }()
	<-e.n.Ready()
	cancel()
	select {
	case <-e.n.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done was not closed")
	}
	if !e.n.Started() {
		t.Error("Started is false after Run")
	}
}

// countCursors is a memCursors that counts Save calls.
type countCursors struct {
	memCursors
	saves atomic.Int64
}

func (c *countCursors) Save(ctx context.Context, seq int64, at time.Time) error {
	c.saves.Add(1)
	return c.memCursors.Save(ctx, seq, at)
}

// resumedEnv has a card for hum_x (message 55, version 1) in the store, the item changed since (version 2, a Created
// entry after the saved position), and a saved position that is recent: the first window is a resumed one.
func resumedEnv(t *testing.T) (*env, notify.Options) {
	t.Helper()
	e := newEnv(t, 1)
	e.retry = 10 * time.Millisecond
	e.rd.set(action("hum_x", 2, false))
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_x", changefeed.Created, 2)
	l := linkFor(1)
	_ = e.maps.Save(context.Background(), []notify.Mapping{{ChatID: l.ChatID, MessageID: 55, LinkID: l.ID, SpaceID: "spc_1",
		Key: notify.Key{Type: notify.TypeHumanAction, ID: "hum_x"}, Version: 1, CreatedAt: e.clk.Now(), Kind: notify.MappingCard}})
	opts := e.options(e.maps)
	opts.Cursors = &memCursors{seq: 0, at: e.clk.Now(), ok: true}
	return e, opts
}

func runOpts(e *env, opts notify.Options) (stop func()) {
	e.n = notify.New(opts)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.n.Run(ctx); close(done) }()
	return func() { cancel(); <-done }
}

// A Created entry in a resumed window may have its card from before a crash: the stored card is loaded and edited
// (the fake server doesn't hold the seeded message, so the edit may fall back to a send: the edit call is the proof).
func TestResumedWindowEditsAnExistingCard(t *testing.T) {
	e, opts := resumedEnv(t)
	defer runOpts(e, opts)()
	e.waitCalls("editMessageText", 1)
	if e.maps.lookupCount() == 0 {
		t.Error("the stored card was not looked up")
	}
}

// A feed read that fails in a resumed window leaves it resumed: the retry still loads the stored card and edits it.
func TestFailedReadKeepsTheWindowResumed(t *testing.T) {
	e, opts := resumedEnv(t)
	e.feed.failAt.Store(1)
	defer runOpts(e, opts)()
	e.waitCalls("editMessageText", 1)
	if e.feed.reads.Load() < 2 {
		t.Errorf("feed reads = %d; the failed read was not retried", e.feed.reads.Load())
	}
}

// lateSender wires a sender that exists once up is set, and counts how often it is asked.
func lateSender(e *env, opts *notify.Options) (up *atomic.Bool, asks *atomic.Int64) {
	up, asks = new(atomic.Bool), new(atomic.Int64)
	real := e.snd
	opts.Sender = func() *sender.Sender {
		asks.Add(1)
		if up.Load() {
			return real
		}
		return nil
	}
	return up, asks
}

func waitAsks(t *testing.T, asks *atomic.Int64, n int64) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for asks.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("the sender was asked %d times, want %d", asks.Load(), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// While the window waits for a sender the position isn't saved, neither on the flush tick nor at shutdown: the saved
// position and its time stay old.
func TestPositionIsNotSavedWhileWaitingForASender(t *testing.T) {
	e, opts := resumedEnv(t)
	cur := &countCursors{memCursors: memCursors{seq: 0, at: e.clk.Now(), ok: true}}
	opts.Cursors = cur
	opts.FlushEvery = 5 * time.Millisecond
	_, asks := lateSender(e, &opts)
	stop := runOpts(e, opts)
	waitAsks(t, asks, 3)
	e.clk.Advance(2 * time.Minute) // the last save is old (a flush tick would save it again), still within the bound
	base := asks.Load()
	waitAsks(t, asks, base+5) // the loop and the flush ticker have run since
	stop()                    // and shutdown
	if n := cur.saves.Load(); n != 0 {
		t.Errorf("saves = %d while no sender existed; want 0", n)
	}
	if cur.seq != 0 || !cur.at.Equal(t0) {
		t.Errorf("saved position = %d at %v; want it unchanged", cur.seq, cur.at)
	}
}

// A window that waited for a sender longer than the replay bound is given up: nothing of it is sent, and a change
// made afterwards is delivered.
func TestWindowIsGivenUpAfterTheReplayBound(t *testing.T) {
	e := newEnv(t, 1)
	e.retry = 10 * time.Millisecond
	opts := e.options(nil)
	up, asks := lateSender(e, &opts)
	e.n = notify.New(opts)
	if err := e.n.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.rd.set(action("hum_boot", 1, false))
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_boot", changefeed.Created, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.n.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	<-e.n.Ready()
	waitAsks(t, asks, 3) // the window waits
	latests := e.feed.latests.Load()
	e.clk.Advance(notify.DefaultMaxReplay + time.Second)
	deadline := time.Now().Add(20 * time.Second)
	for e.feed.latests.Load() == latests { // giving up reads the newest change
		if time.Now().After(deadline) {
			t.Fatal("the window was not given up")
		}
		time.Sleep(2 * time.Millisecond)
	}
	up.Store(true)
	e.rd.set(action("hum_after", 1, false))
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_after", changefeed.Created, 1)
	e.ring()
	e.endWindow()
	calls := e.waitCalls("sendMessage", 1)
	time.Sleep(100 * time.Millisecond)
	all := e.fake.Calls("sendMessage")
	if len(all) != 1 || !strings.Contains(calls[0].Params["text"], "hum_after") {
		t.Errorf("sends = %d, first %q; want only hum_after (hum_boot was given up)", len(all), calls[0].Params["text"])
	}
}
