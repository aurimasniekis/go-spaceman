// Package human is the human-action queue (SM-HUM-*): requests that need a person (questions, permission approvals,
// to-dos and messages), separate from the inbox, the execution queue and agent messaging. Lifecycle (status) is separate
// from outcome; finalization is atomic and optimistic; originals are kept and corrections are explicit follow-ups. A
// response creates a durable notification for the requester, and blockers affect only the related task.
//
// An approval action on its own records a decision (advisory). When an approval policy gates an operation, the
// approvals package creates the action and turns its decision into an enforced, digest-bound approval (ADR-0024),
// through OnFinalized.
package human

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/attachments"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/schemautil"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Types, statuses and outcomes (SM-HUM-003/005).
const (
	TypeQuestion = "question"
	TypeApproval = "approval"
	TypeTodo     = "todo"
	TypeMessage  = "message"

	Open       = "open"
	InProgress = "in_progress"
	Resolved   = "resolved"
	Cancelled  = "cancelled"
	Expired    = "expired"
)

// Limits (SM-HUM-011): the defaults of the pending limits; spaces override them through the limits model (ADR-0036)
// within MaxPendingCeiling.
const (
	MaxPendingPerSpace     = 500
	MaxPendingPerRequester = 25
	MaxPendingCeiling      = 5000
	gap                    = 1024
)

// QuestionRequest asks a person something.
type QuestionRequest struct {
	Options   []string `json:"options,omitempty" maxItems:"20" doc:"Offered answers"`
	AllowText bool     `json:"allow_text" doc:"Free-text answers are allowed (always true without options)"`
}

// ApprovalRequest asks for permission to do something.
type ApprovalRequest struct {
	Action  string `json:"action" maxLength:"500" doc:"What would be done"`
	Details string `json:"details,omitempty" maxLength:"20000"`
}

// TodoRequest asks a person to do something outside SpaceMan.
type TodoRequest struct {
	Steps []string `json:"steps,omitempty" maxItems:"50"`
}

// MessageRequest informs a person; with RequiresAck it is action-required.
type MessageRequest struct {
	RequiresAck bool `json:"requires_ack"`
}

// Response is a person's answer.
type Response struct {
	Answer string          `json:"answer,omitempty" maxLength:"50000"`
	Note   string          `json:"note,omitempty" maxLength:"20000"`
	Data   json.RawMessage `json:"data,omitempty" doc:"Custom types: the response, validated against the type version's response schema"`
}

