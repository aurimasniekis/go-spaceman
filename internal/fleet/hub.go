package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// SecretsResolver returns the secrets to attach to a run's launch at dispatch time (accounts, M3). They are only put on
// the wire, never stored with the command (ADR-0009).
type SecretsResolver func(ctx context.Context, run *Run) (map[string]string, error)

// ErrCredentialsUnavailable is returned (wrapped) by a SecretsResolver when the run's account can no longer be used;
// the run is then failed instead of being retried forever.
var ErrCredentialsUnavailable = errors.New("the run's account is no longer usable")

// ErrNodeTooOld is returned (wrapped) when a launch needs a newer node protocol than the node's session negotiated;
// the run is failed instead of starting without what it asked for.
var ErrNodeTooOld = errors.New("the node's agent is too old for this run")

// errBadPayload is returned (wrapped) when a stored command's payload doesn't decode. Stored payloads never change, so
// retrying can't help: a run's command fails the run as invalid_command.
var errBadPayload = errors.New("the stored command payload is invalid")

// ToolDispatcher runs an authenticated agent's tool call (ADR-0012). The catalog call (proto.ToolCatalog) returns
// the run's tools as []proto.AgentTool.
type ToolDispatcher func(ctx context.Context, p *RunPrincipal, tool string, args json.RawMessage) (json.RawMessage, error)

// toolCallTimeout bounds one agent tool call: MCP calls are never held open indefinitely (SM-COMM-004).
const toolCallTimeout = 25 * time.Second

// Hub holds the live node connections. Each connection has one reader loop and one writer goroutine; work for a node
// is triggered by Kick (after a commit) rather than by polling (SM-ARCH-004).
type Hub struct {
	svc *Service
	mu  sync.Mutex
	// sessions maps node ID to its live connection (a newer connection replaces an older one).
	sessions map[string]*session
	// newest maps node ID to the sequence number of the node's newest connection whose fleet.node_connect transaction
	// ran; seq numbers connections. Only that connection's end records the node's disconnect (onDisconnect).
	newest map[string]uint64
	seq    uint64
	// Secrets attaches run secrets to launches (nil: none).
	Secrets SecretsResolver
	// RepoKey returns a space repository's private SSH key ("" when it has none) for the wire message of a clone or
	// fetch (repos.Service.Key, ADR-0057; nil: none).
	RepoKey func(ctx context.Context, spaceID, repoID string) (string, error)
	// Tools runs agent tool calls (nil: agents only have the node's local tools).
	Tools ToolDispatcher
	// Memory stores role memory the node syncs (ADR-0025); nil refuses syncs.
	Memory MemoryStore
	// KBResult receives the outcomes of kb.accept commands (ADR-0026).
	KBResult func(ctx context.Context, nodeID string, res proto.KBResult)
	// Usage stores runs' usage and transcripts (ADR-0028); nil drops them.
	Usage UsageStore
	// RepoStatus receives the node's reports of a space's configured repositories (ADR-0044); nil drops them.
	RepoStatus func(ctx context.Context, nodeID string, rep proto.RepoStatusReport)
	// OnConnected is called (on its own goroutine) once a node's connection is set up and its leases granted.
	OnConnected func(ctx context.Context, nodeID string)

	// flushes are memory flushes waiting for a node's memory.flushed, by flush ID (ADR-0045).
	flushMu sync.Mutex
	flushes map[string]*pendingFlush

	// mints are gateway token mints running in the background, by run ID (Options.GatewayMintWait). mintWG tracks their
	// goroutines and mintStop cancels them, so central's shutdown waits for them before it closes the store.
	mintMu      sync.Mutex
	mints       map[string]*mintJob
	mintWG      sync.WaitGroup
	mintCtx     context.Context
	mintStop    context.CancelFunc
	mintStopped bool
}

// mintJob is a gateway run's token mint the dispatch loop stopped waiting for. It holds only the outcome: once it is
// done, the next dispatch gets the token from Gateway.Token, which reuses the stored one.
type mintJob struct {
	done chan struct{}
	err  error
}

// errMintPending: the gateway run's token is still being minted; the launch stays pending until the mint kicks the node.
var errMintPending = errors.New("the run's gateway token is still being minted")

// Background mint bounds: a mint gets at most mintJobTimeout (Token's own retries take about 75 s at worst), and an
// outcome no dispatch collects is dropped after mintJobKeep.
const (
	mintJobTimeout = 2 * time.Minute
	mintJobKeep    = 5 * time.Minute
)

