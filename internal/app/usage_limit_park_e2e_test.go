package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/limits"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// parkHello is the hello of a scripted minor-12 node with Claude Code.
func parkHello() proto.Hello {
	return proto.Hello{AgentVersion: "test", MinMinor: proto.MinMinor, MaxMinor: proto.MinorUsageLimit, Runs: []proto.RunReport{}, Leases: []proto.LeaseState{},
		Capabilities: proto.Capabilities{Harnesses: []proto.HarnessInfo{{Kind: "claude-code", Version: "2.1.322", Available: true, Capabilities: []string{}}}}}
}

// command reads the node's commands, acknowledging each, until one of kind for run arrives.
func (n *rawNode) command(t *testing.T, runID, kind string) proto.Command {
	t.Helper()
	for {
		var c proto.Command
		if err := n.next(t, proto.TypeCommand).Decode(&c); err != nil {
			t.Fatal(err)
		}
		n.send(t, proto.TypeAck, proto.Ack{CommandID: c.CommandID, Result: proto.AckAccepted})
		if c.RunID == runID && c.Kind == kind {
			return c
		}
	}
}

// launchFor reads the node's commands until a launch of the instance arrives, skipping (redelivered) launches of the
// runs in skip.
func (n *rawNode) launchFor(t *testing.T, instanceID string, skip ...string) (proto.Command, proto.LaunchPayload) {
	t.Helper()
	for {
		var c proto.Command
		if err := n.next(t, proto.TypeCommand).Decode(&c); err != nil {
			t.Fatal(err)
		}
		n.send(t, proto.TypeAck, proto.Ack{CommandID: c.CommandID, Result: proto.AckAccepted})
		if c.Kind != proto.CmdLaunch || slices.Contains(skip, c.RunID) {
			continue
		}
		var p proto.LaunchPayload
		if err := json.Unmarshal(c.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.InstanceID == instanceID {
			return c, p
		}
	}
}

// enrollNode enrolls another node and returns its credential.
func (e *fleetEnv) enrollNode(name string) (string, string) {
	e.t.Helper()
	en, err := e.c.Fleet.CreateNode(context.Background(), admin, fleet.CreateNodeInput{Name: name})
	if err != nil {
		e.t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"token": en.Token})
	res, err := http.Post(e.srv.URL+fleet.EnrollPath, "application/json", bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	var er fleet.EnrollResult
	if err := json.Unmarshal(b, &er); err != nil || res.StatusCode != http.StatusOK {
		e.t.Fatalf("enroll %s: %d %s", name, res.StatusCode, b)
	}
	return en.Node.ID, er.Credential
}

// runLimitWaits runs central's deadline loop with the limit-wait job, as Serve does (the test central isn't served).
func runLimitWaits(t *testing.T, e *fleetEnv) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go e.c.Human.Run(ctx, human.Deadline{Name: "limit_waits", Next: e.c.Fleet.NextLimitWait, Due: e.c.Fleet.LimitWaitsDue})
}

// assignedWorker launches a worker for a new task and assigns the task to it, as control's spaceman_worker_launch does.
func assignedWorker(t *testing.T, e *fleetEnv, n *rawNode, spaceID, title, account string) (*fleet.Run, int64, *queue.Task) {
	t.Helper()
	ctx := context.Background()
	task, _, err := e.c.Queue.Create(ctx, admin, spaceID, queue.CreateInput{Title: title, Status: queue.Ready})
	if err != nil {
		t.Fatal(err)
	}
	w, gen := launchGen(t, e, n, spaceID, fleet.LaunchInput{Role: "free", Kind: "worker", Harness: "claude-code", Prompt: "do the task",
		AccountRef: account})
	if task, err = e.c.Queue.SetStatus(ctx, admin, spaceID, task.ID, task.Version, queue.StatusInput{Status: queue.Running}); err != nil {
		t.Fatal(err)
	}
	if task, err = e.c.Queue.Update(ctx, admin, spaceID, task.ID, task.Version, queue.UpdateInput{Title: task.Title, Priority: task.Priority,
		AssigneeRun: w.ID}); err != nil {
		t.Fatal(err)
	}
	return w, gen, task
}

