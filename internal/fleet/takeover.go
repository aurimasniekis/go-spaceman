package fleet

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Terminal takeover (SM-UI-004, ADR-0046). An operator holding terminal:takeover takes over a running run's terminal:
// the node attaches interactively (herdr-core terminal.attach without observe, verified in S-3 and S-5) and types the
// operator's keystrokes, and every automated input to the terminal is refused until the takeover ends. All automated
// input reaches a terminal as a run.input command (operator "Message the agent", terminal notices for messages and
// human responses), so refusing it here (SendInput, QueueNotice) and on the node (run.input) pauses it completely.
//
// A takeover lasts while its holder renews it: it ends on release, on expiry (the holder's page closed, or their
// session or token was revoked, so renewals fail), when the run ends, or when the node cannot hold it. The state is
// durable, so a takeover survives a central restart until its expiry. Keystrokes are never stored, journaled, logged or
// audited; the audit records who took over which terminal, when, how it ended and how many bytes were typed.

// TakeoverTTL is how long a takeover lasts without a renewal. The web UI renews it every 10 s. Tests shorten it before
// central starts.
var TakeoverTTL = 30 * time.Second

// Takeover input limits (per takeover): messages and bytes per second.
const (
	takeoverMsgsPerSec  = 50
	takeoverBytesPerSec = 64 << 10
)

// Why a takeover ended.
const (
	TakeoverReleased  = "released"    // the holder released it
	TakeoverEndedBy   = "ended"       // another operator with terminal:takeover ended it
	TakeoverExpired   = "expired"     // no renewal within TakeoverTTL
	TakeoverRunEnded  = "run_ended"   // the run ended
	TakeoverNodeEnded = "node_failed" // the node could not attach, or its attach closed
)

// Takeover is a terminal takeover of a run.
type Takeover struct {
	ID          string     `json:"id"`
	RunID       string     `json:"run_id"`
	Holder      string     `json:"holder" doc:"Who holds it: user:<id> or root"`
	HolderLabel string     `json:"holder_label,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	ExpiresAt   time.Time  `json:"expires_at" doc:"It ends unless the holder renews it before this"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	EndedBy     string     `json:"ended_by,omitempty"`
	EndReason   string     `json:"end_reason,omitempty" enum:",released,ended,expired,run_ended,node_failed"`
}

func takeoverFromRow(t db.RunTakeover) *Takeover {
	return &Takeover{ID: t.ID, RunID: t.RunID, Holder: t.Holder, HolderLabel: t.HolderLabel, StartedAt: time.UnixMicro(t.StartedAt).UTC(),
		ExpiresAt: time.UnixMicro(t.ExpiresAt).UTC(), EndedAt: microsPtr(t.EndedAt), EndedBy: t.EndedBy, EndReason: t.EndReason}
}

// ErrTakenOver refuses automated input to a taken-over terminal.
var ErrTakenOver = apperr.New(apperr.CodeConflict, "an operator has taken over this terminal; input is paused until the takeover ends")

// takeovers is the in-memory side of takeovers: input meters and the expiry timer's wake-up.
type takeovers struct {
	mu     sync.Mutex
	meters map[string]*takeoverMeter
	wake   chan struct{}
}

type takeoverMeter struct {
	window      time.Time
	msgs, bytes int
	total       int64
}

func (t *takeovers) kick() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// allow meters one input message; it returns false over the per-second limits.
func (t *takeovers) allow(id string, n int, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.meters[id]
	if m == nil {
		m = &takeoverMeter{window: now}
		t.meters[id] = m
	}
	if now.Sub(m.window) >= time.Second {
		m.window, m.msgs, m.bytes = now, 0, 0
	}
	if m.msgs+1 > takeoverMsgsPerSec || m.bytes+n > takeoverBytesPerSec {
		return false
	}
	m.msgs++
	m.bytes += n
	m.total += int64(n)
	return true
}

// done forgets a takeover's meter and returns the bytes typed through this central process (a restart resets it).
func (t *takeovers) done(id string) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := int64(0)
	if m := t.meters[id]; m != nil {
		n = m.total
	}
	delete(t.meters, id)
	return n
}