// gatewayToken returns a gateway run's token secrets, waiting at most Options.GatewayMintWait for a mint; a longer one
// goes on in the background and errMintPending is returned. The background mint kicks the node when it is done. Only the
// pass that starts a mint waits: while one is already running, every later pass returns errMintPending at once, so a
// node with many slow mints doesn't wait GatewayMintWait for each of them before it gets to its stops and kills.
func (h *Hub) gatewayToken(ctx context.Context, nodeID string, run *Run, session string) (map[string]string, error) {
	gw := h.svc.opts.Gateway
	h.mintMu.Lock()
	if h.mintStopped {
		h.mintMu.Unlock()
		return nil, errMintPending // central is shutting down: the next central dispatches the launch
	}
	j := h.mints[run.ID]
	started := j == nil
	if started {
		j = &mintJob{done: make(chan struct{})}
		if h.mints == nil {
			h.mints = map[string]*mintJob{}
		}
		if h.mintCtx == nil {
			h.mintCtx, h.mintStop = context.WithCancel(context.WithoutCancel(ctx))
		}
		h.mints[run.ID] = j
		h.mintWG.Add(1)
		stopCtx := h.mintCtx
		go func() {
			defer h.mintWG.Done()
			// Detached from the connection: a mint that already reached Harbor Pilot finishes and is stored (or queued
			// for revocation), whatever happens to this connection. Only central's shutdown cancels it (StopMints).
			mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mintJobTimeout)
			defer cancel()
			defer context.AfterFunc(stopCtx, cancel)()
			_, j.err = gw.Token(mctx, run, session)
			close(j.done)
			time.AfterFunc(mintJobKeep, func() { h.dropMint(run.ID, j) })
			h.Kick(nodeID)
		}()
	}
	h.mintMu.Unlock()
	if started {
		t := time.NewTimer(h.svc.opts.GatewayMintWait)
		defer t.Stop()
		select {
		case <-j.done:
		case <-t.C:
			return nil, errMintPending
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	} else {
		select {
		case <-j.done:
		default:
			return nil, errMintPending
		}
	}
	h.dropMint(run.ID, j)
	if j.err != nil {
		return nil, j.err
	}
	return gw.Token(ctx, run, session) // reuses the token the job stored
}

// StopMints ends the background gateway mints at central's shutdown, before the store is closed: it waits up to grace
// for them to finish, then cancels the rest and waits for them to return. A cancelled mint that already reached
// Harbor Pilot queues its credential for revocation (that write is not cancelled). No new mint starts afterwards.
func (h *Hub) StopMints(grace time.Duration) {
	h.mintMu.Lock()
	h.mintStopped = true
	stop := h.mintStop
	h.mintMu.Unlock()
	done := make(chan struct{})
	go func() { h.mintWG.Wait(); close(done) }()
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-done:
		return
	case <-t.C:
	}
	if stop != nil {
		stop()
	}
	<-done
}

func (h *Hub) dropMint(runID string, j *mintJob) {
	h.mintMu.Lock()
	if h.mints[runID] == j {
		delete(h.mints, runID)
	}
	h.mintMu.Unlock()
}

// UsageStore is central's usage and transcript storage as the hub sees it.
type UsageStore interface {
	Report(ctx context.Context, nodeID string, rep proto.UsageReport)
	Chunk(ctx context.Context, nodeID string, ch proto.TranscriptChunk) proto.TranscriptAck
}

// MemoryStore is central's role-memory storage as the hub sees it.
type MemoryStore interface {
	Apply(ctx context.Context, nodeID string, m proto.MemorySync) proto.MemoryAck
	Report(ctx context.Context, nodeID string, st proto.MemoryStatus)
}

type session struct {
	nodeID string
	credID string
	conn   *websocket.Conn
	out    chan []byte
	kick   chan struct{}
	done   chan struct{}
	once   sync.Once
	log    *slog.Logger
	// tools bounds concurrent agent tool calls from this node.
	tools chan struct{}
	// memory serializes this node's memory syncs (git runs outside any transaction, one at a time per node).
	memory chan struct{}
	// minor is the negotiated protocol minor version (set before the session is registered).
	minor int
	// seq numbers the connection among all of the hub's connections (set before fleet.node_connect).
	seq uint64
	// counted is true once the connection counts toward the connected-nodes gauge.
	counted bool
	// The stale-run sweeper's view of this connection (ADR-0074). connectedAt, liveAtConnect and inHello are set before
	// the session is registered and never change; helloApplied is guarded by Hub.mu.
	//
	// connectedAt is the clock's reading when the session was registered. It is only ever subtracted from another
	// reading of the same clock (the system clock's readings carry a monotonic part), so a wall-clock step does not move
	// the sweeper's grace.
	connectedAt time.Time
	// liveAtConnect holds the node's live runs (ID to version) as fleet.node_connect read them, in its transaction.
	liveAtConnect map[string]int64
	// inHello holds the IDs of the runs the node's hello reported, whether or not their report applied.
	inHello map[string]bool
	// helloApplied is true once every hello report applied; it stays false when one failed (the sweeper then leaves the
	// connection's runs alone).
	helloApplied bool
	// sizes serializes terminal.sizes pushes: each reads the run sizes and sends them under it, so a push that read
	// older sizes can never be sent after a newer one (ADR-0075).
	sizes sync.Mutex
}

func newHub(svc *Service) *Hub {
	return &Hub{svc: svc, sessions: map[string]*session{}, newest: map[string]uint64{}}
}

// staleView returns the node's live connection and whether all its hello reports applied (nil: no live connection).
func (h *Hub) staleView(nodeID string) (*session, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.sessions[nodeID]
	if s == nil {
		return nil, false
	}
	return s, s.helloApplied
}

// Connected reports whether a node has a live connection.
func (h *Hub) Connected(nodeID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.sessions[nodeID]
	return ok
}

