package agent

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// Run returns only after the catch-up pass a session started has ended: a pass still shipping after Run returned read
// package settings (endedCatchUpRate) that the caller restores next.
func TestRunJoinsTheCatchUpPass(t *testing.T) {
	e := newShipEnv(t)
	e.run("run_0")
	e.add("run_0", 600) // more than one batch
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	shipBatchHook = func(string, int) { once.Do(func() { close(entered); <-release }) }
	e.fc = newWSCentral(t, proto.MaxMinor)
	e.a.o.ReconnectMax = 10 * time.Millisecond
	e.a.o.Clock, e.a.o.CentralURL, e.a.o.Credential = clock.NewFake(time.Now()), e.fc.srv.URL, "spaceman_nd_test"
	e.a.o.BridgeSocket = filepath.Join(t.TempDir(), "b.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.a.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	go func() { // central stores and acknowledges what it receives, the catch-up's probes included
		for {
			select {
			case env := <-e.fc.msgs:
				e.apply(env)
			case <-done:
				return
			}
		}
	}()
	unhold := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unhold) // runs first: never leave the pass held
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the catch-up pass never shipped a batch")
	}
	cancel()
	select {
	case <-done:
		t.Fatal("Run returned while its catch-up pass was still running")
	case <-time.After(300 * time.Millisecond):
	}
	unhold()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return once the catch-up pass ended")
	}
}