func (e *fleetEnv) task(spaceID, id string) *queue.Task {
	e.t.Helper()
	tk, err := e.c.Queue.Get(context.Background(), spaceID, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return tk
}

// ADR-0079 decisions 4 and 5, UL-7 (simulated: a scripted minor-12 node, central on a fake clock, the deadline loop
// running as in Serve). A worker reaches its weekly limit, far beyond the keep-alive: SpaceMan stops it and parks it;
// its end is usage_limit, its task stays assigned, and control gets one "parked" message and no run-ended notice. The
// node then goes away. At the reset the resume is put off (no lease); the node's hello brings it back at once: a new
// run of the same instance, under the current lease, with --resume of its session, the task pointing at it, and one
// "resumed" message to control.
func TestUsageLimitParkAndResume(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	e := newFleetEnvClock(t, clk)
	e.enroll()
	ctx := context.Background()
	n, sp := hvNode(t, e, "ulpark", proto.MinorUsageLimit, false)
	runLimitWaits(t, e)
	w1, gen, task := assignedWorker(t, e, n, sp.ID, "Park me", "")

	since := clk.Now()
	reset := since.Add(3 * 24 * time.Hour)
	limited := &proto.RunLimit{Kind: proto.LimitSevenDay, ResetsAt: &reset, Since: since}
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: w1.ID, SpaceID: sp.ID, Generation: gen, State: proto.RunRunning, Status: "idle", Turn: 2,
		SessionID: "ses-park", Limit: limited, ObservedAt: time.Now().UTC()})
	// Parked at once (the reset is beyond the 6 h keep-alive): central stops it.
	n.command(t, w1.ID, proto.CmdStop)
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: w1.ID, SpaceID: sp.ID, Generation: gen, State: proto.RunExited, Reason: "stopped", Turn: 2,
		SessionID: "ses-park", Limit: limited, ObservedAt: time.Now().UTC()})
	eventually(t, "parked", func() bool {
		r := e.run1(w1.ID)
		return r.Observed == proto.RunExited && r.EndReason == fleet.EndUsageLimit
	})
	wait, err := e.c.Fleet.LimitWaitForRun(ctx, w1.ID)
	if err != nil || wait == nil || wait.State != fleet.WaitWaiting || wait.TaskID != task.ID || !wait.ResumeAt.Equal(reset.Add(2*time.Minute)) {
		t.Fatalf("wait %+v %v", wait, err)
	}
	if tk := e.task(sp.ID, task.ID); tk.Status != queue.Running || tk.AssigneeRun != w1.ID {
		t.Fatalf("parked task %+v", tk)
	}
	eventually(t, "control told it parked", func() bool { return systemToControl(t, e, sp.ID, "parked at its usage limit") == 1 })

	// The node goes away; the reset comes.
	_ = n.conn.Close(websocket.StatusNormalClosure, "node restarts")
	clk.Set(reset.Add(2 * time.Minute))
	e.c.Human.Kick()
	eventually(t, "resume put off: node away", func() bool {
		w, _ := e.c.Fleet.LimitWaitForRun(ctx, w1.ID)
		return w.LastError == fleet.WaitNodeOffline && w.State == fleet.WaitWaiting
	})

	// The node's hello brings the resume forward at once.
	n2 := dialRawNodeHello(t, e, parkHello())
	cmd, p := n2.launchFor(t, w1.InstanceID)
	if p.SessionID != "ses-park" || !p.Resume || p.Kind != "worker" || p.Role != w1.Role || !strings.Contains(p.Prompt, "usage limit; it has reset") {
		t.Fatalf("resume launch %+v", p)
	}
	s, _ := e.c.Fleet.GetSpace(ctx, sp.ID)
	if s.Lease == nil || cmd.Generation != s.Lease.Generation {
		t.Fatalf("resumed under generation %d, lease %+v", cmd.Generation, s.Lease)
	}
	resumed := e.run1(cmd.RunID)
	if resumed.InstanceID != w1.InstanceID || resumed.ID == w1.ID || resumed.SessionID != "ses-park" {
		t.Fatalf("resumed run %+v", resumed)
	}
	eventually(t, "task re-pointed", func() bool { return e.task(sp.ID, task.ID).AssigneeRun == resumed.ID })
	if w, _ := e.c.Fleet.LimitWaitForRun(ctx, w1.ID); w.State != fleet.WaitResumed || w.NewRunID != resumed.ID {
		t.Fatalf("wait after resume %+v", w)
	}
	eventually(t, "control told it resumed", func() bool { return systemToControl(t, e, sp.ID, "resumed after its usage limit") == 1 })
	// The resumed run hits a limit again (the wrap-up path: no reset) and is lost: re-parked, with the release time carried
	// forward and no new message to control.
	again := &proto.RunLimit{Kind: proto.LimitUnknown, Since: clk.Now()}
	n2.send(t, proto.TypeRunReport, proto.RunReport{RunID: resumed.ID, SpaceID: sp.ID, Generation: cmd.Generation, State: proto.RunRunning,
		Status: "idle", Turn: 1, SessionID: "ses-park", Limit: again, ObservedAt: time.Now().UTC()})
	n2.send(t, proto.TypeRunReport, proto.RunReport{RunID: resumed.ID, SpaceID: sp.ID, Generation: cmd.Generation, State: proto.RunLost,
		Reason: "lost", Turn: 1, SessionID: "ses-park", Limit: again, ObservedAt: time.Now().UTC()})
	eventually(t, "re-parked", func() bool { w, _ := e.c.Fleet.LimitWaitForRun(ctx, resumed.ID); return w != nil })
	if w, _ := e.c.Fleet.LimitWaitForRun(ctx, resumed.ID); w.ReleaseAt == nil || !w.ReleaseAt.Equal(*wait.ReleaseAt) || w.Attempts != 1 {
		t.Fatalf("re-park %+v, first %+v", w, wait)
	}
	time.Sleep(300 * time.Millisecond)
	if c := systemToControl(t, e, sp.ID, "run ended"); c != 0 {
		t.Fatalf("control got %d run-ended notices for a parked run", c)
	}
	if c := systemToControl(t, e, sp.ID, "parked at its usage limit") + systemToControl(t, e, sp.ID, "resumed after its usage limit"); c != 2 {
		t.Fatalf("control got %d park/resume messages, want 2", c)
	}
}

