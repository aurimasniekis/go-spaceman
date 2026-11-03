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

// SourceHumanResponse is the source kind of an item that reports a final response to a human action, created when the
// action type's human policy says so (response_inbox, ADR-0049). Its body is metadata only: the response itself stays
// on the action, readable by the runs it concerns (SM-HUM-009).
const SourceHumanResponse = "human_response"

// OwnerKind is the attachment owner kind of inbox items.
const OwnerKind = "inbox_item"

// Limits.
const (
	MaxBody        = 200_000
	MaxTitle       = 200
	MaxAttachments = 20
)

var (
	sourceKinds = map[string]bool{"operator": true, "worker": true, "human_response": true, "dreamer": true, "kb": true, "cross_space": true, "event": true, "webhook": true, "schedule": true}
	urgencies   = map[string]bool{"low": true, "normal": true, "high": true, "urgent": true}
	statuses    = map[string]bool{StatusNew: true, StatusInProgress: true, StatusDeferred: true, StatusRejected: true, StatusDone: true}
)

// Item is one inbox item.
type Item struct {
	ID            string         `json:"id"`
	SpaceID       string         `json:"space_id"`
	SourceKind    string         `json:"source_kind" enum:"operator,worker,human_response,dreamer,kb,cross_space,event,webhook,schedule"`
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
	// NoticeDeferred: control has not been told about the item yet because its subscription defers notices while the
	// space is paused; it is told when the space resumes (SM-EVT-014, ADR-0049).
	NoticeDeferred bool `json:"notice_deferred" doc:"Control is told about this item when the paused space resumes (its subscription's while_paused policy is defer)"`
	// AutomationDepth is how many SpaceMan event ingestions led to the item (ADR-0049).
	AutomationDepth int64     `json:"automation_depth" doc:"Automation depth: how many SpaceMan event ingestions led to this item (0: not from an event). Bounded by the limit event_depth (SM-EVT-016)."`
	ReceivedAt      time.Time `json:"received_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	CreatedBy       string    `json:"created_by"`
	Version         int64     `json:"version"`
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
	// AutomationDepth is the item's automation depth (ADR-0049): the inbound event's spaceman-depth plus one for an event, else 0.
	AutomationDepth int64
	// DeferNoticeWhilePaused holds back control's notification while the space is paused (decided in the creating
	// transaction, against the space's state at that moment); it is sent when the space resumes (SM-EVT-014).
	DeferNoticeWhilePaused bool
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
	if in.AutomationDepth < 0 {
		v.Add("depth", "must not be negative")
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
	var id string
	created := false
	err := svc.s.W().TxNamed(ctx, "inbox.create", func(tx *store.Tx) error {
		var err error
		id, created, err = svc.create(ctx, tx, actor, spaceID, in)
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

// CreateTx adds an item inside the caller's transaction (another service's change that must land together with the
// item, such as a cross-space request). OnCreated runs after the commit.
func (svc *Service) CreateTx(ctx context.Context, tx *store.Tx, actor audit.Actor, spaceID string, in CreateInput) (string, bool, error) {
	if err := in.normalize(); err != nil {
		return "", false, err
	}
	id, created, err := svc.create(ctx, tx, actor, spaceID, in)
	if err == nil && created && svc.OnCreated != nil {
		base := context.WithoutCancel(ctx)
		tx.AfterCommit(func() {
			if it, err := svc.Get(base, spaceID, id); err == nil {
				svc.OnCreated(it)
			}
		})
	}
	return id, created, err
}

func (svc *Service) create(ctx context.Context, tx *store.Tx, actor audit.Actor, spaceID string, in CreateInput) (string, bool, error) {
	trusted, err := json.Marshal(in.Trusted)
	if err != nil {
		return "", false, err
	}
	id := ids.New(ids.InboxItem)
	sp, err := tx.Q.GetSpace(ctx, spaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, apperr.New(apperr.CodeNotFound, "space not found")
	} else if err != nil {
		return "", false, err
	}
	if sp.State == "archived" {
		return "", false, apperr.New(apperr.CodePrecondition, "the space is archived")
	}
	deferred := in.DeferNoticeWhilePaused && sp.State == "paused"
	n, err := tx.Q.InsertInboxItem(ctx, db.InsertInboxItemParams{ID: id, SpaceID: spaceID, SourceKind: in.SourceKind, SourceID: in.SourceID,
		SourceLabel: in.SourceLabel, Title: in.Title, ContentType: in.ContentType, Body: in.Body, PayloadRef: in.PayloadRef,
		CorrelationID: in.CorrelationID, DedupKey: in.DedupKey, TrustedJson: string(trusted), SubscriptionID: in.Subscription,
		Urgency: in.Urgency, ReceivedAt: tx.NowMicros(), CreatedBy: actor.ID, UpdatedAt: tx.NowMicros(), NoticeDeferred: b2i(deferred), Depth: in.AutomationDepth})
	if err != nil {
		return "", false, err
	}
	if n == 0 {
		existing, err := tx.Q.GetInboxItemByDedup(ctx, db.GetInboxItemByDedupParams{SpaceID: spaceID, SourceKind: in.SourceKind, DedupKey: in.DedupKey})
		if err != nil {
			return "", false, err
		}
		return existing.ID, false, nil
	}
	if err := attachments.Reference(ctx, tx, spaceID, OwnerKind, id, in.AttachmentIDs); err != nil {
		return "", false, err
	}
	if err := svc.changes.Record(ctx, tx, spaceID, "inbox_item", id, "created", 1); err != nil {
		return "", false, err
	}
	corr := outbox.CleanID(in.CorrelationID)
	if corr == "" {
		corr = id
	}
	if _, err := svc.Outbox.Enqueue(ctx, tx, outbox.Event{Payload: outbox.InboxItemCreated{SpaceID: spaceID, ItemID: id, SourceKind: in.SourceKind,
		SourceID: outbox.CleanID(in.SourceID), SubscriptionID: outbox.CleanID(in.Subscription), Urgency: in.Urgency}, Correlation: corr,
		Causation: in.CausationID, Depth: in.AutomationDepth}); err != nil {
		return "", false, err
	}
	// Event ingestion (the system actor) is recorded per delivery instead; people, tokens and agents are audited.
	if actor.Type != audit.ActorSystem {
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "inbox.item_created", Target: audit.Target{Type: "inbox_item", ID: id},
			Details: map[string]any{"space_id": spaceID, "source_kind": in.SourceKind, "content_type": in.ContentType, "attachments": len(in.AttachmentIDs)}})
	}
	return id, true, err
}

func fromRow(r db.InboxItem) *Item {
	it := &Item{ID: r.ID, SpaceID: r.SpaceID, SourceKind: r.SourceKind, SourceID: r.SourceID, SourceLabel: r.SourceLabel, Title: r.Title,
		ContentType: r.ContentType, Body: r.Body, PayloadRef: r.PayloadRef, CorrelationID: r.CorrelationID, DedupKey: r.DedupKey,
		Subscription: r.SubscriptionID, Urgency: r.Urgency, Status: r.Status, StatusReason: r.StatusReason, LinkedKind: r.LinkedKind,
		LinkedID: r.LinkedID, ReceivedAt: time.UnixMicro(r.ReceivedAt).UTC(), UpdatedAt: time.UnixMicro(r.UpdatedAt).UTC(), CreatedBy: r.CreatedBy,
		Version: r.Version, Trusted: map[string]any{}, NoticeDeferred: r.NoticeDeferred == 1, AutomationDepth: r.Depth}
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

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// maxDeferred bounds one round of deferred notices; a space resuming with more is handled again until none is left.
const maxDeferred = 500

// DeferredNotices returns the items of a space whose control notification waits for the space to resume, oldest first.
func (svc *Service) DeferredNotices(ctx context.Context, spaceID string) ([]*Item, error) {
	rows, err := svc.s.R().ListDeferredInboxNotices(ctx, db.ListDeferredInboxNoticesParams{SpaceID: spaceID, Limit: maxDeferred})
	if err != nil {
		return nil, err
	}
	out := make([]*Item, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromRow(r))
	}
	return out, nil
}

// SpacesWithDeferredNotices lists the active spaces that still have deferred notices (a resume that central did not
// finish handling, for example because it stopped).
func (svc *Service) SpacesWithDeferredNotices(ctx context.Context) ([]string, error) {
	return svc.s.R().ListSpacesWithDeferredNotices(ctx)
}

// NoticeSent clears an item's deferred notice once control was told about it. The item's version does not change: a
// notice is not an edit, and control's status changes must not conflict with it.
func (svc *Service) NoticeSent(ctx context.Context, it *Item) error {
	return svc.s.W().TxNamed(ctx, "inbox.notice_sent", func(tx *store.Tx) error {
		n, err := tx.Q.ClearInboxNoticeDeferred(ctx, it.ID)
		if err != nil || n == 0 {
			return err
		}
		return svc.changes.Record(ctx, tx, it.SpaceID, "inbox_item", it.ID, "updated", it.Version)
	})
}
