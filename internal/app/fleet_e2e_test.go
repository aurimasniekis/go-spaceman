package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// fleetEnv is a central on a real HTTP server (restartable on the same data directory) plus a node agent with an
// in-memory runner. This is simulated evidence for SM-NODE-*; real herdr-core behavior is covered by the `real`
// suite in internal/runner/hc.
type fleetEnv struct {
	t        *testing.T
	cfg      *config.Config
	srv      *httptest.Server
	handler  atomic.Pointer[http.Handler]
	c        *app.Central
	run      *runnertest.Fake
	agentFn  context.CancelFunc
	agentJ   *agent.Journal
	agentDir string
	cred     string
	done     chan struct{}
}

func newFleetEnv(t *testing.T) *fleetEnv {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Mode = config.ModeDevelopment
	cfg.DataDir = filepath.Join(dir, "central")
	cfg.KEK.File = filepath.Join(dir, "kek")
	cfg.Nodes.HeartbeatInterval = config.Duration(300 * time.Millisecond)
	cfg.Nodes.LeaseTTL = config.Duration(time.Second)
	cfg.Nodes.DisconnectGrace = config.Duration(500 * time.Millisecond)
	k, _ := keyring.GenerateKEK()
	if err := keyring.WriteKEKFile(cfg.KEK.File, k); err != nil {
		t.Fatal(err)
	}
	e := &fleetEnv{t: t, cfg: cfg, run: runnertest.New(), agentDir: filepath.Join(dir, "agent")}
	e.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { (*e.handler.Load()).ServeHTTP(w, r) }))
	cfg.PublicURL = "http://" + e.srv.Listener.Addr().String()
	e.startCentral()
	e.srv.Start()
	t.Cleanup(func() {
		e.stopAgent()
		e.srv.Close()
		if e.c != nil {
			_ = e.c.Close()
		}
	})
	return e
}

func (e *fleetEnv) startCentral() {
	c, err := app.OpenCentral(context.Background(), e.cfg, testLog(), app.Options{Environ: []string{}})
	if err != nil {
		e.t.Fatal(err)
	}
	e.c = c
	h := c.Handler(c.APIDeps())
	e.handler.Store(&h)
}

// restartCentral closes central (dropping every node connection) and opens it again on the same data directory.
func (e *fleetEnv) restartCentral() {
	e.srv.CloseClientConnections()
	_ = e.c.Close()
	e.startCentral()
}

var admin = audit.Actor{Type: audit.ActorUser, ID: "usr_test"}

func (e *fleetEnv) enroll() {
	e.t.Helper()
	en, err := e.c.Fleet.CreateNode(context.Background(), admin, fleet.CreateNodeInput{Name: "node-a"})
	if err != nil {
		e.t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"token": en.Token})
	res, err := http.Post(e.srv.URL+fleet.EnrollPath, "application/json", bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		e.t.Fatalf("enroll: %d %s", res.StatusCode, b)
	}
	var er fleet.EnrollResult
	_ = json.Unmarshal(b, &er)
	e.cred = er.Credential
	// Single use.
	res2, _ := http.Post(e.srv.URL+fleet.EnrollPath, "application/json", bytes.NewReader(body))
	if res2.StatusCode != http.StatusUnauthorized {
		e.t.Fatalf("enrollment token reused: %d", res2.StatusCode)
	}
	res2.Body.Close()
}

func (e *fleetEnv) startAgent() {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	j, err := agent.OpenJournal(ctx, filepath.Join(e.agentDir, "journal"), discard(), nil)
	if err != nil {
		e.t.Fatal(err)
	}
	a := agent.New(j, agent.Options{DataDir: e.agentDir, CentralURL: e.srv.URL, Credential: e.cred, Runner: e.run,
		RunnerInfo: proto.RunnerInfo{Kind: "fake", CoreID: "fakecore"}, HarnessBinaries: map[string]string{hns.KindClaude: "/usr/bin/true", hns.KindCodex: "/usr/bin/true"},
		AgentBinary: "/usr/bin/true", ReconnectMax: 200 * time.Millisecond, Capacity: 4, Log: testLog()})
	e.agentFn, e.agentJ, e.done = cancel, j, make(chan struct{})
	go func() { _ = a.Run(ctx); close(e.done) }()
}

