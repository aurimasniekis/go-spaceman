package kb

import (
	"context"

	"github.com/aurimasniekis/go-spaceman/internal/agenttools"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
)

type requestIn struct {
	RequestID string `json:"request_id" maxLength:"64"`
}

// Tools registers the KB tools. Requesters list, ask, read and cancel; KB sessions (kind kb) answer and propose. No KB
// tool starts workers or changes policy (SM-KB-006).
func Tools(svc *Service) func(r *agenttools.Registry) {
	askers := []string{agenttools.KindControl, agenttools.KindWorker, agenttools.KindKB, agenttools.KindDreamer, agenttools.KindDiagnostic}
	kbOnly := []string{agenttools.KindKB}
	return func(r *agenttools.Registry) {
		agenttools.Add(r, "spaceman_kb_list", "Knowledge bases in this space you may ask, with what they cover.", askers, true,
			func(ctx context.Context, p *fleet.RunPrincipal, _ struct{}) (any, error) {
				l, err := svc.List(ctx, p.SpaceID)
				if err != nil {
					return nil, err
				}
				type item struct {
					Slug, Name, Description, WritePolicy string
					Admitted                             bool
				}
				out := []item{}
				for _, k := range l {
					if !k.Enabled {
						continue
					}
					adm := false
					for _, role := range k.AllowedRoles {
						adm = adm || role == "*" || role == p.Role
					}
					out = append(out, item{Slug: k.Slug, Name: k.Name, Description: k.Description, WritePolicy: k.WritePolicy, Admitted: adm})
				}
				return out, nil
			})
		agenttools.Add(r, "spaceman_kb_ask", "Ask a knowledge base for context, decisions, discoveries, proposals, documentation or a consistency check. "+
			"It returns at once with a request ID; your own KB session answers and you are notified. Read the answer with spaceman_kb_get. Do not wait or poll.",
			askers, false, func(ctx context.Context, p *fleet.RunPrincipal, in AskInput) (any, error) {
				return svc.Ask(ctx, p, in)
			})
		agenttools.Add(r, "spaceman_kb_get", "Read a KB request: its state, the answer, its basis (committed knowledge or a proposal) and evidence.",
			askers, true, func(ctx context.Context, p *fleet.RunPrincipal, in requestIn) (any, error) {
				return svc.GetRequest(ctx, p, in.RequestID)
			})
		agenttools.Add(r, "spaceman_kb_cancel", "Withdraw a KB request you no longer need.", askers, false,
			func(ctx context.Context, p *fleet.RunPrincipal, in requestIn) (any, error) {
				return map[string]bool{"cancelled": true}, svc.Cancel(ctx, audit.Actor{Type: audit.ActorAgent, ID: p.RunID}, p, p.SpaceID, in.RequestID)
			})
		agenttools.Add(r, "spaceman_kb_answer", "Answer a request sent to this KB support session. Cite the KB files (and commits) as evidence and say "+
			"whether the answer rests on committed knowledge, on a proposal, or the KB does not know.", kbOnly, false,
			func(ctx context.Context, p *fleet.RunPrincipal, in AnswerInput) (any, error) {
				return svc.Answer(ctx, p, in)
			})
		agenttools.Add(r, "spaceman_kb_propose", "Offer a commit on this session's branch as a change to the KB. It is a proposal until it is "+
			"accepted (by a maintainer, or serially after the KB's checks, depending on the KB).", kbOnly, false,
			func(ctx context.Context, p *fleet.RunPrincipal, in ProposeInput) (any, error) {
				return svc.Propose(ctx, p, in)
			})
	}
}