// Kick wakes a node's session to deliver pending commands (no-op when the node is not connected).
func (h *Hub) Kick(nodeID string) {
	h.mu.Lock()
	s := h.sessions[nodeID]
	h.mu.Unlock()
	if s != nil {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
}

// Refresh re-grants a node's leases (for example after a space was created on it) and delivers pending commands.
func (h *Hub) Refresh(nodeID string) { h.Kick(nodeID) }

// CloseAll closes every node connection (central shutdown). Nodes reconnect with backoff; until then they start no
// work and keep running work within the lease plus grace.
func (h *Hub) CloseAll(reason string) {
	h.mu.Lock()
	ss := make([]*session, 0, len(h.sessions))
	for _, s := range h.sessions {
		ss = append(ss, s)
	}
	h.mu.Unlock()
	for _, s := range ss {
		s.close(websocket.StatusGoingAway, reason)
	}
}

// Disconnect closes a node's live connection (revocation, re-enrollment, disable).
func (h *Hub) Disconnect(nodeID, reason string) {
	h.mu.Lock()
	s := h.sessions[nodeID]
	h.mu.Unlock()
	if s != nil {
		s.close(websocket.StatusPolicyViolation, reason)
	}
}

func (s *session) close(code websocket.StatusCode, reason string) {
	s.once.Do(func() {
		close(s.done)
		_ = s.conn.Close(code, truncate(reason, 120))
	})
}

func (s *session) send(typ string, now time.Time, body any) error {
	b, err := proto.Encode(ids.New(ids.Message), typ, now, body)
	if err != nil {
		return err
	}
	select {
	case s.out <- b:
		return nil
	case <-s.done:
		return net.ErrClosed
	}
}

// ServeHTTP is the node endpoint (proto.ConnectPath).
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m := h.svc.opts.Metrics
	cred := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	node, credID, err := h.svc.AuthenticateNode(ctx, cred)
	if err != nil {
		m.NodeConnect(metrics.NodeAuthFailed)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{proto.Subprotocol}})
	if err != nil {
		return
	}
	if conn.Subprotocol() != proto.Subprotocol {
		m.NodeConnect(metrics.NodeIncompatible)
		_ = conn.Close(websocket.StatusPolicyViolation, "subprotocol "+proto.Subprotocol+" required")
		return
	}
	conn.SetReadLimit(proto.MaxMessage)
	s := &session{nodeID: node.ID, credID: credID, conn: conn, out: make(chan []byte, 256), kick: make(chan struct{}, 1), done: make(chan struct{}), tools: make(chan struct{}, 8), memory: make(chan struct{}, 1),
		log: obs.Component(h.svc.opts.Log, "fleet").With(slog.String(obs.FieldNodeID, node.ID))}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if err := h.run(context.WithoutCancel(ctx), s, ip); err != nil && !errors.Is(err, net.ErrClosed) {
		s.log.Info("node connection ended", obs.Err(err))
	}
}

