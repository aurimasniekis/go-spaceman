// Package inbox is each space's inbox (SM-INBOX-*): incoming operator requests, events, worker messages and other
// inputs that control interprets. An item is not a task. Bodies are Markdown or JSON and untrusted; trusted metadata is
// set only by the authenticated source (an operator session, a subscription, a run credential), never read from the
// body (SM-INBOX-004). Duplicate deliveries with the same source dedup key never create a second item.
package inbox

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/attachments"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Content types.
const (
	Markdown = "text/markdown"
	JSON     = "application/json"
)

// Statuses.
const (
	StatusNew        = "new"
	StatusInProgress = "in_progress"
	StatusDeferred   = "deferred"
	StatusRejected   = "rejected"
	StatusDone       = "done"
)

// OwnerKind is the attachment owner kind of inbox items.
const OwnerKind = "inbox_item"

// Limits.
const (
	MaxBody        = 200_000
	MaxTitle       = 200
	MaxAttachments = 20
)

var (
	sourceKinds = map[string]bool{"operator": true, "worker": true, "human_response": true, "dreamer": true, "kb": true, "cross_space": true, "event": true, "webhook": true}
	urgencies   = map[string]bool{"low": true, "normal": true, "high": true, "urgent": true}
	statuses    = map[string]bool{StatusNew: true, StatusInProgress: true, StatusDeferred: true, StatusRejected: true, StatusDone: true}
)

