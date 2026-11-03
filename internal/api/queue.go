package api

import (
	"context"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
)

// QueueRules are the authorization rules of the queue operations.
var QueueRules = map[string]Rule{
	"list-tasks":            InSpace(authz.QueueRead),
	"get-task":              InSpace(authz.QueueRead),
	"queue-next":            InSpace(authz.QueueRead),
	"queue-decisions":       InSpace(authz.QueueRead),
	"create-task":           InSpace(authz.QueueWrite),
	"update-task":           InSpace(authz.QueueWrite),
	"move-task":             InSpace(authz.QueueWrite),
	"escalate-task":         InSpace(authz.QueueWrite),
	"set-task-status":       InSpace(authz.QueueWrite),
	"set-task-dependencies": InSpace(authz.QueueWrite),
	"add-task-blocker":      InSpace(authz.QueueWrite),
	"resolve-task-blocker":  InSpace(authz.QueueWrite),
	"set-queue-policy":      InSpace(authz.SpaceConfigWrite),
	// Confirming an incident proposal and resuming preempted work are operator decisions (SM-QUEUE-009, ADR-0050).
	"decide-incident": InSpace(authz.QueueWrite),
	"resume-task":     InSpace(authz.QueueWrite),
}

// TaskPath identifies a task.
type TaskPath struct {
	SpaceID string `path:"space_id" maxLength:"64"`
	TaskID  string `path:"task_id" maxLength:"64"`
}

// TaskBody creates a task.
type TaskBody struct {
	Title         string   `json:"title" maxLength:"200"`
	Objective     string   `json:"objective,omitempty" maxLength:"100000"`
	Acceptance    string   `json:"acceptance,omitempty" maxLength:"50000"`
	Kind          string   `json:"kind,omitempty" enum:"ordinary,incident"`
	Priority      string   `json:"priority,omitempty" enum:"low,normal,high,urgent"`
	Status        string   `json:"status,omitempty" enum:"backlog,ready"`
	ParentID      string   `json:"parent_id,omitempty" maxLength:"64"`
	Exclusions    []string `json:"exclusions,omitempty" maxItems:"20"`
	AssigneeRole  string   `json:"assignee_role,omitempty" maxLength:"48"`
	Workspace     string   `json:"workspace,omitempty" maxLength:"256"`
	DependsOn     []string `json:"depends_on,omitempty" maxItems:"50"`
	FromInboxItem string   `json:"from_inbox_item,omitempty" maxLength:"64" doc:"Create the task from an inbox item: the item is marked done and linked to the task"`
	AttachmentIDs []string `json:"attachment_ids,omitempty" maxItems:"20" doc:"Attachments of this space the task references (an inbox item's attachments are added)"`
}

// QueueOut is a space's queue in its effective order.
type QueueOut struct {
	Tasks  []*queue.Task `json:"tasks"`
	Policy queue.Policy  `json:"policy"`
}

// ResumeOut is a resumed task and what happened.
type ResumeOut struct {
	Task    *queue.Task `json:"task"`
	Outcome string      `json:"outcome" doc:"resumed, relaunch (no live worker), waiting: <reason> (still preempted) or superseded"`
}

type taskOut struct {
	ETag string `header:"ETag"`
	Body *queue.Task
}

func taskResult(t *queue.Task) *taskOut { return &taskOut{ETag: ETag(t.Version), Body: t} }

