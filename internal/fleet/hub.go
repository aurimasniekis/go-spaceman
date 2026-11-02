package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

// Hub holds the live node connections. Each connection has one reader loop and one writer goroutine; work for a node
// is triggered by Kick (after a commit) rather than by polling (SM-ARCH-004).
type Hub struct {
	svc *Service
	mu  sync.Mutex
	// sessions maps node ID to its live connection (a newer connection replaces an older one).
	sessions map[string]*session
	// Secrets attaches run secrets to launches (nil: none).
	Secrets SecretsResolver
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
	s := &session{nodeID: node.ID, credID: credID, conn: conn, out: make(chan []byte, 256), kick: make(chan struct{}, 1), done: make(chan struct{}),
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
	caps, _ := json.Marshal(hello.Capabilities)
	if err := svc.s.W().TxNamed(ctx, "fleet.node_connect", func(tx *store.Tx) error {
		return tx.Q.RecordNodeConnect(ctx, db.RecordNodeConnectParams{AgentVersion: truncate(hello.AgentVersion, 64), ProtocolMinor: int64(minor),
			CapabilitiesJson: string(caps), Capacity: int64(hello.Capacity), Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, LastIp: ip, ID: s.nodeID})
	}); err != nil {
		return err
	}
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
	if err := h.syncLeases(ctx, s, nil); err != nil {
		return err
	}
	if err := h.sendReconcile(ctx, s); err != nil {
		return err
	}
	go h.dispatchLoop(ctx, s)
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
		return svc.s.W().TxNamed(ctx, "fleet.ack", func(tx *store.Tx) error {
			_, err := tx.Q.AckCommand(ctx, db.AckCommandParams{State: a.Result, Reason: truncate(a.Reason, 500), AckedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
				ID: a.CommandID, NodeID: s.nodeID})
			return err
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
		for _, c := range claimed {
			if gen, ok := held[c.SpaceID]; !ok || gen != c.Generation {
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
			msg, err := h.materialize(ctx, c)
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

// materialize turns a stored command into the wire message, attaching the run token and secrets to launches.
func (h *Hub) materialize(ctx context.Context, c db.NodeCommand) (proto.Command, error) {
	msg := proto.Command{CommandID: c.ID, Kind: c.Kind, SpaceID: c.SpaceID, RunID: c.RunID.String, Generation: c.Generation,
		IdempotencyKey: c.IdempotencyKey, Payload: json.RawMessage(c.PayloadJson)}
	if c.Kind != proto.CmdLaunch {
		return msg, nil
	}
	var p proto.LaunchPayload
	if err := json.Unmarshal([]byte(c.PayloadJson), &p); err != nil {
		return msg, err
	}
	tok, err := h.svc.issueRunToken(ctx, c.RunID.String)
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
