// Package apptest is an in-process SpaceMan installation for suites outside internal/app (the fault suite in
// test/fault): central on a real HTTP server, restartable on the same data directory, and one node agent with the
// in-memory runner (internal/runner/runnertest). Everything it shows is simulated evidence; real herdr-core behaviour
// is covered by the `real` suites.
package apptest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/accounts"
	"github.com/aurimasniekis/go-spaceman/internal/agent"
	"github.com/aurimasniekis/go-spaceman/internal/app"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/runner/runnertest"
)

// Admin is the actor of setup calls made directly on the services.
var Admin = audit.Actor{Type: audit.ActorUser, ID: "usr_test"}

// Env is one installation: central, its HTTP server, and a node agent.
type Env struct {
	T   testing.TB
	Cfg *config.Config
	// Srv serves central (through the handler swapped by Unreachable and Reachable).
	Srv *httptest.Server
	C   *app.Central
	// Runner is the node's in-memory runner. It outlives agent restarts, as herdr-core outlives the agent.
	Runner   *runnertest.Fake
	AgentDir string
	Cred     string
	NodeID   string
	// Proxy sits between the agent and central when the env was built WithProxy (network partitions).
	Proxy *Proxy
	// AgentOptions adjusts the agent's options before every start (fault injection).
	AgentOptions func(*agent.Options)
	// Central options for every (re)start.
	CentralOptions app.Options

	handler   atomic.Pointer[http.Handler]
	bridge    string
	cancel    context.CancelFunc
	journal   *agent.Journal
	done      chan struct{}
	withProxy bool
}

// Option configures New.
type Option func(*Env)

// WithConfig adjusts the central configuration before central first opens.
func WithConfig(f func(*config.Config)) Option { return func(e *Env) { f(e.Cfg) } }

// ShortLease makes leases expire quickly (1 s plus a 0.5 s grace) for tests of what happens without central.
func ShortLease() Option {
	return WithConfig(func(cfg *config.Config) {
		cfg.Nodes.LeaseTTL = config.Duration(time.Second)
		cfg.Nodes.DisconnectGrace = config.Duration(500 * time.Millisecond)
	})
}

// WithProxy routes the agent's connection through a TCP proxy that can cut or black-hole it.
func WithProxy() Option { return func(e *Env) { e.withProxy = true } }

// New starts central in a fresh data directory. Leases last 5 s with a 1 s grace by default (longer than a central
// restart under the race detector).
func New(t testing.TB, opts ...Option) *Env {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Mode = config.ModeDevelopment
	cfg.DataDir = filepath.Join(dir, "central")
	cfg.KEK.File = filepath.Join(dir, "kek")
	cfg.Nodes.HeartbeatInterval = config.Duration(300 * time.Millisecond)
	cfg.Nodes.LeaseTTL = config.Duration(5 * time.Second)
	cfg.Nodes.DisconnectGrace = config.Duration(time.Second)
	k, err := keyring.GenerateKEK()
	if err != nil {
		t.Fatal(err)
	}
	if err := keyring.WriteKEKFile(cfg.KEK.File, k); err != nil {
		t.Fatal(err)
	}
	// The MCP bridge socket path must stay short (unix socket limit).
	sock, err := os.MkdirTemp("/tmp", "smf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sock) })
	e := &Env{T: t, Cfg: cfg, Runner: runnertest.New(), AgentDir: filepath.Join(dir, "agent"), bridge: filepath.Join(sock, "b.sock"),
		CentralOptions: app.Options{Environ: []string{}}}
	for _, o := range opts {
		o(e)
	}
	e.Srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { (*e.handler.Load()).ServeHTTP(w, r) }))
	cfg.PublicURL = "http://" + e.Srv.Listener.Addr().String()
	e.StartCentral()
	e.Srv.Start()
	if e.withProxy {
		e.Proxy = NewProxy(t, e.Srv.Listener.Addr().String())
	}
	t.Cleanup(func() {
		e.StopAgent()
		if e.Proxy != nil {
			e.Proxy.Close()
		}
		e.Srv.Close()
		if e.C != nil {
			_ = e.C.Close()
		}
	})
	return e
}

