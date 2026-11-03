package events

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/events/template"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Delivery is one handled event (SM-EVT-017 delivery history).
type Delivery struct {
	ID          int64     `json:"id"`
	EventID     string    `json:"event_id"`
	Subject     string    `json:"subject"`
	Type        string    `json:"type"`
	StreamSeq   int64     `json:"stream_seq"`
	Outcome     string    `json:"outcome" enum:"accepted,duplicate,filtered,quarantined,replayed"`
	Origin      string    `json:"origin" enum:"live,redelivery,replay,reprocess" doc:"Why it was handled: first delivery, a retry by the stream, an operator's replay, or a reprocessed quarantine entry"`
	Attempt     int64     `json:"attempt" doc:"How many times this subscription has handled this event, this one included"`
	ReplayID    string    `json:"replay_id,omitempty" doc:"The replay that re-delivered it"`
	InboxItemID string    `json:"inbox_item_id,omitempty"`
	Detail      string    `json:"detail,omitempty"`
	At          time.Time `json:"at"`
}

// QuarantineEntry is an event that failed validation (SM-EVT-013). Its payload is untrusted.
type QuarantineEntry struct {
	ID          string     `json:"id"`
	EventID     string     `json:"event_id"`
	Subject     string     `json:"subject"`
	Type        string     `json:"type"`
	Source      string     `json:"source"`
	EventTime   time.Time  `json:"event_time"`
	ContentType string     `json:"content_type"`
	Payload     string     `json:"payload" doc:"The raw payload (at most 1 MiB), untrusted"`
	Error       string     `json:"error"`
	Depth       int64      `json:"depth" doc:"The automation depth the event carried (its spaceman-depth header; 0: none); reprocessing passes it on"`
	Status      string     `json:"status" enum:"open,reprocessed,discarded"`
	CreatedAt   time.Time  `json:"created_at"`
	ResolvedAt  *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy  string     `json:"resolved_by,omitempty"`
}

func (svc *Service) ownedSub(ctx context.Context, spaceID, subID string) (db.EventSubscription, error) {
	r, err := svc.s.R().GetEventSubscription(ctx, subID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && r.SpaceID != spaceID) {
		return r, notFound("subscription")
	}
	return r, err
}

// Deliveries returns a subscription's most recent deliveries.
func (svc *Service) Deliveries(ctx context.Context, spaceID, subID string, limit int) ([]*Delivery, error) {
	if _, err := svc.ownedSub(ctx, spaceID, subID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := svc.s.R().ListEventDeliveries(ctx, db.ListEventDeliveriesParams{SubscriptionID: subID, Limit: int64(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]*Delivery, 0, len(rows))
	for _, r := range rows {
		out = append(out, &Delivery{ID: r.ID, EventID: r.EventID, Subject: r.Subject, Type: r.Type, StreamSeq: r.StreamSeq, Outcome: r.Outcome,
			Origin: r.Origin, Attempt: r.Attempt, ReplayID: r.ReplayID, InboxItemID: r.InboxItemID, Detail: r.Detail, At: fromMicros(r.At)})
	}
	return out, nil
}

func quarantineFromRow(r db.EventQuarantine) *QuarantineEntry {
	q := &QuarantineEntry{ID: r.ID, EventID: r.EventID, Subject: r.Subject, Type: r.Type, Source: r.Source, EventTime: fromMicros(r.EventTime),
		ContentType: r.ContentType, Payload: string(r.Payload), Error: r.Error, Depth: r.Depth, Status: r.Status, CreatedAt: fromMicros(r.CreatedAt), ResolvedBy: r.ResolvedBy}
	if r.ResolvedAt.Valid {
		t := fromMicros(r.ResolvedAt.Int64)
		q.ResolvedAt = &t
	}
	return q
}

// Quarantine lists a subscription's quarantine (status "" = all).
func (svc *Service) Quarantine(ctx context.Context, spaceID, subID, status string) ([]*QuarantineEntry, error) {
	if _, err := svc.ownedSub(ctx, spaceID, subID); err != nil {
		return nil, err
	}
	rows, err := svc.s.R().ListQuarantine(ctx, db.ListQuarantineParams{SubscriptionID: subID, Status: status, Lim: 200})
	if err != nil {
		return nil, err
	}
	out := make([]*QuarantineEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, quarantineFromRow(r))
	}
	return out, nil
}

// ResolveQuarantine reprocesses an entry with the subscription's current definition (after fixing the template, say) or
// discards it. Reprocessing that fails again keeps the entry open with the new error.
func (svc *Service) ResolveQuarantine(ctx context.Context, actor audit.Actor, spaceID, subID, entryID string, reprocess bool) (*QuarantineEntry, error) {
	r, err := svc.ownedSub(ctx, spaceID, subID)
	if err != nil {
		return nil, err
	}
	q, err := svc.s.R().GetQuarantine(ctx, entryID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && q.SubscriptionID != subID) {
		return nil, notFound("quarantine entry")
	} else if err != nil {
		return nil, err
	}
	if q.Status != "open" {
		return nil, apperr.New(apperr.CodePrecondition, "the entry is already %s", q.Status)
	}
	status := "discarded"
	if reprocess {
		perr := svc.Process(ctx, r.ID, template.Event{ID: q.EventID, Subject: q.Subject, Type: q.Type, Source: q.Source, ContentType: q.ContentType,
			Time: fromMicros(q.EventTime), Data: q.Payload, Depth: depthHeader(q.Depth)}, 0, Origin{Kind: OriginReprocess})
		var qe *QuarantinedError
		if errors.As(perr, &qe) {
			return svc.quarantineEntry(ctx, entryID) // failed again: still open, with the new error
		} else if perr != nil {
			return nil, perr
		}
		status = "reprocessed"
	}
	err = svc.s.W().TxNamed(ctx, "events.quarantine_resolve", func(tx *store.Tx) error {
		if _, err := tx.Q.ResolveQuarantine(ctx, db.ResolveQuarantineParams{Status: status, Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
			By: actor.ID, ID: entryID}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "event_subscription", subID, "updated", r.Version); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "event_quarantine." + status, Target: audit.Target{Type: "event_quarantine", ID: entryID},
			Details: map[string]any{"subscription_id": subID, "event_id": q.EventID}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.quarantineEntry(ctx, entryID)
}

// depthHeader is a stored depth as the spaceman-depth header carried it (0: no header).
func depthHeader(d int64) string {
	if d <= 0 {
		return ""
	}
	return strconv.FormatInt(d, 10)
}

func (svc *Service) quarantineEntry(ctx context.Context, id string) (*QuarantineEntry, error) {
	q, err := svc.s.R().GetQuarantine(ctx, id)
	if err != nil {
		return nil, err
	}
	return quarantineFromRow(q), nil
}

// PurgeDeliveries removes delivery history older than the retention.
func (svc *Service) PurgeDeliveries(ctx context.Context) (int64, error) {
	var n int64
	err := svc.s.W().TxNamed(ctx, "events.delivery_purge", func(tx *store.Tx) error {
		var err error
		n, err = tx.Q.PurgeEventDeliveries(ctx, svc.o.Clock.Now().Add(-svc.o.DeliveryRetention).UnixMicro())
		return err
	})
	return n, err
}
