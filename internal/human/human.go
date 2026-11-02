// Package human is the human-action queue (SM-HUM-*): requests that need a person (questions, permission approvals,
// to-dos and messages), separate from the inbox, the execution queue and agent messaging. Lifecycle (status) is separate
// from outcome; finalization is atomic and optimistic; originals are kept and corrections are explicit follow-ups. A
// response creates a durable notification for the requester, and blockers affect only the related task.
//
// In this version approvals record a decision; enforcement of approved operations (digest binding) arrives with the
// approval gateway (SM-APPR, M3). Until then an approval is advisory and the UI says so.
package human

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
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

// Limits (SM-HUM-011).
const (
	MaxPendingPerSpace     = 500
	MaxPendingPerRequester = 25
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
	Answer string `json:"answer,omitempty" maxLength:"50000"`
	Note   string `json:"note,omitempty" maxLength:"20000"`
}

// Action is one human action (SM-HUM-004).
type Action struct {
	ID               string          `json:"id"`
	SpaceID          string          `json:"space_id"`
	Type             string          `json:"type" enum:"question,approval,todo,message"`
	TypeVersion      int64           `json:"type_version"`
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
	Outcome          string          `json:"outcome,omitempty" enum:",answered,approved,denied,completed,acknowledged"`
	Response         json.RawMessage `json:"response"`
	RespondedBy      string          `json:"responded_by,omitempty"`
	RespondedAt      *time.Time      `json:"responded_at,omitempty"`
	StatusReason     string          `json:"status_reason,omitempty"`
	FollowUpOf       string          `json:"follow_up_of,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
	Version          int64           `json:"version"`
	Comments         []Comment       `json:"comments,omitempty"`
	Notifications    []Notification  `json:"notifications,omitempty"`
}

// Comment is discussion on an action; it is never a response.
type Comment struct {
	ID          string    `json:"id"`
	Body        string    `json:"body"`
	Author      string    `json:"author"`
	AuthorLabel string    `json:"author_label,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// Notification is a durable, correlated notice of an outcome to the requester (SM-HUM-009).
type Notification struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind" enum:"resolved,cancelled,expired"`
	TargetRun  string     `json:"target_run,omitempty"`
	TargetRole string     `json:"target_role,omitempty"`
	State      string     `json:"state" enum:"pending,delivered,acknowledged"`
	CreatedAt  time.Time  `json:"created_at"`
	Delivered  *time.Time `json:"delivered_at,omitempty"`
}

// Requester identifies who asked (set by the authenticated caller, never by the request body).
type Requester struct {
	Kind, ID, Role, Run, Session, Label string
}

// CreateInput is a new human action.
type CreateInput struct {
	Type          string
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
}

// Service manages human actions.
type Service struct {
	s       *store.Store
	audit   *audit.Writer
	changes *changefeed.Recorder
	clock   clock.Clock
}

// New returns the service.
func New(s *store.Store, aw *audit.Writer, ch *changefeed.Recorder, c clock.Clock) *Service {
	if c == nil {
		c = clock.System{}
	}
	return &Service{s: s, audit: aw, changes: ch, clock: c}
}

var (
	errNotFound   = apperr.New(apperr.CodeNotFound, "human action not found")
	priorityNames = []string{"low", "normal", "high", "urgent"}
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
	return &Action{ID: r.ID, SpaceID: r.SpaceID, Type: r.Type, TypeVersion: r.TypeVersion, Title: r.Title, Body: r.Body, Request: json.RawMessage(r.RequestJson),
		RequesterKind: r.RequesterKind, RequesterID: r.RequesterID, RequesterRole: r.RequesterRole, RequesterRun: r.RequesterRun, RequesterSession: r.RequesterSession,
		RequesterLabel: r.RequesterLabel, TaskID: r.TaskID, InboxItemID: r.InboxItemID, CorrelationID: r.CorrelationID, DedupKey: r.DedupKey, BlocksTask: r.BlocksTask == 1,
		Priority: r.Priority, Rank: r.Rank, DueAt: tn(r.DueAt), ExpiresAt: tn(r.ExpiresAt), Assignee: r.Assignee, Status: r.Status, Outcome: r.Outcome,
		Response: json.RawMessage(r.ResponseJson), RespondedBy: r.RespondedBy, RespondedAt: tn(r.RespondedAt), StatusReason: r.StatusReason, FollowUpOf: r.FollowUpOf,
		CreatedAt: time.UnixMicro(r.CreatedAt).UTC(), UpdatedAt: time.UnixMicro(r.UpdatedAt).UTC(), Version: r.Version}
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
	req, canBlock, err := validateRequest(in.Type, in.Request)
	if err != nil {
		v.Add("request", "%v", err)
	}
	if in.Priority == "" {
		in.Priority = "normal"
	}
	if !slices.Contains(priorityNames, in.Priority) {
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
	err = svc.s.W().TxNamed(ctx, "human.create", func(tx *store.Tx) error {
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
		for _, p := range pend {
			if p.SpaceID == spaceID && p.N >= MaxPendingPerSpace {
				return apperr.New(apperr.CodeRateLimited, "this space has %d pending human actions; resolve some first", p.N)
			}
		}
		if in.Requester.Kind == "agent" && in.Requester.Run != "" {
			n := 0
			rows, err := tx.Q.ListPendingHumanActions(ctx, 5000)
			if err != nil {
				return err
			}
			for _, r := range rows {
				if r.RequesterRun == in.Requester.Run {
					n++
				}
			}
			if n >= MaxPendingPerRequester {
				return apperr.New(apperr.CodeRateLimited, "this run has %d pending human actions", n)
			}
		}
		max, err := tx.Q.MaxHumanRank(ctx, spaceID)
		if err != nil {
			return err
		}
		if err := tx.Q.InsertHumanAction(ctx, db.InsertHumanActionParams{ID: id, SpaceID: spaceID, Type: in.Type, Title: in.Title, Body: in.Body, RequestJson: string(req),
			RequesterKind: in.Requester.Kind, RequesterID: in.Requester.ID, RequesterRole: in.Requester.Role, RequesterRun: in.Requester.Run,
			RequesterSession: in.Requester.Session, RequesterLabel: in.Requester.Label, TaskID: in.TaskID, InboxItemID: in.InboxItemID, CorrelationID: in.CorrelationID,
			DedupKey: in.DedupKey, BlocksTask: b2i(in.BlocksTask), Priority: in.Priority, Rank: max + gap, DueAt: nt(in.DueAt), ExpiresAt: nt(in.ExpiresAt),
			FollowUpOf: in.FollowUpOf, CreatedAt: tx.NowMicros(), CreatedBy: actor.ID, UpdatedAt: tx.NowMicros()}); err != nil {
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
		if err := svc.changes.Record(ctx, tx, spaceID, "human_action", id, "created", 1); err != nil {
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
	cs, err := svc.s.R().ListHumanComments(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		a.Comments = append(a.Comments, Comment{ID: c.ID, Body: c.Body, Author: c.Author, AuthorLabel: c.AuthorLabel, CreatedAt: time.UnixMicro(c.CreatedAt).UTC()})
	}
	ns, err := svc.s.R().ListHumanNotifications(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, n := range ns {
		a.Notifications = append(a.Notifications, Notification{ID: n.ID, Kind: n.Kind, TargetRun: n.TargetRun, TargetRole: n.TargetRole, State: n.State,
			CreatedAt: time.UnixMicro(n.CreatedAt).UTC(), Delivered: tn(n.DeliveredAt)})
	}
	return a, nil
}
