// Package agent is the node daemon `spaceman-agent` (SM-ARCH-002, SM-NODE-*): an authenticated outbound connection to
// central, a durable local execution journal, run supervision through the runner (herdr-core), harness
// materialization, and enforcement of execution authority.
//
// The agent is not a controller: it starts nothing on its own. It executes central's commands, reports what it
// observes, and — when central is gone — stops starting work at once and stops running work once the lease plus the
// disconnect grace has passed. A per-space lease-guard terminal enforces the same bound if the daemon itself dies
// (ADR-0017).
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/aurimasniekis/go-spaceman/internal/buildinfo"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/runner"
)

// Options configure the agent.
type Options struct {
	DataDir    string
	CentralURL string
	Credential string
	HTTPClient *http.Client
	// Runner supervises programs (herdr-core in production).
	Runner runner.Runner
	// RunnerInfo describes the runner for the capability inventory.
	RunnerInfo proto.RunnerInfo
	// Harnesses maps harness kind to its binary path.
	HarnessBinaries map[string]string
	// AgentBinary is the spaceman-agent executable used for the launcher shim, lease guard and MCP bridge.
	AgentBinary string
	// BridgeSocket is the local socket the MCP bridge connects to.
	BridgeSocket string
	Capacity     int
	ReconnectMax time.Duration
	Clock        clock.Clock
	Log          *slog.Logger
	// Wrap overrides how a harness launch becomes a runner argv (tests); nil uses the launcher shim.
	Wrap func(runDir string, l harness.Launch) []string
}

// Agent is a running node agent.
type Agent struct {
	o   Options
	j   *Journal
	log *slog.Logger

	mu        sync.Mutex
	conn      *websocket.Conn
	connected bool
	leases    map[string]leaseRow
	heartbeat time.Duration
	// recorders are the live recordings by run ID.
	recorders map[string]context.CancelFunc
	// lostAt is when central was last lost (zero while connected).
	lostAt time.Time
	wake   chan struct{}
	// runTokens are the hashes of active runs' bridge tokens.
	runTokens map[string][32]byte
}

// New prepares an agent; Run starts it.
func New(j *Journal, o Options) *Agent {
	if o.Clock == nil {
		o.Clock = clock.System{}
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.ReconnectMax <= 0 {
		o.ReconnectMax = 30 * time.Second
	}
	if o.HTTPClient == nil {
		o.HTTPClient = http.DefaultClient
	}
	return &Agent{o: o, j: j, log: obs.Component(o.Log, "agent"), leases: map[string]leaseRow{}, heartbeat: 10 * time.Second,
		recorders: map[string]context.CancelFunc{}, wake: make(chan struct{}, 1)}
}

func (a *Agent) now() time.Time { return a.o.Clock.Now().UTC() }

// Paths.
func (a *Agent) spaceDir(workdir string) string { return filepath.Join(a.o.DataDir, "spaces", workdir) }
func (a *Agent) runDir(runID string) string     { return filepath.Join(a.o.DataDir, "runs", runID) }
func (a *Agent) leaseFile(spaceID string) string {
	return filepath.Join(a.o.DataDir, "leases", spaceID+".json")
}

// GuardName and RunName are the herdr-core terminal names (unique among live terminals; the launch idempotency key).
func GuardName(spaceID string) string { return "g-" + strings.TrimPrefix(spaceID, "spc_") }
func RunName(runID string) string     { return "r-" + strings.TrimPrefix(runID, "run_") }

// Run connects to central and serves until ctx ends. It first reconciles the journal with the runner, so a restarted
// agent adopts its programs instead of starting duplicates (SM-NODE-009).
func (a *Agent) Run(ctx context.Context) error {
	for _, d := range []string{"spaces", "runs", "leases"} {
		if err := os.MkdirAll(filepath.Join(a.o.DataDir, d), 0o700); err != nil {
			return err
		}
	}
	if err := a.loadLeases(ctx); err != nil {
		return err
	}
	if err := a.reconcileRunner(ctx); err != nil {
		a.log.Warn("initial runner reconcile", obs.Err(err))
	}
	go a.monitor(ctx)
	go a.enforce(ctx)
	go func() {
		if err := a.ServeBridge(ctx); err != nil {
			a.log.Warn("mcp bridge socket", obs.Err(err))
		}
	}()
	backoff := time.Second
	for {
		start := a.now()
		err := a.session(ctx)
		if ctx.Err() != nil {
			return nil //nolint:nilerr // shutdown: the session error is the cancellation itself
		}
		a.log.Warn("central connection lost", obs.Err(err))
		if a.now().Sub(start) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, a.o.ReconnectMax)
	}
}

func (a *Agent) loadLeases(ctx context.Context) error {
	ls, err := a.j.leases(ctx)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, l := range ls {
		if old, ok := a.leases[l.SpaceID]; ok {
			l.DisconnectPolicy, l.Grace = old.DisconnectPolicy, old.Grace
		}
		if l.DisconnectPolicy == "" {
			l.DisconnectPolicy = "interrupt"
		}
		a.leases[l.SpaceID] = l
	}
	return nil
}

func wsURL(central string) (string, error) {
	u, err := url.Parse(central)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("central url must be http(s), got %q", u.Scheme)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + proto.ConnectPath
	return u.String(), nil
}

