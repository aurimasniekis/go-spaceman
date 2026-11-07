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
	"list-pending-human-actions": FilteredRead(),
	"list-human-actions":         InSpace(authz.HumanRead),
	"get-human-action":           InSpace(authz.HumanRead),
	"create-human-action":        InSpace(authz.HumanRespond),
	"respond-human-action":       InSpace(authz.HumanRespond),
	"assign-human-action":        InSpace(authz.HumanRespond),
	"cancel-human-action":        InSpace(authz.HumanRespond),
	"comment-human-action":       InSpace(authz.HumanRespond),
	"clarify-human-action":       InSpace(authz.HumanRespond),
	"move-human-action":          InSpace(authz.HumanRespond),
	"list-human-types":           Global(authz.HumanTypeRead),
	"get-human-type":             Global(authz.HumanTypeRead),
	"create-human-type":          Global(authz.HumanTypeWrite),
	"save-human-type-draft":      Global(authz.HumanTypeWrite),
	"publish-human-type":         Global(authz.HumanTypeWrite),
	"list-space-human-types":     InSpace(authz.HumanRead),
	"list-human-type-options":    InSpace(authz.SpaceConfigRead),
	"enable-space-human-type":    InSpace(authz.SpaceConfigWrite),
	"disable-space-human-type":   InSpace(authz.SpaceConfigWrite),
	"route-human-notification":   InSpace(authz.HumanRespond),
	"list-human-policies":        Global(authz.HumanTypeRead),
	"set-human-policy":           Global(authz.HumanTypeWrite),
	"delete-human-policy":        Global(authz.HumanTypeWrite),
	"list-space-human-policies":  InSpace(authz.SpaceConfigRead),
	"set-space-human-policy":     InSpace(authz.SpaceConfigWrite),
	"delete-space-human-policy":  InSpace(authz.SpaceConfigWrite),
}

// HumanPolicies lists stored policies and the policy in force for each type.
type HumanPolicies struct {
	Items     []human.Policy `json:"items" doc:"Stored policies of this scope"`
	Effective []human.Policy `json:"effective" doc:"The policy that applies to each type here, with its source"`
}

// HumanPolicyPath identifies the action type a policy names.
type HumanPolicyPath struct {
	ActionType string `path:"action_type" enum:"all,question,approval,todo,message,custom"`
}

// optionalVersion is the If-Match version when one is sent, else 0 (create or overwrite).
func optionalVersion(m IfMatch) (int64, error) {
	if m.IfMatch == "" {
		return 0, nil
	}
	return m.Version()
}

// HumanTypeDraftInput is the editable content of a type version.
type HumanTypeDraftInput struct {
	RequestSchema        json.RawMessage `json:"request_schema,omitempty" doc:"JSON Schema 2020-12, root {\"type\": \"object\"}; external references are refused"`
	ResponseSchema       json.RawMessage `json:"response_schema,omitempty"`
	UI                   json.RawMessage `json:"ui,omitempty" doc:"{order: [field…], fields: {field: {label, help, widget, placeholder}}}"`
	Outcomes             []string        `json:"outcomes,omitempty" maxItems:"10"`
	DefaultPriority      string          `json:"default_priority,omitempty" enum:"low,normal,high,urgent"`
	DefaultExpiryMinutes int64           `json:"default_expiry_minutes,omitempty" minimum:"0"`
	Notes                string          `json:"notes,omitempty" maxLength:"2000"`
}

func (b HumanTypeDraftInput) draft() human.TypeDraft {
	return human.TypeDraft{RequestSchema: b.RequestSchema, ResponseSchema: b.ResponseSchema, UI: b.UI, Outcomes: b.Outcomes, DefaultPriority: b.DefaultPriority,
		DefaultExpiryMinutes: b.DefaultExpiryMinutes, Notes: b.Notes}
}

// HumanTypeOption is a published catalog type a space can enable.
type HumanTypeOption struct {
	ID          string           `json:"id"`
	Slug        string           `json:"slug"`
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Versions    []PromptVersionN `json:"versions"`
}

// HumanTypePath identifies a type (ID or slug).
type HumanTypePath struct {
	TypeID string `path:"type_id" maxLength:"64"`
}

// HumanCommentInput is a comment or a clarification request.
type HumanCommentInput struct {
	Body          string   `json:"body" maxLength:"20000" doc:"Markdown"`
	AttachmentIDs []string `json:"attachment_ids,omitempty" maxItems:"20" doc:"Attachments of this space the comment references"`
}

