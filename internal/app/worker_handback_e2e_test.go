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
	"github.com/aurimasniekis/go-spaceman/internal/limits"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
)

// ADR-0099 addendum: an idle worker whose task is still running is reminded once through its terminal, and the reminder
// is audited as worker.watchdog_nudged.
func TestWorkerHandbackReminderAudited(t *testing.T) {
	t.Parallel()
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
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "handback", Name: "Handback", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
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
	task, _, err := e.c.Queue.Create(ctx, admin, sp.ID, queue.CreateInput{Title: "Idle task", Status: queue.Ready})
	if err != nil {
		t.Fatal(err)
	}
	res := e.bridgeCall(ctrl.ID, e.runToken(ctrl.ID), "spaceman_worker_launch", map[string]any{"task_id": task.ID, "role": "implementor", "repo": "repos/api"})
	if !res.OK {
		t.Fatalf("launch: %+v", res)
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(res.Result, &out)
	eventually(t, "worker running", func() bool { return e.run1(out.RunID).Observed == "running" })
	term := e.run1(out.RunID).TerminalID
	e.run.SetStatus(term, "idle")
	eventually(t, "worker idle", func() bool { return e.run1(out.RunID).Status == "idle" })

	unit := 150 * time.Millisecond
	e.c.Supervisor.IdleUnit = unit
	if _, err := e.c.Limits.Set(ctx, admin, sp.ID, 0, limits.Input{Values: map[string]int64{limits.ControlWatchdog: 0, limits.WorkerIdleWatchdog: 1}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "worker reminder audited", func() bool {
		e.c.Watchdog.Pass(ctx)
		return e.auditCount("worker.watchdog_nudged", audit.OutcomeSuccess) >= 1
	})
	eventually(t, "worker reminder typed", func() bool {
		for _, in := range e.run.Inputs(term) {
			if strings.HasPrefix(in, "SpaceMan watchdog:") && strings.Contains(in, task.ID) {
				return true
			}
		}
		return false
	})
	if n := e.auditCount("worker.watchdog_nudged", audit.OutcomeSuccess); n != 1 {
		t.Fatalf("%d worker nudge audit events", n)
	}
}