// RegisterQueue registers the queue operations.
func RegisterQueue(svc *queue.Service, ib *inbox.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"queue"}
		huma.Register(api, huma.Operation{OperationID: "list-tasks", Method: http.MethodGet, Path: "/spaces/{space_id}/tasks", Summary: "A space's queue in its effective order",
			Description: "Ready tasks carry the reasons they are not eligible now (waiting).", Tags: tags},
			func(ctx context.Context, in *SpacePath) (*struct{ Body QueueOut }, error) {
				l, p, err := svc.List(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "queue.list", err)
				}
				return &struct{ Body QueueOut }{Body: QueueOut{Tasks: l, Policy: p}}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-task", Method: http.MethodGet, Path: "/spaces/{space_id}/tasks/{task_id}", Summary: "A task", Tags: tags},
			func(ctx context.Context, in *TaskPath) (*taskOut, error) {
				t, err := svc.Get(ctx, in.SpaceID, in.TaskID)
				if err != nil {
					return nil, d.fail(ctx, "queue.get", err)
				}
				return taskResult(t), nil
			})
		huma.Register(api, huma.Operation{OperationID: "queue-next", Method: http.MethodGet, Path: "/spaces/{space_id}/queue/next",
			Summary: "The next eligible task and why higher tasks were skipped", Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				Record bool `query:"record" doc:"Store the decision for inspection (needs queue:write)"`
			}) (*struct{ Body *queue.Selection }, error) {
				// Storing the decision is a write: a viewer may ask what is next, not record it (security review SR-L07).
				if in.Record {
					p := PrincipalFrom(ctx)
					if d.Authz == nil || p == nil || !d.Authz.AllowedAll(p.Subjects(), in.SpaceID, authz.QueueWrite) {
						return nil, d.fail(ctx, "queue.next", apperr.New(apperr.CodeForbidden, "recording a decision needs queue:write"))
					}
				}
				s, err := svc.Next(ctx, Actor(ctx), in.SpaceID, in.Record)
				if err != nil {
					return nil, d.fail(ctx, "queue.next", err)
				}
				return &struct{ Body *queue.Selection }{Body: s}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "queue-decisions", Method: http.MethodGet, Path: "/spaces/{space_id}/queue/decisions",
			Summary: "Recorded selections", Tags: tags},
			func(ctx context.Context, in *SpacePath) (*struct {
				Body struct {
					Items []queue.Decision `json:"items"`
				}
			}, error) {
				l, err := svc.Decisions(ctx, in.SpaceID, 50)
				if err != nil {
					return nil, d.fail(ctx, "queue.decisions", err)
				}
				out := &struct {
					Body struct {
						Items []queue.Decision `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "create-task", Method: http.MethodPost, Path: "/spaces/{space_id}/tasks", Summary: "Add a task to the queue",
			Description: "Priority and the queue policy decide where it is inserted.", Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				SpacePath
				IdempotencyKey
				Body TaskBody
			}) (*taskOut, error) {
				t, _, err := Idempotent(ctx, d, "create-task", in.IdempotencyKey, map[string]any{"space": in.SpaceID, "body": in.Body}, false, func() (*queue.Task, error) {
					ci := queue.CreateInput{Title: in.Body.Title, Objective: in.Body.Objective, Acceptance: in.Body.Acceptance, Kind: in.Body.Kind, Priority: in.Body.Priority,
						Status: in.Body.Status, ParentID: in.Body.ParentID, Exclusions: in.Body.Exclusions, AssigneeRole: in.Body.AssigneeRole, Workspace: in.Body.Workspace,
						DependsOn: in.Body.DependsOn, AttachmentIDs: in.Body.AttachmentIDs, Triggers: []string{queue.TriggerOperator}}
					var item *inbox.Item
					if in.Body.FromInboxItem != "" {
						it, err := ib.Get(ctx, in.SpaceID, in.Body.FromInboxItem)
						if err != nil {
							return nil, err
						}
						item = it
						ci.OriginKind, ci.OriginID = "inbox_item", it.ID
						for _, att := range it.Attachments {
							if !slices.Contains(ci.AttachmentIDs, att.ID) && len(ci.AttachmentIDs) < 20 {
								ci.AttachmentIDs = append(ci.AttachmentIDs, att.ID)
							}
						}
					}
					t, _, err := svc.Create(ctx, Actor(ctx), in.SpaceID, ci)
					if err != nil {
						return nil, err
					}
					if item != nil {
						if _, err := ib.SetStatus(ctx, Actor(ctx), in.SpaceID, item.ID, item.Version, inbox.StatusInput{Status: inbox.StatusDone, LinkedKind: "task", LinkedID: t.ID,
							Reason: "became a task"}); err != nil {
							return nil, err
						}
					}
					return t, nil
				})
				if err != nil {
					return nil, d.fail(ctx, "queue.create", err)
				}
				return taskResult(t), nil
			})
		huma.Register(api, huma.Operation{OperationID: "update-task", Method: http.MethodPut, Path: "/spaces/{space_id}/tasks/{task_id}", Summary: "Edit a task", Tags: tags},
			func(ctx context.Context, in *struct {
				TaskPath
				IfMatch
				Body struct {
					Title        string   `json:"title" maxLength:"200"`
					Objective    string   `json:"objective" maxLength:"100000"`
					Acceptance   string   `json:"acceptance" maxLength:"50000"`
					Priority     string   `json:"priority" enum:"low,normal,high,urgent"`
					Exclusions   []string `json:"exclusions" maxItems:"20"`
					AssigneeRole string   `json:"assignee_role,omitempty" maxLength:"48"`
					AssigneeRun  string   `json:"assignee_run,omitempty" maxLength:"64"`
					Workspace    string   `json:"workspace,omitempty" maxLength:"256"`
				}
			}) (*taskOut, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "queue.update", err)
				}
				t, err := svc.Update(ctx, Actor(ctx), in.SpaceID, in.TaskID, v, queue.UpdateInput{Title: in.Body.Title, Objective: in.Body.Objective, Acceptance: in.Body.Acceptance,
					Priority: in.Body.Priority, Exclusions: in.Body.Exclusions, AssigneeRole: in.Body.AssigneeRole, AssigneeRun: in.Body.AssigneeRun, Workspace: in.Body.Workspace})
				if err != nil {
					return nil, d.fail(ctx, "queue.update", err)
				}
				return taskResult(t), nil
			})
		huma.Register(api, huma.Operation{OperationID: "move-task", Method: http.MethodPost, Path: "/spaces/{space_id}/tasks/{task_id}/move",
			Summary: "Place a task in the queue", Description: "Operator placement is authoritative: policy and duplicate triggers never move the task again. Moving never stops work.",
			Tags: tags},
			func(ctx context.Context, in *struct {
				TaskPath
				Body struct {
					BeforeID string `json:"before_id,omitempty" maxLength:"64"`
					AfterID  string `json:"after_id,omitempty" maxLength:"64"`
					To       string `json:"to,omitempty" enum:"top,bottom"`
				}
			}) (*taskOut, error) {
				t, err := svc.Move(ctx, Actor(ctx), in.SpaceID, in.TaskID, queue.MoveInput{BeforeID: in.Body.BeforeID, AfterID: in.Body.AfterID, To: in.Body.To})
				if err != nil {
					return nil, d.fail(ctx, "queue.move", err)
				}
				return taskResult(t), nil
			})
		huma.Register(api, huma.Operation{OperationID: "escalate-task", Method: http.MethodPost, Path: "/spaces/{space_id}/tasks/{task_id}/escalate",
			Summary: "Escalate a task", Description: "Raises it to urgent; moves it to the top only when the queue policy allows escalation to reorder.", Tags: tags},
			func(ctx context.Context, in *struct {
				TaskPath
				Body struct {
					Reason string `json:"reason" maxLength:"2000"`
				}
			}) (*taskOut, error) {
				t, err := svc.Escalate(ctx, Actor(ctx), in.SpaceID, in.TaskID, in.Body.Reason)
				if err != nil {
					return nil, d.fail(ctx, "queue.escalate", err)
				}
				return taskResult(t), nil
			})
		huma.Register(api, huma.Operation{OperationID: "set-task-status", Method: http.MethodPost, Path: "/spaces/{space_id}/tasks/{task_id}/status",
			Summary: "Change a task's state", Description: "Starting needs the task to be eligible. running → ready is a preemption, not a failure.", Tags: tags},
			func(ctx context.Context, in *struct {
				TaskPath
				IfMatch
				Body struct {
					Status string `json:"status" enum:"backlog,ready,running,blocked,review,done,failed,cancelled"`
					Reason string `json:"reason,omitempty" maxLength:"2000"`
					Result string `json:"result,omitempty" maxLength:"100000"`
				}
			}) (*taskOut, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "queue.status", err)
				}
				t, err := svc.SetStatus(ctx, Actor(ctx), in.SpaceID, in.TaskID, v, queue.StatusInput{Status: in.Body.Status, Reason: in.Body.Reason, Result: in.Body.Result})
				if err != nil {
					return nil, d.fail(ctx, "queue.status", err)
				}
				return taskResult(t), nil
			})
		huma.Register(api, huma.Operation{OperationID: "set-task-dependencies", Method: http.MethodPut, Path: "/spaces/{space_id}/tasks/{task_id}/dependencies",
			Summary: "Replace a task's dependencies (cycles are rejected)", Tags: tags},
			func(ctx context.Context, in *struct {
				TaskPath
				Body struct {
					DependsOn []string `json:"depends_on" maxItems:"50"`
				}
			}) (*taskOut, error) {
				t, err := svc.SetDependencies(ctx, Actor(ctx), in.SpaceID, in.TaskID, in.Body.DependsOn)
				if err != nil {
					return nil, d.fail(ctx, "queue.dependencies", err)
				}
				return taskResult(t), nil
			})
		huma.Register(api, huma.Operation{OperationID: "add-task-blocker", Method: http.MethodPost, Path: "/spaces/{space_id}/tasks/{task_id}/blockers",
			Summary: "Block a task explicitly", Description: "Blocked tasks are skipped with the reason; other tasks continue.", Tags: tags},
			func(ctx context.Context, in *struct {
				TaskPath
				Body struct {
					Kind  string `json:"kind" enum:"human_action,external"`
					RefID string `json:"ref_id,omitempty" maxLength:"64"`
					Note  string `json:"note,omitempty" maxLength:"1000"`
				}
			}) (*taskOut, error) {
				t, err := svc.AddBlocker(ctx, Actor(ctx), in.SpaceID, in.TaskID, in.Body.Kind, in.Body.RefID, in.Body.Note)
				if err != nil {
					return nil, d.fail(ctx, "queue.blocker", err)
				}
				return taskResult(t), nil
			})
		huma.Register(api, huma.Operation{OperationID: "resolve-task-blocker", Method: http.MethodDelete, Path: "/spaces/{space_id}/tasks/{task_id}/blockers/{blocker_id}",
			Summary: "Remove a blocker", Tags: tags},
			func(ctx context.Context, in *struct {
				TaskPath
				BlockerID string `path:"blocker_id" maxLength:"64"`
			}) (*taskOut, error) {
				t, err := svc.ResolveBlocker(ctx, Actor(ctx), in.SpaceID, in.TaskID, in.BlockerID)
				if err != nil {
					return nil, d.fail(ctx, "queue.unblock", err)
				}
				return taskResult(t), nil
			})
		huma.Register(api, huma.Operation{OperationID: "decide-incident", Method: http.MethodPost, Path: "/spaces/{space_id}/tasks/{task_id}/incident",
			Summary: "Confirm or dismiss an incident proposal",
			Description: "A source the queue policy does not trust asked for this task to be an incident. Confirming makes it an incident raised by you " +
				"(the policy's insertion and preemption apply now); dismissing keeps it an ordinary task.", Tags: tags},
			func(ctx context.Context, in *struct {
				TaskPath
				IfMatch
				Body struct {
					Decision string `json:"decision" enum:"confirm,dismiss"`
				}
			}) (*taskOut, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "queue.incident", err)
				}
				t, err := svc.DecideIncident(ctx, Actor(ctx), in.SpaceID, in.TaskID, v, in.Body.Decision == "confirm")
				if err != nil {
					return nil, d.fail(ctx, "queue.incident", err)
				}
				return taskResult(t), nil
			})
		huma.Register(api, huma.Operation{OperationID: "resume-task", Method: http.MethodPost, Path: "/spaces/{space_id}/tasks/{task_id}/resume",
			Summary: "Resume a task an incident preempted",
			Description: "A ready, eligible task with live workers runs again and its paused workers are told to continue; a task whose workers are " +
				"gone stays ready for control to launch a new one. The outcome says which.", Tags: tags},
			func(ctx context.Context, in *TaskPath) (*struct {
				ETag string `header:"ETag"`
				Body ResumeOut
			}, error) {
				t, outcome, err := svc.Resume(ctx, Actor(ctx), in.SpaceID, in.TaskID)
				if err != nil {
					return nil, d.fail(ctx, "queue.resume", err)
				}
				return &struct {
					ETag string `header:"ETag"`
					Body ResumeOut
				}{ETag: ETag(t.Version), Body: ResumeOut{Task: t, Outcome: outcome}}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "set-queue-policy", Method: http.MethodPut, Path: "/spaces/{space_id}/queue/policy", Summary: "Set the queue policy",
			Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				Body queue.Policy
			}) (*struct{ Body queue.Policy }, error) {
				p, err := svc.SetPolicy(ctx, Actor(ctx), in.SpaceID, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "queue.policy", err)
				}
				return &struct{ Body queue.Policy }{Body: p}, nil
			})
	}
}
