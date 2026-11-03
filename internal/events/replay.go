package events

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Delivery origins (SM-EVT-015): why an event was handled, kept apart from what came of it (the outcome).
const (
	// OriginLive is the first delivery of an event to the subscription.
	OriginLive = "live"
	// OriginRedelivery is the stream delivering an event again on its own: after a negative acknowledgement, an
	// acknowledgement timeout, a restart or a restore. It is a retry, never a replay.
	OriginRedelivery = "redelivery"
	// OriginReplay is an event delivered again because an operator replayed the subscription.
	OriginReplay = "replay"
	// OriginReprocess is a quarantined event an operator reprocessed.
	OriginReprocess = "reprocess"
)

// Origin says why an event is being processed. The zero value is a stream delivery: live or, when the subscription
// has already handled the event, a redelivery.
type Origin struct {
	Kind     string
	ReplayID string
	// Fresh lets a replay create new inbox items for events that already produced one (once per replay).
	Fresh bool
	// attempt is set by Process from the delivery history.
	attempt int64
}

// Replay is one explicit replay of a subscription: who asked, why, from where, and when it took effect.
type Replay struct {
	ID          string     `json:"id"`
	Generation  int64      `json:"generation" doc:"The cursor generation the replay created"`
	Policy      string     `json:"policy" enum:"all,time,seq"`
	Time        *time.Time `json:"time,omitempty"`
	Seq         uint64     `json:"seq,omitempty"`
	FreshItems  bool       `json:"fresh_items" doc:"Events that already produced an inbox item produce a new one (once per replay)"`
	Reason      string     `json:"reason"`
	RequestedBy string     `json:"requested_by"`
	RequestedAt time.Time  `json:"requested_at"`
	Boundary    *time.Time `json:"boundary,omitempty" doc:"When the consumer was recreated: events stored before it are delivered as replayed"`
	Applied     bool       `json:"applied" doc:"The ingestion manager recreated the consumer"`
	Superseded  bool       `json:"superseded" doc:"A later cursor change replaced it"`
}

// ReplayInput asks for a replay.
type ReplayInput struct {
	Policy     string     `json:"policy" enum:"all,time,seq" doc:"all: everything the stream still holds; time: from a time; seq: from a stream sequence"`
	Time       *time.Time `json:"time,omitempty"`
	Seq        uint64     `json:"seq,omitempty"`
	Reason     string     `json:"reason" minLength:"1" maxLength:"500" doc:"Why; recorded in the audit log and the replay history"`
	FreshItems bool       `json:"fresh_items,omitempty" doc:"Create new inbox items even for events that already produced one (default: deduplicated as before)"`
}

func replayFromRow(r db.SubscriptionReplay, current int64) *Replay {
	out := &Replay{ID: r.ID, Generation: r.Generation, Policy: r.Policy, FreshItems: r.FreshItems == 1, Reason: r.Reason, RequestedBy: r.RequestedBy,
		RequestedAt: fromMicros(r.RequestedAt), Applied: r.BoundaryAt.Valid, Superseded: r.Generation < current}
	if r.StartTime.Valid {
		t := fromMicros(r.StartTime.Int64)
		out.Time = &t
	}
	if r.StartSeq.Valid {
		out.Seq = uint64(r.StartSeq.Int64) //nolint:gosec // stored from a uint64
	}
	if r.BoundaryAt.Valid {
		t := fromMicros(r.BoundaryAt.Int64)
		out.Boundary = &t
	}
	return out
}

// recordReplay stores a replay for the cursor generation gen inside the caller's transaction.
func recordReplay(ctx context.Context, tx *store.Tx, actor audit.Actor, r db.EventSubscription, gen int64, c Cursor, reason string, fresh bool) (string, error) {
	id := ids.New("rpl")
	policy, st, ss := cursorCols(c)
	err := tx.Q.InsertSubscriptionReplay(ctx, db.InsertSubscriptionReplayParams{ID: id, SubscriptionID: r.ID, SpaceID: r.SpaceID, Generation: gen, Policy: policy,
		StartTime: st, StartSeq: ss, FreshItems: b2i(fresh), Reason: reason, RequestedBy: actorRef(actor), RequestedAt: tx.NowMicros()})
	return id, err
}

