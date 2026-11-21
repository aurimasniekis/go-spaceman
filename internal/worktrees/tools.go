package worktrees

import (
	"context"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/agenttools"
	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
)

// Bounds of the prune tool.
const (
	DefaultPruneLimit = 10
	MaxPruneLimit     = 25
)

// ListInput is spaceman_worktrees_list's input.
type ListInput struct {
	Repo string `json:"repo,omitempty" maxLength:"256" doc:"Only this repository (name, ID or folder); default every repository"`
}

// RemoveInput is spaceman_worktree_remove's input: a worktree to remove, or the command of an earlier removal to ask about.
type RemoveInput struct {
	Repo      string `json:"repo,omitempty" maxLength:"256" doc:"The repository (name, ID or folder), as spaceman_worktrees_list shows it"`
	Name      string `json:"name,omitempty" maxLength:"255" doc:"The worktree's folder name inside worktrees/, as spaceman_worktrees_list shows it"`
	CommandID string `json:"command_id,omitempty" maxLength:"64" doc:"Instead of repo and name: the command_id of an earlier removal that came back pending; answers with its outcome and removes nothing"`
}

// PruneInput is spaceman_worktrees_prune's input.
type PruneInput struct {
	Repo          string `json:"repo,omitempty" maxLength:"256" doc:"Only this repository; default every repository"`
	OlderThanDays int    `json:"older_than_days,omitempty" minimum:"0" maximum:"36500" doc:"Only worktrees whose run ended at least this many days ago (default 0: any ended run)"`
	Limit         int    `json:"limit,omitempty" minimum:"1" maximum:"25" doc:"At most this many worktrees (default 10), the oldest first"`
	Apply         bool   `json:"apply,omitempty" doc:"Remove them. Without it nothing is removed: the candidates are only listed (a dry run)"`
}

// PruneResult is spaceman_worktrees_prune's answer.
type PruneResult struct {
	Applied    bool      `json:"applied"`
	Candidates int       `json:"candidates" doc:"Worktrees that matched, before the limit"`
	Results    []Outcome `json:"results,omitempty" doc:"One per worktree tried, with the node's answer or refusal code"`
	Would      []Entry   `json:"would_remove,omitempty" doc:"A dry run's candidates"`
	Note       string    `json:"note"`
}

// Tools registers the worktree tools for control runs only (ADR-0107 decision 5). Removal is one worktree at a time
// through the node's safe-removal path; the tools never force anything.
func Tools(svc *Service) func(r *agenttools.Registry) {
	kinds := []string{agenttools.KindControl}
	return func(r *agenttools.Registry) {
		agenttools.Add(r, "spaceman_worktrees_list", "List the worktrees of your space's repositories from the node's last report: path, branch, HEAD, "+
			"changed and untracked counts, the run and task that own each one (none: made by hand), the last change central knows of, whether "+
			"the branch was pushed as of the last fetch, and whether the prune tool would pick it. Pull request state is always unknown: central has "+
			"no event tied to a branch. Read only. Branch names and paths come from the repositories (untrusted).",
			kinds, true, func(ctx context.Context, p *fleet.RunPrincipal, in ListInput) (any, error) {
				return svc.List(ctx, p.SpaceID, in.Repo)
			})
		agenttools.Add(r, "spaceman_worktree_remove", "Remove one worktree through the node's safe path: only a worktree with no change of its own, "+
			"whose HEAD the remote has, that no live run holds and that lies directly inside worktrees/. The branch is kept. Answers with status "+
			"removed, or refused with the node's refusal code (invalid, outside, not_worktree, locked, no_remote, fetch_failed, live_run, dirty, "+
			"untracked, unpushed, git_error) and reason, or pending with a command_id when the node has not answered within a minute: ask again "+
			"with command_id. Nothing can force it; a refusal means leave the worktree or have its work pushed.",
			kinds, false, func(ctx context.Context, p *fleet.RunPrincipal, in RemoveInput) (any, error) {
				if in.CommandID != "" {
					if in.Repo != "" || in.Name != "" {
						return nil, apperr.New(apperr.CodeInvalid, "give command_id alone, or repo and name")
					}
					return svc.Result(ctx, p.SpaceID, in.CommandID)
				}
				if in.Repo == "" || in.Name == "" {
					return nil, apperr.New(apperr.CodeInvalid, "repo and name are required (or command_id to ask about an earlier removal)")
				}
				return svc.Remove(ctx, actorOf(p), p.SpaceID, in.Repo, in.Name, 0)
			})
		agenttools.Add(r, "spaceman_worktrees_prune", "Remove the worktrees of finished work: those whose owning task is done or cancelled and whose "+
			"owning run ended (at least older_than_days ago), oldest first, at most limit. Each goes through the same safe removal as "+
			"spaceman_worktree_remove, so dirty, unpushed or held ones are refused with their code and stay. Unowned worktrees and those of "+
			"failed or open tasks are never picked. Without apply it only lists what it would try. The node forgets stale worktree entries "+
			"whose folder is gone as part of each removal; there is no separate stale-only prune.",
			kinds, false, func(ctx context.Context, p *fleet.RunPrincipal, in PruneInput) (any, error) {
				limit := in.Limit
				if limit <= 0 {
					limit = DefaultPruneLimit
				}
				if limit > MaxPruneLimit {
					return nil, apperr.New(apperr.CodeInvalid, "limit: at most %d", MaxPruneLimit)
				}
				if in.OlderThanDays < 0 || in.OlderThanDays > 36500 {
					return nil, apperr.New(apperr.CodeInvalid, "older_than_days: 0 to 36500")
				}
				all, err := svc.Candidates(ctx, p.SpaceID, in.Repo, in.OlderThanDays, 0)
				if err != nil {
					return nil, err
				}
				picked := all[:min(len(all), limit)]
				res := &PruneResult{Applied: in.Apply, Candidates: len(all),
					Note: "Finished work only (task done or cancelled, run ended). The node checks every worktree again and refuses what has work only on the node."}
				if !in.Apply {
					res.Would = picked
					return res, nil
				}
				// Queue every removal first, then wait for the answers together.
				pending := make([]*Outcome, 0, len(picked))
				for _, e := range picked {
					o, err := svc.Request(ctx, actorOf(p), p.SpaceID, e.Repo, e.Name)
					if err != nil {
						o = &Outcome{Repo: e.Repo, Name: e.Name, Status: StatusError, Reason: err.Error()}
					}
					pending = append(pending, o)
				}
				deadline := time.Now().Add(svc.wait())
				for i, o := range pending {
					if o.Status == StatusPending {
						if got, err := svc.Await(ctx, p.SpaceID, o, max(time.Until(deadline), time.Second)); err == nil {
							pending[i] = got
						}
					}
					res.Results = append(res.Results, *pending[i])
				}
				return res, nil
			})
	}
}

func actorOf(p *fleet.RunPrincipal) audit.Actor {
	return audit.Actor{Type: audit.ActorAgent, ID: p.RunID}
}
