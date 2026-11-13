package fleet

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// ADR-0079 decision 3: idle-gated input to a run waiting for its usage limit to reset is refused (QueueNotice) or not
// sent (NoticeWorkerTx, the incident preemption notice); nothing reaches the node. After the clear it goes again.
func TestLimitWaitingRunGetsNoNotices(t *testing.T) {
	e := ulEnv(t)
	e.runOn("run_a", "spc_a", "acc_1", "")
	reset := e.t0.Add(time.Hour)
	since := e.t0.Add(-time.Minute)
	e.report("run_a", "spc_a", 1, proto.RunRunning, &proto.RunLimit{Kind: proto.LimitFiveHour, ResetsAt: &reset, Since: since})

	notice := func() (queued error, worker bool) {
		t.Helper()
		if err := e.s.W().TxNamed(e.ctx, "notice", func(tx *store.Tx) error {
			_, queued = e.svc.QueueNotice(e.ctx, tx, "run_a", "SpaceMan: 1 new message")
			var err error
			worker, err = e.svc.NoticeWorkerTx(e.ctx, tx, "spc_a", "run_a", "SpaceMan: incident")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return queued, worker
	}
	queued, worker := notice()
	if !errors.Is(queued, ErrLimitWaiting) || worker {
		t.Fatalf("limit-waiting run: QueueNotice %v, NoticeWorkerTx %v", queued, worker)
	}
	if n := e.count(`SELECT count(*) FROM node_commands WHERE run_id = 'run_a' AND kind = ?`, proto.CmdInput); n != 0 {
		t.Fatalf("%d input commands queued for a limit-waiting run", n)
	}
	e.report("run_a", "spc_a", 1, proto.RunRunning, &proto.RunLimit{Cleared: true, Since: since})
	if queued, worker := notice(); queued != nil || !worker {
		t.Fatalf("after the clear: QueueNotice %v, NoticeWorkerTx %v", queued, worker)
	}
}

type limitCall struct{ observed, ended, opened string }

// ADR-0079 decision 11: OnRunLimitTx is called once per limit window change, inside the report's transaction: a limit
// opening, a newer window replacing it (both in one call), and its clear. Repeats, a reset filled in and a
// reclassification (the same since) are no change. A limit that clears while the program is idle counts as turning
// idle, so held notices go out and control's idle stop is armed again; a hook error rolls the whole report back.
func TestRunLimitWindowHook(t *testing.T) {
	e := ulEnv(t)
	var mu sync.Mutex
	var calls []limitCall
	var idle []string
	kind := func(l *RunLimit) string {
		if l == nil {
			return ""
		}
		return l.Kind + "@" + l.Since.Format(time.TimeOnly)
	}
	e.svc.opts.OnRunLimitTx = func(_ context.Context, _ *store.Tx, r *Run, ended, opened *RunLimit) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, limitCall{r.Observed, kind(ended), kind(opened)})
		return nil
	}
	e.svc.opts.OnRunIdle = func(id string) { mu.Lock(); idle = append(idle, id); mu.Unlock() }
	take := func() ([]limitCall, int) {
		mu.Lock()
		defer mu.Unlock()
		c, n := calls, len(idle)
		calls, idle = nil, nil
		return c, n
	}
	e.runOn("run_a", "spc_a", "acc_1", "")
	e.report("run_a", "spc_a", 1, proto.RunRunning, nil)
	if c, _ := take(); len(c) != 0 {
		t.Fatalf("no limit: %+v", c)
	}
	s1 := e.t0.Add(-2 * time.Minute)
	e.report("run_a", "spc_a", 1, proto.RunRunning, &proto.RunLimit{Kind: proto.LimitUnknown, Since: s1})
	if c, n := take(); len(c) != 1 || c[0] != (limitCall{proto.RunRunning, "", "unknown@17:31:00"}) || n != 0 {
		t.Fatalf("opened: %+v, idle %d", c, n)
	}
	reset := e.t0.Add(time.Hour)
	for range 2 {
		e.report("run_a", "spc_a", 1, proto.RunRunning, &proto.RunLimit{Kind: proto.LimitFiveHour, ResetsAt: &reset, Since: s1})
	}
	if c, _ := take(); len(c) != 0 {
		t.Fatalf("reclassified and repeated: %+v", c)
	}
	s2 := e.t0.Add(-time.Minute)
	e.report("run_a", "spc_a", 1, proto.RunRunning, &proto.RunLimit{Kind: proto.LimitSevenDay, Since: s2})
	if c, _ := take(); len(c) != 1 || c[0] != (limitCall{proto.RunRunning, "five_hour@17:31:00", "seven_day@17:32:00"}) {
		t.Fatalf("new window: %+v", c)
	}
	for range 2 {
		e.report("run_a", "spc_a", 1, proto.RunRunning, &proto.RunLimit{Cleared: true, Since: s2})
	}
	if c, n := take(); len(c) != 1 || c[0] != (limitCall{proto.RunRunning, "seven_day@17:32:00", ""}) || n != 1 {
		t.Fatalf("cleared: %+v, idle %d", c, n)
	}

	// A failing hook fails the report: nothing of it is stored.
	e.svc.opts.OnRunLimitTx = func(context.Context, *store.Tx, *Run, *RunLimit, *RunLimit) error { return errors.New("boom") }
	if _, err := e.svc.applyReport(e.ctx, "nod_1", proto.RunReport{RunID: "run_a", SpaceID: "spc_a", Generation: 1, State: proto.RunRunning,
		Status: "idle", ObservedAt: e.clk.Now(), Limit: &proto.RunLimit{Kind: proto.LimitFiveHour, Since: e.t0}}); err == nil {
		t.Fatal("want the hook's error")
	}
	if n := e.count(`SELECT count(*) FROM runs WHERE id = 'run_a' AND limit_kind <> ''`); n != 0 {
		t.Fatal("a failed hook's report was stored")
	}
}
