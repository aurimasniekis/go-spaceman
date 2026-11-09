package app_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
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
	// bridge is the agent's MCP bridge socket (short path: unix socket limit).
	bridge string
	// staleGrace overrides the stale-run sweeper's grace on the next startCentral (0: the default, ADR-0074).
	staleGrace time.Duration
}

// newFleetEnv starts central for an end-to-end test. Leases last 5 s with a 1 s grace by default, so that restarting
// the agent under the race detector never outlives a lease; tests whose runs must survive restartCentral pass
// longLease, and tests of lease expiry pass shortLease.
func newFleetEnv(t *testing.T, opts ...func(*config.Config)) *fleetEnv {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Mode = config.ModeDevelopment
	cfg.DataDir = filepath.Join(dir, "central")
	cfg.KEK.File = filepath.Join(dir, "kek")
	cfg.Nodes.HeartbeatInterval = config.Duration(300 * time.Millisecond)
	cfg.Nodes.LeaseTTL = config.Duration(5 * time.Second)
	cfg.Nodes.DisconnectGrace = config.Duration(time.Second)
	// Control is not started or stopped on its own unless a test asks (TestControlAutostart): most tests count runs.
	cfg.Limits.ControlAutostart, cfg.Limits.ControlIdleStopMinutes = 0, 0
	for _, o := range opts {
		o(cfg)
	}
	k, _ := keyring.GenerateKEK()
	if err := keyring.WriteKEKFile(cfg.KEK.File, k); err != nil {
		t.Fatal(err)
	}
	sock, err := os.MkdirTemp("/tmp", "smb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sock) })
	e := &fleetEnv{t: t, cfg: cfg, run: runnertest.New(), agentDir: filepath.Join(dir, "agent"), bridge: filepath.Join(sock, "b.sock")}
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

// longLease makes leases last 30 s, for tests whose runs must survive restartCentral. With StoreReaders: 4 a restart
// takes about 1 s even under the race detector, so the default 5 s lease is normally enough; the long lease is a margin
// for a loaded host, where the node's reconnects are refused until the store is open and a short lease could expire
// and the node stop the space's runs.
func longLease(cfg *config.Config) { cfg.Nodes.LeaseTTL = config.Duration(30 * time.Second) }

// shortLease makes leases expire quickly (1 s plus a 0.5 s grace) for tests of what happens without central.
func shortLease(cfg *config.Config) {
	cfg.Nodes.LeaseTTL = config.Duration(time.Second)
	cfg.Nodes.DisconnectGrace = config.Duration(500 * time.Millisecond)
}

func (e *fleetEnv) startCentral() {
	storetest.Seed(e.t, e.cfg.DataDir)
	// A small reader pool keeps a restart well inside the lease: store.Open checks every reader connection, and with the
	// default (one per CPU) that alone took 5-7 s on a 32-CPU host under -race, so leases lapsed during restartCentral.
	c, err := app.OpenCentral(context.Background(), e.cfg, testLog(), app.Options{Environ: []string{}, StoreReaders: 4, StaleRunGrace: e.staleGrace})
	if err != nil {
		e.t.Fatal(err)
	}
	// The reader pool must stay small (see above); asserted after every open, so the regression shows as a count, not as
	// a restart that a loaded host happened to make slow.
	if n := c.Store.ReadDB().Stats().MaxOpenConnections; n != 4 {
		e.t.Fatalf("central's store has %d reader connections, want 4: check app.Options.StoreReaders", n)
	}
	e.c = c
	h := c.Handler(c.APIDeps())
	e.handler.Store(&h)
}

// restartCentral closes central (dropping every node connection) and opens it again on the same data directory. Close
// returns only once the store's connections are closed, so a Close error (a connection still in use after the drain
// timeout) fails the test here rather than as SQLITE_BUSY in the next open.
func (e *fleetEnv) restartCentral() {
	e.t.Helper()
	e.srv.CloseClientConnections()
	if err := e.c.Close(); err != nil {
		e.t.Fatalf("close central: %v", err)
	}
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
		AgentBinary: "/usr/bin/true", BridgeSocket: e.bridge, ReconnectMax: 200 * time.Millisecond, Capacity: 4, Log: testLog()})
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

// eventually waits for cond. The deadline only bounds how long a failing test waits: most tests here run in parallel
// (t.Parallel), so under the race detector on a small runner a condition can take several seconds to hold.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
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
	// Not parallel: central restarts must finish within the node lease, which parallel tests can starve.
	e := newFleetEnv(t, longLease)
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
	accts := bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	// SM-UI-006: the grant is a change of the space, so open pages refetch it (renewals are not).
	changes, err := e.c.Store.R().ChangesAfter(ctx, db.ChangesAfterParams{After: 0, Lim: 1000})
	if err != nil {
		t.Fatal(err)
	}
	spaceChanges := 0
	for _, c := range changes {
		if c.ResourceType == "space" && c.ResourceID == sp.ID {
			spaceChanges++
		}
	}
	if spaceChanges < 2 { // created + lease granted
		t.Fatalf("space changes = %d, want at least 2 (created, lease granted)", spaceChanges)
	}

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
	// SM-ACCT-004: the account's credential reaches the run only through the shim's file (the fake runner does not run
	// the shim, so the file is still there), never through the runner's environment; the run records the account.
	sec, err := os.ReadFile(filepath.Join(e.agentDir, "runs", ctrl.ID, agent.SecretsFile))
	if err != nil || !strings.Contains(string(sec), `"CLAUDE_CODE_OAUTH_TOKEN":"stub-claude-token"`) {
		t.Fatalf("secrets file %s %v", sec, err)
	}
	for _, spec := range e.run.Specs {
		for k, v := range spec.Env {
			if strings.Contains(v, "stub-claude-token") {
				t.Fatalf("credential in the runner environment: %s", k)
			}
		}
		if strings.Contains(strings.Join(spec.Argv, " "), "stub-claude-token") {
			t.Fatal("credential in the runner argv")
		}
	}
	// ADR-0068: the run's settings.json has SpaceMan's deny-only PermissionRequest hook, run by the shipped agent binary,
	// and the run's own space folder (its real path), nothing wider, as an additional directory; every instance's memory
	// is denied, and the run's own auto memory is outside the space folder.
	spaceDir, _ := filepath.EvalSymlinks(filepath.Join(e.agentDir, "spaces", sp.Workdir))
	raw, err := os.ReadFile(filepath.Join(spaceDir, "instances", e.run1(ctrl.ID).InstanceID, "claude", "settings.json"))
	var settings struct {
		Hooks struct {
			PermissionRequest []struct {
				Hooks []struct{ Command string } `json:"hooks"`
			} `json:"PermissionRequest"`
		} `json:"hooks"`
		Permissions struct {
			AdditionalDirectories []string `json:"additionalDirectories"`
			Deny                  []string `json:"deny"`
		} `json:"permissions"`
		AutoMemoryDirectory string `json:"autoMemoryDirectory"`
	}
	if err != nil || json.Unmarshal(raw, &settings) != nil || len(settings.Hooks.PermissionRequest) != 1 ||
		settings.Hooks.PermissionRequest[0].Hooks[0].Command != hns.ClaudePermissionHookCommand("/usr/bin/true") ||
		!slices.Equal(settings.Permissions.AdditionalDirectories, []string{spaceDir}) ||
		!slices.Contains(settings.Permissions.Deny, "Edit(/"+filepath.Dir(spaceDir)+"/*/instances/*/claude/settings.json)") ||
		!slices.Contains(settings.Permissions.Deny, "Edit(/"+filepath.Dir(spaceDir)+"/*/instances/*/claude/projects/**)") ||
		!strings.HasPrefix(settings.AutoMemoryDirectory, filepath.Join(e.agentDir, "automemory", sp.ID, e.run1(ctrl.ID).InstanceID, "projects")+"/") {
		t.Fatalf("run settings %s %v", raw, err)
	}
	if e.run1(ctrl.ID).Launch.AccountRef != accts["claude_oauth_token"] {
		t.Fatalf("run account %q", e.run1(ctrl.ID).Launch.AccountRef)
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
	// Central restart: pending/sent commands are re-delivered and acknowledged as duplicates. The restart must stay
	// inside the lease: once lease + grace pass without central, the agent stops the runs (SM-NODE-007), which is
	// correct, and they never come back to running.
	restartAt := time.Now()
	e.restartCentral()
	// startCentral asserts the reader pool itself; this loose bound only catches a restart gone pathologically slow
	// (a reader pool per CPU took 5-7 s under -race; a loaded host can take over 5 s even with 4 readers).
	if d := time.Since(restartAt); d >= 15*time.Second {
		t.Fatalf("central restart took %v, not less than 15 s: check app.Options.StoreReaders (each reader parses the schema on open)", d)
	}
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

// Central restarts back to back on the same data directory while a node is connected and reconnecting. Each Close
// races the node's in-flight requests; the next open must still find the database free (it failed with "storage busy:
// open database ... SQLITE_BUSY" when Close released the directory before connections still in use had closed, and the
// writer's first pragma ran without a busy timeout).
func TestCentralRestartBackToBack(t *testing.T) {
	// Not parallel: like TestFleetLifecycle, the restarts must stay inside the node lease.
	e := newFleetEnv(t, longLease)
	ctx := context.Background()
	e.enroll()
	e.startAgent()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	nodeID := nodes[0].ID
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodeID); return n.Connected })
	for i := range 5 {
		e.restartCentral()
		if i%2 == 1 { // every other restart lands while the node is still reconnecting
			eventually(t, "reconnected after central restart", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodeID); return n.Connected })
		}
	}
	eventually(t, "reconnected after the last restart", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodeID); return n.Connected })
}

