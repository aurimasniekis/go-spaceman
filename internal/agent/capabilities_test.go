package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/harness/dist"
	"github.com/aurimasniekis/go-spaceman/internal/harness/dist/disttest"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// wsCentral accepts one node connection at a time, welcomes it at minor and forwards everything the node sends.
type wsCentral struct {
	srv  *httptest.Server
	msgs chan proto.Envelope
	// hold, when set, delays each welcome until it is closed.
	hold chan struct{}
	// drop, when set (before the agent connects), is asked about every message: true closes the connection without
	// forwarding it (the message is lost with the connection).
	drop func(env proto.Envelope) bool
}

func newWSCentral(t *testing.T, minor int) *wsCentral { return newHeldWSCentral(t, minor, nil) }

// newHeldWSCentral is newWSCentral with the welcome held until hold is closed (nil: welcomed at once).
func newHeldWSCentral(t *testing.T, minor int, hold chan struct{}) *wsCentral {
	t.Helper()
	fc := &wsCentral{msgs: make(chan proto.Envelope, 256), hold: hold}
	fc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{proto.Subprotocol}})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		conn.SetReadLimit(proto.MaxMessage)
		welcomed := false
		for {
			_, b, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			env, err := proto.Decode(b)
			if err != nil {
				return
			}
			if fc.drop != nil && fc.drop(env) {
				return
			}
			select {
			case fc.msgs <- env:
			default:
			}
			if env.Type == proto.TypeHello && !welcomed {
				welcomed = true
				if fc.hold != nil {
					select {
					case <-fc.hold:
					case <-r.Context().Done():
						return
					}
				}
				out, _ := proto.Encode("msg_welcome", proto.TypeWelcome, time.Now(), proto.Welcome{NodeID: "nod_test", Minor: minor, HeartbeatInterval: time.Hour})
				if conn.Write(r.Context(), websocket.MessageText, out) != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(fc.srv.Close)
	return fc
}

// next returns the capabilities of the next hello or node.capabilities message, or fails after a timeout.
func (fc *wsCentral) next(t *testing.T, typ string) proto.Capabilities {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case env := <-fc.msgs:
			switch {
			case env.Type != typ:
			case typ == proto.TypeHello:
				var h proto.Hello
				if err := env.Decode(&h); err != nil {
					t.Fatal(err)
				}
				return h.Capabilities
			default:
				var c proto.NodeCapabilities
				if err := env.Decode(&c); err != nil {
					t.Fatal(err)
				}
				return c.Capabilities
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", typ)
		}
	}
}

// none fails when a message of type typ arrives within d.
func (fc *wsCentral) none(t *testing.T, typ string, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case env := <-fc.msgs:
			if env.Type == typ {
				t.Fatalf("unexpected %s", typ)
			}
		case <-deadline:
			return
		}
	}
}

func claudeEntry(t *testing.T, c proto.Capabilities) proto.HarnessInfo {
	t.Helper()
	for _, h := range c.Harnesses {
		if h.Kind == harness.KindClaude {
			return h
		}
	}
	t.Fatalf("no Claude Code entry in %+v", c)
	return proto.HarnessInfo{}
}

// runAgent connects a to central with a fake clock and a short check tick; it stops when the test ends.
func runAgent(t *testing.T, a *Agent, fc *wsCentral) *clock.Fake {
	t.Helper()
	old := capsCheck
	capsCheck = 5 * time.Millisecond
	clk := clock.NewFake(time.Now())
	a.o.Clock, a.o.CentralURL, a.o.Credential = clk, fc.srv.URL, "spaceman_nd_test"
	a.o.BridgeSocket = filepath.Join(t.TempDir(), "b.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		capsCheck = old
	})
	return clk
}

