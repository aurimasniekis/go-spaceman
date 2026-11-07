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
}

func newHub(svc *Service) *Hub { return &Hub{svc: svc, sessions: map[string]*session{}} }

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
	if err := svc.s.W().TxNamed(ctx, "fleet.node_connect", func(tx *store.Tx) error {
		return tx.Q.RecordNodeConnect(ctx, db.RecordNodeConnectParams{AgentVersion: truncate(hello.AgentVersion, 64), ProtocolMinor: int64(minor),
			CapabilitiesJson: string(caps), Capacity: int64(hello.Capacity), Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, LastIp: ip, ID: s.nodeID})
	}); err != nil {
		return err
	}
	s.minor = minor
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
	defer h.onDisconnect(ctx, s)

	go h.writer(ctx, s)
	if err := s.send(proto.TypeWelcome, svc.now(), proto.Welcome{NodeID: s.nodeID, Minor: minor, ServerTime: svc.now(),
		HeartbeatInterval: svc.opts.HeartbeatInterval, LeaseTTL: svc.opts.LeaseTTL, DisconnectGrace: svc.opts.DisconnectGrace}); err != nil {
		return err
	}
	// Reconcile before anything new is sent: the node's observations update central first (SM-NODE-009).
	for _, rep := range hello.Runs {
		if _, err := svc.applyReport(ctx, s.nodeID, rep); err != nil {
			s.log.Warn("apply hello report", obs.Err(err))
		}
	}
	// The leases the node claims are checked at once: one it no longer holds (for example after a relocation) is
	// revoked before any command is delivered.
	if err := h.syncLeases(ctx, s, hello.Leases); err != nil {
		return err
	}
	if err := h.sendReconcile(ctx, s); err != nil {
		return err
	}
	// The node learns which of its terminals are taken over before any command (it refuses run.input for them).
	if s.minor >= proto.MinorTakeover {
		if err := h.sendTakeovers(ctx, s); err != nil {
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

// dispatchLoop delivers undelivered commands whenever kicked. Delivery is at-least-once: a command stays "sent"
// until acknowledged and is re-sent on the next connection; the node acknowledges repeats as duplicates.
func (h *Hub) dispatchLoop(ctx context.Context, s *session) {
	for {
		select {
		case <-s.done:
			return
		case <-s.kick:
		}
		if err := h.syncLeases(ctx, s, nil); err != nil {
			s.log.Warn("lease sync", obs.Err(err))
		}
		cmds, err := h.svc.s.R().ListUndeliveredCommands(ctx, s.nodeID)
		if err != nil {
			s.log.Warn("list commands", obs.Err(err))
			continue
		}
		for _, c := range cmds {
			msg, err := h.materialize(ctx, c, s.minor)
			if endReason := undispatchable(err); endReason != "" && c.RunID.Valid {
				if ferr := h.svc.failUndispatched(ctx, c.RunID.String, endReason, err.Error()); ferr != nil {
					s.log.Warn("fail undispatched run", slog.String(obs.FieldCommandID, c.ID), obs.Err(ferr))
				}
				continue
			}
			if err != nil {
				s.log.Warn("materialize command", slog.String(obs.FieldCommandID, c.ID), obs.Err(err))
				continue
			}
			if err := s.send(proto.TypeCommand, h.svc.now(), msg); err != nil {
				return
			}
			_ = h.svc.s.W().TxNamed(ctx, "fleet.command_sent", func(tx *store.Tx) error {
				return tx.Q.MarkCommandSent(ctx, db.MarkCommandSentParams{SentAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, ID: c.ID})
			})
		}
	}
}

// undispatchable returns the end reason for a materialize error that fails the run instead of retrying it, or "".
func undispatchable(err error) string {
	switch {
	case errors.Is(err, ErrCredentialsUnavailable):
		return "account_unavailable"
	case errors.Is(err, ErrNodeTooOld):
		return "node_too_old"
	}
	return ""
}

// materialize turns a stored command into the wire message, attaching the run token and secrets to launches and
// repository SSH keys to clones and fetches (minor ≥ 7, ADR-0057). Stored payloads never hold any of them.
func (h *Hub) materialize(ctx context.Context, c db.NodeCommand, minor int) (proto.Command, error) {
	msg := proto.Command{CommandID: c.ID, Kind: c.Kind, SpaceID: c.SpaceID, RunID: c.RunID.String, Generation: c.Generation,
		IdempotencyKey: c.IdempotencyKey, Payload: json.RawMessage(c.PayloadJson)}
	keys := h.RepoKey != nil && minor >= proto.MinorRepoKeys
	if c.Kind == proto.CmdReposSync && keys {
		var p proto.ReposSyncPayload
		if err := json.Unmarshal([]byte(c.PayloadJson), &p); err != nil {
			return msg, err
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
	if c.Kind != proto.CmdLaunch {
		return msg, nil
	}
	var p proto.LaunchPayload
	if err := json.Unmarshal([]byte(c.PayloadJson), &p); err != nil {
		return msg, err
	}
	// An older agent would ignore the settings and run without the rules they add, deny rules included.
	if len(p.ClaudeSettings) > 0 && minor < proto.MinorClaudeSettings {
		return msg, fmt.Errorf("%w: the role's harness profile has Claude Code settings, which need protocol minor %d; the node negotiated %d: upgrade its agent",
			ErrNodeTooOld, proto.MinorClaudeSettings, minor)
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
	if h.Secrets != nil {
		run, err := h.svc.GetRun(ctx, c.RunID.String)
		if err != nil {
			return msg, err
		}
		if p.Secrets, err = h.Secrets(ctx, run); err != nil {
			return msg, err
		}
	}
	msg.Payload = mustJSON(p)
	return msg, nil
}

func (h *Hub) onDisconnect(ctx context.Context, s *session) {
	h.mu.Lock()
	if h.sessions[s.nodeID] == s {
		delete(h.sessions, s.nodeID)
	}
	h.mu.Unlock()
	h.svc.opts.Metrics.NodeConnected(-1)
	_ = h.svc.s.W().TxNamed(ctx, "fleet.node_disconnect", func(tx *store.Tx) error {
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
