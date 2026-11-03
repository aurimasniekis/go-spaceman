package app_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
)

// SM-CTRL-001/003, SM-REPO-002: control starts a worker for an eligible task through its tools; the worker runs in its
// own git worktree, is assigned to the task, and reports review; workers cannot launch runs; control cannot be
// stopped by control.
func TestControlLaunchesWorkersInWorktrees(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := newFleetEnv(t)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	admin := audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "ctl", Name: "Ctl", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	// A repository in the space folder on the node.
	repo := filepath.Join(e.agentDir, "spaces", sp.Workdir, "repos", "api")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	ctrl, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "control", Role: "control", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "control running", func() bool { return e.run1(ctrl.ID).Observed == "running" })
	ct := e.runToken(ctrl.ID)
	task, _, err := e.c.Queue.Create(ctx, admin, sp.ID, queue.CreateInput{Title: "Add rate limiting", Objective: "429 after 100 req/min", Status: queue.Ready})
	if err != nil {
		t.Fatal(err)
	}
	blocked, _, _ := e.c.Queue.Create(ctx, admin, sp.ID, queue.CreateInput{Title: "Blocked one", Status: queue.Ready})
	if _, err := e.c.Queue.AddBlocker(ctx, admin, sp.ID, blocked.ID, "external", "", "waiting for the vendor"); err != nil {
		t.Fatal(err)
	}
	if res := e.bridgeCall(ctrl.ID, ct, "spaceman_worker_launch", map[string]any{"task_id": blocked.ID, "role": "implementor"}); res.OK || res.Code != "precondition_failed" {
		t.Fatalf("blocked task launched: %+v", res)
	}

	res := e.bridgeCall(ctrl.ID, ct, "spaceman_worker_launch", map[string]any{"task_id": task.ID, "role": "implementor-backend", "repo": "repos/api", "prompt": "Keep it small."})
	if !res.OK {
		t.Fatalf("launch: %+v", res)
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(res.Result, &out)
	eventually(t, "worker running", func() bool { return e.run1(out.RunID).Observed == "running" })
	w := e.run1(out.RunID)
	if w.Kind != "worker" || w.Role != "implementor-backend" || !strings.Contains(w.Launch.Prompt, "Add rate limiting") || !strings.Contains(w.Launch.Prompt, "Keep it small.") {
		t.Fatalf("worker %+v", w)
	}
	got, _ := e.c.Queue.Get(ctx, sp.ID, task.ID)
	if got.Status != "running" || got.AssigneeRun != out.RunID || got.Workspace != "worktrees/spaceman-"+task.ID {
		t.Fatalf("task %+v", got)
	}
	// The worker's terminal runs in its own worktree on the task branch.
	var cwd string
	for _, s := range e.run.Specs {
		if s.Labels["spaceman.run"] == out.RunID {
			cwd = s.Cwd
		}
	}
	if !strings.HasSuffix(cwd, filepath.Join("worktrees", "spaceman-"+task.ID)) {
		t.Fatalf("worker cwd %q", cwd)
	}
	if b, err := exec.Command("git", "-C", cwd, "branch", "--show-current").Output(); err != nil || strings.TrimSpace(string(b)) != "spaceman/"+task.ID {
		t.Fatalf("worktree branch %q %v", b, err)
	}

	wt := e.runToken(out.RunID)
	if res := e.bridgeCall(out.RunID, wt, "spaceman_worker_launch", map[string]any{"task_id": task.ID, "role": "x"}); res.OK || res.Code != "forbidden" {
		t.Fatalf("worker launching: %+v", res)
	}
	if res := e.bridgeCall(out.RunID, wt, "spaceman_task_set_status", map[string]any{"task_id": task.ID, "status": "review", "result": "Done in branch."}); !res.OK {
		t.Fatalf("worker review: %+v", res)
	}
	if res := e.bridgeCall(ctrl.ID, ct, "spaceman_runs_list", nil); !res.OK || !strings.Contains(string(res.Result), task.ID) {
		t.Fatalf("runs list: %+v", res)
	}
	if res := e.bridgeCall(ctrl.ID, ct, "spaceman_worker_stop", map[string]any{"run_id": ctrl.ID}); res.OK || res.Code != "forbidden" {
		t.Fatalf("control stopping itself: %+v", res)
	}
	if res := e.bridgeCall(ctrl.ID, ct, "spaceman_worker_stop", map[string]any{"run_id": out.RunID, "mode": "kill", "reason": "review done"}); !res.OK {
		t.Fatalf("stop worker: %+v", res)
	}
	eventually(t, "worker ended", func() bool { o := e.run1(out.RunID).Observed; return o == "exited" || o == "failed" })
}