// activeTakeover returns a run's takeover while it is in force (not ended, not expired).
func activeTakeover(ctx context.Context, q *db.Queries, runID string, now time.Time) (db.RunTakeover, bool, error) {
	t, err := q.GetActiveTakeover(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return db.RunTakeover{}, false, nil
	} else if err != nil {
		return db.RunTakeover{}, false, err
	}
	return t, t.ExpiresAt > now.UnixMicro(), nil
}

// refuseIfTakenOver fails automated input while the run's terminal is taken over.
func refuseIfTakenOver(ctx context.Context, tx *store.Tx, runID string) error {
	_, on, err := activeTakeover(ctx, tx.Q, runID, tx.Now)
	if err != nil {
		return err
	}
	if on {
		return ErrTakenOver
	}
	return nil
}

// ActiveTakeover returns the takeover in force on a run, or nil.
func (svc *Service) ActiveTakeover(ctx context.Context, runID string) (*Takeover, error) {
	t, on, err := activeTakeover(ctx, svc.s.R(), runID, svc.now())
	if err != nil || !on {
		return nil, err
	}
	return takeoverFromRow(t), nil
}

// StartTakeover takes over a running run's terminal for the actor. It fails while another takeover is in force, when
// the run has no live terminal, and when the run's node is not connected or predates takeovers.
func (svc *Service) StartTakeover(ctx context.Context, actor audit.Actor, label, runID string) (*Takeover, error) {
	r, err := svc.s.R().GetRun(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errRunNotFound
	} else if err != nil {
		return nil, err
	}
	if err := takeoverRunnable(r); err != nil {
		return nil, err
	}
	switch minor, ok := svc.hub.minor(r.NodeID); {
	case !ok:
		return nil, apperr.New(apperr.CodeUnavailable, "the run's node is not connected")
	case minor < proto.MinorTakeover:
		return nil, apperr.New(apperr.CodeUnavailable, "the run's node agent is too old for terminal takeover (node protocol 1.%d needed); upgrade spaceman-agent", proto.MinorTakeover)
	}
	var out db.RunTakeover
	err = svc.s.W().TxNamed(ctx, "fleet.takeover_start", func(tx *store.Tx) error {
		r, err := tx.Q.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		if err := takeoverRunnable(r); err != nil {
			return err
		}
		cur, on, err := activeTakeover(ctx, tx.Q, runID, tx.Now)
		if err != nil {
			return err
		}
		if on {
			who := cur.HolderLabel
			if who == "" {
				who = cur.Holder
			}
			return apperr.New(apperr.CodeConflict, "the terminal is already taken over by %s", who)
		}
		if cur.ID != "" { // ended by expiry, not yet recorded
			if err := svc.endTakeoverTx(ctx, tx, r, cur, "system:takeovers", TakeoverExpired); err != nil {
				return err
			}
		}
		out = db.RunTakeover{ID: ids.New(ids.Takeover), RunID: r.ID, SpaceID: r.SpaceID, Holder: holderOf(actor), HolderLabel: truncate(label, 120),
			StartedAt: tx.NowMicros(), ExpiresAt: tx.Now.Add(TakeoverTTL).UnixMicro()}
		if err := tx.Q.InsertTakeover(ctx, db.InsertTakeoverParams{ID: out.ID, RunID: out.RunID, SpaceID: out.SpaceID, Holder: out.Holder,
			HolderLabel: out.HolderLabel, StartedAt: out.StartedAt, ExpiresAt: out.ExpiresAt}); err != nil {
			return err
		}
		if _, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "run.takeover_started", Target: audit.Target{Type: "run", ID: r.ID},
			Details: map[string]any{"space_id": r.SpaceID, "takeover_id": out.ID}}); err != nil {
			return err
		}
		tx.AfterCommit(func() {
			svc.tko.kick()
			svc.hub.pushTakeovers(context.WithoutCancel(ctx), r.NodeID)
		})
		return svc.record(ctx, tx, r.SpaceID, "run", r.ID, "updated", r.Version)
	})
	if err != nil {
		return nil, err
	}
	return takeoverFromRow(out), nil
}