func (h *Hub) run(ctx context.Context, s *session, ip string) error {
	svc := h.svc
	m := svc.opts.Metrics
	defer s.close(websocket.StatusNormalClosure, "")
	helloCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	env, err := h.read(helloCtx, s)
	cancel()
	if err != nil {
		return err
	}
	var hello proto.Hello
	if env.Type != proto.TypeHello || env.Decode(&hello) != nil {
		m.NodeConnect(metrics.NodeProtocolError)
		return errors.New("first message must be hello")
	}
	minor, err := proto.Negotiate(hello.MinMinor, hello.MaxMinor)
	if err != nil {
		m.NodeConnect(metrics.NodeIncompatible)
		_ = h.writeNow(ctx, s, proto.TypeError, proto.Error{Code: "incompatible", Message: err.Error()})
		return err
	}
	s.minor = minor
	caps, _ := json.Marshal(hello.Capabilities)
	s.inHello = make(map[string]bool, len(hello.Runs))
	for _, rep := range hello.Runs {
		s.inHello[rep.RunID] = true
	}
	h.mu.Lock()
	h.seq++
	s.seq = h.seq
	h.mu.Unlock()
	// From here on this connection's end records the node's disconnect, unless a newer connection has begun by then.
	defer h.onDisconnect(ctx, s)
	if err := svc.s.W().TxNamed(ctx, "fleet.node_connect", func(tx *store.Tx) error {
		// Write transactions are serialized, so this and onDisconnect's check agree on which connection is the newest.
		h.mu.Lock()
		h.newest[s.nodeID] = s.seq
		h.mu.Unlock()
		if err := tx.Q.RecordNodeConnect(ctx, db.RecordNodeConnectParams{AgentVersion: truncate(hello.AgentVersion, 64), ProtocolMinor: int64(minor),
			CapabilitiesJson: string(caps), Capacity: int64(hello.Capacity), Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, LastIp: ip, ID: s.nodeID}); err != nil {
			return err
		}
		// The stale-run sweeper only ever considers the runs that were live when the node connected (ADR-0074).
		live, err := tx.Q.ListLiveRunVersionsByNode(ctx, s.nodeID)
		if err != nil {
			return err
		}
		s.liveAtConnect = make(map[string]int64, len(live))
		for _, r := range live {
			s.liveAtConnect[r.ID] = r.Version
		}
		return nil
	}); err != nil {
		return err
	}
	s.minor = minor
	s.connectedAt = svc.opts.Clock.Now()
	h.mu.Lock()
	old := h.sessions[s.nodeID]
	h.sessions[s.nodeID] = s
	h.mu.Unlock()
	if old != nil {
		m.NodeConnect(metrics.NodeDuplicate)
		old.close(websocket.StatusPolicyViolation, "replaced by a newer connection")
	}
	m.NodeConnect(metrics.NodeAccepted)
	m.NodeConnected(1)
	s.counted = true

	go h.writer(ctx, s)
	if err := s.send(proto.TypeWelcome, svc.now(), proto.Welcome{NodeID: s.nodeID, Minor: minor, ServerTime: svc.now(),
		HeartbeatInterval: svc.opts.HeartbeatInterval, LeaseTTL: svc.opts.LeaseTTL, DisconnectGrace: svc.opts.DisconnectGrace}); err != nil {
		return err
	}
	// Reconcile before anything new is sent: the node's observations update central first (SM-NODE-009).
	applied := true
	for _, rep := range hello.Runs {
		if _, err := svc.applyReport(ctx, s.nodeID, rep); err != nil {
			applied = false
			s.log.Warn("apply hello report", obs.Err(err))
		}
	}
	h.mu.Lock()
	s.helloApplied = applied // false: the stale-run sweeper leaves this connection's runs alone (ADR-0074)
	h.mu.Unlock()
	// The leases the node claims are checked at once: one it no longer holds (for example after a relocation) is
	// revoked before any command is delivered.
	if err := h.syncLeases(ctx, s, hello.Leases); err != nil {
		return err
	}
	if err := h.sendReconcile(ctx, s); err != nil {
		return err
	}
	// The stale-run sweep's due time depends on which nodes are connected and since when (ADR-0074).
	svc.wakeDeadlines()
	// The node learns which of its terminals are taken over before any command (it refuses run.input for them).
	if s.minor >= proto.MinorTakeover {
		if err := h.sendTakeovers(ctx, s); err != nil {
			return err
		}
	}
	// And at which size to hold its runs' terminals (ADR-0075).
	if s.minor >= proto.MinorTerminalSize {
		if err := h.sendTerminalSizes(ctx, s); err != nil {
			return err
		}
	}
	go h.dispatchLoop(ctx, s)
	go h.credentialLoop(ctx, s)
	// Relocations waiting for this node resume now (for example after a central restart).
	svc.kickNodeRelocations(ctx, s.nodeID)
	if h.OnConnected != nil {
		go h.OnConnected(context.WithoutCancel(ctx), s.nodeID)
	}
	s.kick <- struct{}{}

	for {
		env, err := h.read(ctx, s)
		if err != nil {
			return err
		}
		if err := h.handle(ctx, s, env); err != nil {
			s.log.Warn("node message", slog.String("type", env.Type), obs.Err(err))
		}
	}
}

func (h *Hub) read(ctx context.Context, s *session) (proto.Envelope, error) {
	typ, data, err := s.conn.Read(ctx)
	if err != nil {
		return proto.Envelope{}, err
	}
	if typ != websocket.MessageText {
		return proto.Envelope{}, errors.New("binary messages are not part of the protocol")
	}
	return proto.Decode(data)
}

func (h *Hub) writeNow(ctx context.Context, s *session, typ string, body any) error {
	b, err := proto.Encode(ids.New(ids.Message), typ, h.svc.now(), body)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.conn.Write(wctx, websocket.MessageText, b)
}

func (h *Hub) writer(ctx context.Context, s *session) {
	for {
		select {
		case <-s.done:
			return
		case b := <-s.out:
			wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := s.conn.Write(wctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				s.close(websocket.StatusGoingAway, "write failed")
				return
			}
		}
	}
}

// toolCall authenticates the run itself (the node's word is not enough), checks that the run belongs to this node, and
// runs the tool with a deadline. Internal errors are not described to the agent.
func (h *Hub) toolCall(ctx context.Context, s *session, call proto.ToolCall) {
	ctx, cancel := context.WithTimeout(ctx, toolCallTimeout)
	defer cancel()
	reply := func(res proto.ToolResult) {
		res.CallID = call.CallID
		_ = s.send(proto.TypeToolResult, h.svc.now(), res)
	}
	p, err := h.svc.AuthenticateRun(ctx, call.RunToken)
	if err != nil || p.RunID != call.RunID {
		reply(proto.ToolResult{Error: "the run credential is not valid (the run ended or was relaunched)", Code: string(apperr.CodeUnauthorized)})
		return
	}
	if r, err := h.svc.s.R().GetRun(ctx, p.RunID); err != nil || r.NodeID != s.nodeID {
		reply(proto.ToolResult{Error: "the run is not assigned to this node", Code: string(apperr.CodeForbidden)})
		return
	}
	if h.Tools == nil {
		reply(proto.ToolResult{Error: "agent tools are not available", Code: string(apperr.CodeUnavailable)})
		return
	}
	out, err := h.Tools(ctx, p, call.Tool, call.Args)
	if err != nil {
		code := apperr.CodeOf(err)
		msg := err.Error()
		if code == "" {
			s.log.Warn("agent tool call failed", slog.String("tool", call.Tool), slog.String(obs.FieldRunID, p.RunID), obs.Err(err))
			code, msg = "internal", "internal error"
		}
		reply(proto.ToolResult{Error: msg, Code: string(code)})
		return
	}
	reply(proto.ToolResult{OK: true, Result: out})
}