// SM-HARN-008 (simulated: fake release server and central): a managed node re-reports its capabilities when its cache
// changes and once the interval has passed, and never to a central older than minor 11.
func TestCapabilitiesReReported(t *testing.T) {
	srv := disttest.New(t)
	for _, v := range []string{"2.1.900", "2.1.901"} {
		srv.Add(v, disttest.StubBinary(v))
	}
	srv.SetPointer(dist.ChannelLatest, "2.1.900")
	dir := t.TempDir()
	m := srv.Manager(t, filepath.Join(dir, "harness", "claude-code"), nil)
	if _, err := m.Ensure(context.Background(), "2.1.900"); err != nil {
		t.Fatal(err)
	}
	a, _ := ccAgent(t, dir, m, func(o *Options) { delete(o.HarnessBinaries, harness.KindClaude) })
	fc := newWSCentral(t, proto.MinorHarnessVersion)
	clk := runAgent(t, a, fc)

	hello := claudeEntry(t, fc.next(t, proto.TypeHello))
	if !hello.Managed || !slices.Contains(hello.Cached, "2.1.900") {
		t.Fatalf("hello %+v", hello)
	}
	// The channel moves on and its target is installed: each change is re-reported, without waiting for the interval.
	srv.SetPointer(dist.ChannelLatest, "2.1.901")
	if _, err := m.Ensure(context.Background(), "2.1.901"); err != nil {
		t.Fatal(err)
	}
	var got proto.HarnessInfo
	for !slices.Contains(got.Cached, "2.1.901") {
		got = claudeEntry(t, fc.next(t, proto.TypeNodeCapabilities))
	}
	if !got.Managed || got.CacheBytes <= hello.CacheBytes {
		t.Fatalf("after the install %+v (hello %+v)", got, hello)
	}
	// Nothing changes: no report until the interval has passed on the agent's clock.
	fc.none(t, proto.TypeNodeCapabilities, 100*time.Millisecond)
	clk.Advance(proto.NodeCapabilitiesInterval)
	if got := claudeEntry(t, fc.next(t, proto.TypeNodeCapabilities)); !slices.Equal(got.Cached, []string{"2.1.901", "2.1.900"}) {
		t.Fatalf("periodic report %+v", got)
	}
}

// SM-HARN-008: capabilities taken again after the connection's hello was built, before central's welcome, are
// re-reported once the welcome arrives (capsAfterHello): exactly one node.capabilities, not lost until the interval.
func TestCapabilitiesChangedBeforeWelcomeReReported(t *testing.T) {
	srv := disttest.New(t)
	for _, v := range []string{"2.1.900", "2.1.901"} {
		srv.Add(v, disttest.StubBinary(v))
	}
	srv.SetPointer(dist.ChannelLatest, "2.1.900")
	dir := t.TempDir()
	m := srv.Manager(t, filepath.Join(dir, "harness", "claude-code"), nil)
	if _, err := m.Ensure(context.Background(), "2.1.900"); err != nil {
		t.Fatal(err)
	}
	a, _ := ccAgent(t, dir, m, func(o *Options) { delete(o.HarnessBinaries, harness.KindClaude) })
	hold := make(chan struct{})
	fc := newHeldWSCentral(t, proto.MinorHarnessVersion, hold)
	runAgent(t, a, fc)
	if h := claudeEntry(t, fc.next(t, proto.TypeHello)); slices.Contains(h.Cached, "2.1.901") {
		t.Fatalf("hello %+v", h)
	}
	// The cache changes while the welcome is held: capsLoop takes the capabilities again but can't report them yet.
	if _, err := m.Ensure(context.Background(), "2.1.901"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the change detected and no detection pending", func() bool {
		a.mu.Lock()
		caps, idle := a.caps, a.capsStarts == a.capsTaken
		a.mu.Unlock()
		return len(m.Changes()) == 0 && idle && slices.Contains(claudeEntry(t, caps).Cached, "2.1.901")
	})
	fc.none(t, proto.TypeNodeCapabilities, 100*time.Millisecond)
	close(hold)
	if got := claudeEntry(t, fc.next(t, proto.TypeNodeCapabilities)); !slices.Equal(got.Cached, []string{"2.1.901", "2.1.900"}) {
		t.Fatalf("report after the welcome %+v", got)
	}
	fc.none(t, proto.TypeNodeCapabilities, 300*time.Millisecond)
}