// Action is one human action (SM-HUM-004).
type Action struct {
	ID               string          `json:"id"`
	SpaceID          string          `json:"space_id"`
	Type             string          `json:"type" enum:"question,approval,todo,message,custom"`
	TypeVersion      int64           `json:"type_version" doc:"For custom types, the type version number the action was created with"`
	TypeVersionID    string          `json:"type_version_id,omitempty"`
	TypeSlug         string          `json:"type_slug,omitempty" doc:"For custom types, the catalog type"`
	Title            string          `json:"title"`
	Body             string          `json:"body" doc:"Markdown, untrusted when an agent wrote it"`
	Request          json.RawMessage `json:"request"`
	RequesterKind    string          `json:"requester_kind" enum:"agent,operator,system"`
	RequesterID      string          `json:"requester_id,omitempty"`
	RequesterRole    string          `json:"requester_role,omitempty"`
	RequesterRun     string          `json:"requester_run,omitempty"`
	RequesterSession string          `json:"requester_session,omitempty"`
	RequesterLabel   string          `json:"requester_label,omitempty"`
	TaskID           string          `json:"task_id,omitempty"`
	InboxItemID      string          `json:"inbox_item_id,omitempty"`
	CorrelationID    string          `json:"correlation_id,omitempty"`
	DedupKey         string          `json:"dedup_key,omitempty"`
	BlocksTask       bool            `json:"blocks_task"`
	Priority         string          `json:"priority" enum:"low,normal,high,urgent"`
	Rank             int64           `json:"rank"`
	DueAt            *time.Time      `json:"due_at,omitempty"`
	ExpiresAt        *time.Time      `json:"expires_at,omitempty"`
	Assignee         string          `json:"assignee,omitempty"`
	Status           string          `json:"status" enum:"open,in_progress,resolved,cancelled,expired"`
	Outcome          string          `json:"outcome,omitempty" doc:"answered, approved, denied, completed or acknowledged; custom types define their own"`
	Response         json.RawMessage `json:"response"`
	RespondedBy      string          `json:"responded_by,omitempty"`
	RespondedAt      *time.Time      `json:"responded_at,omitempty"`
	StatusReason     string          `json:"status_reason,omitempty"`
	FollowUpOf       string          `json:"follow_up_of,omitempty"`
	ReminderCount    int64           `json:"reminder_count" doc:"How often people were reminded (SM-HUM-011)"`
	RemindedAt       *time.Time      `json:"reminded_at,omitempty"`
	NextReminderAt   *time.Time      `json:"next_reminder_at,omitempty"`
	EscalateAt       *time.Time      `json:"escalate_at,omitempty" doc:"When the action escalates if nobody resolves it"`
	EscalatedAt      *time.Time      `json:"escalated_at,omitempty"`
	EscalatedFrom    string          `json:"escalated_from,omitempty" doc:"The priority before escalation"`
	ResponseHidden   bool            `json:"response_hidden,omitempty" doc:"Agent tools: the response, comments and reason are hidden from runs the action is not related to"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
	Version          int64           `json:"version"`
	// TypeSpec is the captured custom type version (schemas, safe UI metadata, outcomes); set by Get.
	TypeSpec    *TypeSpec                 `json:"type_spec,omitempty"`
	Attachments []*attachments.Attachment `json:"attachments,omitempty" doc:"Files the request references; set by Get"`
	// ResponseAttachments are files the response references; set by Get.
	ResponseAttachments []*attachments.Attachment `json:"response_attachments,omitempty"`
	Comments            []Comment                 `json:"comments,omitempty"`
	Notifications       []Notification            `json:"notifications,omitempty"`
}

// Comment is discussion on an action; it is never a response.
type Comment struct {
	ID          string                    `json:"id"`
	Kind        string                    `json:"kind" enum:"comment,clarification,reply" doc:"clarification: a person asked the requester for more information; reply: the requester answered"`
	Body        string                    `json:"body"`
	Author      string                    `json:"author"`
	AuthorLabel string                    `json:"author_label,omitempty"`
	CreatedAt   time.Time                 `json:"created_at"`
	Attachments []*attachments.Attachment `json:"attachments,omitempty"`
}

// Notification is a durable, correlated notice of an outcome to the requester (SM-HUM-009), with where it is routed
// and why.
type Notification struct {
	ID             string     `json:"id"`
	Kind           string     `json:"kind" enum:"resolved,cancelled,expired,clarification" doc:"clarification: a person asked the requester for more information; the action stays pending"`
	TargetKind     string     `json:"target_kind" enum:"run,instance,control,operators" doc:"run: one run; instance: the next run of the requester's agent instance; control: the space's control; operators: no agent can take it (undeliverable)"`
	TargetRun      string     `json:"target_run,omitempty"`
	TargetRole     string     `json:"target_role,omitempty"`
	TargetInstance string     `json:"target_instance,omitempty"`
	HoldUntil      *time.Time `json:"hold_until,omitempty" doc:"instance: rerouted when no run of the instance starts before this"`
	OriginalRun    string     `json:"original_run,omitempty" doc:"The requester's run"`
	RoutedFrom     string     `json:"routed_from,omitempty" doc:"The previous target (run:<id>, instance:<id>, control, operators)"`
	RouteReason    string     `json:"route_reason,omitempty" enum:",requester_ended,run_ended,hold_expired,operator,too_many_routes"`
	RoutedAt       *time.Time `json:"routed_at,omitempty"`
	RouteCount     int64      `json:"route_count,omitempty"`
	Wake           bool       `json:"wake" doc:"An idle target is woken with a terminal notice"`
	State          string     `json:"state" enum:"pending,delivered,acknowledged"`
	CreatedAt      time.Time  `json:"created_at"`
	Delivered      *time.Time `json:"delivered_at,omitempty"`
	Acknowledged   *time.Time `json:"acknowledged_at,omitempty"`
}

// Requester identifies who asked (set by the authenticated caller, never by the request body).
type Requester struct {
	Kind, ID, Role, Run, Session, Label string
}

// CreateInput is a new human action.
type CreateInput struct {
	Type          string
	TypeSlug      string // custom types: the catalog type enabled in the space
	Title, Body   string
	Request       json.RawMessage
	Requester     Requester
	TaskID        string
	InboxItemID   string
	CorrelationID string
	DedupKey      string
	BlocksTask    bool
	Priority      string
	DueAt         *time.Time
	ExpiresAt     *time.Time
	FollowUpOf    string
	// AttachmentIDs are attachments of the space the request references (SM-ATT-001).
	AttachmentIDs []string
}

// Attachment owner kinds (SM-ATT-001): the request, the response and each comment reference their own attachments,
// which are served with the space's attachment permission (SM-ATT-004).
const (
	OwnerAction   = "human_action"
	OwnerResponse = "human_response"
	OwnerComment  = "human_comment"
	// MaxAttachments bounds the attachments of one request, response or comment.
	MaxAttachments = 20
)

// Service manages human actions.
type Service struct {
	s       *store.Store
	audit   *audit.Writer
	changes *changefeed.Recorder
	clock   clock.Clock
	// OnNotify is called after a commit that stored or routed a notification for an agent (wakes the notifier).
	OnNotify func()
	// Log receives the deadline loop's warnings and counts (never action content).
	Log *slog.Logger
	// wake kicks the deadline loop (Run).
	wake chan struct{}
	// OnFinalized is called inside the transaction that finalizes an action (enforced approvals record the decision).
	OnFinalized func(ctx context.Context, tx *store.Tx, r db.HumanAction, status, outcome string, actor audit.Actor) error
	// Outbox records lifecycle events (safe metadata only) for publication to Pulse (SM-OBS-004, SM-HUI-007). Nil:
	// nothing is published.
	Outbox *outbox.Outbox
	// PendingLimits returns a space's effective pending limits (per space, per requesting run) inside the creating
	// transaction (the limits model, ADR-0036). Nil: MaxPendingPerSpace and MaxPendingPerRequester.
	PendingLimits func(ctx context.Context, q *db.Queries, spaceID string) (perSpace, perRun int64)
	// Inbox receives the inbox items of responses when a policy's response_inbox asks for them (SM-INBOX-001,
	// ADR-0049). Nil: responses are notifications only.
	Inbox *inbox.Service
}

func (svc *Service) pendingLimits(ctx context.Context, q *db.Queries, spaceID string) (int64, int64) {
	if svc.PendingLimits == nil {
		return MaxPendingPerSpace, MaxPendingPerRequester
	}
	a, b := svc.PendingLimits(ctx, q, spaceID)
	if a <= 0 || a > MaxPendingCeiling {
		a = MaxPendingPerSpace
	}
	if b <= 0 || b > MaxPendingCeiling {
		b = MaxPendingPerRequester
	}
	return a, b
}

// meta is the safe metadata of an action for outbound events: identifiers, type, status, outcome and priority only,
// never the title, body, request, response, comments or reasons.
func (svc *Service) meta(r db.HumanAction, status, outcome string) outbox.HumanAction {
	return outbox.HumanAction{SpaceID: r.SpaceID, ActionID: r.ID, Type: r.Type, TypeSlug: outbox.CleanID(r.TypeSlug),
		TypeVersion: r.TypeVersion, Status: status, Outcome: outbox.CleanID(outcome), Priority: r.Priority, RequesterKind: r.RequesterKind,
		TaskID: r.TaskID, BlocksTask: r.BlocksTask == 1, FollowUpOf: r.FollowUpOf}
}

// event records the lifecycle event of r entering status with outcome.
func (svc *Service) event(ctx context.Context, tx *store.Tx, r db.HumanAction, status, outcome, causation string) error {
	p := outbox.HumanEvent(svc.meta(r, status, outcome))
	if p == nil {
		return nil
	}
	return svc.publish(ctx, tx, p, r, causation)
}

// publish enqueues an outbound event about r, correlated by its correlation ID, task or ID.
func (svc *Service) publish(ctx context.Context, tx *store.Tx, p outbox.Payload, r db.HumanAction, causation string) error {
	corr := outbox.CleanID(r.CorrelationID)
	if corr == "" {
		corr = r.TaskID
	}
	if corr == "" {
		corr = r.ID
	}
	_, err := svc.Outbox.Enqueue(ctx, tx, outbox.Event{Payload: p, Correlation: corr, Causation: causation})
	return err
}

func (svc *Service) notified() {
	if svc.OnNotify != nil {
		svc.OnNotify()
	}
}

// New returns the service.
func New(s *store.Store, aw *audit.Writer, ch *changefeed.Recorder, c clock.Clock) *Service {
	if c == nil {
		c = clock.System{}
	}
	return &Service{s: s, audit: aw, changes: ch, clock: c, wake: make(chan struct{}, 1)}
}

var (
	errNotFound   = apperr.New(apperr.CodeNotFound, "human action not found")
	priorityNames = []string{"low", "normal", "high", "urgent"}
	json0         = json.RawMessage(`{}`)
)

// validateRequest checks and normalizes a type's request.
func validateRequest(typ string, raw json.RawMessage) (json.RawMessage, bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage(`{}`)
	}
	dec := func(v any) error {
		d := json.NewDecoder(strings.NewReader(string(raw)))
		d.DisallowUnknownFields()
		return d.Decode(v)
	}
	blocking := true
	var out any
	switch typ {
	case TypeQuestion:
		var r QuestionRequest
		if err := dec(&r); err != nil {
			return nil, false, err
		}
		if len(r.Options) > 20 {
			return nil, false, errors.New("at most 20 options")
		}
		if len(r.Options) == 0 {
			r.AllowText = true
		}
		out = r
	case TypeApproval:
		var r ApprovalRequest
		if err := dec(&r); err != nil {
			return nil, false, err
		}
		if strings.TrimSpace(r.Action) == "" {
			return nil, false, errors.New("approval requests name the action")
		}
		out = r
	case TypeTodo:
		var r TodoRequest
		if err := dec(&r); err != nil {
			return nil, false, err
		}
		out = r
	case TypeMessage:
		var r MessageRequest
		if err := dec(&r); err != nil {
			return nil, false, err
		}
		blocking = r.RequiresAck
		out = r
	default:
		return nil, false, fmt.Errorf("unknown type %q (question, approval, todo or message)", typ)
	}
	b, _ := json.Marshal(out)
	return b, blocking, nil
}

func nt(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixMicro(), Valid: true}
}

func tn(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMicro(v.Int64).UTC()
	return &t
}

func fromRow(r db.HumanAction) *Action {
	return &Action{ID: r.ID, SpaceID: r.SpaceID, Type: r.Type, TypeVersion: r.TypeVersion, TypeVersionID: r.TypeVersionID, TypeSlug: r.TypeSlug, Title: r.Title, Body: r.Body, Request: json.RawMessage(r.RequestJson),
		RequesterKind: r.RequesterKind, RequesterID: r.RequesterID, RequesterRole: r.RequesterRole, RequesterRun: r.RequesterRun, RequesterSession: r.RequesterSession,
		RequesterLabel: r.RequesterLabel, TaskID: r.TaskID, InboxItemID: r.InboxItemID, CorrelationID: r.CorrelationID, DedupKey: r.DedupKey, BlocksTask: r.BlocksTask == 1,
		Priority: r.Priority, Rank: r.Rank, DueAt: tn(r.DueAt), ExpiresAt: tn(r.ExpiresAt), Assignee: r.Assignee, Status: r.Status, Outcome: r.Outcome,
		Response: json.RawMessage(r.ResponseJson), RespondedBy: r.RespondedBy, RespondedAt: tn(r.RespondedAt), StatusReason: r.StatusReason, FollowUpOf: r.FollowUpOf,
		ReminderCount: r.ReminderCount, RemindedAt: tn(r.RemindedAt), NextReminderAt: tn(r.NextReminderAt), EscalateAt: tn(r.EscalateAt), EscalatedAt: tn(r.EscalatedAt),
		EscalatedFrom: r.EscalatedFrom,
		CreatedAt:     time.UnixMicro(r.CreatedAt).UTC(), UpdatedAt: time.UnixMicro(r.UpdatedAt).UTC(), Version: r.Version}
}

// Create adds a human action. The ID is durable immediately; a dedup key that exists returns the existing action
// (created=false). A blocking action on a task records a task blocker in the same transaction; informational messages
// never block (SM-HUM-008/010).
func (svc *Service) Create(ctx context.Context, actor audit.Actor, spaceID string, in CreateInput) (*Action, bool, error) {
	v := &apperr.Validation{}
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" || utf8.RuneCountInString(in.Title) > 200 {
		v.Add("title", "is required (at most 200 characters)")
	}
	if len(in.Body) > 100_000 {
		v.Add("body", "at most 100000 bytes")
	}
	if len(in.AttachmentIDs) > MaxAttachments {
		v.Add("attachment_ids", "at most %d attachments", MaxAttachments)
	}
	var req json.RawMessage
	canBlock := true
	if in.Type == TypeCustom {
		if in.TypeSlug == "" {
			v.Add("type_slug", "custom actions name their type")
		}
		if len(in.Request) == 0 || string(in.Request) == "null" {
			in.Request = json.RawMessage(`{}`)
		}
		req = in.Request
	} else {
		var err error
		if req, canBlock, err = validateRequest(in.Type, in.Request); err != nil {
			v.Add("request", "%v", err)
		}
	}
	if in.Priority != "" && !slices.Contains(priorityNames, in.Priority) {
		v.Add("priority", "must be low, normal, high or urgent")
	}
	if in.BlocksTask && in.TaskID == "" {
		v.Add("blocks_task", "needs task_id")
	}
	if in.BlocksTask && !canBlock {
		v.Add("blocks_task", "informational messages never block; set requires_ack to make it action-required")
	}
	if in.Requester.Kind != "agent" && in.Requester.Kind != "operator" && in.Requester.Kind != "system" {
		v.Add("requester", "unknown requester kind")
	}
	if in.ExpiresAt != nil && !in.ExpiresAt.After(svc.clock.Now()) {
		v.Add("expires_at", "must be in the future")
	}
	if err := v.Err(); err != nil {
		return nil, false, err
	}
	id := ids.New(ids.HumanAction)
	created := false
	typeVersion, typeVersionID := int64(1), ""
	err := svc.s.W().TxNamed(ctx, "human.create", func(tx *store.Tx) error {
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
		if in.DedupKey != "" {
			if ex, err := tx.Q.GetHumanActionByDedup(ctx, db.GetHumanActionByDedupParams{SpaceID: spaceID, DedupKey: in.DedupKey}); err == nil {
				id = ex.ID
				return nil
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		// The space's type permission decides which agents may ask with this type (SM-HUI-008).
		if err := checkAgentRequester(ctx, tx.Q, spaceID, &in); err != nil {
			return err
		}
		if in.Type == TypeCustom {
			st, err := spaceTypeBySlug(ctx, tx.Q, spaceID, in.TypeSlug)
			if err != nil {
				return err
			}
			sch, err := schemautil.CompileObject(st.RequestSchema)
			if err != nil {
				return err
			}
			if err := schemautil.Validate(sch, in.Request); err != nil {
				return apperr.New(apperr.CodeInvalid, "the request does not match %s v%d: %v", st.Slug, st.Number, err)
			}
			typeVersion, typeVersionID = st.Number, st.VersionID
			if in.Priority == "" {
				in.Priority = st.DefaultPriority
			}
			if in.ExpiresAt == nil && st.DefaultExpiryMinutes > 0 {
				e := svc.clock.Now().Add(time.Duration(st.DefaultExpiryMinutes) * time.Minute)
				in.ExpiresAt = &e
			}
		}
		if in.Priority == "" {
			in.Priority = "normal"
		}
		if in.TaskID != "" {
			t, err := tx.Q.GetTask(ctx, in.TaskID)
			if err != nil || t.SpaceID != spaceID {
				return apperr.New(apperr.CodeInvalid, "task %q is not a task of this space", in.TaskID)
			}
		}
		if in.FollowUpOf != "" {
			o, err := tx.Q.GetHumanAction(ctx, in.FollowUpOf)
			if err != nil || o.SpaceID != spaceID {
				return apperr.New(apperr.CodeInvalid, "follow_up_of %q is not an action of this space", in.FollowUpOf)
			}
		}
		pend, err := tx.Q.CountPendingHuman(ctx)
		if err != nil {
			return err
		}
		perSpace, _ := svc.pendingLimits(ctx, tx.Q, spaceID)
		for _, p := range pend {
			if p.SpaceID == spaceID && p.N >= perSpace {
				return apperr.New(apperr.CodeRateLimited, "this space has %d pending human actions (limit %d); resolve some first", p.N, perSpace)
			}
		}
		pol, err := policyFor(ctx, tx.Q, spaceID, in.Type)
		if err != nil {
			return err
		}
		if in.Requester.Kind == "agent" {
			if err := svc.limit(ctx, tx, spaceID, in, pol); err != nil {
				return err
			}
		}
		max, err := tx.Q.MaxHumanRank(ctx, spaceID)
		if err != nil {
			return err
		}
		remind, escalate := schedule(db.HumanAction{CreatedAt: tx.NowMicros()}, pol.PolicyFields)
		if err := tx.Q.InsertHumanAction(ctx, db.InsertHumanActionParams{NextReminderAt: remind, EscalateAt: escalate, ID: id, SpaceID: spaceID, Type: in.Type, TypeVersion: typeVersion, TypeVersionID: typeVersionID,
			TypeSlug: in.TypeSlug, Title: in.Title, Body: in.Body, RequestJson: string(req),
			RequesterKind: in.Requester.Kind, RequesterID: in.Requester.ID, RequesterRole: in.Requester.Role, RequesterRun: in.Requester.Run,
			RequesterSession: in.Requester.Session, RequesterLabel: in.Requester.Label, TaskID: in.TaskID, InboxItemID: in.InboxItemID, CorrelationID: in.CorrelationID,
			DedupKey: in.DedupKey, BlocksTask: b2i(in.BlocksTask), Priority: in.Priority, Rank: max + gap, DueAt: nt(in.DueAt), ExpiresAt: nt(in.ExpiresAt),
			FollowUpOf: in.FollowUpOf, CreatedAt: tx.NowMicros(), CreatedBy: actor.ID, UpdatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		if err := attachments.Reference(ctx, tx, spaceID, OwnerAction, id, in.AttachmentIDs); err != nil {
			return err
		}
		if in.BlocksTask {
			if err := tx.Q.InsertTaskBlocker(ctx, db.InsertTaskBlockerParams{ID: ids.New("blk"), TaskID: in.TaskID, Kind: "human_action", RefID: id,
				Note: in.Title, CreatedAt: tx.NowMicros(), CreatedBy: actor.ID}); err != nil {
				return err
			}
			if err := svc.changes.Record(ctx, tx, spaceID, "task", in.TaskID, "updated", 0); err != nil {
				return err
			}
		}
		created = true
		tx.AfterCommit(svc.Kick)
		if err := svc.changes.Record(ctx, tx, spaceID, "human_action", id, "created", 1); err != nil {
			return err
		}
		cause := in.InboxItemID
		if cause == "" {
			cause = in.Requester.Run
		}
		if err := svc.event(ctx, tx, db.HumanAction{ID: id, SpaceID: spaceID, Type: in.Type, TypeVersion: typeVersion, TypeSlug: in.TypeSlug,
			RequesterKind: in.Requester.Kind, TaskID: in.TaskID, CorrelationID: in.CorrelationID, BlocksTask: b2i(in.BlocksTask), Priority: in.Priority,
			FollowUpOf: in.FollowUpOf}, Open, "", cause); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "human_action.created", Target: audit.Target{Type: "human_action", ID: id},
			Details: map[string]any{"space_id": spaceID, "type": in.Type, "requester": in.Requester.Kind + ":" + in.Requester.Role, "task_id": in.TaskID, "blocks": in.BlocksTask}})
		return err
	})
	if err != nil {
		return nil, false, err
	}
	a, err := svc.Get(ctx, spaceID, id)
	return a, created, err
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// Get returns an action of a space with its comments and notifications.
func (svc *Service) Get(ctx context.Context, spaceID, id string) (*Action, error) {
	r, err := svc.s.R().GetHumanAction(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && r.SpaceID != spaceID) {
		return nil, errNotFound
	} else if err != nil {
		return nil, err
	}
	a := fromRow(r)
	if r.Type == TypeCustom {
		if tv, err := svc.s.R().GetHumanTypeVersion(ctx, r.TypeVersionID); err == nil {
			v := typeVersionOut(tv)
			a.TypeSpec = &TypeSpec{RequestSchema: v.RequestSchema, ResponseSchema: v.ResponseSchema, UI: v.UI, Outcomes: v.Outcomes}
		}
	}
	cs, err := svc.s.R().ListHumanComments(ctx, id)
	if err != nil {
		return nil, err
	}
	cids := make([]string, 0, len(cs))
	for _, c := range cs {
		cids = append(cids, c.ID)
	}
	byComment, err := attachments.ByOwners(ctx, svc.s.R(), OwnerComment, cids)
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		a.Comments = append(a.Comments, Comment{ID: c.ID, Kind: c.Kind, Body: c.Body, Author: c.Author, AuthorLabel: c.AuthorLabel,
			CreatedAt: time.UnixMicro(c.CreatedAt).UTC(), Attachments: byComment[c.ID]})
	}
	if a.Attachments, err = svc.attachmentsOf(ctx, OwnerAction, id); err != nil {
		return nil, err
	}
	if a.ResponseAttachments, err = svc.attachmentsOf(ctx, OwnerResponse, id); err != nil {
		return nil, err
	}
	ns, err := svc.s.R().ListHumanNotifications(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, n := range ns {
		a.Notifications = append(a.Notifications, Notification{ID: n.ID, Kind: n.Kind, TargetKind: n.TargetKind, TargetRun: n.TargetRun, TargetRole: n.TargetRole,
			TargetInstance: n.TargetInstance, HoldUntil: tn(n.HoldUntil), OriginalRun: n.OriginalRun, RoutedFrom: n.RoutedFrom, RouteReason: n.RouteReason,
			RoutedAt: tn(n.RoutedAt), RouteCount: n.RouteCount, Wake: n.Wake == 1, State: n.State, CreatedAt: time.UnixMicro(n.CreatedAt).UTC(),
			Delivered: tn(n.DeliveredAt), Acknowledged: tn(n.AcknowledgedAt)})
	}
	return a, nil
}

// attachmentsOf returns the attachments an owner references (nil when none, so the field is omitted).
func (svc *Service) attachmentsOf(ctx context.Context, kind, id string) ([]*attachments.Attachment, error) {
	rows, err := svc.s.R().ListAttachmentsByOwner(ctx, db.ListAttachmentsByOwnerParams{OwnerKind: kind, OwnerID: id})
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]*attachments.Attachment, 0, len(rows))
	for _, r := range rows {
		out = append(out, attachments.FromRow(r))
	}
	return out, nil
}

// limit enforces the pending limit and the rate limits of an agent requester (SM-HUM-011). The errors tell the agent
// that waiting is enough: it is notified of every response.
func (svc *Service) limit(ctx context.Context, tx *store.Tx, spaceID string, in CreateInput, pol Policy) error {
	const wait = "; you are notified when a person responds, so wait for that instead of asking again or polling"
	since := tx.Now.Add(-rateWindow).UnixMicro()
	if in.Requester.Run != "" {
		n, err := tx.Q.CountPendingHumanByRun(ctx, in.Requester.Run)
		if err != nil {
			return err
		}
		if _, perRun := svc.pendingLimits(ctx, tx.Q, spaceID); n >= perRun {
			return apperr.New(apperr.CodeRateLimited, "this run has %d pending human actions (limit %d)%s", n, perRun, wait)
		}
		n, err = tx.Q.CountHumanActionsByRunSince(ctx, db.CountHumanActionsByRunSinceParams{RequesterRun: in.Requester.Run, Type: in.Type, CreatedAt: since})
		if err != nil {
			return err
		}
		if n >= pol.RatePerRunHour {
			return apperr.New(apperr.CodeRateLimited, "this run asked %d %s actions in the last hour (limit %d, policy %s)%s", n, policyKey(in.Type),
				pol.RatePerRunHour, pol.Source, wait)
		}
	}
	n, err := tx.Q.CountAgentHumanActionsInSpaceSince(ctx, db.CountAgentHumanActionsInSpaceSinceParams{SpaceID: spaceID, Type: in.Type, CreatedAt: since})
	if err != nil {
		return err
	}
	if n >= pol.RatePerSpaceHour {
		return apperr.New(apperr.CodeRateLimited, "agents of this space asked %d %s actions in the last hour (limit %d, policy %s)%s", n, policyKey(in.Type),
			pol.RatePerSpaceHour, pol.Source, wait)
	}
	return nil
}
