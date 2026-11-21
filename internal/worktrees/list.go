package worktrees

import (
	"context"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// RunRef is the run that owns a worktree: the newest run SpaceMan launched with the worktree's repository and branch.
type RunRef struct {
	ID      string     `json:"id"`
	Role    string     `json:"role,omitempty"`
	State   string     `json:"state" doc:"The run's observed state"`
	Live    bool       `json:"live" doc:"The run is not ended: the worktree is held and is never removed"`
	EndedAt *time.Time `json:"ended_at,omitempty"`
}

// TaskRef is the task that owns a worktree: the task the owning run is assigned to.
type TaskRef struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// PullRequest is the pull request state central knows for the worktree's branch. Central only knows what events it
// received and no event is tied to a branch, so the state is always "unknown": no forge is asked per worktree
// (ADR-0107 decision 3).
type PullRequest struct {
	State string `json:"state" enum:"unknown"`
}

// Entry is one worktree of a configured repository.
type Entry struct {
	Repo      string `json:"repo" doc:"The repository's folder; pass it as repo to spaceman_worktree_remove"`
	Name      string `json:"name" doc:"The worktree's folder name inside worktrees/; pass it as name to spaceman_worktree_remove"`
	Path      string `json:"path"`
	Branch    string `json:"branch,omitempty"`
	Head      string `json:"head"`
	Dirty     int    `json:"dirty"`
	Untracked int    `json:"untracked"`
	// CountsFrom says where Dirty and Untracked come from: fresh, cached (an idle worktree's earlier counts) or
	// unchecked (unknown, never clean).
	CountsFrom string     `json:"counts_from,omitempty" enum:"fresh,cached,unchecked"`
	CountsAt   *time.Time `json:"counts_at,omitempty"`
	Owned      bool       `json:"owned" doc:"SpaceMan launched a run here; unowned worktrees (made by hand) are never pruned, only removed on request"`
	Run        *RunRef    `json:"run,omitempty"`
	Task       *TaskRef   `json:"task,omitempty"`
	// LastChange is the last time central knows anything happened in the worktree: the owning run's latest activity.
	// The node's report carries no folder times, so an unowned worktree has none.
	LastChange *time.Time `json:"last_change,omitempty" doc:"The owning run's latest activity (central's record; the node's report carries no folder times)"`
	// Pushed is whether the worktree's branch has no commit the remote lacks, as of the repository's last fetch
	// (PushedAsOf). Informational: a removal always checks again. Absent when it cannot be told (detached HEAD).
	Pushed      *bool        `json:"pushed,omitempty" doc:"The branch has no commit that no remote has, as of the last fetch (pushed_as_of); a removal checks again"`
	PushedAsOf  *time.Time   `json:"pushed_as_of,omitempty"`
	PullRequest *PullRequest `json:"pull_request,omitempty"`
	// Prunable is set when spaceman_worktrees_prune would pick it (older_than_days 0): the owning task is done or
	// cancelled and the owning run has ended.
	Prunable bool `json:"prunable" doc:"The owning task is done or cancelled and its run ended: the prune tool would try it (the node still checks everything)"`
}

// Listing is a space's worktrees.
type Listing struct {
	Worktrees  []Entry    `json:"worktrees"`
	ReportedAt *time.Time `json:"reported_at,omitempty" doc:"When the node's report these come from was received"`
	Stale      bool       `json:"stale,omitempty" doc:"The report is from an earlier node of the space"`
	Note       string     `json:"note"`
}

// List joins the node's last repository report with central's runs and tasks. repo (a repository name, ID or folder)
// limits it to one repository.
func (svc *Service) List(ctx context.Context, spaceID, repo string) (*Listing, error) {
	ov, err := svc.repos.List(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	runs, err := svc.fleet.ListRuns(ctx, spaceID, 1000)
	if err != nil {
		return nil, err
	}
	tasks, _, err := svc.queue.List(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	byRun := map[string]*TaskRef{}
	for _, t := range tasks {
		if t.AssigneeRun != "" {
			byRun[t.AssigneeRun] = &TaskRef{ID: t.ID, Title: t.Title, Status: t.Status}
		}
	}
	// ListRuns is newest first: the first run seen for a repository and folder is the owner.
	type key struct{ repo, name string }
	owners := map[key]*fleet.Run{}
	for _, r := range runs {
		if w := r.Launch.Worktree; w != nil && w.Branch != "" {
			k := key{strings.Trim(w.Repo, "/"), folderOf(w.Branch)}
			if _, ok := owners[k]; !ok {
				owners[k] = r
			}
		}
	}
	res := &Listing{Worktrees: []Entry{}, ReportedAt: ov.ReportedAt, Stale: ov.Stale,
		Note: "From the node's last repository report; counts and pushed state can be older (counts_at, pushed_as_of). Branch names and paths come from the repositories. " +
			"A removal always checks the worktree again on the node."}
	found := repo == ""
	for _, rp := range ov.Items {
		if repo != "" && repo != rp.ID && repo != rp.Name && repo != rp.Path {
			continue
		}
		found = true
		if rp.Status == nil {
			continue
		}
		branches := map[string]proto.BranchCheck{}
		for _, b := range rp.Status.Branches {
			branches[b.Name] = b
		}
		for _, w := range rp.Status.Worktrees {
			name, ok := strings.CutPrefix(w.Path, "worktrees/")
			if !ok || name == "" || strings.Contains(name, "/") {
				continue // only folders directly inside worktrees/ can be removed by SpaceMan
			}
			e := Entry{Repo: rp.Path, Name: name, Path: w.Path, Branch: w.Branch, Head: w.Head, Dirty: w.Dirty, Untracked: w.Untracked,
				CountsFrom: w.Status, CountsAt: w.StatusAt, PullRequest: &PullRequest{State: "unknown"}}
			if b, ok := branches[w.Branch]; ok && w.Branch != "" && rp.Status.LastFetchAt != nil {
				pushed := b.Unpushed == 0
				e.Pushed, e.PushedAsOf = &pushed, rp.Status.LastFetchAt
			}
			if r := owners[key{strings.Trim(rp.Path, "/"), name}]; r != nil {
				e.Owned = true
				e.Run = &RunRef{ID: r.ID, Role: r.Role, State: r.Observed, Live: isLive(r), EndedAt: r.EndedAt}
				e.Task = byRun[r.ID]
				e.LastChange = lastActivity(r)
				e.Prunable = prunable(e, 0, time.Time{})
			}
			res.Worktrees = append(res.Worktrees, e)
		}
	}
	if !found {
		return nil, apperr.New(apperr.CodeNotFound, "repository %q is not configured in this space", repo)
	}
	sort.SliceStable(res.Worktrees, func(i, j int) bool {
		a, b := res.Worktrees[i], res.Worktrees[j]
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		return a.Name < b.Name
	})
	return res, nil
}

func isLive(r *fleet.Run) bool {
	return slices.Contains([]string{"pending", "starting", "running", "stopping", "unknown"}, r.Observed)
}

// lastActivity is the newest time central recorded for the run.
func lastActivity(r *fleet.Run) *time.Time {
	t := r.CreatedAt
	for _, p := range []*time.Time{r.StartedAt, r.ObservedAt, r.EndedAt} {
		if p != nil && p.After(t) {
			t = *p
		}
	}
	return &t
}

// prunable reports whether an owned worktree is a candidate for the auto-prune rule (ADR-0107 decision 4): its
// owning task is done or cancelled and its owning run ended at least days before now (days 0: ended at all).
// Unowned worktrees, worktrees of failed or open tasks and worktrees with a live run are never candidates.
func prunable(e Entry, days int, now time.Time) bool {
	if !e.Owned || e.Run == nil || e.Run.Live || e.Run.EndedAt == nil || e.Task == nil {
		return false
	}
	if e.Task.Status != "done" && e.Task.Status != "cancelled" {
		return false
	}
	return days <= 0 || !e.Run.EndedAt.After(now.AddDate(0, 0, -days))
}

// Candidates are the worktrees the retention rule would remove now, oldest run end first, at most limit (ADR-0107
// decision 4). repo limits it to one repository; days is the age the owning run must have reached (0: any).
func (svc *Service) Candidates(ctx context.Context, spaceID, repo string, days, limit int) ([]Entry, error) {
	l, err := svc.List(ctx, spaceID, repo)
	if err != nil {
		return nil, err
	}
	now := svc.clk.Now()
	var out []Entry
	for _, e := range l.Worktrees {
		if prunable(e, days, now) {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Run.EndedAt.Before(*out[j].Run.EndedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
