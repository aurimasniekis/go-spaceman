package notify_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/notify"
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