// SM-HARN-008: a detection that started earlier but finished later never replaces a newer one (a session's hello and
// capsLoop may detect at once).
func TestCapabilitiesDetectionNewestWins(t *testing.T) {
	a, _ := ccAgent(t, t.TempDir(), nil, nil)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	old := detectCapabilities
	detectCapabilities = func(*Agent, context.Context) proto.Capabilities {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return proto.Capabilities{OS: "older"}
		}
		return proto.Capabilities{OS: "newer"}
	}
	t.Cleanup(func() { detectCapabilities = old })
	type take struct {
		c   proto.Capabilities
		gen uint64
	}
	slow := make(chan take, 1)
	go func() { c, gen := a.takeCapabilities(context.Background()); slow <- take{c, gen} }()
	<-started
	if c, gen := a.takeCapabilities(context.Background()); c.OS != "newer" || gen != 1 {
		t.Fatalf("newer detection %q gen %d", c.OS, gen)
	}
	close(release)
	if got := <-slow; got.c.OS != "newer" || got.gen != 1 {
		t.Fatalf("the older detection returned %q gen %d", got.c.OS, got.gen)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.caps.OS != "newer" || a.capsGen != 1 {
		t.Fatalf("cached %q gen %d", a.caps.OS, a.capsGen)
	}
}

// SM-HARN-008: an old central (minor < 11) gets capabilities only in hello; the node never sends it node.capabilities,
// which it would reject.
func TestCapabilitiesNotSentToOldCentral(t *testing.T) {
	srv := disttest.New(t)
	srv.Add("2.1.900", disttest.StubBinary("2.1.900"))
	dir := t.TempDir()
	m := srv.Manager(t, filepath.Join(dir, "harness", "claude-code"), nil)
	a, _ := ccAgent(t, dir, m, nil)
	fc := newWSCentral(t, proto.MinorHarnessVersion-1)
	clk := runAgent(t, a, fc)
	fc.next(t, proto.TypeHello)
	if _, err := m.Ensure(context.Background(), "2.1.900"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(proto.NodeCapabilitiesInterval)
	fc.none(t, proto.TypeNodeCapabilities, 300*time.Millisecond)
	// The cache still follows, so the next hello carries the change.
	waitUntil(t, "capabilities cache refreshed", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		for _, h := range a.caps.Harnesses {
			if h.Kind == harness.KindClaude && slices.Contains(h.Cached, "2.1.900") {
				return true
			}
		}
		return false
	})
}

// SM-HARN-008: an unmanaged node detects its harnesses again on the interval, so a Claude Code updated outside
// SpaceMan is reported.
func TestUnmanagedCapabilitiesRedetected(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin", "claude")
	stub := func(v string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
			t.Fatal(err)
		}
		// Written to a new file and renamed: the old one may be executing (ETXTBSY otherwise).
		tmp := bin + ".new"
		if err := os.WriteFile(tmp, []byte("#!/bin/sh\necho '"+v+" (Claude Code)'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, bin); err != nil {
			t.Fatal(err)
		}
	}
	stub("2.1.5")
	a, _ := ccAgent(t, dir, nil, func(o *Options) { o.HarnessBinaries[harness.KindClaude] = bin })
	fc := newWSCentral(t, proto.MinorHarnessVersion)
	clk := runAgent(t, a, fc)
	if h := claudeEntry(t, fc.next(t, proto.TypeHello)); h.Version != "2.1.5" || h.Path != bin || h.Managed {
		t.Fatalf("hello %+v", h)
	}
	stub("2.1.6")
	fc.none(t, proto.TypeNodeCapabilities, 100*time.Millisecond)
	clk.Advance(proto.NodeCapabilitiesInterval)
	if h := claudeEntry(t, fc.next(t, proto.TypeNodeCapabilities)); h.Version != "2.1.6" || !h.Available || h.Managed {
		t.Fatalf("re-detected %+v", h)
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