func takeoverRunnable(r db.Run) error {
	if r.Desired != "running" || ended(r.Observed) || r.TerminalID == "" {
		return apperr.New(apperr.CodePrecondition, "only a running run with a live terminal can be taken over")
	}
	return nil
}

// holderTakeover returns the takeover a holder acts on: in force, of this run, held by the actor.
func (svc *Service) holderTakeover(ctx context.Context, q *db.Queries, actor audit.Actor, runID, takeoverID string, now time.Time) (db.RunTakeover, error) {
	t, err := q.GetTakeover(ctx, takeoverID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && t.RunID != runID) {
		return db.RunTakeover{}, apperr.New(apperr.CodeNotFound, "takeover not found")
	} else if err != nil {
		return db.RunTakeover{}, err
	}
	if t.EndedAt.Valid || t.ExpiresAt <= now.UnixMicro() {
		return db.RunTakeover{}, apperr.New(apperr.CodePrecondition, "the takeover has ended")
	}
	if t.Holder != holderOf(actor) {
		return db.RunTakeover{}, apperr.New(apperr.CodeForbidden, "only the operator who took over the terminal can type or renew")
	}
	return t, nil
}

// RenewTakeover extends the holder's takeover by TakeoverTTL.
func (svc *Service) RenewTakeover(ctx context.Context, actor audit.Actor, runID, takeoverID string) (*Takeover, error) {
	var out db.RunTakeover
	err := svc.s.W().TxNamed(ctx, "fleet.takeover_renew", func(tx *store.Tx) error {
		t, err := svc.holderTakeover(ctx, tx.Q, actor, runID, takeoverID, tx.Now)
		if err != nil {
			return err
		}
		t.ExpiresAt = tx.Now.Add(TakeoverTTL).UnixMicro()
		if _, err := tx.Q.RenewTakeover(ctx, db.RenewTakeoverParams{ExpiresAt: t.ExpiresAt, ID: t.ID}); err != nil {
			return err
		}
		out = t
		return nil
	})
	if err != nil {
		return nil, err
	}
	return takeoverFromRow(out), nil
}

// TakeoverInput forwards the holder's keystrokes to the run's node, which types them into the terminal. Nothing about
// the bytes is stored or logged; they are metered per takeover.
func (svc *Service) TakeoverInput(ctx context.Context, actor audit.Actor, runID, takeoverID string, data []byte) error {
	if len(data) == 0 || len(data) > proto.MaxTakeoverInput {
		return apperr.New(apperr.CodeInvalid, "input must be 1-%d bytes", proto.MaxTakeoverInput)
	}
	t, err := svc.holderTakeover(ctx, svc.s.R(), actor, runID, takeoverID, svc.now())
	if err != nil {
		return err
	}
	r, err := svc.s.R().GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if err := takeoverRunnable(r); err != nil {
		return err
	}
	if !svc.tko.allow(t.ID, len(data), svc.now()) {
		return apperr.New(apperr.CodeRateLimited, "typing too fast for the terminal; slow down")
	}
	return svc.hub.takeoverInput(r.NodeID, proto.TakeoverInput{TakeoverID: t.ID, RunID: r.ID, Data: data})
}

