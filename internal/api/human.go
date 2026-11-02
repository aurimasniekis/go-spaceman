package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/human"
)

// HumanRules are the authorization rules of the human-action operations. The global list is filtered per space;
// approval decisions additionally need approval:decide in the action's space.
var HumanRules = map[string]Rule{
	"list-pending-human-actions": {Session: true},
	"list-human-actions":         InSpace(authz.HumanRead),
	"get-human-action":           InSpace(authz.HumanRead),
	"create-human-action":        InSpace(authz.HumanRespond),
	"respond-human-action":       InSpace(authz.HumanRespond),
	"assign-human-action":        InSpace(authz.HumanRespond),
	"cancel-human-action":        InSpace(authz.HumanRespond),
	"comment-human-action":       InSpace(authz.HumanRespond),
	"move-human-action":          InSpace(authz.HumanRespond),
}

// HumanActionPath identifies a human action.
type HumanActionPath struct {
	SpaceID  string `path:"space_id" maxLength:"64"`
	ActionID string `path:"action_id" maxLength:"64"`
}

// CreateHumanActionInput is an operator's new human action (agents create theirs through MCP tools).
type CreateHumanActionInput struct {
	Type          string          `json:"type" enum:"question,approval,todo,message"`
	Title         string          `json:"title" maxLength:"200"`
	Body          string          `json:"body,omitempty" maxLength:"100000" doc:"Markdown"`
	Request       json.RawMessage `json:"request,omitempty" doc:"Type-specific request: question {options, allow_text}, approval {action, details}, todo {steps}, message {requires_ack}"`
	TaskID        string          `json:"task_id,omitempty" maxLength:"64"`
	BlocksTask    bool            `json:"blocks_task,omitempty" doc:"Block the task until a person responds"`
	Priority      string          `json:"priority,omitempty" enum:"low,normal,high,urgent"`
	DueAt         *time.Time      `json:"due_at,omitempty"`
	ExpiresAt     *time.Time      `json:"expires_at,omitempty"`
	DedupKey      string          `json:"dedup_key,omitempty" maxLength:"256"`
	CorrelationID string          `json:"correlation_id,omitempty" maxLength:"256"`
	FollowUpOf    string          `json:"follow_up_of,omitempty" maxLength:"64" doc:"Correct an earlier action without changing it"`
}

// HumanActionList is a list of human actions.
type HumanActionList struct {
	Items []*human.Action `json:"items"`
}

// PendingHumanActions is the global view: pending actions across the spaces you can read, with counts per space.
type PendingHumanActions struct {
	Items  []*human.Action  `json:"items"`
	Counts map[string]int64 `json:"counts" doc:"Pending actions per space you can read"`
	Total  int64            `json:"total"`
}

type humanOut struct {
	ETag string `header:"ETag"`
	Body *human.Action
}

func humanResult(a *human.Action) *humanOut { return &humanOut{ETag: ETag(a.Version), Body: a} }

func actorLabel(ctx context.Context) string {
	if p := PrincipalFrom(ctx); p != nil && p.User != nil {
		return p.User.DisplayName
	}
	return "root"
}

