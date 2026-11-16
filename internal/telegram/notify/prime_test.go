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
	"github.com/aurimasniekis/go-spaceman/internal/telegram/tgtest"
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
	var calls []tgtest.Call
	if wantSend {
		calls = e.waitCalls("sendMessage", 1)
		time.Sleep(100 * time.Millisecond) // and no more than one
	} else {
		time.Sleep(300 * time.Millisecond)
	}
	calls = e.fake.Calls("sendMessage")
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