// SM-NODE-007: without central, the node starts no work and stops running work after lease + grace.
func TestDisconnectEnforcement(t *testing.T) {
	// Not parallel: short leases (shortLease) expire if parallel tests starve the node.
	e := newFleetEnv(t, shortLease)
	ctx := context.Background()
	e.enroll()
	e.startAgent()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "dc-test", Name: "DC", NodeID: nodes[0].ID, DisconnectPolicy: "kill"})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
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

// SM-REC-002/004, SM-UI-004: node recordings reach central, replay through the API and stream live to watch-only
// viewers (with the terminal:watch permission).
func TestRecordingShippedReplayedAndStreamed(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	ctx := context.Background()
	e.enroll()
	e.startAgent()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "rec-test", Name: "Rec", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor-backend", Harness: hns.KindCodex})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "running", func() bool { return e.run1(r.ID).Observed == "running" })
	term := e.run.LiveByLabel("spaceman.run", r.ID)[0].ID
	e.run.Output(term, "hello from the run\r\n")
	stored := func(want string) bool {
		res, err := e.c.Fleet.Recordings().Read(context.Background(), r.ID, 0)
		if err != nil {
			return false
		}
		var out strings.Builder
		for _, rec := range res.Records {
			out.Write(rec.Data)
		}
		return strings.Contains(out.String(), want) && res.Records[0].Kind == "open"
	}
	eventually(t, "recording shipped to central", func() bool { return stored("hello from the run") })

	rootSecret, _, err := e.c.Auth.IssueRootToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/spaces/"+sp.ID+"/runs/"+r.ID+path, nil)
		req.Header.Set("Authorization", "Bearer "+rootSecret)
		return http.DefaultClient.Do(req)
	}
	res, err := get("/recording")
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("replay: %v %v", res.StatusCode, err)
	}
	var page struct {
		Records []struct {
			Kind string `json:"k"`
		} `json:"records"`
	}
	_ = json.NewDecoder(res.Body).Decode(&page)
	res.Body.Close()
	if len(page.Records) < 2 || page.Records[0].Kind != "open" {
		t.Fatalf("replay page %+v", page)
	}

	// Live stream: catch-up, then a new chunk, then the run's end.
	res, err = get("/terminal")
	if err != nil || res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("terminal stream: %v %v", res, err)
	}
	defer res.Body.Close()
	lines := make(chan string, 256)
	go func() {
		buf := make([]byte, 64<<10)
		var acc strings.Builder
		for {
			n, err := res.Body.Read(buf)
			acc.Write(buf[:n])
			for {
				s := acc.String()
				i := strings.Index(s, "\n\n")
				if i < 0 {
					break
				}
				lines <- s[:i]
				acc.Reset()
				acc.WriteString(s[i+2:])
			}
			if err != nil {
				close(lines)
				return
			}
		}
	}()
	e.run.Output(term, "live line\r\n")
	sawLive := false
	deadline := time.After(10 * time.Second)
	for !sawLive {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("stream ended early")
			}
			if strings.Contains(l, "event: record") && strings.Contains(l, `"d":"`+b64("live line\r\n")+`"`) {
				sawLive = true
			}
		case <-deadline:
			t.Fatal("live record not streamed")
		}
	}
	e.run.Exit(term, 0, "exited")
	deadline = time.After(10 * time.Second)
	for {
		select {
		case l, ok := <-lines:
			if !ok || strings.Contains(l, "event: end") {
				return
			}
		case <-deadline:
			t.Fatal("stream did not end with the run")
		}
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// A report that arrives after a run ended (the node sends reports concurrently) must not reopen the run: it would end
// it a second time and publish a second run_ended event. The late report is fenced and the node told to stop it.
func TestLateReportDoesNotReopenEndedRun(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	ctx := context.Background()
	e.enroll()
	e.startAgent()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "late-report", Name: "Late report", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "dev", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "running", func() bool { return e.run1(r.ID).Observed == "running" })
	if _, err := e.c.Fleet.StopRun(ctx, admin, r.ID, "kill", "test"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "exited", func() bool { o := e.run1(r.ID).Observed; return o == "exited" || o == "failed" })
	e.stopAgent()

	// The node's stale report, as if it had been sent before the exit but delivered after it. The run is compared with
	// its state just before the report, once the agent's own last updates have landed (slower under -race).
	n := dialRawNode(t, e, proto.MaxMinor)
	ended := e.run1(r.ID)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		time.Sleep(300 * time.Millisecond)
		next := e.run1(r.ID)
		if next.Version == ended.Version {
			break
		}
		ended = next
	}
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: r.ID, SpaceID: sp.ID, Generation: ended.Generation, State: proto.RunRunning,
		TerminalID: ended.TerminalID, Status: "working"})
	cmd := n.next(t, proto.TypeCommand)
	var c proto.Command
	if err := cmd.Decode(&c); err != nil {
		t.Fatal(err)
	}
	if c.Kind != proto.CmdStop || c.RunID != r.ID {
		t.Fatalf("late report answered with %+v", c)
	}
	if got := e.run1(r.ID); got.Observed != ended.Observed || got.Version != ended.Version {
		t.Fatalf("late report reopened the run: %s v%d (was %s v%d)", got.Observed, got.Version, ended.Observed, ended.Version)
	}
}