// EndTakeover ends a run's takeover: the holder releases it, or another operator with terminal:takeover ends it.
// takeoverID may be empty (the takeover in force).
func (svc *Service) EndTakeover(ctx context.Context, actor audit.Actor, runID, takeoverID string) (*Takeover, error) {
	var out db.RunTakeover
	err := svc.s.W().TxNamed(ctx, "fleet.takeover_end", func(tx *store.Tx) error {
		r, err := tx.Q.GetRun(ctx, runID)
		if errors.Is(err, sql.ErrNoRows) {
			return errRunNotFound
		} else if err != nil {
			return err
		}
		var t db.RunTakeover
		if takeoverID == "" {
			if t, err = tx.Q.GetActiveTakeover(ctx, runID); errors.Is(err, sql.ErrNoRows) {
				return apperr.New(apperr.CodePrecondition, "the terminal is not taken over")
			}
		} else {
			t, err = tx.Q.GetTakeover(ctx, takeoverID)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && t.RunID != runID) {
				return apperr.New(apperr.CodeNotFound, "takeover not found")
			}
		}
		if err != nil {
			return err
		}
		if t.EndedAt.Valid {
			out = t // already ended: idempotent
			return nil
		}
		reason := TakeoverEndedBy
		switch {
		case t.ExpiresAt <= tx.NowMicros():
			reason = TakeoverExpired
		case t.Holder == holderOf(actor):
			reason = TakeoverReleased
		}
		by := holderOf(actor)
		if reason == TakeoverExpired {
			by = "system:takeovers"
		}
		if err := svc.endTakeoverTx(ctx, tx, r, t, by, reason); err != nil {
			return err
		}
		out, err = tx.Q.GetTakeover(ctx, t.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return takeoverFromRow(out), nil
}

// endTakeoverTx records the end of a takeover in the caller's transaction; after the commit the node detaches and
// held automated input goes out.
func (svc *Service) endTakeoverTx(ctx context.Context, tx *store.Tx, r db.Run, t db.RunTakeover, by, reason string) error {
	n, err := tx.Q.EndTakeover(ctx, db.EndTakeoverParams{EndedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, EndedBy: by, EndReason: reason, ID: t.ID})
	if err != nil || n == 0 {
		return err
	}
	actor := audit.System
	if by != "" && by != "system:takeovers" {
		actor = actorFromRef(by)
	}
	end := tx.Now
	if reason == TakeoverExpired {
		end = time.UnixMicro(t.ExpiresAt)
	}
	typed := svc.tko.done(t.ID)
	if _, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "run.takeover_ended", Target: audit.Target{Type: "run", ID: r.ID},
		Details: map[string]any{"space_id": r.SpaceID, "takeover_id": t.ID, "holder": t.Holder, "reason": reason,
			"seconds": int64(end.Sub(time.UnixMicro(t.StartedAt)).Seconds()), "bytes_typed": typed}}); err != nil {
		return err
	}
	tx.AfterCommit(func() {
		svc.hub.pushTakeovers(context.WithoutCancel(ctx), r.NodeID)
		if svc.opts.OnRunIdle != nil && reason != TakeoverRunEnded {
			svc.opts.OnRunIdle(r.ID) // notices held back during the takeover go out now
		}
	})
	return svc.record(ctx, tx, r.SpaceID, "run", r.ID, "updated", r.Version)
}

// holderOf is how a takeover's holder is stored: the user (any of their sessions and tokens), or root (any root
// token; one rotated mid-takeover keeps it).
func holderOf(a audit.Actor) string {
	if a.Type == audit.ActorRoot {
		return audit.ActorRoot
	}
	return actorRef(a)
}

// actorFromRef turns a stored actor reference (user:<id>, root) back into an audit actor.
func actorFromRef(ref string) audit.Actor {
	for i := range len(ref) {
		if ref[i] == ':' {
			return audit.Actor{Type: ref[:i], ID: ref[i+1:]}
		}
	}
	return audit.Actor{Type: ref}
}