func (h *Hub) handle(ctx context.Context, s *session, env proto.Envelope) error {
	svc := h.svc
	switch env.Type {
	case proto.TypeHeartbeat:
		var hb proto.Heartbeat
		if err := env.Decode(&hb); err != nil {
			return err
		}
		_ = svc.s.W().TxNamed(ctx, "fleet.node_seen", func(tx *store.Tx) error {
			return tx.Q.RecordNodeSeen(ctx, db.RecordNodeSeenParams{LastSeenAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, ID: s.nodeID})
		})
		return h.syncLeases(ctx, s, hb.Leases)
	case proto.TypeAck:
		var a proto.Ack
		if err := env.Decode(&a); err != nil {
			return err
		}
		switch a.Result {
		case proto.AckAccepted, proto.AckDuplicate, proto.AckRejected:
		default:
			return apperr.New(apperr.CodeInvalid, "unknown ack result %q", a.Result)
		}
		cmd, _ := svc.s.R().GetNodeCommand(ctx, a.CommandID)
		svc.opts.Metrics.NodeCommand(metrics.CommandType(cmd.Kind), metrics.CommandResult(a.Result))
		if proto.IsRelocation(cmd.Kind) && a.Result == proto.AckRejected && cmd.NodeID == s.nodeID {
			go svc.relocationRejected(context.WithoutCancel(ctx), a.CommandID, truncate(a.Reason, 300))
		}
		return svc.s.W().TxNamed(ctx, "fleet.ack", func(tx *store.Tx) error {
			n, err := tx.Q.AckCommand(ctx, db.AckCommandParams{State: a.Result, Reason: truncate(a.Reason, 500), AckedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
				ID: a.CommandID, NodeID: s.nodeID})
			if err != nil || n == 0 || svc.opts.OnCommandAck == nil {
				return err
			}
			return svc.opts.OnCommandAck(ctx, tx, a.CommandID, cmd.Kind, a.Result, a.Reason)
		})
	case proto.TypeRunReport:
		var rep proto.RunReport
		if err := env.Decode(&rep); err != nil {
			return err
		}
		fenced, err := svc.applyReport(ctx, s.nodeID, rep)
		if err == nil && fenced && !ended(rep.State) {
			// A run of an older generation (or unknown to central) is still running on the node: stop it.
			_ = s.send(proto.TypeCommand, svc.now(), proto.Command{CommandID: ids.New(ids.Command), Kind: proto.CmdStop, SpaceID: rep.SpaceID,
				RunID: rep.RunID, Generation: rep.Generation, IdempotencyKey: "fence:" + rep.RunID,
				Payload: mustJSON(proto.StopPayload{Mode: "kill", Reason: "fenced: stale lease generation or unknown run"})})
		}
		return err
	case proto.TypeMemorySync:
		var m proto.MemorySync
		if err := env.Decode(&m); err != nil {
			return err
		}
		go func() {
			ack := proto.MemoryAck{SyncID: m.SyncID, SpaceID: m.SpaceID, Role: m.Role, Result: proto.MemoryError, Message: "central does not store memory"}
			if h.Memory != nil {
				select {
				case s.memory <- struct{}{}:
				case <-ctx.Done():
					return
				}
				ack = h.Memory.Apply(ctx, s.nodeID, m)
				<-s.memory
			}
			_ = s.send(proto.TypeMemoryAck, svc.now(), ack)
		}()
		return nil
	case proto.TypeUsage:
		var rep proto.UsageReport
		if err := env.Decode(&rep); err != nil {
			return err
		}
		if h.Usage != nil {
			go h.Usage.Report(context.WithoutCancel(ctx), s.nodeID, rep)
		}
		return nil
	case proto.TypeTranscript:
		var ch proto.TranscriptChunk
		if err := env.Decode(&ch); err != nil {
			return err
		}
		go func() {
			ack := proto.TranscriptAck{RunID: ch.RunID, Error: "central does not store transcripts"}
			if h.Usage != nil {
				ack = h.Usage.Chunk(context.WithoutCancel(ctx), s.nodeID, ch)
			}
			_ = s.send(proto.TypeTranscriptAck, svc.now(), ack)
		}()
		return nil
	case proto.TypeKBResult:
		var res proto.KBResult
		if err := env.Decode(&res); err != nil {
			return err
		}
		if h.KBResult != nil {
			go h.KBResult(context.WithoutCancel(ctx), s.nodeID, res)
		}
		return nil
	case proto.TypeRepoStatus:
		var rep proto.RepoStatusReport
		if err := env.Decode(&rep); err != nil {
			return err
		}
		if h.RepoStatus != nil {
			go h.RepoStatus(context.WithoutCancel(ctx), s.nodeID, rep)
		}
		return nil
	case proto.TypeMemoryStatus:
		var st proto.MemoryStatus
		if err := env.Decode(&st); err != nil {
			return err
		}
		if h.Memory != nil {
			go h.Memory.Report(context.WithoutCancel(ctx), s.nodeID, st)
		}
		return nil
	case proto.TypeToolCall:
		var call proto.ToolCall
		if err := env.Decode(&call); err != nil {
			return err
		}
		select {
		case s.tools <- struct{}{}:
			go func() {
				defer func() { <-s.tools }()
				h.toolCall(ctx, s, call)
			}()
		default:
			_ = s.send(proto.TypeToolResult, svc.now(), proto.ToolResult{CallID: call.CallID, Error: "too many concurrent tool calls from this node; retry",
				Code: string(apperr.CodeRateLimited)})
		}
		return nil
	case proto.TypeRelocationReport:
		var rep proto.RelocationReport
		if err := env.Decode(&rep); err != nil {
			return err
		}
		go svc.relocationReport(context.WithoutCancel(ctx), s.nodeID, rep)
		return nil
	case proto.TypeRelocationUpload:
		var up proto.RelocationUpload
		if err := env.Decode(&up); err != nil {
			return err
		}
		go func() {
			_ = s.send(proto.TypeRelocationUploadAck, svc.now(), svc.relocationUpload(context.WithoutCancel(ctx), s.nodeID, up))
		}()
		return nil
	case proto.TypeMemoryFlushed:
		var res proto.MemoryFlushed
		if err := env.Decode(&res); err != nil {
			return err
		}
		h.flushed(s.nodeID, res)
		return nil
	case proto.TypeRelocationFetch:
		var f proto.RelocationFetch
		if err := env.Decode(&f); err != nil {
			return err
		}
		go func() {
			_ = s.send(proto.TypeRelocationData, svc.now(), svc.relocationFetch(context.WithoutCancel(ctx), s.nodeID, f))
		}()
		return nil
	case proto.TypeTakeoverEnded:
		var m proto.TakeoverEnded
		if err := env.Decode(&m); err != nil {
			return err
		}
		return svc.nodeEndedTakeover(ctx, s.nodeID, m)
	case proto.TypeRecording:
		var c proto.RecordingChunk
		if err := env.Decode(&c); err != nil {
			return err
		}
		r, err := svc.s.R().GetRun(ctx, c.RunID)
		if err != nil || r.NodeID != s.nodeID || r.SpaceID != c.SpaceID {
			return apperr.New(apperr.CodeForbidden, "recording for a run not assigned to this node")
		}
		if svc.opts.RecordingsDir == "" {
			return nil
		}
		seq, err := svc.rec.apply(c)
		_ = s.send(proto.TypeRecordingAck, svc.now(), proto.RecordingAck{RunID: c.RunID, Seq: seq})
		return err
	default:
		return apperr.New(apperr.CodeInvalid, "unexpected message type %q", env.Type)
	}
}