// capabilities is the node inventory sent in hello (SM-NODE-002).
func (a *Agent) capabilities(ctx context.Context) proto.Capabilities {
	c := proto.Capabilities{OS: runtime.GOOS, Arch: runtime.GOARCH, Runner: a.o.RunnerInfo, Features: []string{"lease-guard", "launch-shim", "recording.zstd"}}
	for _, kind := range []string{harness.KindClaude, harness.KindCodex} {
		h, _ := harness.Get(kind)
		bin := a.o.HarnessBinaries[kind]
		if bin == "" {
			c.Harnesses = append(c.Harnesses, proto.HarnessInfo{Kind: kind, Error: "not configured", Capabilities: []string{}})
			continue
		}
		c.Harnesses = append(c.Harnesses, h.Detect(ctx, bin))
	}
	return c
}

// session runs one connection.
func (a *Agent) session(ctx context.Context) error {
	u, err := wsURL(a.o.CentralURL)
	if err != nil {
		return err
	}
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	conn, resp, err := websocket.Dial(dctx, u, &websocket.DialOptions{HTTPClient: a.o.HTTPClient, Subprotocols: []string{proto.Subprotocol},
		HTTPHeader: http.Header{"Authorization": {"Bearer " + a.o.Credential}}})
	cancel()
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return err
	}
	conn.SetReadLimit(proto.MaxMessage)
	defer func() { _ = conn.CloseNow() }()
	runs, err := a.j.runs(ctx, false)
	if err != nil {
		return err
	}
	hello := proto.Hello{AgentVersion: buildinfo.Get().Version, MinMinor: proto.MinMinor, MaxMinor: proto.MaxMinor, Capabilities: a.capabilities(ctx),
		Capacity: a.o.Capacity, Runs: []proto.RunReport{}, Leases: a.leaseStates()}
	for _, r := range runs {
		hello.Runs = append(hello.Runs, r.report())
	}
	a.mu.Lock()
	a.conn = conn
	a.mu.Unlock()
	if err := a.send(ctx, proto.TypeHello, hello); err != nil {
		return err
	}
	defer func() {
		a.mu.Lock()
		a.conn, a.connected, a.lostAt = nil, false, a.now()
		a.mu.Unlock()
		a.poke()
	}()
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			return errors.New("unexpected binary message")
		}
		env, err := proto.Decode(data)
		if err != nil {
			return err
		}
		switch env.Type {
		case proto.TypeWelcome:
			var w proto.Welcome
			if err := env.Decode(&w); err != nil {
				return err
			}
			a.mu.Lock()
			a.connected, a.lostAt = true, time.Time{}
			if w.HeartbeatInterval > 0 {
				a.heartbeat = w.HeartbeatInterval
			}
			a.mu.Unlock()
			go a.heartbeatLoop(hbCtx)
			a.log.Info("connected to central", slog.String(obs.FieldNodeID, w.NodeID), slog.Int("protocol_minor", w.Minor))
		case proto.TypeLeaseGrant:
			var g proto.LeaseGrant
			if err := env.Decode(&g); err != nil {
				return err
			}
			a.grant(ctx, g)
		case proto.TypeLeaseRevoke:
			var r proto.LeaseRevoke
			if err := env.Decode(&r); err != nil {
				return err
			}
			a.revoke(ctx, r)
		case proto.TypeCommand:
			var c proto.Command
			if err := env.Decode(&c); err != nil {
				return err
			}
			a.command(ctx, c)
		case proto.TypeReconcile:
			var rec proto.Reconcile
			if err := env.Decode(&rec); err != nil {
				return err
			}
			a.reconcileDesired(ctx, rec)
		case proto.TypeError:
			var e proto.Error
			_ = env.Decode(&e)
			return fmt.Errorf("central refused the connection: %s: %s", e.Code, e.Message)
		}
	}
}

func (a *Agent) send(ctx context.Context, typ string, body any) error {
	b, err := proto.Encode(ids.New(ids.Message), typ, a.now(), body)
	if err != nil {
		return err
	}
	a.mu.Lock()
	conn := a.conn
	a.mu.Unlock()
	if conn == nil {
		return errors.New("not connected")
	}
	wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b)
}

func (a *Agent) leaseStates() []proto.LeaseState {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []proto.LeaseState{}
	for _, l := range a.leases {
		out = append(out, proto.LeaseState{SpaceID: l.SpaceID, Generation: l.Generation, ExpiresAt: l.ExpiresAt})
	}
	return out
}

func (a *Agent) heartbeatLoop(ctx context.Context) {
	a.mu.Lock()
	every := a.heartbeat
	a.mu.Unlock()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		running := 0
		if rs, err := a.j.runs(ctx, true); err == nil {
			running = len(rs)
		}
		if err := a.send(ctx, proto.TypeHeartbeat, proto.Heartbeat{Leases: a.leaseStates(), Running: running}); err != nil {
			return
		}
	}
}

// report sends a run's journaled state when connected (state-based: the next hello re-sends everything, so a report
// lost to a disconnect is not lost information).
func (a *Agent) report(ctx context.Context, r runRow) {
	if err := a.send(ctx, proto.TypeRunReport, r.report()); err != nil {
		a.log.Debug("run report deferred until reconnect", slog.String(obs.FieldRunID, r.RunID))
	}
}

func (a *Agent) ack(ctx context.Context, c proto.Command, result, reason string) {
	_ = a.send(ctx, proto.TypeAck, proto.Ack{CommandID: c.CommandID, Result: result, Reason: reason})
}

func (a *Agent) poke() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// lease returns a space's lease and whether it currently authorizes execution of generation gen.
func (a *Agent) lease(spaceID string, gen int64) (leaseRow, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	l, ok := a.leases[spaceID]
	return l, ok && l.Generation == gen && a.now().Before(l.ExpiresAt.Add(l.Grace))
}

func writeJSONFile(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
