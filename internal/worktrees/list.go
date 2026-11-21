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
	"github.com/aurimasniekis/go-spaceman/internal/queue"
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
	Prunable bool `json:"prunable" doc:"Every task of the owning run is done or cancelled and the run ended: the prune tool would try it (the node still checks everything)"`

	// tasksDone is set when every task assigned to the owning run is done or cancelled; finishedAt is the latest
	// finished_at of those tasks, nil when there are none or any lacks one (then the worktree is never a candidate).
	tasksDone  bool
	finishedAt *time.Time
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
	byRun := map[string]*runTasks{}
	for _, t := range tasks {
		if t.AssigneeRun != "" {
			rt := byRun[t.AssigneeRun]
			if rt == nil {
				rt = &runTasks{done: true}
				byRun[t.AssigneeRun] = rt
			}
			rt.add(t)
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
				if rt := byRun[r.ID]; rt != nil {
					e.Task, e.tasksDone, e.finishedAt = rt.ref, rt.done, rt.finishedAt()
				}
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

// runTasks collects the tasks assigned to one run (a task can be pointed at an existing run, so a run can have several).
type runTasks struct {
	ref     *TaskRef // the first task that is not finished, else the first task
	done    bool     // every task is done or cancelled
	latest  *time.Time
	missing bool // a finished task has no finished_at
}

func (rt *runTasks) add(t *queue.Task) {
	finished := t.Status == "done" || t.Status == "cancelled"
	if rt.ref == nil || (rt.done && !finished) {
		rt.ref = &TaskRef{ID: t.ID, Title: t.Title, Status: t.Status}
	}
	if !finished {
		rt.done = false
		return
	}
	if t.FinishedAt == nil {
		rt.missing = true
	} else if rt.latest == nil || t.FinishedAt.After(*rt.latest) {
		rt.latest = t.FinishedAt
	}
}

func (rt *runTasks) finishedAt() *time.Time {
	if rt.missing {
		return nil
	}
	return rt.latest
}

// finishedSince is when the work counts as finished: the later of the run's end and the latest finished_at of the
// run's tasks (a worker usually ends its run when it hands the task over, and the task is marked done later). Nil
// when either is unknown.
func finishedSince(e Entry) *time.Time {
	if e.Run == nil || e.Run.EndedAt == nil || e.finishedAt == nil {
		return nil
	}
	t := *e.Run.EndedAt
	if e.finishedAt.After(t) {
		t = *e.finishedAt
	}
	return &t
}

// prunable reports whether an owned worktree is a candidate for the auto-prune rule (ADR-0107 decision 4): every task
// of its owning run is done or cancelled (each with a finished_at), the run ended, and the later of the run's end and
// the latest finished_at is at least days before now (days 0: any age). Unowned worktrees, worktrees with a failed or
// open task and worktrees with a live run are never candidates.
func prunable(e Entry, days int, now time.Time) bool {
	if !e.Owned || e.Run == nil || e.Run.Live || e.Run.EndedAt == nil || e.Task == nil || !e.tasksDone {
		return false
	}
	since := finishedSince(e)
	if since == nil {
		return false
	}
	return days <= 0 || !since.After(now.AddDate(0, 0, -days))
}

// Candidates are the worktrees the retention rule would remove now, at most limit (ADR-0107 decision 4). repo limits
// it to one repository; days is the age the finished work must have reached (0: any), counted from the later of the
// run's end and its tasks' latest finished_at. Order: worktrees whose last removal was not refused at their current
// HEAD first, then the ones that were (a refusal repeats until the worktree changes, so they must not use up the batch
// every day); within each, the oldest first.
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
	refused, err := svc.refusedHeads(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	again := func(e Entry) bool { h, ok := refused[[2]string{e.Repo, e.Name}]; return ok && h != "" && h == e.Head }
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := again(out[i]), again(out[j]); a != b {
			return !a
		}
		return finishedSince(out[i]).Before(*finishedSince(out[j]))
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
