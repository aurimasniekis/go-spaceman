package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	// skip, when set (before the agent connects), is asked about every message: true loses it (not forwarded) while
	// the connection stays.
	skip func(env proto.Envelope) bool
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
			if fc.skip != nil && fc.skip(env) {
				continue
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

// drain discards messages until none has arrived for d. Messages that keep arriving past an overall deadline fail the
// test: the agent is over-reporting, and waiting for quiet would hang until the binary timeout.
func (fc *wsCentral) drain(t *testing.T, d time.Duration) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-fc.msgs:
		case <-time.After(d):
			return
		case <-deadline:
			t.Fatalf("messages still arriving after 2s: the agent is over-reporting")
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
	// A report can legitimately repeat. capsAfterHello re-sends the capabilities once the welcome arrives; it runs late,
	// because the test doesn't wait for the welcome, so it re-sends caps that already include the change, overlapping
	// the capsLoop send with the same content. Let the agent finish before asserting that nothing is reported without a
	// change.
	waitUntil(t, "the change detected and no detection pending", func() bool {
		a.mu.Lock()
		idle := a.capsStarts == a.capsTaken
		a.mu.Unlock()
		return len(m.Changes()) == 0 && idle
	})
	fc.drain(t, 100*time.Millisecond)
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
	var detecting atomic.Int32 // detections in flight
	realDetect := detectCapabilities
	detectCapabilities = func(a *Agent, ctx context.Context) proto.Capabilities {
		detecting.Add(1)
		defer detecting.Add(-1)
		return realDetect(a, ctx)
	}
	t.Cleanup(func() { detectCapabilities = realDetect })
	a, _ := ccAgent(t, dir, nil, func(o *Options) { o.HarnessBinaries[harness.KindClaude] = bin })
	fc := newWSCentral(t, proto.MinorHarnessVersion)
	clk := runAgent(t, a, fc)
	if h := claudeEntry(t, fc.next(t, proto.TypeHello)); h.Version != "2.1.5" || h.Path != bin || h.Managed {
		t.Fatalf("hello %+v", h)
	}
	// Wait for every detection to finish before the binary changes, so that any report still on its way carries what
	// the startup detection saw (2.1.5), never the new binary. capsLoop does not detect before that one has cached
	// (TestCapsLoopWaitsForFirstDetection), so this is normally already true; the wait keeps the test from depending on
	// a loaded runner's scheduling.
	waitUntil(t, "startup detections finished", func() bool { return detecting.Load() == 0 })
	stub("2.1.6")
	// Not yet: nothing may report the new version before the interval has passed (a leftover report of 2.1.5 is fine).
	quiet := time.After(100 * time.Millisecond)
quiet:
	for {
		select {
		case env := <-fc.msgs:
			if env.Type == proto.TypeNodeCapabilities {
				var nc proto.NodeCapabilities
				if err := env.Decode(&nc); err != nil {
					t.Fatal(err)
				}
				if h := claudeEntry(t, nc.Capabilities); h.Version == "2.1.6" {
					t.Fatalf("the new binary was reported before the interval: %+v", h)
				}
			}
		case <-quiet:
			break quiet
		}
	}
	clk.Advance(proto.NodeCapabilitiesInterval)
	if h := claudeEntry(t, fc.next(t, proto.TypeNodeCapabilities)); h.Version != "2.1.6" || !h.Available || h.Managed {
		t.Fatalf("re-detected %+v", h)
	}
}

// SM-HARN-008: capsLoop's first tick must not detect before the session's own startup detection has cached the
// capabilities. A zero capsAt used to count as overdue, so the loop's detection could overlap the session's, finish
// after the welcome and report a stale inventory: an unexpected node.capabilities (the flake this test pins). The
// session's detection is held until capsLoop's detection starts, so that overlap happens on every run when the gate is
// missing, and the welcome is held back until the test has checked nothing was detected early.
func TestCapsLoopWaitsForFirstDetection(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin+".new", []byte("#!/bin/sh\necho '2.1.5 (Claude Code)'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(bin+".new", bin); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	loopStarted, release := make(chan struct{}), make(chan struct{})
	releaseLoop := sync.OnceFunc(func() { close(release) })
	realDetect := detectCapabilities
	detectCapabilities = func(a *Agent, ctx context.Context) proto.Capabilities {
		switch calls.Add(1) {
		case 1:
			// The session's detection: it waits until capsLoop's has started (one second if capsLoop never detects early).
			select {
			case <-loopStarted:
			case <-time.After(time.Second):
			}
		case 2:
			// capsLoop's detection: held until the welcome has arrived, so a report from it would follow the welcome.
			close(loopStarted)
			<-release
		}
		return realDetect(a, ctx)
	}
	t.Cleanup(func() { detectCapabilities = realDetect })
	a, _ := ccAgent(t, dir, nil, func(o *Options) { o.HarnessBinaries[harness.KindClaude] = bin })
	fc := newWSCentral(t, proto.MinorHarnessVersion)
	runAgent(t, a, fc)
	t.Cleanup(releaseLoop)
	fc.next(t, proto.TypeHello)
	waitUntil(t, "welcome", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.connected
	})
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d detections started before the session's own detection had cached (capsLoop detected early)", n-1)
	}
	releaseLoop()
	fc.none(t, proto.TypeNodeCapabilities, 300*time.Millisecond)
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