// endRunTakeoverTx ends the takeover of a run that ended (inside the run report's transaction).
func (svc *Service) endRunTakeoverTx(ctx context.Context, tx *store.Tx, r db.Run) error {
	t, err := tx.Q.GetActiveTakeover(ctx, r.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	return svc.endTakeoverTx(ctx, tx, r, t, "system:takeovers", TakeoverRunEnded)
}

// nodeEndedTakeover records that a node could not hold a takeover (attach failed or closed).
func (svc *Service) nodeEndedTakeover(ctx context.Context, nodeID string, m proto.TakeoverEnded) error {
	return svc.s.W().TxNamed(ctx, "fleet.takeover_node_ended", func(tx *store.Tx) error {
		t, err := tx.Q.GetTakeover(ctx, m.TakeoverID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		r, err := tx.Q.GetRun(ctx, t.RunID)
		if err != nil || r.NodeID != nodeID || r.ID != m.RunID || t.EndedAt.Valid {
			return err
		}
		reason := TakeoverNodeEnded
		if ended(r.Observed) {
			reason = TakeoverRunEnded
		}
		return svc.endTakeoverTx(ctx, tx, r, t, "system:takeovers", reason)
	})
}

// ExpireTakeovers ends takeovers whose holder stopped renewing them and returns how many ended.
func (svc *Service) ExpireTakeovers(ctx context.Context) (int, error) {
	rows, err := svc.s.R().ListExpiredTakeovers(ctx, svc.now().UnixMicro())
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range rows {
		err := svc.s.W().TxNamed(ctx, "fleet.takeover_expire", func(tx *store.Tx) error {
			cur, err := tx.Q.GetTakeover(ctx, t.ID)
			if err != nil || cur.EndedAt.Valid || cur.ExpiresAt > tx.NowMicros() {
				return err // renewed or ended meanwhile
			}
			r, err := tx.Q.GetRun(ctx, cur.RunID)
			if err != nil {
				return err
			}
			n++
			return svc.endTakeoverTx(ctx, tx, r, cur, "system:takeovers", TakeoverExpired)
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// RunTakeovers ends takeovers when they expire: one timer for the earliest expiry, woken when a takeover starts
// (nothing polls). On start it first ends takeovers that expired while central was down.
func (svc *Service) RunTakeovers(ctx context.Context) {
	log := obs.Component(svc.opts.Log, "takeovers")
	for {
		if n, err := svc.ExpireTakeovers(ctx); err != nil && ctx.Err() == nil {
			log.Warn("expire takeovers", obs.Err(err))
		} else if n > 0 {
			log.Info("takeovers expired", slog.Int("count", n))
		}
		var due <-chan time.Time
		var t *time.Timer
		if next, err := svc.s.R().NextTakeoverExpiry(ctx); err == nil && next > 0 {
			t = time.NewTimer(max(time.UnixMicro(next).Sub(svc.now()), 50*time.Millisecond))
			due = t.C
		}
		select {
		case <-ctx.Done():
			if t != nil {
				t.Stop()
			}
			return
		case <-svc.tko.wake:
		case <-due:
		}
		if t != nil {
			t.Stop()
		}
	}
}

// ---- hub side ----

// minor returns the protocol minor of a node's live connection.
func (h *Hub) minor(nodeID string) (int, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.sessions[nodeID]
	if s == nil {
		return 0, false
	}
	return s.minor, true
}

// pushTakeovers sends the node the complete set of its runs' active takeovers (after connect and on every change).
func (h *Hub) pushTakeovers(ctx context.Context, nodeID string) {
	h.mu.Lock()
	s := h.sessions[nodeID]
	h.mu.Unlock()
	if s == nil || s.minor < proto.MinorTakeover {
		return
	}
	if err := h.sendTakeovers(ctx, s); err != nil {
		s.log.Warn("send takeover state", obs.Err(err))
	}
}

func (h *Hub) sendTakeovers(ctx context.Context, s *session) error {
	rows, err := h.svc.s.R().ListNodeTakeovers(ctx, s.nodeID)
	if err != nil {
		return err
	}
	st := proto.TakeoverState{Takeovers: []proto.Takeover{}}
	now := h.svc.now().UnixMicro()
	for _, r := range rows {
		if r.ExpiresAt <= now {
			continue // expired: ended by the expiry timer
		}
		st.Takeovers = append(st.Takeovers, proto.Takeover{TakeoverID: r.ID, SpaceID: r.SpaceID, RunID: r.RunID, Generation: r.Generation})
	}
	return s.send(proto.TypeTakeoverState, h.svc.now(), st)
}

// takeoverInput hands keystrokes to the node's connection without waiting: a node that cannot keep up refuses typing
// rather than holding the request.
func (h *Hub) takeoverInput(nodeID string, in proto.TakeoverInput) error {
	h.mu.Lock()
	s := h.sessions[nodeID]
	h.mu.Unlock()
	if s == nil || s.minor < proto.MinorTakeover {
		return apperr.New(apperr.CodeUnavailable, "the run's node is not connected")
	}
	b, err := proto.Encode(ids.New(ids.Message), proto.TypeTakeoverInput, h.svc.now(), in)
	if err != nil {
		return err
	}
	select {
	case s.out <- b:
		return nil
	case <-s.done:
		return apperr.New(apperr.CodeUnavailable, "the run's node is not connected")
	default:
		return apperr.New(apperr.CodeUnavailable, "the node's connection is busy; type again")
	}
}