// Item is one inbox item.
type Item struct {
	ID            string         `json:"id"`
	SpaceID       string         `json:"space_id"`
	SourceKind    string         `json:"source_kind" enum:"operator,worker,human_response,dreamer,kb,cross_space,event,webhook"`
	SourceID      string         `json:"source_id,omitempty" doc:"The authenticated source: a user, run, subscription or space ID"`
	SourceLabel   string         `json:"source_label,omitempty"`
	Title         string         `json:"title"`
	ContentType   string         `json:"content_type" enum:"text/markdown,application/json"`
	Body          string         `json:"body" doc:"Untrusted content: Markdown (with spaceman://attachment/<id> references) or JSON"`
	PayloadRef    string         `json:"payload_ref,omitempty" doc:"A retained reference to a large payload"`
	CorrelationID string         `json:"correlation_id,omitempty"`
	DedupKey      string         `json:"dedup_key,omitempty"`
	Trusted       map[string]any `json:"trusted" doc:"Metadata set by the authenticated source, never taken from the body"`
	Subscription  string         `json:"subscription_id,omitempty"`
	Urgency       string         `json:"urgency" enum:"low,normal,high,urgent"`
	Status        string         `json:"status" enum:"new,in_progress,deferred,rejected,done"`
	StatusReason  string         `json:"status_reason,omitempty"`
	DeferUntil    *time.Time     `json:"defer_until,omitempty"`
	LinkedKind    string         `json:"linked_kind,omitempty" doc:"Existing work this item was associated with (task, human_action, …)"`
	LinkedID      string         `json:"linked_id,omitempty"`
	ReceivedAt    time.Time      `json:"received_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	CreatedBy     string         `json:"created_by"`
	Version       int64          `json:"version"`
	// Attachments is filled by Get (not by List).
	Attachments []*attachments.Attachment `json:"attachments,omitempty"`
}

// CreateInput is a new inbox item. Source fields come from the caller's authenticated identity, never from users' input.
type CreateInput struct {
	SourceKind, SourceID, SourceLabel string
	Title, ContentType, Body          string
	PayloadRef, CorrelationID         string
	DedupKey, Subscription, Urgency   string
	Trusted                           map[string]any
	AttachmentIDs                     []string
	// CausationID is what caused the item (the inbound Pulse event ID); it is published as the item event's causation.
	CausationID string
}

// Service manages inboxes.
type Service struct {
	s       *store.Store
	audit   *audit.Writer
	changes *changefeed.Recorder
	att     *attachments.Service
	// OnCreated is called after a commit that added a new item (not a duplicate), e.g. to wake control.
	OnCreated func(it *Item)
	// Outbox records item events for publication to Pulse (SM-OBS-004). Nil: nothing is published.
	Outbox *outbox.Outbox
}

// New returns the service.
func New(s *store.Store, aw *audit.Writer, ch *changefeed.Recorder, att *attachments.Service) *Service {
	return &Service{s: s, audit: aw, changes: ch, att: att}
}

var errNotFound = apperr.New(apperr.CodeNotFound, "inbox item not found")

func (in *CreateInput) normalize() error {
	v := &apperr.Validation{}
	if !sourceKinds[in.SourceKind] {
		v.Add("source_kind", "unknown source kind")
	}
	if in.ContentType == "" {
		in.ContentType = Markdown
	}
	switch in.ContentType {
	case Markdown:
	case JSON:
		if !json.Valid([]byte(in.Body)) {
			v.Add("body", "must be valid JSON for application/json")
		}
	default:
		v.Add("content_type", "must be text/markdown or application/json")
	}
	if len(in.Body) > MaxBody {
		v.Add("body", "must be at most 200000 bytes")
	}
	if !utf8.ValidString(in.Body) {
		v.Add("body", "must be UTF-8")
	}
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		in.Title = derivedTitle(in.Body, in.ContentType)
	}
	if utf8.RuneCountInString(in.Title) > MaxTitle {
		in.Title = string([]rune(in.Title)[:MaxTitle])
	}
	if in.Title == "" {
		v.Add("title", "is required when the body is empty")
	}
	if in.Urgency == "" {
		in.Urgency = "normal"
	}
	if !urgencies[in.Urgency] {
		v.Add("urgency", "must be low, normal, high or urgent")
	}
	if len(in.AttachmentIDs) > MaxAttachments {
		v.Add("attachment_ids", "at most 20 attachments")
	}
	if len(in.DedupKey) > 256 || len(in.CorrelationID) > 256 {
		v.Add("dedup_key", "dedup and correlation keys are at most 256 bytes")
	}
	if in.Trusted == nil {
		in.Trusted = map[string]any{}
	}
	return v.Err()
}

// derivedTitle uses the first non-empty line of a Markdown body (without heading markers), shortened.
func derivedTitle(body, contentType string) string {
	if contentType == JSON {
		return "JSON message"
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#>*-+ "))
		if line != "" {
			if r := []rune(line); len(r) > 120 {
				return string(r[:119]) + "…"
			}
			return line
		}
	}
	return ""
}

// Create adds an item to a space's inbox. When the source's dedup key was already used, the existing item is returned
// with created=false and nothing changes (duplicate deliveries are harmless).
func (svc *Service) Create(ctx context.Context, actor audit.Actor, spaceID string, in CreateInput) (*Item, bool, error) {
	if err := in.normalize(); err != nil {
		return nil, false, err
	}
	trusted, err := json.Marshal(in.Trusted)
	if err != nil {
		return nil, false, err
	}
	id := ids.New(ids.InboxItem)
	created := false
	err = svc.s.W().TxNamed(ctx, "inbox.create", func(tx *store.Tx) error {
		created = false
		sp, err := tx.Q.GetSpace(ctx, spaceID)
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodeNotFound, "space not found")
		} else if err != nil {
			return err
		}
		if sp.State == "archived" {
			return apperr.New(apperr.CodePrecondition, "the space is archived")
		}
		n, err := tx.Q.InsertInboxItem(ctx, db.InsertInboxItemParams{ID: id, SpaceID: spaceID, SourceKind: in.SourceKind, SourceID: in.SourceID,
			SourceLabel: in.SourceLabel, Title: in.Title, ContentType: in.ContentType, Body: in.Body, PayloadRef: in.PayloadRef,
			CorrelationID: in.CorrelationID, DedupKey: in.DedupKey, TrustedJson: string(trusted), SubscriptionID: in.Subscription,
			Urgency: in.Urgency, ReceivedAt: tx.NowMicros(), CreatedBy: actor.ID, UpdatedAt: tx.NowMicros()})
		if err != nil {
			return err
		}
		if n == 0 {
			existing, err := tx.Q.GetInboxItemByDedup(ctx, db.GetInboxItemByDedupParams{SpaceID: spaceID, SourceKind: in.SourceKind, DedupKey: in.DedupKey})
			if err != nil {
				return err
			}
			id = existing.ID
			return nil
		}
		created = true
		if err := attachments.Reference(ctx, tx, spaceID, OwnerKind, id, in.AttachmentIDs); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "inbox_item", id, "created", 1); err != nil {
			return err
		}
		corr := outbox.CleanID(in.CorrelationID)
		if corr == "" {
			corr = id
		}
		if _, err := svc.Outbox.Enqueue(ctx, tx, outbox.Event{Payload: outbox.InboxItemCreated{SpaceID: spaceID, ItemID: id, SourceKind: in.SourceKind,
			SourceID: outbox.CleanID(in.SourceID), SubscriptionID: outbox.CleanID(in.Subscription), Urgency: in.Urgency}, Correlation: corr,
			Causation: in.CausationID}); err != nil {
			return err
		}
		// Event ingestion (the system actor) is recorded per delivery instead; people, tokens and agents are audited.
		if actor.Type != audit.ActorSystem {
			_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "inbox.item_created", Target: audit.Target{Type: "inbox_item", ID: id},
				Details: map[string]any{"space_id": spaceID, "source_kind": in.SourceKind, "content_type": in.ContentType, "attachments": len(in.AttachmentIDs)}})
		}
		return err
	})
	if err != nil {
		return nil, false, err
	}
	it, err := svc.Get(ctx, spaceID, id)
	if err == nil && created && svc.OnCreated != nil {
		svc.OnCreated(it)
	}
	return it, created, err
}

func fromRow(r db.InboxItem) *Item {
	it := &Item{ID: r.ID, SpaceID: r.SpaceID, SourceKind: r.SourceKind, SourceID: r.SourceID, SourceLabel: r.SourceLabel, Title: r.Title,
		ContentType: r.ContentType, Body: r.Body, PayloadRef: r.PayloadRef, CorrelationID: r.CorrelationID, DedupKey: r.DedupKey,
		Subscription: r.SubscriptionID, Urgency: r.Urgency, Status: r.Status, StatusReason: r.StatusReason, LinkedKind: r.LinkedKind,
		LinkedID: r.LinkedID, ReceivedAt: time.UnixMicro(r.ReceivedAt).UTC(), UpdatedAt: time.UnixMicro(r.UpdatedAt).UTC(), CreatedBy: r.CreatedBy,
		Version: r.Version, Trusted: map[string]any{}}
	_ = json.Unmarshal([]byte(r.TrustedJson), &it.Trusted)
	if r.DeferUntil.Valid {
		t := time.UnixMicro(r.DeferUntil.Int64).UTC()
		it.DeferUntil = &t
	}
	return it
}

// Get returns an item of a space with its attachments.
func (svc *Service) Get(ctx context.Context, spaceID, id string) (*Item, error) {
	r, err := svc.s.R().GetInboxItem(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && r.SpaceID != spaceID) {
		return nil, errNotFound
	} else if err != nil {
		return nil, err
	}
	it := fromRow(r)
	if it.Attachments, err = svc.att.ListByOwner(ctx, OwnerKind, id); err != nil {
		return nil, err
	}
	return it, nil
}

// Page is a page of items, newest first.
type Page struct {
	Items      []*Item `json:"items"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

// List returns a page of a space's inbox, optionally filtered by status.
func (svc *Service) List(ctx context.Context, spaceID, status, cursor string, limit int) (*Page, error) {
	if status != "" && !statuses[status] {
		return nil, apperr.New(apperr.CodeInvalid, "unknown status %q", status)
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var beforeAt int64
	var beforeID string
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		at, id, ok := strings.Cut(string(raw), ".")
		if err != nil || !ok {
			return nil, apperr.New(apperr.CodeInvalid, "invalid cursor")
		}
		if beforeAt, err = strconv.ParseInt(at, 10, 64); err != nil {
			return nil, apperr.New(apperr.CodeInvalid, "invalid cursor")
		}
		beforeID = id
	}
	rows, err := svc.s.R().ListInboxItems(ctx, db.ListInboxItemsParams{SpaceID: spaceID, Status: status, BeforeAt: beforeAt, BeforeID: beforeID,
		Lim: int64(limit + 1)})
	if err != nil {
		return nil, err
	}
	p := &Page{Items: []*Item{}}
	for i, r := range rows {
		if i == limit {
			last := rows[limit-1]
			p.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(last.ReceivedAt, 10) + "." + last.ID))
			break
		}
		p.Items = append(p.Items, fromRow(r))
	}
	return p, nil
}

