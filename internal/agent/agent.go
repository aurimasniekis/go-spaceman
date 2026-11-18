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
	"github.com/aurimasniekis/go-spaceman/internal/harness/dist"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/recording"
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
	// RecordingCreate creates recording segment files; nil uses the filesystem (fault tests inject a full disk).
	RecordingCreate func(path string) (recording.File, error)
	// RecordingRetryAfter is the first wait before a recording whose disk failed is tried again; 0 is
	// recording.DefaultRetryAfter (fault tests shorten it).
	RecordingRetryAfter time.Duration
	// Claude configures node-managed Claude Code (ADR-0077); the zero value keeps runs on HarnessBinaries.
	Claude ClaudeOptions
	// JournalRetention is how long the journal keeps an ended run (prune.go); 0 and anything below the Claude Code pin window
	// (30 days) mean that window.
	JournalRetention time.Duration
	// DefaultCols and DefaultRows are the size the runner starts programs at (runner.terminal_size); a takeover open
	// when a run's held size is cleared is resized back to it (ADR-0075). 0: left as it is.
	DefaultCols, DefaultRows int
	// NoMemoryFastForward turns ADR-0094's fast-forward of idle memory checkouts off (memory.fast_forward: false); run
	// start then refreshes a checkout as before.
	NoMemoryFastForward bool
}