// HumanActionPath identifies a human action.
type HumanActionPath struct {
	SpaceID  string `path:"space_id" maxLength:"64"`
	ActionID string `path:"action_id" maxLength:"64"`
}

// CreateHumanActionInput is an operator's new human action (agents create theirs through MCP tools).
type CreateHumanActionInput struct {
	Type          string          `json:"type" enum:"question,approval,todo,message,custom"`
	TypeSlug      string          `json:"type_slug,omitempty" maxLength:"63" doc:"For type custom: a catalog type enabled in the space"`
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
	AttachmentIDs []string        `json:"attachment_ids,omitempty" maxItems:"20" doc:"Attachments of this space the request references (upload them first)"`
}

// HumanListQuery are the search, filter and sort parameters of the human-action listings (SM-HUM-006, SM-HUI-001).
type HumanListQuery struct {
	Type      string `query:"type" enum:"question,approval,todo,message,custom," doc:"One type"`
	Q         string `query:"q" maxLength:"200" doc:"Search the title and Markdown body (case-insensitive; every word must occur)"`
	Status    string `query:"status" enum:"open,in_progress,resolved,cancelled,expired," doc:"One status"`
	Assignee  string `query:"assignee" maxLength:"64" doc:"A user ID, me (the signed-in user) or unassigned"`
	Priority  string `query:"priority" enum:"low,normal,high,urgent," doc:"One priority"`
	Requester string `query:"requester" maxLength:"200" doc:"The requester's kind (agent, operator, system), ID, run or role, or part of its label"`
	Due       string `query:"due" enum:"overdue,due,expiring," doc:"overdue: pending past its due date; due: has a due date; expiring: pending with an expiry"`
	Sort      string `query:"sort" enum:"rank,priority,due,newest,oldest,updated," doc:"Display order; never changes the space's durable order. Default: rank (space), oldest (global)"`
	Limit     int    `query:"limit" minimum:"0" maximum:"500"`
}

