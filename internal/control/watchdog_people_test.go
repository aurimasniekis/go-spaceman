package control

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/limits"
)

func (e *peopleEnv) watchRaise(restart int64, usedRestarts int) (*fleet.Run, string) {
	e.t.Helper()
	e.sv.Limit = func(_ context.Context, _, key string) int64 {
		if key == limits.ControlWatchdogRestart {
			return restart
		}
		return 0
	}
	e.exec(`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, status, launch_json,
		created_at, created_by, updated_at) VALUES ('run_ctl', 'spc_a', 'nod_1', 1, 'control', 'control', 'claude-code', 'ins_c', 'running', 'running', 'idle', '{}', 0, 't', 0)`)
	for i := range usedRestarts {
		e.exec(`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json,
			created_at, created_by, updated_at) VALUES (?, 'spc_a', 'nod_1', 1, 'control', 'control', 'claude-code', 'ins_c', 'stopped', 'exited', '{}', ?, 'system:control-supervisor', 0)`,
			"run_old"+string(rune('a'+i)), e.clk.Now().Add(-time.Minute).UnixMicro())
	}
	r, err := e.sv.fl.GetRun(e.ctx, "run_ctl")
	if err != nil {
		e.t.Fatal(err)
	}
	w := NewWatchdog(e.sv)
	w.raise(e.ctx, "spc_a", r, &episode{runID: r.ID, nudges: 3, first: e.clk.Now()}, Backlog{Messages: 1}, 20*time.Minute)
	l, err := e.human.List(e.ctx, "spc_a", human.ListFilter{})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, a := range l {
		if strings.HasPrefix(a.DedupKey, "control-watchdog:") {
			return e.run("run_ctl"), a.Body
		}
	}
	e.t.Fatal("no watchdog action raised")
	return nil, ""
}

func (e *peopleEnv) run(id string) *fleet.Run {
	e.t.Helper()
	r, err := e.sv.fl.GetRun(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// With the restart switch on but the supervisor's restart budget already used, the watchdog raises its action, says
// why it did not restart, and does not stop the run; with budget left it stops the run.
func TestWatchdogRaiseRestartBudget(t *testing.T) {
	e := newPeopleEnv(t)
	e.perSpace.Store(100)
	r, body := e.watchRaise(1, int(e.sv.MaxRestarts))
	if r.Desired != "running" || !strings.Contains(body, "already restarted") || !strings.Contains(body, "did not restart it") {
		t.Fatalf("budget used: desired %q, body %q", r.Desired, body)
	}

	e = newPeopleEnv(t)
	e.perSpace.Store(100)
	r, body = e.watchRaise(1, 0)
	if r.Desired != "stopped" || !strings.Contains(body, "The watchdog is restarting control") {
		t.Fatalf("budget left: desired %q, body %q", r.Desired, body)
	}
}

// restartStalled relaunches control, but not when the space was paused between the stop and the run ending.
func TestWatchdogRestartStalledPaused(t *testing.T) {
	for _, state := range []string{"active", "paused"} {
		e := newPoolLaunchEnv(t)
		e.exec(`UPDATE runs SET desired = 'stopped' WHERE id = 'run_ctl'`)
		e.exec(`UPDATE spaces SET state = ? WHERE id = 'spc_a'`, state)
		var logs bytes.Buffer
		e.sv.log = slog.New(slog.NewTextHandler(&logs, nil))
		r := e.run("run_ctl")
		e.sv.restartStalled(e.ctx, r)
		var n int
		if err := e.s.ReadDB().QueryRowContext(e.ctx, `SELECT count(*) FROM runs WHERE id != 'run_ctl'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if want := map[string]int{"active": 1, "paused": 0}[state]; n != want {
			t.Fatalf("%s space: %d relaunched runs, want %d", state, n, want)
		}
		// The launch would be refused in a paused space anyway; the check is that it is not even tried.
		if tried := strings.Contains(logs.String(), "starting stalled control again"); tried != (state == "active") {
			t.Fatalf("%s space: relaunch tried %v", state, tried)
		}
	}
}

// Runs that ended before this control started do not count, even when the activity cutoff is earlier than the start
// (assess folds the start into it, so this holds backlog to its own check).
func TestWatchdogBacklogIgnoresRunsBeforeControl(t *testing.T) {
	o := stalled()
	o.Unacked = nil
	o.Runs = []*fleet.Run{
		run("run_before", "worker", "running", "failed", "", tp(ago(4*time.Hour))), // control started 3 hours ago
		run("run_after", "worker", "running", "failed", "", tp(ago(2*time.Hour))),
	}
	b, _ := backlog(o, time.Time{}, wdNow.Add(-wdN))
	if b.Failed != 1 {
		t.Fatalf("backlog %+v, want only the run that ended after control started", b)
	}
}

// Kick names a space; the watchdog looks at that space only.
func TestWatchdogKickChecksOnlyItsSpace(t *testing.T) {
	e := newWDEnv(t)
	var seen []string
	e.w.observe = func(_ context.Context, id string) (observation, error) {
		e.w.mu.Lock()
		seen = append(seen, id)
		e.w.mu.Unlock()
		return observation{SpaceState: "paused"}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.w.Run(ctx); close(done) }()
	e.w.Kick("spc_b")
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.w.mu.Lock()
		n, k := len(seen), len(e.w.kicked)
		e.w.mu.Unlock()
		if n > 0 && k == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	e.w.mu.Lock()
	defer e.w.mu.Unlock()
	if len(seen) != 1 || seen[0] != "spc_b" {
		t.Fatalf("checked %v, want only spc_b", seen)
	}
}