// Log is the logger of central and the agent (SM_TEST_LOG=1 prints it).
func Log() *slog.Logger {
	if os.Getenv("SM_TEST_LOG") == "" {
		return slog.New(slog.DiscardHandler)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// StartCentral opens central on the data directory and serves it.
func (e *Env) StartCentral() {
	e.T.Helper()
	c, err := app.OpenCentral(context.Background(), e.Cfg, Log(), e.CentralOptions)
	if err != nil {
		e.T.Fatal(err)
	}
	e.C = c
	h := c.Handler(c.APIDeps())
	e.handler.Store(&h)
}

// RestartCentral closes central (dropping every node connection) and opens it again on the same data directory.
func (e *Env) RestartCentral() {
	e.T.Helper()
	e.Srv.CloseClientConnections()
	_ = e.C.Close()
	e.StartCentral()
}

// Unreachable makes central refuse every request and drops the node connections, as if the network or the server
// were gone (central's state is untouched).
func (e *Env) Unreachable() {
	h := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	e.handler.Store(&h)
	e.C.Fleet.Hub().CloseAll("test: central unreachable")
}

// Reachable serves central again after Unreachable.
func (e *Env) Reachable() {
	h := e.C.Handler(e.C.APIDeps())
	e.handler.Store(&h)
}

// Enroll creates a node and redeems its enrollment token over HTTP.
func (e *Env) Enroll() {
	e.T.Helper()
	en, err := e.C.Fleet.CreateNode(context.Background(), Admin, fleet.CreateNodeInput{Name: "node-a"})
	if err != nil {
		e.T.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"token": en.Token})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, e.Srv.URL+fleet.EnrollPath, bytes.NewReader(body))
	if err != nil {
		e.T.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.T.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		e.T.Fatalf("enroll: %d %s", res.StatusCode, b)
	}
	var er fleet.EnrollResult
	if err := json.Unmarshal(b, &er); err != nil {
		e.T.Fatal(err)
	}
	e.Cred = er.Credential
	nodes, err := e.C.Fleet.ListNodes(context.Background())
	if err != nil || len(nodes) != 1 {
		e.T.Fatalf("nodes %v %v", nodes, err)
	}
	e.NodeID = nodes[0].ID
}

// AgentURL is the central URL the agent dials (the proxy's when there is one).
func (e *Env) AgentURL() string {
	if e.Proxy != nil {
		return e.Proxy.URL()
	}
	return e.Srv.URL
}

// StartAgent starts the node agent on its data directory (reconciling with the runner first).
func (e *Env) StartAgent() {
	e.T.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	j, err := agent.OpenJournal(ctx, filepath.Join(e.AgentDir, "journal"), Log(), nil)
	if err != nil {
		cancel()
		e.T.Fatal(err)
	}
	o := agent.Options{DataDir: e.AgentDir, CentralURL: e.AgentURL(), Credential: e.Cred, Runner: e.Runner,
		RunnerInfo: proto.RunnerInfo{Kind: "fake", CoreID: "fakecore"}, HarnessBinaries: map[string]string{hns.KindClaude: "/usr/bin/true", hns.KindCodex: "/usr/bin/true"},
		AgentBinary: "/usr/bin/true", BridgeSocket: e.bridge, ReconnectMax: 200 * time.Millisecond, Capacity: 4, Log: Log()}
	if e.AgentOptions != nil {
		e.AgentOptions(&o)
	}
	a := agent.New(j, o)
	e.cancel, e.journal, e.done = cancel, j, make(chan struct{})
	done := e.done
	go func() { _ = a.Run(ctx); close(done) }()
}

