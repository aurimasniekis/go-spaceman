package app_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
)

// SM-ARCH-004, ADR-0043: a space without live runs keeps no lease-guard process polling its lease file on the node.
// The guard lives while the space has runs, ends with the space's last run, and the next launch starts a new one
// before its run (which is parented to it, so dead-daemon enforcement still applies, ADR-0017).
func TestIdleSpaceKeepsNoLeaseGuard(t *testing.T) {
	e := newFleetEnv(t)
	ctx := context.Background()
	e.enroll()
	e.startAgent()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "idle-guard", Name: "Idle", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	if n := len(e.run.LiveByLabel("spaceman.guard", sp.ID)); n != 0 {
		t.Fatalf("a space that never ran has %d guards", n)
	}
	guard := func() string {
		g := e.run.LiveByLabel("spaceman.guard", sp.ID)
		if len(g) != 1 {
			return ""
		}
		return g[0].ID
	}
	launch := func(role string) *fleet.Run {
		r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: role, Harness: hns.KindClaude})
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, role+" running", func() bool { return e.run1(r.ID).Observed == "running" })
		return r
	}
	stop := func(r *fleet.Run) {
		if _, err := e.c.Fleet.StopRun(ctx, admin, r.ID, "kill", "test"); err != nil {
			t.Fatal(err)
		}
		eventually(t, "run ended", func() bool { o := e.run1(r.ID).Observed; return o == "exited" || o == "failed" })
	}
	parent := func(r *fleet.Run) string { return e.run.LiveByLabel("spaceman.run", r.ID)[0].Parent }

	a, b := launch("implementor"), launch("reviewer")
	g1 := guard()
	if g1 == "" || parent(a) != g1 || parent(b) != g1 {
		t.Fatalf("runs not under one guard: guard %q, parents %q %q", g1, parent(a), parent(b))
	}
	// One run ends: the other still needs the guard.
	stop(a)
	time.Sleep(300 * time.Millisecond)
	if guard() != g1 {
		t.Fatal("the guard ended while the space still had a live run")
	}
	// The last run ends: the idle space keeps no guard.
	stop(b)
	eventually(t, "guard retired with the space's last run", func() bool { return len(e.run.LiveByLabel("spaceman.guard", sp.ID)) == 0 })
	// The next launch starts a fresh guard first and runs under it.
	c := launch("planner")
	g2 := guard()
	if g2 == "" || g2 == g1 || parent(c) != g2 {
		t.Fatalf("relaunch: guard %q (was %q), parent %q", g2, g1, parent(c))
	}
}

// SM-INV-005, SM-RUN-003: the node daemon is not a controller. While central is unreachable it refuses agent tool
// calls (orchestration, inbox, queue) instead of acting on them itself, starts no worker, and does not replay the
// refused calls when central returns; managed workers are created only through central.
func TestNodeDaemonIsNotAController(t *testing.T) {
	e := newFleetEnv(t)
	ctx := context.Background()
	e.enroll()
	e.startAgent()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "no-ctl", Name: "No controller", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	ctrl, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "control", Role: "control", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "control running", func() bool { return e.run1(ctrl.ID).Observed == "running" })
	tok := e.runToken(ctrl.ID)
	if res := e.bridgeCall(ctrl.ID, tok, "spaceman_inbox_add", map[string]any{"title": "while connected", "body": "x"}); !res.OK {
		t.Fatalf("inbox add while connected: %+v", res)
	}
	items := func() int {
		p, err := e.c.Inbox.List(ctx, sp.ID, "", "", 100)
		if err != nil {
			t.Fatal(err)
		}
		return len(p.Items)
	}
	if items() != 1 {
		t.Fatal("item not stored")
	}

	// Central becomes unreachable (shorter than the lease, so the control run keeps running).
	starts := e.run.StartCount()
	h := e.c.Handler(e.c.APIDeps())
	e.handler.Store(ptr(http.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusServiceUnavailable) }))))
	e.c.Fleet.Hub().CloseAll("test: central unreachable")
	eventually(t, "node seen disconnected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return !n.Connected })
	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{"spaceman_inbox_add", map[string]any{"title": "while unreachable", "body": "x"}},
		{"spaceman_task_create", map[string]any{"title": "while unreachable"}},
		{"spaceman_worker_launch", map[string]any{"task_id": "tsk_x", "role": "implementor"}},
		{"spaceman_queue_next", map[string]any{}},
	} {
		if res := e.bridgeCall(ctrl.ID, tok, call.tool, call.args); res.OK || res.Code != "unavailable" {
			t.Fatalf("%s without central: %+v", call.tool, res)
		}
	}
	if got := e.run.StartCount(); got != starts {
		t.Fatalf("the node started programs on its own: starts %d → %d", starts, got)
	}

	// Central returns: nothing refused above is replayed by the node.
	e.handler.Store(&h)
	eventually(t, "reconnected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	time.Sleep(600 * time.Millisecond) // heartbeats, reconcile and any re-delivery
	if n := items(); n != 1 {
		t.Fatalf("inbox items after reconnect = %d, want 1", n)
	}
	if q, _, err := e.c.Queue.List(ctx, sp.ID); err != nil || len(q) != 0 {
		t.Fatalf("tasks after reconnect: %d %v", len(q), err)
	}
	if got := e.run.StartCount(); got != starts {
		t.Fatalf("programs started after reconnect: starts %d → %d", starts, got)
	}
	runs, err := e.c.Fleet.ListRuns(ctx, sp.ID, 100)
	if err != nil || len(runs) != 1 || runs[0].ID != ctrl.ID {
		t.Fatalf("runs after reconnect: %+v %v", runs, err)
	}
	if res := e.bridgeCall(ctrl.ID, tok, "spaceman_inbox_add", map[string]any{"title": "after reconnect", "body": "x"}); !res.OK {
		t.Fatalf("inbox add after reconnect: %+v", res)
	}
}
