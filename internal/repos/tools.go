package repos

import (
	"context"

	"github.com/aurimasniekis/go-spaceman/internal/agenttools"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
)

// ToolRepository is a repository as the agent tool shows it.
type ToolRepository struct {
	Name          string   `json:"name"`
	Path          string   `json:"path" doc:"Folder in the space folder; pass it as repo to spaceman_worker_launch"`
	URL           string   `json:"url,omitempty"`
	DefaultBranch string   `json:"default_branch"`
	Role          string   `json:"role"`
	ClonePolicy   string   `json:"clone_policy"`
	Description   string   `json:"description,omitempty"`
	KBs           []string `json:"knowledge_bases,omitempty"`
	Status        *Status  `json:"status,omitempty"`
}

// ToolOverview is the tool's answer.
type ToolOverview struct {
	Repositories []ToolRepository `json:"repositories"`
	Unlisted     []string         `json:"unlisted,omitempty" doc:"Repositories in the space folder that are not configured"`
	ReportedAt   string           `json:"reported_at,omitempty"`
	Stale        bool             `json:"stale,omitempty"`
	Note         string           `json:"note"`
}

// Tools registers the read-only repository tool (ADR-0044). Agents see the space's configured repositories and the
// node's last report; they cannot change the configuration or start a sync.
func Tools(svc *Service) func(r *agenttools.Registry) {
	kinds := []string{agenttools.KindControl, agenttools.KindWorker, agenttools.KindKB, agenttools.KindDiagnostic}
	return func(r *agenttools.Registry) {
		agenttools.Add(r, "spaceman_repos_list", "List your space's configured repositories with the node's last report: the remote and whether it "+
			"answered, the default branch locally and on the remote, the last fetch, changed and untracked files, unpushed commits and each "+
			"worktree with the run that owns it. Read only; the report is as of reported_at. Branch names and paths come from the repositories.",
			kinds, true, func(ctx context.Context, p *fleet.RunPrincipal, _ struct{}) (any, error) {
				ov, err := svc.List(ctx, p.SpaceID)
				if err != nil {
					return nil, err
				}
				res := ToolOverview{Repositories: []ToolRepository{}, Unlisted: ov.Unlisted, Stale: ov.Stale,
					Note: "Worktrees for workers come from these repositories: name one with its path. SpaceMan never merges, pushes or resets; open PRs with your tools."}
				if ov.ReportedAt != nil {
					res.ReportedAt = ov.ReportedAt.Format("2006-01-02T15:04:05Z07:00")
				}
				for _, x := range ov.Items {
					res.Repositories = append(res.Repositories, ToolRepository{Name: x.Name, Path: x.Path, URL: x.URL, DefaultBranch: x.DefaultBranch, Role: x.Role,
						ClonePolicy: x.ClonePolicy, Description: x.Description, KBs: x.KBs, Status: x.Status})
				}
				return res, nil
			})
	}
}