// Counts returns the number of items per status.
func (svc *Service) Counts(ctx context.Context, spaceID string) (map[string]int64, error) {
	rows, err := svc.s.R().CountInboxByStatus(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{StatusNew: 0, StatusInProgress: 0, StatusDeferred: 0, StatusRejected: 0, StatusDone: 0}
	for _, r := range rows {
		out[r.Status] = r.N
	}
	return out, nil
}

// StatusInput changes an item's processing status.
type StatusInput struct {
	Status     string
	Reason     string
	DeferUntil *time.Time
	LinkedKind string
	LinkedID   string
}

// SetStatus changes an item's status, based on the version the caller saw (a concurrent change is a conflict).
func (svc *Service) SetStatus(ctx context.Context, actor audit.Actor, spaceID, id string, version int64, in StatusInput) (*Item, error) {
	v := &apperr.Validation{}
	if !statuses[in.Status] {
		v.Add("status", "unknown status")
	}
	if in.Status == StatusDeferred && in.DeferUntil == nil {
		v.Add("defer_until", "is required to defer")
	}
	if in.Status != StatusDeferred && in.DeferUntil != nil {
		v.Add("defer_until", "is only valid when deferring")
	}
	if (in.LinkedKind == "") != (in.LinkedID == "") {
		v.Add("linked_id", "linked_kind and linked_id go together")
	}
	if len(in.Reason) > 2000 {
		v.Add("reason", "at most 2000 characters")
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	err := svc.s.W().TxNamed(ctx, "inbox.status", func(tx *store.Tx) error {
		r, err := tx.Q.GetInboxItem(ctx, id)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && r.SpaceID != spaceID) {
			return errNotFound
		} else if err != nil {
			return err
		}
		if r.Version != version {
			return apperr.New(apperr.CodePrecondition, "the item changed since version %d (now %d)", version, r.Version)
		}
		var defer_ sql.NullInt64
		if in.DeferUntil != nil {
			defer_ = sql.NullInt64{Int64: in.DeferUntil.UnixMicro(), Valid: true}
		}
		if _, err := tx.Q.UpdateInboxStatus(ctx, db.UpdateInboxStatusParams{Status: in.Status, StatusReason: in.Reason, DeferUntil: defer_,
			LinkedKind: in.LinkedKind, LinkedID: in.LinkedID, Now: tx.NowMicros(), ID: id, Version: version}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "inbox_item", id, "updated", version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "inbox.item_status", Target: audit.Target{Type: "inbox_item", ID: id},
			Details: map[string]any{"space_id": spaceID, "from": r.Status, "to": in.Status, "linked_kind": in.LinkedKind, "linked_id": in.LinkedID}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, spaceID, id)
}