// StopAgent stops the agent and waits for it (its bounded shutdown included).
func (e *Env) StopAgent() {
	if e.cancel != nil {
		e.cancel()
		<-e.done
		_ = e.journal.Close()
		e.cancel = nil
	}
}

// KillAgent ends the agent the way a crash would, as far as an in-process agent can: the journal's database is
// closed under it at once (no checkpoint of in-flight work, later journal writes fail), its connection drops, and
// the runner's programs keep running. The goroutines' remaining work runs against a dead journal, like the
// unrecorded tail of a killed process.
func (e *Env) KillAgent() {
	if e.cancel == nil {
		return
	}
	_ = e.journal.Close()
	e.cancel()
	select {
	case <-e.done:
	case <-time.After(10 * time.Second):
		e.T.Fatal("killed agent did not stop")
	}
	e.cancel = nil
}

// AgentRunning reports whether an agent is started.
func (e *Env) AgentRunning() bool { return e.cancel != nil }

// Space creates a space pinned to the node with stub Claude and Codex accounts bound, and waits for its lease.
func (e *Env) Space(slug string, in fleet.CreateSpaceInput) *fleet.Space {
	e.T.Helper()
	ctx := context.Background()
	in.Slug, in.Name, in.NodeID = slug, slug, e.NodeID
	sp, err := e.C.Fleet.CreateSpace(ctx, Admin, in)
	if err != nil {
		e.T.Fatal(err)
	}
	var bs []accounts.Binding
	for _, a := range []accounts.Input{
		{Name: "stub-claude-" + slug, Kind: "claude_oauth_token", Secret: "stub-claude-token", Enabled: true},
		{Name: "stub-codex-" + slug, Kind: "openai_api_key", Secret: "stub-openai-key", Enabled: true}, //nolint:gosec // a fixed stub, not a credential
	} {
		acc, err := e.C.Accounts.Create(ctx, Admin, a)
		if err != nil {
			e.T.Fatal(err)
		}
		bs = append(bs, accounts.Binding{AccountID: acc.ID, Default: true})
	}
	if _, err := e.C.Accounts.SetBindings(ctx, Admin, sp.ID, bs); err != nil {
		e.T.Fatal(err)
	}
	Eventually(e.T, "lease granted", func() bool { s, _ := e.C.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	return sp
}

// Connected waits until the node is connected.
func (e *Env) Connected() {
	e.T.Helper()
	Eventually(e.T, "node connected", func() bool { n, _ := e.C.Fleet.GetNode(context.Background(), e.NodeID); return n != nil && n.Connected })
}

// Launch launches a worker run in the space and waits until it is observed running.
func (e *Env) Launch(spaceID, role, harness string) *fleet.Run {
	e.T.Helper()
	r, err := e.C.Fleet.LaunchRun(context.Background(), Admin, spaceID, fleet.LaunchInput{Kind: "worker", Role: role, Harness: harness, Prompt: "work"})
	if err != nil {
		e.T.Fatal(err)
	}
	Eventually(e.T, "run running", func() bool { return e.Run(r.ID).Observed == "running" })
	return r
}

// Run reads a run from central.
func (e *Env) Run(id string) *fleet.Run {
	e.T.Helper()
	r, err := e.C.Fleet.GetRun(context.Background(), id)
	if err != nil {
		e.T.Fatal(err)
	}
	return r
}

// Terminal is the runner process of a live run (fails unless exactly one is live).
func (e *Env) Terminal(runID string) string {
	e.T.Helper()
	ps := e.Runner.LiveByLabel("spaceman.run", runID)
	if len(ps) != 1 {
		e.T.Fatalf("live processes of %s: %d, want 1", runID, len(ps))
	}
	return ps[0].ID
}

// RootToken issues a root token for API calls.
func (e *Env) RootToken() string {
	e.T.Helper()
	secret, _, err := e.C.Auth.IssueRootToken(context.Background())
	if err != nil {
		e.T.Fatal(err)
	}
	return secret
}

// Eventually polls cond for up to 20 s.
func Eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