// syncLeases grants or renews the lease of every space pinned to the node that may run (not archived, not relocating
// away), and revokes leases the node claims but no longer holds.
func (h *Hub) syncLeases(ctx context.Context, s *session, claimed []proto.LeaseState) error {
	svc := h.svc
	var grants []proto.LeaseGrant
	var revokes []proto.LeaseRevoke
	err := svc.s.W().TxNamed(ctx, "fleet.sync_leases", func(tx *store.Tx) error {
		grants, revokes = nil, nil
		spaces, err := tx.Q.ListSpacesByNode(ctx, s.nodeID)
		if err != nil {
			return err
		}
		held := map[string]int64{}
		for _, sp := range spaces {
			if sp.State == "archived" || sp.DesiredNodeID.Valid {
				n, err := tx.Q.RevokeLease(ctx, db.RevokeLeaseParams{Now: tx.NowMicros(), SpaceID: sp.ID})
				if err != nil {
					return err
				}
				if n > 0 {
					if err := svc.record(ctx, tx, sp.ID, "space", sp.ID, "updated", sp.Version); err != nil {
						return err
					}
				}
				continue
			}
			g, err := svc.leaseFor(ctx, tx, sp)
			if err != nil {
				return err
			}
			grants = append(grants, g)
			held[sp.ID] = g.Generation
		}
		// Only a claimed lease for a space this node no longer holds is revoked (for example after a relocation). A held
		// space with another generation needs no revoke: its grant above supersedes the claimed generation, and an
		// "interrupt" revoke sent after it would stop runs that are allowed to continue.
		for _, c := range claimed {
			if _, ok := held[c.SpaceID]; !ok {
				revokes = append(revokes, proto.LeaseRevoke{SpaceID: c.SpaceID, Generation: c.Generation, Reason: "lease no longer held", Mode: "interrupt"})
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, g := range grants {
		if err := s.send(proto.TypeLeaseGrant, svc.now(), g); err != nil {
			return err
		}
	}
	for _, r := range revokes {
		if err := s.send(proto.TypeLeaseRevoke, svc.now(), r); err != nil {
			return err
		}
	}
	return nil
}

func (h *Hub) sendReconcile(ctx context.Context, s *session) error {
	runs, err := h.svc.s.R().ListActiveRunsByNode(ctx, s.nodeID)
	if err != nil {
		return err
	}
	rec := proto.Reconcile{Runs: []proto.DesiredRun{}}
	for _, r := range runs {
		rec.Runs = append(rec.Runs, proto.DesiredRun{RunID: r.ID, SpaceID: r.SpaceID, Generation: r.Generation, Desired: r.Desired})
	}
	return s.send(proto.TypeReconcile, h.svc.now(), rec)
}

// credentialLoop re-checks the connection's node credential and the node's state every CredentialRecheck and closes
// the connection once either is no longer valid (SM-AUTH-007, ADR-0033). Disabling, revoking and re-enrolling a node
// close the connection at once; this is the backstop for credential expiry and changes made outside this process.
// Without central the node stops its runs after the lease and disconnect grace.
func (h *Hub) credentialLoop(ctx context.Context, s *session) {
	t := time.NewTicker(h.svc.opts.CredentialRecheck)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			if !h.svc.nodeCredentialValid(ctx, s.nodeID, s.credID) {
				s.log.Info("node credential no longer valid; closing the connection")
				s.close(websocket.StatusPolicyViolation, "node credential no longer valid")
				return
			}
		}
	}
}

// dispatchRetryFirst and dispatchRetryMax bound the backoff after a pass that left a command undelivered for a
// transient reason (a failed read, secrets that could not be resolved): the pass is repeated on its own instead of
// waiting for the next kick, which may never come.
var dispatchRetryFirst, dispatchRetryMax = 250 * time.Millisecond, 30 * time.Second

// dispatchLoop delivers undelivered commands whenever kicked. Delivery is at-least-once: a command stays "sent"
// until acknowledged and is re-sent on the next connection; the node acknowledges repeats as duplicates.
func (h *Hub) dispatchLoop(ctx context.Context, s *session) {
	var again <-chan time.Time
	backoff := time.Duration(0)
	for {
		select {
		case <-s.done:
			return
		case <-s.kick:
		case <-again:
		}
		retry, ok := h.dispatch(ctx, s)
		if !ok {
			return
		}
		again = nil
		if retry {
			backoff = min(max(2*backoff, dispatchRetryFirst), dispatchRetryMax)
			again = time.After(backoff)
		} else {
			backoff = 0
		}
	}
}

// dispatch is one pass of dispatchLoop. retry reports a command left undelivered for a transient reason; ok is false
// once the connection is gone.
func (h *Hub) dispatch(ctx context.Context, s *session) (retry, ok bool) {
	if err := h.syncLeases(ctx, s, nil); err != nil {
		s.log.Warn("lease sync", obs.Err(err))
	}
	cmds, err := h.svc.s.R().ListUndeliveredCommands(ctx, s.nodeID)
	if err != nil {
		s.log.Warn("list commands", obs.Err(err))
		return true, true
	}
	for _, c := range cmds {
		msg, err := h.materialize(ctx, c, s.nodeID, s.minor)
		if endReason := undispatchable(err); endReason != "" && c.RunID.Valid {
			if ferr := h.svc.failUndispatched(ctx, c.RunID.String, endReason, err.Error()); ferr != nil {
				s.log.Warn("fail undispatched run", slog.String(obs.FieldCommandID, c.ID), obs.Err(ferr))
				retry = true
			}
			continue
		}
		if errors.Is(err, errMintPending) {
			continue // the mint kicks this loop when it is done
		}
		if err != nil {
			s.log.Warn("materialize command", slog.String(obs.FieldCommandID, c.ID), obs.Err(err))
			retry = true
			continue
		}
		if err := s.send(proto.TypeCommand, h.svc.now(), msg); err != nil {
			return false, false
		}
		_ = h.svc.s.W().TxNamed(ctx, "fleet.command_sent", func(tx *store.Tx) error {
			return tx.Q.MarkCommandSent(ctx, db.MarkCommandSentParams{SentAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, ID: c.ID})
		})
	}
	return retry, true
}

// undispatchable returns the end reason for a materialize error that fails the run instead of retrying it, or "".
func undispatchable(err error) string {
	switch {
	case errors.Is(err, ErrCredentialsUnavailable):
		return "account_unavailable"
	case errors.Is(err, ErrNodeTooOld):
		return "node_too_old"
	case errors.Is(err, ErrGatewayUnavailable):
		return "gateway_unavailable"
	case errors.Is(err, ErrAgentTooOld):
		return "agent_too_old"
	case errors.Is(err, errBadPayload):
		return "invalid_command"
	}
	return ""
}

// materialize turns a stored command into the wire message, attaching the run token and secrets to launches and
// repository SSH keys to clones, fetches and relocation clones (minor ≥ 7, ADR-0057). Stored payloads never hold any of them.
func (h *Hub) materialize(ctx context.Context, c db.NodeCommand, nodeID string, minor int) (proto.Command, error) {
	msg := proto.Command{CommandID: c.ID, Kind: c.Kind, SpaceID: c.SpaceID, RunID: c.RunID.String, Generation: c.Generation,
		IdempotencyKey: c.IdempotencyKey, Payload: json.RawMessage(c.PayloadJson)}
	keys := h.RepoKey != nil && minor >= proto.MinorRepoKeys
	if c.Kind == proto.CmdReposSync && keys {
		var p proto.ReposSyncPayload
		if err := json.Unmarshal([]byte(c.PayloadJson), &p); err != nil {
			return msg, fmt.Errorf("%w: %w", errBadPayload, err)
		}
		for i := range p.Repositories {
			k, err := h.RepoKey(ctx, c.SpaceID, p.Repositories[i].ID)
			if err != nil {
				return msg, err
			}
			p.Repositories[i].SSHKey = k
		}
		msg.Payload = mustJSON(p)
		return msg, nil
	}
	if c.Kind == proto.CmdRelocatePrepare && keys {
		var p proto.RelocatePreparePayload
		if err := json.Unmarshal([]byte(c.PayloadJson), &p); err != nil {
			return msg, fmt.Errorf("%w: %w", errBadPayload, err)
		}
		keyed := false
		for i := range p.Repos {
			if p.Repos[i].Source == nil {
				continue
			}
			k, err := h.RepoKey(ctx, c.SpaceID, p.Repos[i].Source.ID)
			if err != nil {
				return msg, err
			}
			p.Repos[i].Source.SSHKey, keyed = k, keyed || k != ""
		}
		if keyed {
			msg.Payload = mustJSON(p)
		}
		return msg, nil
	}
	if c.Kind != proto.CmdLaunch {
		return msg, nil
	}
	var p proto.LaunchPayload
	if err := json.Unmarshal([]byte(c.PayloadJson), &p); err != nil {
		return msg, fmt.Errorf("%w: %w", errBadPayload, err)
	}
	// An older agent would ignore the settings and run without the rules they add, deny rules included.
	if len(p.ClaudeSettings) > 0 && minor < proto.MinorClaudeSettings {
		return msg, fmt.Errorf("%w: the role's harness profile has Claude Code settings, which need protocol minor %d; the node negotiated %d: upgrade its agent",
			ErrNodeTooOld, proto.MinorClaudeSettings, minor)
	}
	// An older agent would ignore the pin and run whatever version it has (ADR-0077).
	if p.HarnessVersion != "" && minor < proto.MinorHarnessVersion {
		name := nodeID
		if h.svc != nil {
			if n, err := h.svc.s.R().GetNode(ctx, nodeID); err == nil && n.Name != "" {
				name = n.Name
			}
		}
		return msg, fmt.Errorf("%w: node %s doesn't support pinned Claude Code versions; upgrade it (the role's harness profile pins %s, which needs protocol minor %d; the node negotiated %d)",
			ErrNodeTooOld, name, p.HarnessVersion, proto.MinorHarnessVersion, minor)
	}
	if p.Repository != nil && keys {
		k, err := h.RepoKey(ctx, c.SpaceID, p.Repository.ID)
		if err != nil {
			return msg, err
		}
		p.Repository.SSHKey = k
	}
	tok, err := h.svc.runToken(ctx, c.RunID.String)
	if err != nil {
		return msg, err
	}
	p.RunToken = tok
	if h.Secrets != nil || h.svc.opts.Gateway != nil {
		run, err := h.svc.GetRun(ctx, c.RunID.String)
		if err != nil {
			return msg, err
		}
		switch {
		case run.Launch.Gateway != nil:
			// A gateway run gets its own child token and never an account's secret (ADR-0060 decision 4, Q1).
			if h.svc.opts.Gateway == nil {
				return msg, fmt.Errorf("%w: this central has no gateway configured", ErrGatewayUnavailable)
			}
			// An older agent would drop the token and start the harness on the node user's own login: refused before
			// anything is minted.
			if minor < proto.MinorGatewayToken {
				return msg, fmt.Errorf("%w: gateway runs need protocol minor %d, which delivers the run's token to the harness; the node negotiated %d: "+
					"upgrade its agent", ErrAgentTooOld, proto.MinorGatewayToken, minor)
			}
			// A non-Claude run (or one that resumes nothing) binds the run ID as its session.
			session := p.SessionID
			if session == "" {
				session = run.ID
			}
			if p.Secrets, err = h.gatewayToken(ctx, nodeID, run, session); err != nil {
				return msg, err
			}
			if p.Gateway, err = h.svc.opts.Gateway.Address(ctx); err != nil {
				return msg, err
			}
		case h.Secrets != nil:
			if p.Secrets, err = h.Secrets(ctx, run); err != nil {
				return msg, err
			}
		}
	}
	msg.Payload = mustJSON(p)
	return msg, nil
}

// onDisconnect unregisters a connection that ended and records the node's disconnect. A connection that a newer one
// replaced (or that ended while a newer one was starting) records nothing: the node is connected again, and marking its
// runs unknown or recording a disconnect would overwrite what the newer connection established. The check runs in the
// write transaction, which fleet.node_connect also claims the node in, so a disconnect and a reconnect cannot interleave.
func (h *Hub) onDisconnect(ctx context.Context, s *session) {
	h.mu.Lock()
	if h.sessions[s.nodeID] == s {
		delete(h.sessions, s.nodeID)
	}
	h.mu.Unlock()
	if s.counted {
		h.svc.opts.Metrics.NodeConnected(-1)
	}
	defer h.svc.wakeDeadlines()
	_ = h.svc.s.W().TxNamed(ctx, "fleet.node_disconnect", func(tx *store.Tx) error {
		h.mu.Lock()
		newest := h.newest[s.nodeID] == s.seq
		h.mu.Unlock()
		if !newest {
			return nil // replaced: the newer connection owns the node's state
		}
		if err := tx.Q.RecordNodeDisconnect(ctx, db.RecordNodeDisconnectParams{LastDisconnectAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
			UpdatedAt: tx.NowMicros(), ID: s.nodeID}); err != nil {
			return err
		}
		// Execution on a disconnected node is unknown, not failed (SM-NODE-005).
		_, err := tx.Q.MarkNodeRunsUnknown(ctx, db.MarkNodeRunsUnknownParams{Now: tx.NowMicros(), NodeID: s.nodeID})
		return err
	})
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