// Decisions 5 and 6, UL-7 (simulated: two scripted nodes, fake clock): a worker parked on node A resumes after the space
// moved to node B in a fresh session (its session is node-local) with its original prompt, and control is told; a
// worker whose wait outlives usage_limit_max_wait_hours (lowered to 1 h) is given up: its task is ready again, unassigned, and control
// is told once.
func TestUsageLimitParkRelocatedAndReleased(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	e := newFleetEnvClock(t, clk, func(c *config.Config) { c.Limits.MaxTopLevelTasks = 5 })
	e.enroll()
	ctx := context.Background()
	n, sp := hvNode(t, e, "ulreloc", proto.MinorUsageLimit, false)
	runLimitWaits(t, e)
	endAtLimit := func(r *fleet.Run, gen int64, l *proto.RunLimit) {
		t.Helper()
		n.send(t, proto.TypeRunReport, proto.RunReport{RunID: r.ID, SpaceID: sp.ID, Generation: gen, State: proto.RunRunning, Status: "idle", Turn: 1,
			SessionID: "ses-" + r.ID, Limit: l, ObservedAt: time.Now().UTC()})
		n.send(t, proto.TypeRunReport, proto.RunReport{RunID: r.ID, SpaceID: sp.ID, Generation: gen, State: proto.RunLost, Reason: "lost", Turn: 1,
			SessionID: "ses-" + r.ID, Limit: l, ObservedAt: time.Now().UTC()})
		eventually(t, "parked", func() bool { w, _ := e.c.Fleet.LimitWaitForRun(ctx, r.ID); return w != nil })
	}
	now := clk.Now()
	soon, late := now.Add(2*time.Hour), now.Add(5*24*time.Hour)
	w1, gen1, t1 := assignedWorker(t, e, n, sp.ID, "Relocate me", "")
	endAtLimit(w1, gen1, &proto.RunLimit{Kind: proto.LimitFiveHour, ResetsAt: &soon, Since: now})
	// The space's maximum wait drops to an hour (it applies to runs parked from now on), and a second worker parks, on
	// another account: the first's is held now.
	if _, err := e.c.Limits.Set(ctx, admin, sp.ID, 0, limits.Input{Values: map[string]int64{limits.UsageLimitMaxWait: 1}}); err != nil {
		t.Fatal(err)
	}
	w2, gen2, t2 := assignedWorker(t, e, n, sp.ID, "Give me up", bindAnotherClaude(t, e, sp.ID))
	endAtLimit(w2, gen2, &proto.RunLimit{Kind: proto.LimitSevenDay, ResetsAt: &late, Since: now})
	if w, _ := e.c.Fleet.LimitWaitForRun(ctx, w2.ID); w.ReleaseAt == nil || !w.ReleaseAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("release at %+v, want 1 h (the space's limit)", w.ReleaseAt)
	}

	// The space moves to node B (as a finished relocation leaves it), whose hello takes the lease.
	nodeB, credB := e.enrollNode("node-b")
	if err := e.c.Store.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `UPDATE spaces SET node_id = ? WHERE id = ?`, nodeB, sp.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	credA := e.cred
	e.cred = credB
	nb := dialRawNodeHello(t, e, parkHello())
	e.cred = credA
	eventually(t, "lease on B", func() bool {
		s, _ := e.c.Fleet.GetSpace(ctx, sp.ID)
		return s.Lease != nil && s.Lease.Valid && s.NodeID == nodeB
	})

	// One hour later the worker on B is given up; at the 5-hour reset the other resumes on B in a fresh session.
	clk.Set(now.Add(time.Hour))
	e.c.Human.Kick()
	eventually(t, "released", func() bool { w, _ := e.c.Fleet.LimitWaitForRun(ctx, w2.ID); return w.State == fleet.WaitReleased })
	if tk := e.task(sp.ID, t2.ID); tk.Status != queue.Ready || tk.AssigneeRun != "" {
		t.Fatalf("released task %+v", tk)
	}
	eventually(t, "control told it was given up", func() bool { return systemToControl(t, e, sp.ID, "given up at its usage limit") == 1 })

	clk.Set(soon.Add(2 * time.Minute))
	nb.send(t, proto.TypeHeartbeat, proto.Heartbeat{Leases: []proto.LeaseState{}}) // renews the lease at the new time
	eventually(t, "lease renewed", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	e.c.Human.Kick()
	cmd, p := nb.launchFor(t, w1.InstanceID)
	if p.SessionID != "" || p.Resume || !strings.HasPrefix(p.Prompt, "do the task") || !strings.Contains(p.Prompt, "fresh session") {
		t.Fatalf("relocated resume %+v", p)
	}
	if r := e.run1(cmd.RunID); r.NodeID != nodeB {
		t.Fatalf("resumed on %s", r.NodeID)
	}
	eventually(t, "task re-pointed", func() bool { return e.task(sp.ID, t1.ID).AssigneeRun == cmd.RunID })
	eventually(t, "control told: fresh session", func() bool {
		l, _ := e.c.Messages.List(ctx, sp.ID, 200)
		for _, m := range l {
			if strings.Contains(m.Subject, "resumed after its usage limit") && strings.Contains(m.Body, "fresh session") && strings.Contains(m.Body, cmd.RunID) {
				return true
			}
		}
		return false
	})
	time.Sleep(300 * time.Millisecond)
	if c := systemToControl(t, e, sp.ID, "given up at its usage limit"); c != 1 {
		t.Fatalf("control told %d times about the release", c)
	}
}

