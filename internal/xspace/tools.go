package xspace

import (
	"context"

	"github.com/aurimasniekis/go-spaceman/internal/agenttools"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
)

type requestIn struct {
	To              string `json:"to" jsonschema:"the destination space's slug or ID (spaceman_xspace_discover lists the ones you may reach)"`
	Kind            string `json:"kind" jsonschema:"question (ask its control), delegation (ask its control to do work there) or knowledge (ask one of its knowledge bases)"`
	Title           string `json:"title"`
	Body            string `json:"body,omitempty" jsonschema:"Markdown: the question or the work, with the context the other space needs; for knowledge, the question"`
	KB              string `json:"kb,omitempty" jsonschema:"knowledge: the destination KB's slug"`
	KBKind          string `json:"kb_kind,omitempty" jsonschema:"knowledge: context, decision, discovery, proposal, documentation or consistency"`
	DeadlineMinutes int    `json:"deadline_minutes,omitempty" jsonschema:"5 to 10080 (default 1440); you are notified when it passes"`
	CausedBy        string `json:"caused_by,omitempty" jsonschema:"the incoming cross-space request (xrq_...) this request serves; it inherits that chain's depth"`
	Independent     bool   `json:"independent,omitempty" jsonschema:"set when the request serves none of your open incoming requests"`
	DedupKey        string `json:"dedup_key,omitempty" jsonschema:"repeating a key returns the original request"`
}

type idIn struct {
	RequestID string `json:"request_id" jsonschema:"a cross-space request ID (xrq_...)"`
}

type listIn struct {
	Direction string `json:"direction,omitempty" jsonschema:"outgoing (you asked), incoming (you were asked) or empty for both"`
	All       bool   `json:"all,omitempty" jsonschema:"also list finished requests (default: open ones only)"`
}

type taskIn struct {
	Title      string `json:"title,omitempty" jsonschema:"default: the request's title"`
	Objective  string `json:"objective,omitempty" jsonschema:"Markdown; write it for your own workers"`
	Acceptance string `json:"acceptance,omitempty"`
	Priority   string `json:"priority,omitempty" jsonschema:"low, normal, high or urgent"`
	Ready      bool   `json:"ready,omitempty" jsonschema:"put it in the queue as ready (default: backlog)"`
}

type acceptIn struct {
	RequestID string  `json:"request_id"`
	Task      *taskIn `json:"task,omitempty" jsonschema:"create a local task for it in your queue (yours to plan, run and finish)"`
}

type declineIn struct {
	RequestID string `json:"request_id"`
	Reason    string `json:"reason,omitempty" jsonschema:"at most 500 characters; the requester sees it only if your disclosure level allows text"`
}

type replyIn struct {
	RequestID string `json:"request_id"`
	Outcome   string `json:"outcome,omitempty" jsonschema:"completed (default) or failed"`
	Reply     string `json:"reply,omitempty" jsonschema:"Markdown: the answer or the result; it is reduced to your disclosure level for that space"`
}

type cancelIn struct {
	RequestID string `json:"request_id"`
	Reason    string `json:"reason,omitempty"`
}

func actor(p *fleet.RunPrincipal) audit.Actor {
	return audit.Actor{Type: audit.ActorAgent, ID: p.RunID}
}

