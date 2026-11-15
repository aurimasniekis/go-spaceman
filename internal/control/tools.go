// Package control is the space's coordination layer (SM-CTRL-*): the orchestration tools only control runs may use
// (launching and stopping workers bound to tasks), and the supervisor that wakes control on actionable events and
// restarts it within bounds.
package control

import (
	"context"
	"fmt"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/agenttools"
	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
)

type launchIn struct {
	TaskID  string `json:"task_id" jsonschema:"the ready, eligible task the worker implements"`
	Role    string `json:"role" jsonschema:"the worker's role, e.g. implementor-backend or reviewer"`
	Harness string `json:"harness,omitempty" jsonschema:"claude-code or codex (default: the same as yours)"`
	Prompt  string `json:"prompt,omitempty" jsonschema:"extra instructions for the worker, added after the task"`
	Model   string `json:"model,omitempty"`
	Repo    string `json:"repo,omitempty" jsonschema:"a repository folder inside the space; the worker then gets its own git worktree"`
	Branch  string `json:"branch,omitempty" jsonschema:"the worktree branch (default spaceman/<task id>)"`
	Base    string `json:"base,omitempty" jsonschema:"what a new branch starts from (default the repository's HEAD)"`
}

type stopIn struct {
	RunID  string `json:"run_id"`
	Mode   string `json:"mode,omitempty" jsonschema:"interrupt (default, keeps the session resumable) or kill"`
	Reason string `json:"reason,omitempty"`
}

// RunRow is a run as control sees it.
type RunRow struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Role     string `json:"role"`
	Harness  string `json:"harness"`
	Observed string `json:"observed"`
	Status   string `json:"status,omitempty"`
	TaskID   string `json:"task_id,omitempty"`
	// Limit is the run's open usage limit (ADR-0079): the run is waiting for it to reset.
	Limit *fleet.RunLimit `json:"limit,omitempty"`
}

// Tools registers the orchestration tools (control only, SM-CTRL-001/003). Workers cannot launch runs at all, and no
// tool launches a control run, so a worker can never promote itself or start another control.
func Tools(fl *fleet.Service, q *queue.Service) func(r *agenttools.Registry) {
	ctrl := []string{agenttools.KindControl}
	all := []string{agenttools.KindControl, agenttools.KindWorker, agenttools.KindKB, agenttools.KindDreamer, agenttools.KindDiagnostic}
	return func(r *agenttools.Registry) {
		agenttools.Add(r, "spaceman_runs_list", "List your space's active runs with their role, state, program status, task and usage limit (a run with a limit is waiting for it to reset).", all, true,
			func(ctx context.Context, p *fleet.RunPrincipal, _ struct{}) (any, error) {
				runs, err := fl.ListRuns(ctx, p.SpaceID, 100)
				if err != nil {
					return nil, err
				}
				tasks, _, err := q.List(ctx, p.SpaceID)
				if err != nil {
					return nil, err
				}
				byRun := map[string]string{}
				for _, t := range tasks {
					if t.AssigneeRun != "" {
						byRun[t.AssigneeRun] = t.ID
					}
				}
				out := []RunRow{}
				for _, x := range runs {
					if !fleet.Live(x.Observed) {
						continue
					}
					out = append(out, RunRow{ID: x.ID, Kind: x.Kind, Role: x.Role, Harness: x.Harness, Observed: x.Observed, Status: x.Status, TaskID: byRun[x.ID],
						Limit: x.Limit})
				}
				return out, nil
			})
		agenttools.Add(r, "spaceman_worker_launch", "Start a worker run for a ready, eligible task (control only). The task becomes running and is assigned to the "+
			"worker; with repo set, the worker gets its own git worktree so writers never share a working tree. The worker reports with "+
			"spaceman_task_set_status (review); you decide when the task is done.", ctrl, false,
			func(ctx context.Context, p *fleet.RunPrincipal, in launchIn) (any, error) {
				return launchWorker(ctx, fl, q, p, in)
			})
		agenttools.Add(r, "spaceman_worker_stop", "Stop a worker, KB, dreamer or diagnostic run of your space (control only). Interrupt keeps the session resumable.",
			ctrl, false, func(ctx context.Context, p *fleet.RunPrincipal, in stopIn) (any, error) {
				run, err := fl.GetRun(ctx, in.RunID)
				if err != nil || run.SpaceID != p.SpaceID {
					return nil, apperr.New(apperr.CodeNotFound, "run %q not found in your space", in.RunID)
				}
				if run.Kind == agenttools.KindControl {
					return nil, apperr.New(apperr.CodeForbidden, "control runs are stopped by operators")
				}
				return fl.StopRun(ctx, actor(p), in.RunID, in.Mode, in.Reason)
			})
	}
}