// RegisterHuman registers the human-action operations.
func RegisterHuman(svc *human.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"human-actions"}
		huma.Register(api, huma.Operation{OperationID: "list-pending-human-actions", Method: http.MethodGet, Path: "/human-actions",
			Summary: "Pending human actions across your spaces", Description: "Only spaces where you have human_action:read are included.", Tags: tags},
			func(ctx context.Context, in *struct {
				Type  string `query:"type" enum:"question,approval,todo,message,"`
				Limit int    `query:"limit" minimum:"0" maximum:"500"`
			}) (*struct{ Body PendingHumanActions }, error) {
				p := PrincipalFrom(ctx)
				allowed := func(id string) bool { return d.Authz != nil && d.Authz.AllowedAll(p.Subjects(), id, authz.HumanRead) }
				l, err := svc.ListPending(ctx, allowed, human.ListFilter{Pending: true, Type: in.Type, Limit: in.Limit})
				if err != nil {
					return nil, d.fail(ctx, "human.list_pending", err)
				}
				all, err := svc.PendingCounts(ctx)
				if err != nil {
					return nil, d.fail(ctx, "human.list_pending", err)
				}
				out := PendingHumanActions{Items: l, Counts: map[string]int64{}}
				for id, n := range all {
					if allowed(id) {
						out.Counts[id] = n
						out.Total += n
					}
				}
				return &struct{ Body PendingHumanActions }{Body: out}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-human-actions", Method: http.MethodGet, Path: "/spaces/{space_id}/human-actions",
			Summary: "A space's human actions", Description: "Pending actions first in their order, then recent final ones.", Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				Pending bool   `query:"pending"`
				Type    string `query:"type" enum:"question,approval,todo,message,"`
				Limit   int    `query:"limit" minimum:"0" maximum:"500"`
			}) (*struct{ Body HumanActionList }, error) {
				l, err := svc.List(ctx, in.SpaceID, human.ListFilter{Pending: in.Pending, Type: in.Type, Limit: in.Limit})
				if err != nil {
					return nil, d.fail(ctx, "human.list", err)
				}
				return &struct{ Body HumanActionList }{Body: HumanActionList{Items: l}}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-human-action", Method: http.MethodGet, Path: "/spaces/{space_id}/human-actions/{action_id}",
			Summary: "A human action with its comments and notifications", Tags: tags},
			func(ctx context.Context, in *HumanActionPath) (*humanOut, error) {
				a, err := svc.Get(ctx, in.SpaceID, in.ActionID)
				if err != nil {
					return nil, d.fail(ctx, "human.get", err)
				}
				return humanResult(a), nil
			})
		huma.Register(api, huma.Operation{OperationID: "create-human-action", Method: http.MethodPost, Path: "/spaces/{space_id}/human-actions",
			Summary: "Ask a person for something", Description: "The requester is the signed-in operator. A repeated dedup key returns the original.",
			Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				SpacePath
				IdempotencyKey
				Body CreateHumanActionInput
			}) (*humanOut, error) {
				a, _, err := Idempotent(ctx, d, "create-human-action", in.IdempotencyKey, map[string]any{"space": in.SpaceID, "body": in.Body}, false, func() (*human.Action, error) {
					actor := Actor(ctx)
					b := in.Body
					a, _, err := svc.Create(ctx, actor, in.SpaceID, human.CreateInput{Type: b.Type, Title: b.Title, Body: b.Body, Request: b.Request,
						Requester: human.Requester{Kind: "operator", ID: actor.ID, Label: actorLabel(ctx)}, TaskID: b.TaskID, BlocksTask: b.BlocksTask,
						Priority: b.Priority, DueAt: b.DueAt, ExpiresAt: b.ExpiresAt, DedupKey: b.DedupKey, CorrelationID: b.CorrelationID, FollowUpOf: b.FollowUpOf})
					return a, err
				})
				if err != nil {
					return nil, d.fail(ctx, "human.create", err)
				}
				return humanResult(a), nil
			})
		huma.Register(api, huma.Operation{OperationID: "respond-human-action", Method: http.MethodPost, Path: "/spaces/{space_id}/human-actions/{action_id}/respond",
			Summary: "Respond to a human action",
			Description: "Outcomes depend on the type: question → answered, approval → approved or denied (needs approval:decide), todo → completed, " +
				"message → acknowledged. Responses are final; of concurrent responses exactly one wins.", Tags: tags},
			func(ctx context.Context, in *struct {
				HumanActionPath
				IfMatch
				Body struct {
					Outcome string `json:"outcome" enum:"answered,approved,denied,completed,acknowledged"`
					human.Response
				}
			}) (*humanOut, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "human.respond", err)
				}
				cur, err := svc.Get(ctx, in.SpaceID, in.ActionID)
				if err != nil {
					return nil, d.fail(ctx, "human.respond", err)
				}
				if cur.Type == human.TypeApproval {
					p := PrincipalFrom(ctx)
					if d.Authz == nil || !d.Authz.AllowedAll(p.Subjects(), in.SpaceID, authz.ApprovalDecide) {
						return nil, d.fail(ctx, "human.respond", apperr.New(apperr.CodeForbidden, "deciding approvals needs approval:decide"))
					}
				}
				a, err := svc.Respond(ctx, Actor(ctx), in.SpaceID, in.ActionID, v, human.RespondInput{Outcome: in.Body.Outcome, Response: in.Body.Response})
				if err != nil {
					return nil, d.fail(ctx, "human.respond", err)
				}
				return humanResult(a), nil
			})
		huma.Register(api, huma.Operation{OperationID: "assign-human-action", Method: http.MethodPost, Path: "/spaces/{space_id}/human-actions/{action_id}/assign",
			Summary: "Assign (claim) or unassign a human action", Description: "An empty assignee returns the action to open.", Tags: tags},
			func(ctx context.Context, in *struct {
				HumanActionPath
				IfMatch
				Body struct {
					Assignee string `json:"assignee" maxLength:"64" doc:"A user ID, or empty to unassign"`
				}
			}) (*humanOut, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "human.assign", err)
				}
				a, err := svc.Assign(ctx, Actor(ctx), in.SpaceID, in.ActionID, v, in.Body.Assignee)
				if err != nil {
					return nil, d.fail(ctx, "human.assign", err)
				}
				return humanResult(a), nil
			})
		huma.Register(api, huma.Operation{OperationID: "cancel-human-action", Method: http.MethodPost, Path: "/spaces/{space_id}/human-actions/{action_id}/cancel",
			Summary: "Withdraw a pending human action", Description: "A blocked task is unblocked; the requester is notified.", Tags: tags},
			func(ctx context.Context, in *struct {
				HumanActionPath
				IfMatch
				Body struct {
					Reason string `json:"reason,omitempty" maxLength:"2000"`
				}
			}) (*humanOut, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "human.cancel", err)
				}
				a, err := svc.Cancel(ctx, Actor(ctx), in.SpaceID, in.ActionID, v, in.Body.Reason)
				if err != nil {
					return nil, d.fail(ctx, "human.cancel", err)
				}
				return humanResult(a), nil
			})
		huma.Register(api, huma.Operation{OperationID: "comment-human-action", Method: http.MethodPost, Path: "/spaces/{space_id}/human-actions/{action_id}/comments",
			Summary: "Comment on a human action", Description: "Comments are discussion and never count as a response.", Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				HumanActionPath
				Body struct {
					Body string `json:"body" maxLength:"20000" doc:"Markdown"`
				}
			}) (*humanOut, error) {
				a, err := svc.AddComment(ctx, Actor(ctx), in.SpaceID, in.ActionID, in.Body.Body, actorLabel(ctx))
				if err != nil {
					return nil, d.fail(ctx, "human.comment", err)
				}
				return humanResult(a), nil
			})
		huma.Register(api, huma.Operation{OperationID: "move-human-action", Method: http.MethodPost, Path: "/spaces/{space_id}/human-actions/{action_id}/move",
			Summary: "Reorder a pending human action", Tags: tags},
			func(ctx context.Context, in *struct {
				HumanActionPath
				Body struct {
					Before string `json:"before,omitempty" maxLength:"64" doc:"Place before this pending action; empty places it last"`
				}
			}) (*humanOut, error) {
				a, err := svc.Move(ctx, Actor(ctx), in.SpaceID, in.ActionID, in.Body.Before)
				if err != nil {
					return nil, d.fail(ctx, "human.move", err)
				}
				return humanResult(a), nil
			})
	}
}