// Review S4 and N5 (simulated: scripted node, fake clock): a control parked at its limit is resumed with --resume; the
// resume fails before its first turn. SpaceMan starts it once more in a fresh session (no --resume, its original
// prompt) through its wait; the supervisor doesn't restart it on its own (no restart budget used), and only the wait's
// launches happen.
func TestUsageLimitControlResumeFailsRestartsFresh(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	e := newFleetEnvClock(t, clk)
	e.enroll()
	ctx := context.Background()
	n, sp := hvNode(t, e, "ulctlfail", proto.MinorUsageLimit, false)
	runLimitWaits(t, e)
	ctrl, gen := launchGen(t, e, n, sp.ID, fleet.LaunchInput{Role: "ctl", Kind: "control", Harness: "claude-code", Prompt: "be control"})
	reset := clk.Now().Add(time.Hour)
	limited := &proto.RunLimit{Kind: proto.LimitFiveHour, ResetsAt: &reset, Since: clk.Now()}
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: ctrl.ID, SpaceID: sp.ID, Generation: gen, State: proto.RunRunning, Status: "idle", Turn: 3,
		SessionID: "ses-ctl", Limit: limited, ObservedAt: time.Now().UTC()})
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: ctrl.ID, SpaceID: sp.ID, Generation: gen, State: proto.RunExited, Reason: "exited", Turn: 3,
		SessionID: "ses-ctl", Limit: limited, ObservedAt: time.Now().UTC()})
	eventually(t, "control parked", func() bool { w, _ := e.c.Fleet.LimitWaitForRun(ctx, ctrl.ID); return w != nil })

	clk.Set(reset.Add(2 * time.Minute))
	n.send(t, proto.TypeHeartbeat, proto.Heartbeat{Leases: []proto.LeaseState{}}) // renews the lease at the new time
	eventually(t, "lease renewed", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	e.c.Human.Kick()
	cmd, p := n.launchFor(t, ctrl.InstanceID, ctrl.ID)
	if !p.Resume || p.SessionID != "ses-ctl" {
		t.Fatalf("resume %+v", p)
	}
	// --resume fails: the run ends before its first turn.
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: cmd.RunID, SpaceID: sp.ID, Generation: cmd.Generation, State: proto.RunFailed, Reason: "error",
		Error: "no conversation found", ObservedAt: time.Now().UTC()})
	cmd2, p2 := n.launchFor(t, ctrl.InstanceID, ctrl.ID, cmd.RunID)
	if p2.Resume || p2.SessionID != "" || !strings.HasPrefix(p2.Prompt, "be control") || !strings.Contains(p2.Prompt, "fresh session") {
		t.Fatalf("fresh retry %+v", p2)
	}
	time.Sleep(500 * time.Millisecond)
	var controls, bySupervisor int
	if err := e.c.Store.ReadDB().QueryRowContext(ctx, `SELECT count(*), coalesce(sum(created_by = 'system:control-supervisor'), 0) FROM runs
		WHERE space_id = ? AND kind = 'control'`, sp.ID).Scan(&controls, &bySupervisor); err != nil {
		t.Fatal(err)
	}
	if controls != 3 || bySupervisor != 0 {
		t.Fatalf("%d control runs, %d by the supervisor; want the original, the resume and one fresh retry (%s)", controls, bySupervisor, cmd2.RunID)
	}
}