// Tools registers the cross-space tools (SM-MCP-003). They are for control only: a space speaks to other spaces
// through its one accountable identity, and workers, KB sessions, dreamers and diagnostics cannot reach another space
// at all. Every call is authenticated by the run's credential; the space comes from it, never from arguments.
func Tools(svc *Service) func(r *agenttools.Registry) {
	ctrl := []string{agenttools.KindControl}
	return func(r *agenttools.Registry) {
		agenttools.Add(r, "spaceman_xspace_discover", "List the other spaces that let your space discover them: their profile, what you may send "+
			"(questions, delegation, knowledge and which KBs), what their replies may disclose, and their limits.", ctrl, true,
			func(ctx context.Context, p *fleet.RunPrincipal, _ struct{}) (any, error) {
				return svc.Discover(ctx, p.SpaceID)
			})
		agenttools.Add(r, "spaceman_xspace_request", "Ask another space: a question for its control, work for it to do (delegation; it decides, plans "+
			"and runs it with its own agents), or a question for one of its knowledge bases. It returns at once with a request ID; do not wait or "+
			"check back. You are notified (a terminal notice when idle, and in spaceman_messages) when it is accepted, declined, answered, failed or "+
			"expired; then read it with spaceman_xspace_get. When you ask because another space asked you, set caused_by.", ctrl, false,
			func(ctx context.Context, p *fleet.RunPrincipal, in requestIn) (any, error) {
				req, created, err := svc.Create(ctx, p, RequestInput(in))
				if err != nil {
					return nil, err
				}
				return map[string]any{"request_id": req.ID, "state": req.State, "deadline_at": req.DeadlineAt, "depth": req.Depth,
					"correlation_id": req.CorrelationID, "created": created}, nil
			})
		agenttools.Add(r, "spaceman_xspace_get", "Read a cross-space request your space sent or received: its state, deadline, chain and, for one you "+
			"sent, the reply as the other space's disclosure level allows. Not for waiting: you are notified of changes.", ctrl, true,
			func(ctx context.Context, p *fleet.RunPrincipal, in idIn) (any, error) {
				return svc.Get(ctx, p.SpaceID, in.RequestID)
			})
		agenttools.Add(r, "spaceman_xspace_list", "List your space's cross-space requests, outgoing and incoming (open ones unless all is set).", ctrl, true,
			func(ctx context.Context, p *fleet.RunPrincipal, in listIn) (any, error) {
				return svc.List(ctx, p.SpaceID, in.Direction, !in.All, 50)
			})
		agenttools.Add(r, "spaceman_xspace_accept", "Accept a question or delegation another space sent yours. You own the work: optionally create a "+
			"local task (it stays yours: plan it, start workers for it and finish it as you would any task). Reply with spaceman_xspace_reply.",
			ctrl, false, func(ctx context.Context, p *fleet.RunPrincipal, in acceptIn) (any, error) {
				var t *TaskInput
				if in.Task != nil {
					t = &TaskInput{Title: in.Task.Title, Objective: in.Task.Objective, Acceptance: in.Task.Acceptance, Priority: in.Task.Priority, Ready: in.Task.Ready}
				}
				return svc.Accept(ctx, actor(p), p.SpaceID, in.RequestID, t)
			})
		agenttools.Add(r, "spaceman_xspace_decline", "Decline a request another space sent yours (pending ones; for accepted work that could not be "+
			"done, reply with outcome failed).", ctrl, false, func(ctx context.Context, p *fleet.RunPrincipal, in declineIn) (any, error) {
			return svc.Decline(ctx, actor(p), p.SpaceID, in.RequestID, in.Reason)
		})
		agenttools.Add(r, "spaceman_xspace_reply", "Answer a question or report the outcome of delegated work to the space that asked (completed or "+
			"failed). Only your reply crosses, reduced to the disclosure level your space set for that space; never paste secrets, credentials or "+
			"content that space should not see.", ctrl, false, func(ctx context.Context, p *fleet.RunPrincipal, in replyIn) (any, error) {
			return svc.Reply(ctx, actor(p), p.SpaceID, in.RequestID, in.Outcome, in.Reply)
		})
		agenttools.Add(r, "spaceman_xspace_cancel", "Withdraw a cross-space request your space sent that is still open. The other space is told; "+
			"what it already started there is its decision.", ctrl, false, func(ctx context.Context, p *fleet.RunPrincipal, in cancelIn) (any, error) {
			return svc.Cancel(ctx, actor(p), p.SpaceID, in.RequestID, in.Reason)
		})
	}
}