func actor(p *fleet.RunPrincipal) audit.Actor {
	return audit.Actor{Type: audit.ActorAgent, ID: p.RunID}
}

func launchWorker(ctx context.Context, fl *fleet.Service, q *queue.Service, p *fleet.RunPrincipal, in launchIn) (any, error) {
	t, err := q.Get(ctx, p.SpaceID, in.TaskID)
	if err != nil {
		return nil, err
	}
	if t.Status != queue.Ready {
		return nil, apperr.New(apperr.CodePrecondition, "task %s is %s; only ready tasks get a worker", t.ID, t.Status)
	}
	if len(t.Waiting) > 0 {
		return nil, apperr.New(apperr.CodePrecondition, "task %s is not eligible: %s", t.ID, strings.Join(t.Waiting, "; "))
	}
	if in.Harness == "" {
		me, err := fl.GetRun(ctx, p.RunID)
		if err != nil {
			return nil, err
		}
		in.Harness = me.Harness
	}
	li := fleet.LaunchInput{Kind: agenttools.KindWorker, Role: in.Role, Harness: in.Harness, Model: in.Model, Prompt: workerPrompt(t, in.Prompt)}
	workspace := ""
	if in.Repo != "" {
		if in.Branch == "" {
			in.Branch = "spaceman/" + t.ID
		}
		li.Worktree = &proto.Worktree{Repo: in.Repo, Branch: in.Branch, Base: in.Base}
		workspace = "worktrees/" + strings.ReplaceAll(in.Branch, "/", "-")
	}
	run, err := fl.LaunchRun(ctx, actor(p), p.SpaceID, li)
	if err != nil {
		return nil, err
	}
	started, err := q.SetStatus(ctx, actor(p), p.SpaceID, t.ID, t.Version, queue.StatusInput{Status: queue.Running, Reason: "worker " + run.ID})
	if err == nil {
		started, err = q.Update(ctx, actor(p), p.SpaceID, t.ID, started.Version, queue.UpdateInput{Title: started.Title, Objective: started.Objective,
			Acceptance: started.Acceptance, Priority: started.Priority, Exclusions: started.Exclusions, AssigneeRole: in.Role, AssigneeRun: run.ID, Workspace: workspace})
	}
	if err != nil {
		// The task changed meanwhile (or stopped being eligible): do not leave an unassigned worker running.
		_, _ = fl.StopRun(ctx, actor(p), run.ID, "kill", "its task could not be started: "+err.Error())
		return nil, err
	}
	return map[string]any{"run_id": run.ID, "task": started}, nil
}

// workerPrompt is the worker's first instruction: the task as written (operator or control content), then how to
// report. It never grants anything; the worker's tools are fixed by its kind.
func workerPrompt(t *queue.Task, extra string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are a SpaceMan worker assigned task %s: %s\n", t.ID, t.Title)
	if t.Objective != "" {
		fmt.Fprintf(&b, "\nObjective:\n%s\n", t.Objective)
	}
	if t.Acceptance != "" {
		fmt.Fprintf(&b, "\nAcceptance criteria:\n%s\n", t.Acceptance)
	}
	if extra != "" {
		fmt.Fprintf(&b, "\nFrom control:\n%s\n", extra)
	}
	b.WriteString("\nWhen the work is ready, call spaceman_task_set_status with status review and a short result. If you are blocked, " +
		"message control with spaceman_message_send or ask a person with spaceman_human_ask. Never merge or push to protected branches yourself.")
	return b.String()
}