func actorRef(a audit.Actor) string {
	if a.ID == "" {
		return a.Type
	}
	return a.Type + ":" + a.ID
}

// Replay re-delivers a subscription's events from a position: an explicit, audited operation, recorded in the
// subscription's replay history (SM-EVT-015). The consumer is recreated at the position; events stored before that
// moment are delivered with origin replay. Inbox deduplication still applies unless fresh_items is set, so by default
// an event that already produced an item produces no second one (its delivery is recorded as a duplicate replay).
func (svc *Service) Replay(ctx context.Context, actor audit.Actor, spaceID, subID string, version int64, in ReplayInput) (*Replay, error) {
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Reason == "" || len(in.Reason) > 500 {
		return nil, apperr.New(apperr.CodeInvalid, "reason is required (at most 500 bytes)")
	}
	c := Cursor{Policy: in.Policy, Time: in.Time, Seq: in.Seq}
	if c.Policy == StartNew || c.Policy == "" {
		return nil, apperr.New(apperr.CodeInvalid, "a replay starts at all, a time or a stream sequence (policy all, time or seq)")
	}
	if err := validCursor(&c); err != nil {
		return nil, err
	}
	var replayID string
	err := svc.s.W().TxNamed(ctx, "events.subscription_replay", func(tx *store.Tx) error {
		cur, err := tx.Q.GetEventSubscription(ctx, subID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && cur.SpaceID != spaceID) {
			return notFound("subscription")
		} else if err != nil {
			return err
		}
		if cur.Version != version {
			return apperr.New(apperr.CodePrecondition, "the subscription changed since version %d", version)
		}
		if cur.Delivery == DeliveryCore {
			return errCoreCursor("a replay")
		}
		gen := cur.CursorGeneration + 1
		policy, st, ss := cursorCols(c)
		n, err := tx.Q.SetSubscriptionCursor(ctx, db.SetSubscriptionCursorParams{CursorGeneration: gen, StartPolicy: policy, StartTime: st, StartSeq: ss,
			Now: tx.NowMicros(), ID: subID, Version: version})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodePrecondition, "the subscription changed concurrently")
		}
		if replayID, err = recordReplay(ctx, tx, actor, cur, gen, c, in.Reason, in.FreshItems); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "event_subscription", subID, "updated", version+1); err != nil {
			return err
		}
		details := map[string]any{"space_id": spaceID, "replay_id": replayID, "policy": policy, "generation": gen, "reason": in.Reason, "fresh_items": in.FreshItems}
		if c.Time != nil {
			details["from_time"] = c.Time.UTC().Format(time.RFC3339)
		}
		if c.Seq > 0 {
			details["from_seq"] = c.Seq
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "event_subscription.replayed", Target: audit.Target{Type: "event_subscription", ID: subID},
			Details: details})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.o.Metrics.IngestReplay()
	svc.notify()
	return svc.getReplay(ctx, subID, replayID)
}

func (svc *Service) getReplay(ctx context.Context, subID, id string) (*Replay, error) {
	l, err := svc.Replays(ctx, "", subID, 50)
	if err != nil {
		return nil, err
	}
	for _, r := range l {
		if r.ID == id {
			return r, nil
		}
	}
	return nil, notFound("replay")
}

// Replays returns a subscription's replay history, newest first (spaceID empty: any space).
func (svc *Service) Replays(ctx context.Context, spaceID, subID string, limit int) ([]*Replay, error) {
	r, err := svc.s.R().GetEventSubscription(ctx, subID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && spaceID != "" && r.SpaceID != spaceID) {
		return nil, notFound("subscription")
	} else if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := svc.s.R().ListSubscriptionReplays(ctx, db.ListSubscriptionReplaysParams{SubscriptionID: subID, Limit: int64(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]*Replay, 0, len(rows))
	for _, x := range rows {
		out = append(out, replayFromRow(x, r.CursorGeneration))
	}
	return out, nil
}