func (e *fleetEnv) stopAgent() {
	if e.agentFn != nil {
		e.agentFn()
		<-e.done
		_ = e.agentJ.Close()
		e.agentFn = nil
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (e *fleetEnv) run1(id string) *fleet.Run {
	r, err := e.c.Fleet.GetRun(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// SM-NODE-001…009, SM-SPACE-002/003, SM-INV-002/006: enrollment, connection, leases, launch, duplicate delivery,
// agent and central restarts without duplicate execution, stop, control uniqueness and fencing.
func TestFleetLifecycle(t *testing.T) {
	e := newFleetEnv(t)
	ctx := context.Background()
	e.enroll()
	e.startAgent()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	nodeID := nodes[0].ID
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodeID); return n.Connected && n.State == "active" })
	n, _ := e.c.Fleet.GetNode(ctx, nodeID)
	if n.Capabilities.Runner.Kind != "fake" || len(n.Capabilities.Harnesses) != 2 {
		t.Fatalf("capabilities %+v", n.Capabilities)
	}

	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "demo", Name: "Demo", NodeID: nodeID})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })

	ctrl, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "control", Role: "control", Harness: hns.KindClaude, Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "control running", func() bool { return e.run1(ctrl.ID).Observed == "running" })
	if e.run.Starts != 2 { // the lease guard + the run
		t.Fatalf("starts = %d, want 2 (guard + run)", e.run.Starts)
	}
	procs := e.run.LiveByLabel("spaceman.run", ctrl.ID)
	if len(procs) != 1 || procs[0].Parent == "" || !strings.Contains(strings.Join(procs[0].Argv, " "), "--session-id") {
		t.Fatalf("run process %+v", procs)
	}
	// SM-INV-006: at most one authoritative control run.
	if _, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "control", Role: "control", Harness: hns.KindCodex}); err == nil {
		t.Fatal("second control run accepted")
	}
	worker, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor-backend", Harness: hns.KindCodex, Prompt: "build"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "worker running", func() bool { return e.run1(worker.ID).Observed == "running" })

	// Agent restart: the journal and the runner are reconciled; nothing is started again (SM-NODE-009).
	e.stopAgent()
	eventually(t, "runs unknown after disconnect", func() bool { return e.run1(ctrl.ID).Observed == "unknown" })
	starts := e.run.Starts
	e.startAgent()
	eventually(t, "runs running again", func() bool { return e.run1(ctrl.ID).Observed == "running" && e.run1(worker.ID).Observed == "running" })
	// Central restart: pending/sent commands are re-delivered and acknowledged as duplicates.
	e.restartCentral()
	eventually(t, "reconnected after central restart", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodeID); return n.Connected })
	eventually(t, "runs running after central restart", func() bool { return e.run1(worker.ID).Observed == "running" })
	time.Sleep(500 * time.Millisecond)
	if e.run.Starts != starts {
		t.Fatalf("restart duplicated execution: starts %d → %d", starts, e.run.Starts)
	}
	if len(e.run.LiveByLabel("spaceman.run", ctrl.ID)) != 1 {
		t.Fatal("duplicate control process")
	}

	// Stop: interrupt then observe the exit.
	if _, err := e.c.Fleet.StopRun(ctx, admin, worker.ID, "interrupt", "test"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "worker exited", func() bool { r := e.run1(worker.ID); return r.Observed == "exited" && r.Desired == "stopped" })

	// Pause with interrupt stops active work and blocks launches; resume admits work again.
	s, _ := e.c.Fleet.GetSpace(ctx, sp.ID)
	s, err = e.c.Fleet.SetSpaceState(ctx, admin, sp.ID, s.Version, "paused", fleet.PauseInterrupt)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "control stopped by pause", func() bool { return e.run1(ctrl.ID).Observed == "exited" })
	if _, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "planner", Harness: hns.KindClaude}); err == nil {
		t.Fatal("launch accepted while paused")
	}
	if _, err := e.c.Fleet.SetSpaceState(ctx, admin, sp.ID, s.Version, "active", ""); err != nil {
		t.Fatal(err)
	}
}

// SM-NODE-007: without central, the node starts no work and stops running work after lease + grace.
func TestDisconnectEnforcement(t *testing.T) {
	e := newFleetEnv(t)
	ctx := context.Background()
	e.enroll()
	e.startAgent()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "dc-test", Name: "DC", NodeID: nodes[0].ID, DisconnectPolicy: "kill"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "lease", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor-backend", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "running", func() bool { return e.run1(r.ID).Observed == "running" })
	// Central goes away (server stops accepting): connections drop and reconnects fail.
	e.handler.Store(ptr(http.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusServiceUnavailable) }))))
	e.c.Fleet.Hub().CloseAll("test: central unreachable")
	eventually(t, "run killed after lease+grace without central", func() bool { return len(e.run.LiveByLabel("spaceman.run", r.ID)) == 0 })
	// Launching is refused centrally too: the lease is no longer valid.
	time.Sleep(time.Second)
	if _, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "planner", Harness: hns.KindClaude}); err == nil {
		t.Fatal("launch accepted for a node without a valid lease")
	}
}

func ptr[T any](v T) *T { return &v }