func (q HumanListQuery) filter(ctx context.Context, pendingOnly bool) human.ListFilter {
	assignee := q.Assignee
	if assignee == "me" {
		assignee = Actor(ctx).ID
	}
	return human.ListFilter{Pending: pendingOnly, Type: q.Type, Query: q.Q, Status: q.Status, Assignee: assignee, Priority: q.Priority,
		Requester: q.Requester, Due: q.Due, Sort: q.Sort, Limit: q.Limit}
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
			Summary: "Pending human actions across your spaces", Description: "Only spaces where you have human_action:read are included. " +
				"Filters and search narrow the items; the counts are always all pending actions per space.", Tags: tags},
			func(ctx context.Context, in *struct {
				HumanListQuery
				Space string `query:"space" maxLength:"64" doc:"Only this space"`
			}) (*struct{ Body PendingHumanActions }, error) {
				p := PrincipalFrom(ctx)
				allowed := func(id string) bool {
					return (in.Space == "" || id == in.Space) && d.Authz != nil && d.Authz.AllowedAll(p.Subjects(), id, authz.HumanRead)
				}
				// Type permissions hide actions of types the caller may not see, from the items and the counts (SM-HUI-008).
				h := d.humanAccess(ctx, svc)
				f := in.filter(ctx, true)
				f.Visible = h.visible(ctx)
				l, err := svc.ListPending(ctx, allowed, f)
				if err != nil {
					return nil, d.fail(ctx, "human.list_pending", err)
				}
				all, err := svc.PendingCountsVisible(ctx, func(a *human.Action) bool { return allowed(a.SpaceID) && h.may(ctx, a, humanView) })
				if err != nil {
					return nil, d.fail(ctx, "human.list_pending", err)
				}
				out := PendingHumanActions{Items: l, Counts: map[string]int64{}}
				for id, n := range all {
					out.Counts[id] = n
					out.Total += n
				}
				return &struct{ Body PendingHumanActions }{Body: out}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-human-actions", Method: http.MethodGet, Path: "/spaces/{space_id}/human-actions",
			Summary: "A space's human actions", Description: "Pending actions first in their order, then recent final ones.", Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				Pending bool `query:"pending"`
				HumanListQuery
			}) (*struct{ Body HumanActionList }, error) {
				f := in.filter(ctx, in.Pending)
				f.Visible = d.humanAccess(ctx, svc).visible(ctx)
				l, err := svc.List(ctx, in.SpaceID, f)
				if err != nil {
					return nil, d.fail(ctx, "human.list", err)
				}
				return &struct{ Body HumanActionList }{Body: HumanActionList{Items: l}}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-human-action", Method: http.MethodGet, Path: "/spaces/{space_id}/human-actions/{action_id}",
			Summary: "A human action with its comments and notifications", Tags: tags},
			func(ctx context.Context, in *HumanActionPath) (*humanOut, error) {
				a, err := d.humanAccess(ctx, svc).action(ctx, in.SpaceID, in.ActionID, humanView)
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
					probe := &human.Action{SpaceID: in.SpaceID, Type: b.Type, TypeSlug: b.TypeSlug}
					if h := d.humanAccess(ctx, svc); !h.may(ctx, probe, humanCreate) {
						p := h.sets[in.SpaceID].For(probe)
						return nil, apperr.New(apperr.CodeForbidden, "asking with %s in this space needs the %s role", typeName(probe), p.CreateRole)
					}
					a, _, err := svc.Create(ctx, actor, in.SpaceID, human.CreateInput{Type: b.Type, TypeSlug: b.TypeSlug, Title: b.Title, Body: b.Body, Request: b.Request,
						Requester: human.Requester{Kind: "operator", ID: actor.ID, Label: actorLabel(ctx)}, TaskID: b.TaskID, BlocksTask: b.BlocksTask,
						Priority: b.Priority, DueAt: b.DueAt, ExpiresAt: b.ExpiresAt, DedupKey: b.DedupKey, CorrelationID: b.CorrelationID, FollowUpOf: b.FollowUpOf,
						AttachmentIDs: b.AttachmentIDs})
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
					Outcome string `json:"outcome" maxLength:"32" doc:"answered, approved, denied, completed, acknowledged, or a custom type's outcome"`
					human.Response
					AttachmentIDs []string `json:"attachment_ids,omitempty" maxItems:"20" doc:"Attachments of this space the response references"`
				}
			}) (*humanOut, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "human.respond", err)
				}
				cur, err := d.humanAccess(ctx, svc).action(ctx, in.SpaceID, in.ActionID, humanRespond)
				if err != nil {
					return nil, d.fail(ctx, "human.respond", err)
				}
				if cur.Type == human.TypeApproval {
					p := PrincipalFrom(ctx)
					if d.Authz == nil || !d.Authz.AllowedAll(p.Subjects(), in.SpaceID, authz.ApprovalDecide) {
						return nil, d.fail(ctx, "human.respond", apperr.New(apperr.CodeForbidden, "deciding approvals needs approval:decide"))
					}
				}
				a, err := svc.Respond(ctx, Actor(ctx), in.SpaceID, in.ActionID, v, human.RespondInput{Outcome: in.Body.Outcome, Response: in.Body.Response,
					AttachmentIDs: in.Body.AttachmentIDs})
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
				if _, err := d.humanAccess(ctx, svc).action(ctx, in.SpaceID, in.ActionID, humanRespond); err != nil {
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
				if _, err := d.humanAccess(ctx, svc).action(ctx, in.SpaceID, in.ActionID, humanRespond); err != nil {
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
				Body HumanCommentInput
			}) (*humanOut, error) {
				if _, err := d.humanAccess(ctx, svc).action(ctx, in.SpaceID, in.ActionID, humanRespond); err != nil {
					return nil, d.fail(ctx, "human.comment", err)
				}
				a, err := svc.Comment(ctx, Actor(ctx), in.SpaceID, in.ActionID, human.CommentInput{Body: in.Body.Body, Label: actorLabel(ctx), AttachmentIDs: in.Body.AttachmentIDs})
				if err != nil {
					return nil, d.fail(ctx, "human.comment", err)
				}
				return humanResult(a), nil
			})
		huma.Register(api, huma.Operation{OperationID: "clarify-human-action", Method: http.MethodPost, Path: "/spaces/{space_id}/human-actions/{action_id}/clarify",
			Summary: "Ask the requester for more information",
			Description: "The action stays pending. The question is kept as a clarification comment; an agent requester receives a durable clarification " +
				"notification routed like a response and answers with a reply comment. A clarification is never a response.", Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				HumanActionPath
				Body HumanCommentInput
			}) (*humanOut, error) {
				if _, err := d.humanAccess(ctx, svc).action(ctx, in.SpaceID, in.ActionID, humanRespond); err != nil {
					return nil, d.fail(ctx, "human.clarify", err)
				}
				a, err := svc.RequestClarification(ctx, Actor(ctx), in.SpaceID, in.ActionID, human.CommentInput{Body: in.Body.Body, Label: actorLabel(ctx),
					AttachmentIDs: in.Body.AttachmentIDs})
				if err != nil {
					return nil, d.fail(ctx, "human.clarify", err)
				}
				return humanResult(a), nil
			})
		ttags := []string{"human-action-types"}
		huma.Register(api, huma.Operation{OperationID: "list-human-types", Method: http.MethodGet, Path: "/human-types", Summary: "The human-action type catalog", Tags: ttags},
			func(ctx context.Context, _ *struct{}) (*struct {
				Body struct {
					Items []*human.HumanType `json:"items"`
				}
			}, error) {
				l, err := svc.ListTypes(ctx)
				if err != nil {
					return nil, d.fail(ctx, "human_types.list", err)
				}
				out := &struct {
					Body struct {
						Items []*human.HumanType `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-human-type", Method: http.MethodGet, Path: "/human-types/{type_id}", Summary: "A type with its versions", Tags: ttags},
			func(ctx context.Context, in *HumanTypePath) (*struct{ Body *human.HumanType }, error) {
				t, err := svc.GetType(ctx, in.TypeID)
				if err != nil {
					return nil, d.fail(ctx, "human_types.get", err)
				}
				return &struct{ Body *human.HumanType }{Body: t}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "create-human-type", Method: http.MethodPost, Path: "/human-types", Summary: "Add a type (with a first draft)",
			Tags: ttags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				Body struct {
					Slug        string `json:"slug" maxLength:"63"`
					Name        string `json:"name" maxLength:"120"`
					Description string `json:"description,omitempty" maxLength:"2000"`
					HumanTypeDraftInput
				}
			}) (*struct{ Body *human.HumanType }, error) {
				t, err := svc.CreateType(ctx, Actor(ctx), in.Body.Slug, in.Body.Name, in.Body.Description, in.Body.draft())
				if err != nil {
					return nil, d.fail(ctx, "human_types.create", err)
				}
				return &struct{ Body *human.HumanType }{Body: t}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "save-human-type-draft", Method: http.MethodPut, Path: "/human-types/{type_id}/draft",
			Summary: "Create or update the type's draft", Description: "Without If-Match a new draft is started; with it, the draft at that version is updated.", Tags: ttags},
			func(ctx context.Context, in *struct {
				HumanTypePath
				IfMatch
				Body HumanTypeDraftInput
			}) (*struct{ Body *human.HumanType }, error) {
				v := int64(0)
				if in.IfMatch.IfMatch != "" {
					var err error
					if v, err = in.Version(); err != nil {
						return nil, d.fail(ctx, "human_types.draft", err)
					}
				}
				t, err := svc.SaveTypeDraft(ctx, Actor(ctx), in.TypeID, v, in.Body.draft())
				if err != nil {
					return nil, d.fail(ctx, "human_types.draft", err)
				}
				return &struct{ Body *human.HumanType }{Body: t}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "publish-human-type", Method: http.MethodPost, Path: "/human-types/{type_id}/publish",
			Summary: "Publish the draft as the next immutable version", Description: "Outstanding actions keep the version they were created with.", Tags: ttags},
			func(ctx context.Context, in *struct {
				HumanTypePath
				IfMatch
			}) (*struct{ Body *human.HumanType }, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "human_types.publish", err)
				}
				t, err := svc.PublishType(ctx, Actor(ctx), in.TypeID, v)
				if err != nil {
					return nil, d.fail(ctx, "human_types.publish", err)
				}
				return &struct{ Body *human.HumanType }{Body: t}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-space-human-types", Method: http.MethodGet, Path: "/spaces/{space_id}/human-types",
			Summary: "Custom types enabled in the space, with the version each uses now", Tags: ttags},
			func(ctx context.Context, in *SpacePath) (*struct {
				Body struct {
					Items []human.SpaceType `json:"items"`
				}
			}, error) {
				l, err := svc.SpaceTypes(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "human_types.space", err)
				}
				out := &struct {
					Body struct {
						Items []human.SpaceType `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-human-type-options", Method: http.MethodGet, Path: "/spaces/{space_id}/human-type-options",
			Summary: "Catalog types a space can enable (names and published versions only)", Tags: ttags},
			func(ctx context.Context, in *SpacePath) (*struct {
				Body struct {
					Items []HumanTypeOption `json:"items"`
				}
			}, error) {
				l, err := svc.ListTypes(ctx)
				if err != nil {
					return nil, d.fail(ctx, "human_types.options", err)
				}
				out := &struct {
					Body struct {
						Items []HumanTypeOption `json:"items"`
					}
				}{}
				out.Body.Items = []HumanTypeOption{}
				for _, t := range l {
					o := HumanTypeOption{ID: t.ID, Slug: t.Slug, Name: t.Name, Description: t.Description, Versions: []PromptVersionN{}}
					for _, v := range t.Versions {
						if v.State == "published" {
							o.Versions = append(o.Versions, PromptVersionN{ID: v.ID, Number: v.Number})
						}
					}
					if len(o.Versions) > 0 {
						out.Body.Items = append(out.Body.Items, o)
					}
				}
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "enable-space-human-type", Method: http.MethodPut, Path: "/spaces/{space_id}/human-types/{type_id}",
			Summary: "Enable a type in the space (follow the latest version or pin one; optionally as a variant)", Tags: ttags},
			func(ctx context.Context, in *struct {
				SpacePath
				TypeID string `path:"type_id" maxLength:"64"`
				Body   struct {
					VersionPin           string `json:"version_pin,omitempty" maxLength:"64"`
					Label                string `json:"label,omitempty" maxLength:"120"`
					DefaultPriority      string `json:"default_priority,omitempty" enum:"low,normal,high,urgent"`
					DefaultExpiryMinutes int64  `json:"default_expiry_minutes,omitempty" minimum:"0"`
				}
			}) (*struct {
				Body struct {
					Items []human.SpaceType `json:"items"`
				}
			}, error) {
				if err := svc.EnableType(ctx, Actor(ctx), in.SpaceID, in.TypeID, in.Body.VersionPin, in.Body.Label, in.Body.DefaultPriority, in.Body.DefaultExpiryMinutes); err != nil {
					return nil, d.fail(ctx, "human_types.enable", err)
				}
				l, err := svc.SpaceTypes(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "human_types.enable", err)
				}
				out := &struct {
					Body struct {
						Items []human.SpaceType `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "disable-space-human-type", Method: http.MethodDelete, Path: "/spaces/{space_id}/human-types/{type_id}",
			Summary: "Stop new actions of a type in the space", Description: "Outstanding actions are unaffected.", Tags: ttags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *struct {
				SpacePath
				TypeID string `path:"type_id" maxLength:"64"`
			}) (*struct{}, error) {
				if err := svc.DisableType(ctx, Actor(ctx), in.SpaceID, in.TypeID); err != nil {
					return nil, d.fail(ctx, "human_types.disable", err)
				}
				return &struct{}{}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "route-human-notification", Method: http.MethodPost,
			Path:    "/spaces/{space_id}/human-actions/{action_id}/notifications/{notification_id}/route",
			Summary: "Send a response notification to control or a running role",
			Description: "For a notification no agent has acknowledged yet, typically one that could not be delivered because the requester's run ended " +
				"and no successor or control run could take it. The routing is recorded with its provenance.", Tags: tags},
			func(ctx context.Context, in *struct {
				HumanActionPath
				NotificationID string `path:"notification_id" maxLength:"64"`
				Body           struct {
					To string `json:"to" maxLength:"60" doc:"control, or role:<name> (a running run of that role)"`
				}
			}) (*humanOut, error) {
				a, err := svc.RouteNotification(ctx, Actor(ctx), in.SpaceID, in.ActionID, in.NotificationID, in.Body.To)
				if err != nil {
					return nil, d.fail(ctx, "human.route", err)
				}
				return humanResult(a), nil
			})
		registerHumanTypePermissions(api, d, svc)
		ptags := []string{"human-action-policies"}
		listPolicies := func(ctx context.Context, op, spaceID string) (*struct{ Body HumanPolicies }, error) {
			items, err := svc.Policies(ctx, spaceID)
			if err != nil {
				return nil, d.fail(ctx, op, err)
			}
			eff, err := svc.EffectivePolicies(ctx, spaceID)
			if err != nil {
				return nil, d.fail(ctx, op, err)
			}
			return &struct{ Body HumanPolicies }{Body: HumanPolicies{Items: items, Effective: eff}}, nil
		}
		huma.Register(api, huma.Operation{OperationID: "list-human-policies", Method: http.MethodGet, Path: "/human-policies",
			Summary:     "Installation defaults for reminders, escalation, rate limits and response routing",
			Description: "Spaces override them per type. Without a stored default, the built-in policy of the type applies.", Tags: ptags},
			func(ctx context.Context, _ *struct{}) (*struct{ Body HumanPolicies }, error) {
				return listPolicies(ctx, "human_policies.list", "")
			})
		huma.Register(api, huma.Operation{OperationID: "set-human-policy", Method: http.MethodPut, Path: "/human-policies/{action_type}",
			Summary:     "Set the installation default for a type (all: every type)",
			Description: "With If-Match, the stored policy must be at that version. Pending actions get their reminder and escalation times recomputed.", Tags: ptags},
			func(ctx context.Context, in *struct {
				HumanPolicyPath
				IfMatch
				Body human.PolicyFields
			}) (*struct{ Body *human.Policy }, error) {
				v, err := optionalVersion(in.IfMatch)
				if err != nil {
					return nil, d.fail(ctx, "human_policies.set", err)
				}
				p, err := svc.SetPolicy(ctx, Actor(ctx), "", in.ActionType, v, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "human_policies.set", err)
				}
				return &struct{ Body *human.Policy }{Body: p}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "delete-human-policy", Method: http.MethodDelete, Path: "/human-policies/{action_type}",
			Summary: "Remove an installation default (the next less specific policy applies)", Tags: ptags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *HumanPolicyPath) (*struct{}, error) {
				if err := svc.DeletePolicy(ctx, Actor(ctx), "", in.ActionType); err != nil {
					return nil, d.fail(ctx, "human_policies.delete", err)
				}
				return &struct{}{}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-space-human-policies", Method: http.MethodGet, Path: "/spaces/{space_id}/human-policies",
			Summary: "A space's human-action policies and the policy in force for each type", Tags: ptags},
			func(ctx context.Context, in *SpacePath) (*struct{ Body HumanPolicies }, error) {
				return listPolicies(ctx, "human_policies.space", in.SpaceID)
			})
		huma.Register(api, huma.Operation{OperationID: "set-space-human-policy", Method: http.MethodPut, Path: "/spaces/{space_id}/human-policies/{action_type}",
			Summary:     "Override the policy of a type in the space (all: every type)",
			Description: "The most specific policy applies as a whole: space and type, space and all, default and type, default and all, built-in.", Tags: ptags},
			func(ctx context.Context, in *struct {
				SpacePath
				HumanPolicyPath
				IfMatch
				Body human.PolicyFields
			}) (*struct{ Body *human.Policy }, error) {
				v, err := optionalVersion(in.IfMatch)
				if err != nil {
					return nil, d.fail(ctx, "human_policies.space_set", err)
				}
				p, err := svc.SetPolicy(ctx, Actor(ctx), in.SpaceID, in.ActionType, v, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "human_policies.space_set", err)
				}
				return &struct{ Body *human.Policy }{Body: p}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "delete-space-human-policy", Method: http.MethodDelete, Path: "/spaces/{space_id}/human-policies/{action_type}",
			Summary: "Remove a space override", Tags: ptags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *struct {
				SpacePath
				HumanPolicyPath
			}) (*struct{}, error) {
				if err := svc.DeletePolicy(ctx, Actor(ctx), in.SpaceID, in.ActionType); err != nil {
					return nil, d.fail(ctx, "human_policies.space_delete", err)
				}
				return &struct{}{}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "move-human-action", Method: http.MethodPost, Path: "/spaces/{space_id}/human-actions/{action_id}/move",
			Summary: "Reorder a pending human action", Tags: tags},
			func(ctx context.Context, in *struct {
				HumanActionPath
				Body struct {
					Before string `json:"before,omitempty" maxLength:"64" doc:"Place before this pending action; empty places it last"`
				}
			}) (*humanOut, error) {
				if _, err := d.humanAccess(ctx, svc).action(ctx, in.SpaceID, in.ActionID, humanRespond); err != nil {
					return nil, d.fail(ctx, "human.move", err)
				}
				a, err := svc.Move(ctx, Actor(ctx), in.SpaceID, in.ActionID, in.Body.Before)
				if err != nil {
					return nil, d.fail(ctx, "human.move", err)
				}
				return humanResult(a), nil
			})
	}
}