// SM-CTRL-002: control is notified of actionable events, and an unexpectedly ended control run is restarted (resuming
// its session) within bounds; beyond them SpaceMan stops and raises an urgent human action.
func TestControlSupervisor(t *testing.T) {
	e := newFleetEnv(t)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	admin := audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}
	e.c.Supervisor.MaxRestarts = 2
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "sup", Name: "Sup", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	ctrl, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "control", Role: "control", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "control running", func() bool { return e.run1(ctrl.ID).Observed == "running" })
	toControl := func(subject string) bool {
		l, _ := e.c.Messages.List(ctx, sp.ID, 100)
		for _, m := range l {
			if m.ToKind == "control" && m.FromKind == "system" && strings.Contains(m.Subject, subject) {
				return true
			}
		}
		return false
	}

	// Actionable events become notifications to control.
	if _, _, err := e.c.Inbox.Create(ctx, admin, sp.ID, inbox.CreateInput{SourceKind: "operator", SourceID: "usr_admin", Body: "Customers report slow refunds"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "inbox notification", func() bool { return toControl("New inbox item: Customers report slow refunds") })
	task, _, _ := e.c.Queue.Create(ctx, admin, sp.ID, queue.CreateInput{Title: "Speed up refunds", Status: queue.Ready})
	eventually(t, "new-task notification", func() bool { return toControl("New task: Speed up refunds") })
	// An operator moving a backlog task to ready: control can start it now.
	later, _, _ := e.c.Queue.Create(ctx, admin, sp.ID, queue.CreateInput{Title: "Archive old refunds", Status: queue.Backlog})
	if _, err := e.c.Queue.SetStatus(ctx, admin, sp.ID, later.ID, later.Version, queue.StatusInput{Status: queue.Ready}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "ready notification", func() bool { return toControl("Task ready: Archive old refunds") })
	task, _ = e.c.Queue.SetStatus(ctx, admin, sp.ID, task.ID, task.Version, queue.StatusInput{Status: "running"})
	if _, err := e.c.Queue.SetStatus(ctx, admin, sp.ID, task.ID, task.Version, queue.StatusInput{Status: "review", Result: "PR #7"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "review notification", func() bool { return toControl("Task review: Speed up refunds") })
	wrk, _ := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude})
	eventually(t, "worker running", func() bool { return e.run1(wrk.ID).Observed == "running" })
	e.run.Exit(e.run1(wrk.ID).TerminalID, 2, "crashed")
	eventually(t, "worker-ended notification", func() bool { return toControl("implementor run ended") })

	// Control crashes: it is restarted, resuming its session, until the budget is spent.
	current := ctrl.ID
	for i := 0; i < 2; i++ {
		e.run.Exit(e.run1(current).TerminalID, 1, "crashed")
		var next string
		eventually(t, "control restarted", func() bool {
			runs, _ := e.c.Fleet.ListRuns(ctx, sp.ID, 50)
			for _, r := range runs {
				if r.Kind == "control" && r.ID != current && r.Observed == "running" && r.CreatedBy == "system:control-supervisor" {
					next = r.ID
					return true
				}
			}
			return false
		})
		if n := e.run1(next); n.InstanceID != e.run1(current).InstanceID || (e.run1(current).SessionID != "" && n.SessionID != e.run1(current).SessionID) {
			t.Fatalf("restart did not resume: %+v", n)
		}
		current = next
	}
	e.run.Exit(e.run1(current).TerminalID, 1, "crashed")
	eventually(t, "attention raised", func() bool {
		l, _ := e.c.Human.List(ctx, sp.ID, human.ListFilter{Pending: true})
		return len(l) == 1 && l[0].Priority == "urgent" && strings.Contains(l[0].Title, "Control keeps stopping")
	})
	time.Sleep(500 * time.Millisecond)
	runs, _ := e.c.Fleet.ListRuns(ctx, sp.ID, 50)
	for _, r := range runs {
		if r.Kind == "control" && r.Observed == "running" {
			t.Fatalf("control restarted beyond its budget: %s", r.ID)
		}
	}
	// An operator's stop is not a failure: no restart.
	again, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "control", Role: "control", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "control running", func() bool { return e.run1(again.ID).Observed == "running" })
	if _, err := e.c.Fleet.StopRun(ctx, admin, again.ID, "kill", "done for today"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "control stopped", func() bool { o := e.run1(again.ID).Observed; return o == "exited" || o == "failed" })
	time.Sleep(500 * time.Millisecond)
	runs, _ = e.c.Fleet.ListRuns(ctx, sp.ID, 50)
	for _, r := range runs {
		if r.Kind == "control" && r.Observed == "running" {
			t.Fatalf("control restarted after an operator stop: %s", r.ID)
		}
	}
}