// Agent is a running node agent.
type Agent struct {
	o   Options
	j   *Journal
	log *slog.Logger
	// fetchChunk reads one range of an attachment from central for a save (forwardTool; tests replace it).
	fetchChunk func(context.Context, bridgeRequest) bridgeResponse

	mu        sync.Mutex
	loginMu   sync.Mutex // shared harness logins (login.go)
	conn      *websocket.Conn
	connected bool
	leases    map[string]leaseRow
	heartbeat time.Duration
	// recorders are the live recordings by run ID. recordersStopped is set at shutdown (stopRecorders): no recorder
	// starts after it, so a launch still finishing during drain leaves none running past Run.
	recorders        map[string]*recorder
	recordersStopped bool
	recWG            sync.WaitGroup // every recorder goroutine, so stopRecorders can wait for the flushes it asked for
	// lostAt is when central was last lost (zero while connected).
	lostAt time.Time
	wake   chan struct{}
	// runTokens are the hashes of active runs' bridge tokens.
	runTokens map[string][32]byte
	// caps caches the capability inventory (harness detection runs binaries; never between dial and hello), capsAt is
	// when it was taken and capsGen counts the takes (capsLoop re-reports what a connection's hello missed).
	// capsStarts numbers the detections as they start and capsTaken is the number of the one cached, so a slower
	// detection that started earlier never replaces a newer one.
	caps       proto.Capabilities
	capsAt     time.Time
	capsGen    uint64
	capsStarts uint64
	capsTaken  uint64
	// capsSent is the newest generation sent to central (a hello counts), so an older one is never sent after it;
	// capsSendMu makes reportCapabilities' check and send one step.
	capsSent   uint64
	capsSendMu sync.Mutex
	// acked is central's highest stored recording sequence per run; unshipped marks runs needing catch-up.
	acked     map[string]uint64
	unshipped map[string]bool
	// restored marks runs whose acked value was read from their run directory after a restart (restoreAck) and not yet
	// compared with the local recording by a catch-up.
	restored map[string]bool
	// ackFileMu serialises writing a run's acknowledgement file with removing the run directory (prune).
	ackFileMu sync.Mutex
	// seqKnown marks runs whose recording sequence at central this process knows: it launched the run with an empty
	// recording, or central acknowledged a recording message of it. Until then the run's recorder numbers nothing
	// (seqGate): a process that ran before may have shipped records its failing disk never stored (ADR-0097).
	// seqFloor is, for a run whose first acknowledgement in this process was below its restored one, that restored
	// value: new records are still numbered above it. lowers counts such lowerings per run, so a catch-up that began
	// from the restored value starts again from the lowered one (catchUpRun).
	seqKnown map[string]bool
	seqFloor map[string]uint64
	lowers   map[string]uint64
	// Recording shipping (record.go). sentSeq is the last sequence a send of each run went out for (above acked: on
	// its way, or lost with a connection). held are records a live shipment kept back while it waited for a catch-up.
	// catching is set while the catch-up pass runs, on catchUpConn with catchUpCtx (the newest connection's);
	// catchUpAgain asks it for another sweep; caughtUp are the runs it has caught up. connCtx is the current
	// connection's context.
	sentSeq        map[string]uint64
	held           map[string]*heldRecords
	catching       bool
	catchUpSkipped bool // a sweep skipped a run central did not answer for (catchUpRun)
	catchUpAgain   bool
	catchUpGen     uint64 // counts requests: a sweep marks runs caught up only for the request it serves
	catchUpConn    *websocket.Conn
	catchUpCtx     context.Context
	caughtUp       map[string]bool
	// finalSent and finalAcked are, per ended run, the journal update time of the final state this process sent to central
	// and the one central acknowledged (prune.go); memory only.
	finalSent, finalAcked map[string]int64
	// followSnap is, per run, the journal update time of the final state in this connection's follow-up snapshot
	// (helloRuns): the follow-up reports it, so the connection's reconcile does not report it inline (finalAck).
	followSnap map[string]int64
	connCtx    context.Context
	// minor is the negotiated protocol minor version of the current connection.
	minor int
	// toolCalls are the agent tool calls waiting for central's result, by call ID.
	toolCalls map[string]chan proto.ToolResult
	// mem keeps role memory (ADR-0025).
	mem *memManager
	// bg tracks background work (memory checkpoints, usage, transcript uploads, KB acceptance, dream commits) that Run
	// waits for when it stops; stopping refuses new work.
	bgMu     sync.Mutex
	bg       sync.WaitGroup
	stopping bool
	// renameMu serializes the moves and acknowledgements of role renames (rolerename.go, ADR-0093).
	renameMu sync.Mutex
	// reloc tracks relocation steps and transfers (ADR-0037).
	reloc relocState
	// guardLocks serialize each space's lease guard between launches and retirement (ADR-0043), by space ID.
	guardLocks map[string]*sync.Mutex
	// repoLocks serialize each space's repository syncs and clones (ADR-0044), by space ID.
	repoLocks map[string]*sync.Mutex
	// takeovers are the runs whose terminal an operator has taken over (SM-UI-004), by run ID.
	takeovers map[string]*takeover
	// sizeWant is central's set of run terminal sizes and holds the attaches that keep them (ADR-0075), by run ID;
	// holdsStopped is set at shutdown.
	sizeWant     map[string]proto.TerminalSize
	holds        map[string]*sizeHold
	holdsStopped bool
	// runLocks serialize a run's launch (its last check and start) with its stop, by run ID (runLock).
	runLocks map[string]*refMutex
	// launching counts the launches in flight by run ID, from before the run is journaled until its process is started and
	// observed. The runner reconcile leaves those runs alone (reconcileRunner).
	launching map[string]int
	// endPending holds runs whose row was ended while their adopted program was still live: the monitor's finished
	// event for the program runs their end-of-run work (observe), as the row's end already passed.
	endPending sync.Map
	// claudeStage, when set, runs at a managed Claude Code launch's stages ("acquired": the version is held;
	// "recorded": the journal holds it and the hold is still taken; "released": the hold is released). Tests race GC
	// and stops there.
	claudeStage func(stage string, res dist.Resolution)
	// beforeStartingWrite, when set, runs in a managed launch between the abandoned check and the starting write (tests).
	beforeStartingWrite func()
	// beforeFailWrite and beforeAdoptObserve, when set, run in a launch between its abandoned check and the lock
	// that guards the write (a failure) or the observe (an adoption) (tests).
	beforeFailWrite, beforeAdoptObserve func()
	// beforeEnsureGuard runs in a launch before it takes the guard lock; onRunEndHooks runs at the start of runEndHooks.
	// Test hooks.
	beforeEnsureGuard func()
	onRunEndHooks     func(runID string)
	// beforeRecordAttach, when set, runs before each of a recorder's attachments (tests).
	beforeRecordAttach func(runID string)
	// claudePins are the exact Claude Code versions profiles pinned on the launches this process started, with when it
	// last started one (claudePinned).
	claudePins map[string]time.Time
	// limitMu serializes transcript limit scans, limitSend a limit's journal write and report with every other run
	// report; limitTimers are the pending (debounced) scans and limitFiles the
	// transcript each run was last scanned in (a replaced file is scanned from the start), by run ID.
	limitMu   sync.Mutex
	limitSend sync.Mutex
	// heldReports are the runs whose report came between the hello and the welcome (guarded by mu): the minor is not
	// negotiated yet, so the report would go out stripped of what the central's minor decides. They are re-sent after the welcome.
	heldReports map[string]struct{}
	// preConn is set (under mu) from before the hello snapshot (helloRuns) until the connection is set: a report in that
	// window cannot go out (no connection) and is not in the snapshot, so it is held like one between the hello and the
	// welcome (sendReport). a.conn is not set earlier: another sender would then write before the hello.
	// Lock order: limitSend, then mu (sendReport); session takes only mu here and never limitSend, so no cycle.
	preConn bool
	// helloStage, when set, runs after the hello snapshot and before the connection is set. Tests race a report there.
	helloStage  func()
	limitTimers map[string]*time.Timer
	limitFiles  map[string]os.FileInfo
	// statusSnaps are the latest status-line snapshots and failureClasses the latest non-limit StopFailure class of
	// each run, by run ID (hooks.go); both are dropped when the run ends.
	statusSnaps    map[string]StatusObservation
	failureClasses map[string]string
	stopScans      map[string]time.Time
}