// SM-HARN-008: capsAfterHello reads the cached generation and sends it later; capsLoop may take and report a newer
// one in between. The older inventory must not land after the newer one (central keeps whichever arrives last).
func TestCapabilitiesOlderGenerationNeverFollowsNewer(t *testing.T) {
	a, _ := ccAgent(t, t.TempDir(), nil, nil)
	var osName atomic.Value
	osName.Store("hello")
	oldDetect := detectCapabilities
	detectCapabilities = func(*Agent, context.Context) proto.Capabilities {
		return proto.Capabilities{OS: osName.Load().(string)}
	}
	reached, release := make(chan struct{}), make(chan struct{})
	capsAfterHelloRead = func(c proto.Capabilities) {
		if c.OS == "older" {
			close(reached)
			<-release
		}
	}
	t.Cleanup(func() { detectCapabilities, capsAfterHelloRead = oldDetect, nil })
	hold := make(chan struct{})
	fc := newHeldWSCentral(t, proto.MinorHarnessVersion, hold)
	runAgent(t, a, fc)
	if h := fc.next(t, proto.TypeHello); h.OS != "hello" {
		t.Fatalf("hello %q", h.OS)
	}
	// The cache is taken again after the hello was built, before the welcome.
	osName.Store("older")
	a.takeCapabilities(context.Background())
	close(hold)
	select { // capsAfterHello has read the older generation and not sent it
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("capsAfterHello never read the older generation")
	}
	osName.Store("newer")
	c, gen := a.takeCapabilities(context.Background())
	a.reportCapabilities(context.Background(), c, gen) // what capsLoop does
	close(release)
	var last string
	deadline := time.After(300 * time.Millisecond)
loop:
	for {
		select {
		case env := <-fc.msgs:
			if env.Type == proto.TypeNodeCapabilities {
				var n proto.NodeCapabilities
				if err := env.Decode(&n); err != nil {
					t.Fatal(err)
				}
				last = n.Capabilities.OS
			}
		case <-deadline:
			break loop
		}
	}
	if last != "newer" {
		t.Fatalf("last report %q, want newer", last)
	}
}

// A report read on session 1 is never written on session 2: a reconnect between the report's snapshot and its write
// (forced with capsBeforeSend) makes the write fail on the old connection, and nothing reaches the new one.
func TestCapabilitiesReportNeverCrossesSessions(t *testing.T) {
	a, _ := ccAgent(t, t.TempDir(), nil, nil)
	fc1, fc2 := newWSCentral(t, proto.MinorHarnessVersion), newWSCentral(t, proto.MinorHarnessVersion)
	dial := func(fc *wsCentral) *websocket.Conn {
		u := "ws" + strings.TrimPrefix(fc.srv.URL, "http")
		c, _, err := websocket.Dial(context.Background(), u, &websocket.DialOptions{Subprotocols: []string{proto.Subprotocol}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.CloseNow() })
		return c
	}
	conn1, conn2 := dial(fc1), dial(fc2)
	a.mu.Lock()
	a.conn, a.connected, a.minor = conn1, true, proto.MinorHarnessVersion
	a.mu.Unlock()
	capsBeforeSend = func() { // the full reconnect: the old session is closed, the new one is up
		_ = conn1.CloseNow()
		a.mu.Lock()
		a.conn = conn2
		a.mu.Unlock()
	}
	t.Cleanup(func() { capsBeforeSend = nil })
	a.reportCapabilities(context.Background(), proto.Capabilities{OS: "session1"}, 5)
	capsBeforeSend = nil
	a.mu.Lock()
	sent := a.capsSent
	a.mu.Unlock()
	if sent != 0 {
		t.Fatalf("capsSent %d after a report that was not delivered", sent)
	}
	// A report taken on session 2 does go out, and is the first thing session 2's central sees.
	a.reportCapabilities(context.Background(), proto.Capabilities{OS: "session2"}, 6)
	if c := fc2.next(t, proto.TypeNodeCapabilities); c.OS != "session2" {
		t.Fatalf("session 2 saw %q first, want session2", c.OS)
	}
	fc2.none(t, proto.TypeNodeCapabilities, 200*time.Millisecond)
	fc1.none(t, proto.TypeNodeCapabilities, 50*time.Millisecond)
}

// A new connection that has not been welcomed yet takes no report, even with the old session's minor still recorded:
// the report would otherwise go out before central has answered the hello (or before the hello itself).
func TestCapabilitiesReportWaitsForWelcome(t *testing.T) {
	a, _ := ccAgent(t, t.TempDir(), nil, nil)
	fc := newWSCentral(t, proto.MinorHarnessVersion)
	u := "ws" + strings.TrimPrefix(fc.srv.URL, "http")
	conn, _, err := websocket.Dial(context.Background(), u, &websocket.DialOptions{Subprotocols: []string{proto.Subprotocol}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	a.mu.Lock()
	a.conn, a.connected, a.minor = conn, false, proto.MinorHarnessVersion // session 2 dialled, session 1's minor kept
	a.mu.Unlock()
	a.reportCapabilities(context.Background(), proto.Capabilities{OS: "early"}, 5)
	a.mu.Lock()
	sent := a.capsSent
	a.mu.Unlock()
	if sent != 0 {
		t.Fatalf("capsSent %d after a report on an unwelcomed connection", sent)
	}
	fc.none(t, proto.TypeNodeCapabilities, 200*time.Millisecond)
}