// A Codex ChatGPT login (ADR-0053, SM-ACCT-002) lives on the node as one live file per account that every instance
// links to: Codex refreshes it in place and a refresh token works only once, so instances must not hold copies. A
// refreshed login survives later launches; a rotation in central replaces it. The login never reaches the launcher
// shim's file, the runner's environment or its argv.
func TestCodexChatGPTLoginIsSharedByInstances(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	ctx := context.Background()
	e.enroll()
	e.startAgent()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "chatgpt", Name: "ChatGPT login", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	const seed = `{
  "auth_mode": "chatgpt",
  "tokens": {"id_token": "stub-id", "access_token": "stub-access-1", "refresh_token": "stub-refresh-1", "account_id": "acct-stub"},
  "last_refresh": "2026-11-03T10:00:00Z"
}`
	acc, err := e.c.Accounts.Create(ctx, admin, accounts.Input{Name: "codex-plan", Kind: accounts.KindCodexChatGPT, Secret: seed, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Accounts.SetBindings(ctx, admin, sp.ID, []accounts.Binding{{AccountID: acc.ID, Default: true}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "lease", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	launch := func(role string) *fleet.Run {
		t.Helper()
		r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: role, Harness: hns.KindCodex, Prompt: "go"})
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, role+" running", func() bool { return e.run1(r.ID).Observed == "running" })
		return e.run1(r.ID)
	}
	home := func(r *fleet.Run) string {
		return filepath.Join(e.agentDir, "spaces", sp.Workdir, "instances", r.InstanceID, "codex")
	}
	a, b := launch("dev"), launch("qa")
	if a.InstanceID == b.InstanceID {
		t.Fatal("two roles share an instance")
	}
	ta, errA := os.Readlink(filepath.Join(home(a), "auth.json"))
	tb, errB := os.Readlink(filepath.Join(home(b), "auth.json"))
	if errA != nil || errB != nil || ta != tb {
		t.Fatalf("auth.json links %q %v, %q %v: want one shared target", ta, errA, tb, errB)
	}
	if la, err := os.Readlink(filepath.Join(home(a), "auth.lock")); err != nil || filepath.Dir(la) != filepath.Dir(ta) {
		t.Fatalf("auth.lock link %q %v", la, err)
	}
	live, err := os.ReadFile(ta)
	if err != nil || !strings.Contains(string(live), `"refresh_token":"stub-refresh-1"`) || strings.Contains(string(live), "\n") {
		t.Fatalf("live login %s %v: want the compacted seed", live, err)
	}
	if fi, err := os.Stat(ta); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("live login mode %v %v", fi, err)
	}
	for _, r := range []*fleet.Run{a, b} {
		sec, err := os.ReadFile(filepath.Join(e.agentDir, "runs", r.ID, agent.SecretsFile))
		if err != nil || strings.Contains(string(sec), "stub-refresh") || strings.Contains(string(sec), "stub-access") {
			t.Fatalf("secrets file of %s: %s %v", r.ID, sec, err)
		}
	}
	for _, spec := range e.run.Specs {
		all := strings.Join(spec.Argv, " ")
		for _, v := range spec.Env {
			all += " " + v
		}
		if strings.Contains(all, "stub-refresh") || strings.Contains(all, "stub-access") {
			t.Fatal("the login reached the runner's environment or argv")
		}
	}

	// Codex refreshes the login in place; the next launch keeps the refreshed copy.
	refreshed := `{"auth_mode":"chatgpt","tokens":{"access_token":"stub-access-2","refresh_token":"stub-refresh-2"}}`
	if err := os.WriteFile(ta, []byte(refreshed), 0o600); err != nil {
		t.Fatal(err)
	}
	launch("ops")
	if got, _ := os.ReadFile(ta); string(got) != refreshed {
		t.Fatalf("a launch overwrote the refreshed login: %s", got)
	}

	// A rotation in central replaces the live copy at the next launch.
	cur, err := e.c.Accounts.Get(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	rotated := strings.ReplaceAll(seed, "stub-refresh-1", "stub-refresh-3")
	if _, err := e.c.Accounts.Update(ctx, admin, acc.ID, cur.Version, accounts.Input{Name: cur.Name, Secret: rotated, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	launch("docs")
	if got, _ := os.ReadFile(ta); !strings.Contains(string(got), `"refresh_token":"stub-refresh-3"`) {
		t.Fatalf("the rotated login did not replace the live copy: %s", got)
	}
}
