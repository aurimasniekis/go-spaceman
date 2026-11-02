// Package messages is SpaceMan's durable communication between agents, control and operators (SM-COMM-*). SpaceMan
// owns delivery state: a message is stored until a recipient can see it, available once a notice was accepted by the
// recipient's terminal or the recipient fetched it, acknowledged only through an explicit acknowledgement (the agent's
// tool or an operator action, never terminal text), and resolved when a request is answered or closed.
package messages

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// States.
const (
	Stored       = "stored"
	Available    = "available"
	Acknowledged = "acknowledged"
	Resolved     = "resolved"
)

// Message is one durable message.
type Message struct {
	ID             string     `json:"id"`
	SpaceID        string     `json:"space_id"`
	FromKind       string     `json:"from_kind" enum:"run,operator,system"`
	FromID         string     `json:"from_id,omitempty"`
	FromLabel      string     `json:"from_label,omitempty"`
	ToKind         string     `json:"to_kind" enum:"run,role,control,operators"`
	ToID           string     `json:"to_id,omitempty"`
	Kind           string     `json:"kind" enum:"message,request,reply,notification"`
	Subject        string     `json:"subject"`
	Body           string     `json:"body" doc:"Markdown; untrusted when an agent wrote it"`
	CorrelationID  string     `json:"correlation_id,omitempty"`
	ReplyTo        string     `json:"reply_to,omitempty"`
	State          string     `json:"state" enum:"stored,available,acknowledged,resolved"`
	DeliveredVia   string     `json:"delivered_via,omitempty" enum:",terminal_notice,mcp_fetch,operator_view"`
	AvailableAt    *time.Time `json:"available_at,omitempty"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	AcknowledgedBy string     `json:"acknowledged_by,omitempty"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
	Resolution     string     `json:"resolution,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	Version        int64      `json:"version"`
}

// From identifies a sender; it is set from the caller's identity, never from the request body.
type From struct {
	Kind, ID, Label string
}

// SendInput is a new message. To is "control", "operators", "role:<name>" or "run:<id>".
type SendInput struct {
	To            string
	Kind          string
	Subject, Body string
	CorrelationID string
	ReplyTo       string
	DedupKey      string
}

// Service manages messages.
type Service struct {
	s       *store.Store
	audit   *audit.Writer
	changes *changefeed.Recorder
	// Kick wakes the notifier after a commit (nil: no notifier).
	Kick func()
}

// New returns the service.
func New(s *store.Store, aw *audit.Writer, ch *changefeed.Recorder) *Service {
	return &Service{s: s, audit: aw, changes: ch}
}

var (
	reRole      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)
	errNotFound = apperr.New(apperr.CodeNotFound, "message not found")
)

func tp(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMicro(v.Int64).UTC()
	return &t
}

func fromRow(r db.Message) *Message {
	return &Message{ID: r.ID, SpaceID: r.SpaceID, FromKind: r.FromKind, FromID: r.FromID, FromLabel: r.FromLabel, ToKind: r.ToKind, ToID: r.ToID,
		Kind: r.Kind, Subject: r.Subject, Body: r.Body, CorrelationID: r.CorrelationID, ReplyTo: r.ReplyTo, State: r.State, DeliveredVia: r.DeliveredVia,
		AvailableAt: tp(r.AvailableAt), AcknowledgedAt: tp(r.AcknowledgedAt), AcknowledgedBy: r.AcknowledgedBy, ResolvedAt: tp(r.ResolvedAt),
		Resolution: r.Resolution, CreatedAt: time.UnixMicro(r.CreatedAt).UTC(), Version: r.Version}
}

// ParseTo splits a recipient.
func ParseTo(to string) (kind, id string, err error) {
	switch {
	case to == "control" || to == "operators":
		return to, "", nil
	case strings.HasPrefix(to, "role:") && reRole.MatchString(strings.TrimPrefix(to, "role:")):
		return "role", strings.TrimPrefix(to, "role:"), nil
	case strings.HasPrefix(to, "run:") && ids.Valid(ids.Run, strings.TrimPrefix(to, "run:")):
		return "run", strings.TrimPrefix(to, "run:"), nil
	}
	return "", "", apperr.New(apperr.CodeInvalid, `to must be "control", "operators", "role:<name>" or "run:<run id>"`)
}

// Send stores a message. A repeated dedup key from the same sender returns the original (created=false), so retries
// never duplicate (SM-COMM-004). A reply resolves the request it answers.
func (svc *Service) Send(ctx context.Context, actor audit.Actor, spaceID string, from From, in SendInput) (*Message, bool, error) {
	toKind, toID, err := ParseTo(in.To)
	if err != nil {
		return nil, false, err
	}
	v := &apperr.Validation{}
	if in.Kind == "" {
		in.Kind = "message"
	}
	switch in.Kind {
	case "message", "request", "notification":
	case "reply":
		if in.ReplyTo == "" {
			v.Add("reply_to", "a reply names the message it answers")
		}
	default:
		v.Add("kind", "must be message, request, reply or notification")
	}
	in.Subject = strings.TrimSpace(in.Subject)
	if in.Subject == "" || utf8.RuneCountInString(in.Subject) > 200 {
		v.Add("subject", "is required (at most 200 characters)")
	}
	if len(in.Body) > 100_000 {
		v.Add("body", "at most 100000 bytes")
	}
	if len(in.DedupKey) > 256 || len(in.CorrelationID) > 256 {
		v.Add("dedup_key", "keys are at most 256 characters")
	}
	if err := v.Err(); err != nil {
		return nil, false, err
	}
	id := ids.New(ids.Message)
	created := false
	err = svc.s.W().TxNamed(ctx, "messages.send", func(tx *store.Tx) error {
		created = false
		if in.DedupKey != "" {
			if ex, err := tx.Q.GetMessageByDedup(ctx, db.GetMessageByDedupParams{SpaceID: spaceID, FromKind: from.Kind, FromID: from.ID, DedupKey: in.DedupKey}); err == nil {
				id = ex.ID
				return nil
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if toKind == "run" {
			r, err := tx.Q.GetRun(ctx, toID)
			if err != nil || r.SpaceID != spaceID {
				return apperr.New(apperr.CodeInvalid, "run %q is not a run of this space", toID)
			}
		}
		var orig db.Message
		if in.ReplyTo != "" {
			o, err := tx.Q.GetMessage(ctx, in.ReplyTo)
			if err != nil || o.SpaceID != spaceID {
				return apperr.New(apperr.CodeInvalid, "reply_to %q is not a message of this space", in.ReplyTo)
			}
			orig = o
			if in.CorrelationID == "" {
				in.CorrelationID = o.CorrelationID
			}
		}
		if err := tx.Q.InsertMessage(ctx, db.InsertMessageParams{ID: id, SpaceID: spaceID, FromKind: from.Kind, FromID: from.ID, FromLabel: from.Label,
			ToKind: toKind, ToID: toID, Kind: in.Kind, Subject: in.Subject, Body: in.Body, CorrelationID: in.CorrelationID, ReplyTo: in.ReplyTo,
			DedupKey: in.DedupKey, CreatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		created = true
		if in.Kind == "reply" && orig.Kind == "request" && orig.State != Resolved {
			if _, err := tx.Q.ResolveMessage(ctx, db.ResolveMessageParams{Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, Resolution: "replied: " + id,
				ID: orig.ID}); err != nil {
				return err
			}
			if err := svc.changes.Record(ctx, tx, spaceID, "message", orig.ID, "updated", orig.Version+1); err != nil {
				return err
			}
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "message", id, "created", 1); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "message.sent", Target: audit.Target{Type: "message", ID: id},
			Details: map[string]any{"space_id": spaceID, "to": in.To, "kind": in.Kind, "reply_to": in.ReplyTo}})
		return err
	})
	if err != nil {
		return nil, false, err
	}
	if created && svc.Kick != nil {
		svc.Kick()
	}
	m, err := svc.Get(ctx, spaceID, id)
	return m, created, err
}

// Get returns a message of a space.
func (svc *Service) Get(ctx context.Context, spaceID, id string) (*Message, error) {
	r, err := svc.s.R().GetMessage(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && r.SpaceID != spaceID) {
		return nil, errNotFound
	} else if err != nil {
		return nil, err
	}
	return fromRow(r), nil
}

// List returns a space's recent messages, newest first.
func (svc *Service) List(ctx context.Context, spaceID string, limit int) ([]*Message, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := svc.s.R().ListSpaceMessages(ctx, db.ListSpaceMessagesParams{SpaceID: spaceID, Limit: int64(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]*Message, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromRow(r))
	}
	return out, nil
}

// OperatorPending counts messages waiting for operators in a space.
func (svc *Service) OperatorPending(ctx context.Context, spaceID string) (int64, error) {
	return svc.s.R().CountOperatorMessages(ctx, spaceID)
}

// Recipient is a run reading its messages.
type Recipient struct {
	SpaceID, RunID, Role string
	Control              bool
}

// HumanNotice is a human-action outcome waiting for the requester.
type HumanNotice struct {
	ID       string    `json:"id"`
	ActionID string    `json:"action_id"`
	Title    string    `json:"title"`
	Type     string    `json:"type"`
	Kind     string    `json:"kind" doc:"resolved, cancelled or expired"`
	Outcome  string    `json:"outcome,omitempty"`
	At       time.Time `json:"at"`
}

// Inbox is what a run has waiting: messages (addressed to it, its role, or control) and human-action outcomes.
type Inbox struct {
	Messages []*Message    `json:"messages"`
	Human    []HumanNotice `json:"human_responses"`
}

// Fetch returns a run's waiting items and marks the stored ones available (delivered by fetching).
func (svc *Service) Fetch(ctx context.Context, rc Recipient) (*Inbox, error) {
	out := &Inbox{Messages: []*Message{}, Human: []HumanNotice{}}
	err := svc.s.W().TxNamed(ctx, "messages.fetch", func(tx *store.Tx) error {
		rows, err := tx.Q.ListMessagesFor(ctx, db.ListMessagesForParams{SpaceID: rc.SpaceID, RunID: rc.RunID, Role: rc.Role, IsControl: b2i(rc.Control), Lim: 100})
		if err != nil {
			return err
		}
		var stored []string
		for _, r := range rows {
			if sentBy(r, rc.RunID) {
				continue // a run never receives its own messages (role or control broadcast)
			}
			if r.State == Stored {
				stored = append(stored, r.ID)
				r.State = Available
			}
			out.Messages = append(out.Messages, fromRow(r))
		}
		if len(stored) > 0 {
			if _, err := tx.Q.MarkMessagesAvailable(ctx, db.MarkMessagesAvailableParams{Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, Via: "mcp_fetch", Ids: stored}); err != nil {
				return err
			}
			for _, id := range stored {
				if err := svc.changes.Record(ctx, tx, rc.SpaceID, "message", id, "updated", 0); err != nil {
					return err
				}
			}
		}
		ns, err := tx.Q.ListHumanNotificationsFor(ctx, db.ListHumanNotificationsForParams{SpaceID: rc.SpaceID, RunID: rc.RunID, Lim: 100})
		if err != nil {
			return err
		}
		var pending []string
		for _, n := range ns {
			out.Human = append(out.Human, HumanNotice{ID: n.ID, ActionID: n.ActionID, Title: n.Title, Type: n.Type, Kind: n.Kind, Outcome: n.Outcome,
				At: time.UnixMicro(n.CreatedAt).UTC()})
			pending = append(pending, n.ID)
		}
		if len(pending) > 0 {
			if _, err := tx.Q.MarkNotificationsDelivered(ctx, db.MarkNotificationsDeliveredParams{Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, Ids: pending}); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// AckRun acknowledges a message or a human-action notice for a run. Only a recipient may acknowledge.
func (svc *Service) AckRun(ctx context.Context, actor audit.Actor, rc Recipient, id string) error {
	return svc.s.W().TxNamed(ctx, "messages.ack", func(tx *store.Tx) error {
		now := sql.NullInt64{Int64: tx.NowMicros(), Valid: true}
		if strings.HasPrefix(id, "hnt_") {
			n, err := tx.Q.AckNotification(ctx, db.AckNotificationParams{Now: now, ID: id, RunID: rc.RunID})
			if err != nil {
				return err
			}
			if n == 0 {
				return apperr.New(apperr.CodeNotFound, "no pending human response %q for this run", id)
			}
			return nil
		}
		m, err := tx.Q.GetMessage(ctx, id)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && m.SpaceID != rc.SpaceID) {
			return errNotFound
		} else if err != nil {
			return err
		}
		if !addressedTo(m, rc) {
			return apperr.New(apperr.CodeForbidden, "only a recipient may acknowledge a message")
		}
		return svc.ack(ctx, tx, actor, m, rc.RunID, now)
	})
}

// addressedTo reports whether a run is a recipient of a message.
func addressedTo(m db.Message, rc Recipient) bool {
	switch m.ToKind {
	case "run":
		return m.ToID == rc.RunID
	case "role":
		return m.ToID == rc.Role
	case "control":
		return rc.Control
	}
	return false
}

// sentBy reports whether a run sent a message.
func sentBy(m db.Message, runID string) bool { return m.FromKind == "run" && m.FromID == runID }

func (svc *Service) ack(ctx context.Context, tx *store.Tx, actor audit.Actor, m db.Message, by string, now sql.NullInt64) error {
	n, err := tx.Q.AckMessage(ctx, db.AckMessageParams{Now: now, By: by, ID: m.ID})
	if err != nil {
		return err
	}
	if n == 0 {
		return apperr.New(apperr.CodeConflict, "the message is already %s", m.State)
	}
	if err := svc.changes.Record(ctx, tx, m.SpaceID, "message", m.ID, "updated", m.Version+1); err != nil {
		return err
	}
	_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "message.acknowledged", Target: audit.Target{Type: "message", ID: m.ID},
		Details: map[string]any{"space_id": m.SpaceID}})
	return err
}

// AckOperator acknowledges a message addressed to operators (an explicit action in the UI or API).
func (svc *Service) AckOperator(ctx context.Context, actor audit.Actor, spaceID, id string) (*Message, error) {
	err := svc.s.W().TxNamed(ctx, "messages.ack_operator", func(tx *store.Tx) error {
		m, err := tx.Q.GetMessage(ctx, id)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && m.SpaceID != spaceID) {
			return errNotFound
		} else if err != nil {
			return err
		}
		if m.ToKind != "operators" {
			return apperr.New(apperr.CodeForbidden, "operators acknowledge only messages addressed to operators")
		}
		return svc.ack(ctx, tx, actor, m, actor.ID, sql.NullInt64{Int64: tx.NowMicros(), Valid: true})
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, spaceID, id)
}

// Resolve closes a message (typically a request) with a resolution.
func (svc *Service) Resolve(ctx context.Context, actor audit.Actor, spaceID, id, resolution string) (*Message, error) {
	if len(resolution) > 2000 {
		return nil, apperr.New(apperr.CodeInvalid, "resolution: at most 2000 characters")
	}
	err := svc.s.W().TxNamed(ctx, "messages.resolve", func(tx *store.Tx) error {
		m, err := tx.Q.GetMessage(ctx, id)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && m.SpaceID != spaceID) {
			return errNotFound
		} else if err != nil {
			return err
		}
		n, err := tx.Q.ResolveMessage(ctx, db.ResolveMessageParams{Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, Resolution: resolution, ID: id})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodeConflict, "the message is already resolved")
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "message", id, "updated", m.Version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "message.resolved", Target: audit.Target{Type: "message", ID: id},
			Details: map[string]any{"space_id": spaceID}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, spaceID, id)
}