// agentMaxMinor is the newest protocol minor this agent implements, advertised in the hello. It is a literal on purpose:
// advertising proto.MaxMinor would claim every minor central adds later, before this agent speaks it (ADR-0093). Raise it
// together with the code that handles the new minor.
const agentMaxMinor = 14

// helloMaxMinor is the minor the hello advertises; a variable only so a test can tell it apart from proto.MaxMinor.
var helloMaxMinor = agentMaxMinor

// track runs f as background work Run waits for at shutdown. After shutdown began it does nothing.
func (a *Agent) track(f func()) {
	a.bgMu.Lock()
	if a.stopping {
		a.bgMu.Unlock()
		return
	}
	a.bg.Add(1)
	a.bgMu.Unlock()
	defer a.bg.Done()
	f()
}

// goTrack is track on its own goroutine.
func (a *Agent) goTrack(f func()) {
	a.bgMu.Lock()
	if a.stopping {
		a.bgMu.Unlock()
		return
	}
	a.bg.Add(1)
	a.bgMu.Unlock()
	go func() {
		defer a.bg.Done()
		f()
	}()
}

// drain stops new background work and waits (bounded) for what is running.
func (a *Agent) drain(limit time.Duration) {
	a.bgMu.Lock()
	a.stopping = true
	a.bgMu.Unlock()
	done := make(chan struct{})
	go func() { a.bg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(limit):
		a.log.Warn("background work still running at shutdown")
	}
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
	a := &Agent{o: o, j: j, log: obs.Component(o.Log, "agent"), leases: map[string]leaseRow{}, heartbeat: 10 * time.Second,
		recorders: map[string]*recorder{}, wake: make(chan struct{}, 1), guardLocks: map[string]*sync.Mutex{}, runLocks: map[string]*refMutex{}, launching: map[string]int{}, takeovers: map[string]*takeover{}}
	a.mem = newMemManager(a)
	return a
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
	a.mem.base = context.WithoutCancel(ctx)
	defer a.shutdown(30 * time.Second)
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
	// Role renames the journal holds open (deferred, failed, or interrupted by a stop) are tried again now that the
	// journal knows which runs are still alive (ADR-0093).
	a.goTrack(func() { a.retryRoleRenames(context.WithoutCancel(ctx), "") })
	// Managed Claude Code: after the reconcile, so the journal's live and adoptable runs keep their versions.
	var claudeWG sync.WaitGroup
	defer claudeWG.Wait()
	a.startClaude(ctx, &claudeWG)
	// Run returns after the monitor does (bounded): it reacts to runner events with the agent's state, and a Run that
	// had returned must not leave it still reading what the caller restores or tears down next.
	monitorDone := make(chan struct{})
	defer func() {
		select {
		case <-monitorDone:
		case <-time.After(10 * time.Second):
			a.log.Warn("the runner monitor did not stop")
		}
	}()
	go func() { defer close(monitorDone); a.monitor(ctx) }()
	go a.enforce(ctx)
	go a.capsLoop(ctx)
	go a.pruneLoop(ctx)
	go func() {
		if err := a.ServeBridge(ctx); err != nil {
			a.log.Warn("mcp bridge socket", obs.Err(err))
		}
	}()
	// The first retry waits a second, or less when ReconnectMax is shorter: the cap applies to every attempt.
	first := min(time.Second, a.o.ReconnectMax)
	backoff := first
	for {
		start := a.now()
		err := a.session(ctx)
		if ctx.Err() != nil {
			return nil //nolint:nilerr // shutdown: the session error is the cancellation itself
		}
		a.log.Warn("central connection lost", obs.Err(err))
		if a.now().Sub(start) > time.Minute {
			backoff = first
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, a.o.ReconnectMax)
	}
}

// shutdown is Run's stop sequence: holds, takeovers and recordings end first, then background work drains (bounded
// by limit). Recorders outlive the commands that start them, not the agent: a stopped agent must not keep a second
// recorder attached when the next one adopts the run, so none starts once they stopped, not even from a launch that
// finishes during the drain.
func (a *Agent) shutdown(limit time.Duration) {
	a.stopHolds()
	a.stopTakeovers()
	a.stopRecorders()
	a.drain(limit)
}

// stopRecorders ends every live recording (each flushes and closes its writer) and refuses new ones. It waits, bounded,
// for the flushes: a recorder writing its last records into the data directory after this returns raced the removal
// of that directory (a TempDir cleanup failed with "directory not empty").
func (a *Agent) stopRecorders() {
	a.mu.Lock()
	a.recordersStopped = true
	for _, r := range a.recorders {
		r.cancel()
	}
	a.mu.Unlock()
	done := make(chan struct{})
	go func() { a.recWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		a.log.Warn("recorders still flushing at shutdown")
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
		if kind == harness.KindClaude && a.managedClaude() {
			c.Harnesses = append(c.Harnesses, a.claudeInfo(ctx))
			continue
		}
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
	a.mu.Lock()
	stale := a.capsAt.IsZero() || a.now().Sub(a.capsAt) > 5*time.Minute
	a.mu.Unlock()
	if stale {
		a.takeCapabilities(ctx)
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
	a.mu.Lock()
	a.preConn = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.preConn = false
		a.mu.Unlock()
	}()
	runs, follow, err := a.helloRuns(ctx)
	if err != nil {
		return err
	}
	hello := proto.Hello{AgentVersion: buildinfo.Get().Version, MinMinor: proto.MinMinor, MaxMinor: helloMaxMinor,
		Capacity: a.o.Capacity, Runs: []proto.RunReport{}, Leases: a.leaseStates()}
	a.mu.Lock()
	hello.Capabilities = a.caps
	helloCaps := a.capsGen
	a.capsSent = max(a.capsSent, helloCaps)
	a.mu.Unlock()
	for _, r := range runs {
		// The hello carries no hook data (failure class, status-line reset): the minor is not negotiated yet, and a central
		// below 13 would store a filled reset as the transcript's. reportAllHookData sends it after a minor-13 welcome.
		hello.Runs = append(hello.Runs, r.report(a.now()))
	}
	if a.helloStage != nil {
		a.helloStage()
	}
	a.mu.Lock()
	a.conn, a.preConn = conn, false
	a.mu.Unlock()
	if err := a.send(ctx, proto.TypeHello, hello); err != nil {
		return err
	}
	a.noteSent(runs)
	defer func() {
		a.mu.Lock()
		a.conn, a.connected, a.lostAt, a.heldReports = nil, false, a.now(), nil
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
			// Connected and catching up start together: no live recording shipment can go ahead of the catch-up
			// (shipLive).
			a.mu.Lock()
			a.connected, a.lostAt, a.minor = true, time.Time{}, w.Minor
			if w.HeartbeatInterval > 0 {
				a.heartbeat = w.HeartbeatInterval
			}
			a.connCtx = hbCtx
			startPass := a.startCatchUpLocked()
			held := a.heldReports
			a.heldReports = nil
			a.mu.Unlock()
			if len(held) > 0 {
				go a.resendHeldReports(hbCtx, held)
			}
			if startPass {
				go a.catchUpRecordings()
			}
			go a.heartbeatLoop(hbCtx)
			go a.mem.resume(hbCtx)
			go a.resendKBResults(hbCtx)
			go a.resumeTranscripts(hbCtx)
			go a.resendRelocationReports(hbCtx)
			go a.resendRoleRenames(hbCtx)
			go a.capsAfterHello(hbCtx, helloCaps)
			go a.reportAllHookData(hbCtx)
			if len(follow) > 0 {
				go a.sendFollowUp(hbCtx, conn, follow)
			}
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
			a.finalAck(ctx, rec)
		case proto.TypeRecordingAck:
			var ack proto.RecordingAck
			if err := env.Decode(&ack); err == nil {
				a.recordingAck(ack)
			}
		case proto.TypeToolResult:
			var res proto.ToolResult
			if err := env.Decode(&res); err == nil {
				a.toolResult(res)
			}
		case proto.TypeTakeoverState:
			var st proto.TakeoverState
			if err := env.Decode(&st); err == nil {
				a.applyTakeovers(ctx, st)
			}
		case proto.TypeTerminalSizes:
			var st proto.TerminalSizes
			if err := env.Decode(&st); err == nil {
				a.applyTerminalSizes(ctx, st)
			}
		case proto.TypeTakeoverInput:
			var in proto.TakeoverInput
			if err := env.Decode(&in); err == nil {
				a.takeoverInput(in)
			}
		case proto.TypeTranscriptAck:
			var ack proto.TranscriptAck
			if err := env.Decode(&ack); err == nil {
				go a.transcriptAck(context.WithoutCancel(ctx), ack)
			}
		case proto.TypeRelocationUploadAck:
			var ack proto.RelocationUploadAck
			if err := env.Decode(&ack); err == nil {
				a.relocAnswer(uploadKey(ack.RelocationID, ack.Name), env.Body)
			}
		case proto.TypeRelocationData:
			var d proto.RelocationData
			if err := env.Decode(&d); err == nil {
				a.relocAnswer(fetchKey(d.RelocationID, d.Name, d.Offset), env.Body)
			}
		case proto.TypeMemoryAck:
			var ack proto.MemoryAck
			if err := env.Decode(&ack); err == nil {
				go a.mem.acked(context.WithoutCancel(ctx), ack)
			}
		case proto.TypeMemoryRenamedAck:
			var ack proto.MemoryRenamedAck
			if err := env.Decode(&ack); err == nil {
				go a.track(func() { a.roleRenamedAck(context.WithoutCancel(ctx), ack) })
			}
		case proto.TypeMemoryFlush:
			var req proto.MemoryFlush
			if err := env.Decode(&req); err == nil {
				go a.track(func() { a.memoryFlush(hbCtx, req) })
			}
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
	return writeTo(ctx, conn, b)
}

// sendOn is send on a given connection: it fails, rather than use a newer one, when that connection is gone.
func (a *Agent) sendOn(ctx context.Context, conn *websocket.Conn, typ string, body any) error {
	b, err := proto.Encode(ids.New(ids.Message), typ, a.now(), body)
	if err != nil {
		return err
	}
	return writeTo(ctx, conn, b)
}

func writeTo(ctx context.Context, conn *websocket.Conn, b []byte) error {
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
// report sends a run's report. Its usage limit is read from the journal under limitSend, so a report built from an
// older row never undoes a limit change the scan has sent.
func (a *Agent) report(ctx context.Context, r runRow) {
	a.limitSend.Lock()
	defer a.limitSend.Unlock()
	if cur, err := a.j.getRun(ctx, r.RunID); err == nil {
		r.Limit = cur.Limit
	}
	a.sendReport(ctx, r)
}

// resendHeldReports sends, from the journal, the reports held back before the welcome.
func (a *Agent) resendHeldReports(ctx context.Context, held map[string]struct{}) {
	for id := range held {
		// The row is read under limitSend, so a newer report is never followed by this older state.
		a.limitSend.Lock()
		if r, err := a.j.getRun(ctx, id); err == nil {
			a.sendReport(ctx, r)
		}
		a.limitSend.Unlock()
	}
}

// holdsReportsLocked is whether a report must wait for the welcome (mu held): the hello is being built or sent.
func (a *Agent) holdsReportsLocked() bool { return a.preConn || (a.conn != nil && !a.connected) }

// holdingReports is holdsReportsLocked, taking mu.
func (a *Agent) holdingReports() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.holdsReportsLocked()
}

// sendReport sends a run's report as it is (limitSend held).
func (a *Agent) sendReport(ctx context.Context, r runRow) {
	a.mu.Lock()
	if a.holdsReportsLocked() {
		if a.heldReports == nil {
			a.heldReports = map[string]struct{}{}
		}
		a.heldReports[r.RunID] = struct{}{}
		a.mu.Unlock()
		return
	}
	minor := a.minor
	a.mu.Unlock()
	if err := a.send(ctx, proto.TypeRunReport, a.withHookData(r.report(a.now())).ForMinor(minor)); err != nil {
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
	return a.leaseLocked(spaceID, gen)
}

// leaseLocked is lease with a.mu held.
func (a *Agent) leaseLocked(spaceID string, gen int64) (leaseRow, bool) {
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
