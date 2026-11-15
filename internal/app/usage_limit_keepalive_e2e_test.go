package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/accounts"
	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/limits"
	"github.com/aurimasniekis/go-spaceman/internal/messages"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// launchGen launches a run and returns it with the lease generation of its launch command (a scripted node reads it).
func launchGen(t *testing.T, e *fleetEnv, n *rawNode, spaceID string, in fleet.LaunchInput) (*fleet.Run, int64) {
	t.Helper()
	r, err := e.c.Fleet.LaunchRun(context.Background(), admin, spaceID, in)
	if err != nil {
		t.Fatal(err)
	}
	for {
		var c proto.Command
		if err := n.next(t, proto.TypeCommand).Decode(&c); err != nil {
			t.Fatal(err)
		}
		if c.RunID == r.ID && c.Kind == proto.CmdLaunch {
			return r, c.Generation
		}
	}
}

// systemToControl counts SpaceMan's notifications to control whose subject contains s.
func systemToControl(t *testing.T, e *fleetEnv, spaceID, s string) int {
	t.Helper()
	l, err := e.c.Messages.List(context.Background(), spaceID, 200)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range l {
		if m.ToKind == "control" && m.FromKind == messages.SystemFrom.Kind && strings.Contains(m.Subject, s) {
			n++
		}
	}
	return n
}

// bindAnotherClaude binds a second, non-default Claude account to the space and returns its ID.
func bindAnotherClaude(t *testing.T, e *fleetEnv, spaceID string) string {
	t.Helper()
	ctx := context.Background()
	acc, err := e.c.Accounts.Create(ctx, admin, accounts.Input{Name: "second-claude-" + spaceID[len(spaceID)-6:], Kind: "claude_oauth_token",
		Secret: "stub-claude-token-2", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	bs, err := e.c.Accounts.Bindings(ctx, spaceID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Accounts.SetBindings(ctx, admin, spaceID, append(bs, accounts.Binding{AccountID: acc.ID})); err != nil {
		t.Fatal(err)
	}
	return acc.ID
}

func inputCommands(t *testing.T, e *fleetEnv, runID string) int {
	t.Helper()
	var n int
	if err := e.c.Store.ReadDB().QueryRowContext(context.Background(), `SELECT count(*) FROM node_commands WHERE run_id = ? AND kind = ?`,
		runID, proto.CmdInput).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ADR-0079 decisions 3 and 11, UL-5 (simulated: a scripted minor-12 node): a worker kept alive at its usage limit frees
// its slot in the space's run limit, so another worker launches; it gets no notices while it waits (its message is
// held and noticed after the clear); control gets exactly one "at its usage limit" message however often the node
// repeats or reclassifies the limit, and exactly one "cleared" message.
func TestUsageLimitKeepAlive(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	ctx := context.Background()
	n, sp := hvNode(t, e, "ulka", proto.MinorUsageLimit, true)
	if _, err := e.c.Limits.Set(ctx, admin, sp.ID, 0, limits.Input{Values: map[string]int64{limits.ConcurrentRuns: 1}}); err != nil {
		t.Fatal(err)
	}
	worker := fleet.LaunchInput{Role: "free", Kind: "worker", Harness: "claude-code", Prompt: "go"}
	w1, gen := launchGen(t, e, n, sp.ID, worker)
	report := func(status string, l *proto.RunLimit) {
		n.send(t, proto.TypeRunReport, proto.RunReport{RunID: w1.ID, SpaceID: sp.ID, Generation: gen, State: proto.RunRunning, Status: status,
			Limit: l, ObservedAt: time.Now().UTC()})
	}
	report("working", nil)
	eventually(t, "running", func() bool { return e.run1(w1.ID).Observed == proto.RunRunning })
	if _, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, worker); apperr.CodeOf(err) != apperr.CodeRateLimited {
		t.Fatalf("a second worker at the space limit: %v", err)
	}

	// The limit arrives with the idle report (the wrap-up path: no kind yet, no reset), then its typed record.
	since := time.Now().UTC().Truncate(time.Second)
	report("idle", &proto.RunLimit{Kind: proto.LimitUnknown, Since: since})
	eventually(t, "limit stored", func() bool { return e.run1(w1.ID).Limit != nil })
	reset := since.Add(2 * time.Hour)
	limited := &proto.RunLimit{Kind: proto.LimitFiveHour, ResetsAt: &reset, Since: since}
	for range 3 {
		report("idle", limited)
	}
	eventually(t, "reclassified", func() bool { return e.run1(w1.ID).Limit.Kind == proto.LimitFiveHour })

	// The slot is free: a second worker launches while the first waits (on another account: the first's is held).
	other := worker
	other.AccountRef = bindAnotherClaude(t, e, sp.ID)
	w2, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, other)
	if err != nil {
		t.Fatalf("a limit-waiting worker held the space slot: %v", err)
	}

	// No nudges: a message to the waiting worker is held, whatever the sweeps.
	if _, _, err := e.c.Messages.Send(ctx, admin, sp.ID, messages.From{Kind: "operator", ID: "usr_admin"},
		messages.SendInput{To: "run:" + w1.ID, Subject: "Next step"}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := e.c.Notifier.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if c := inputCommands(t, e, w1.ID); c != 0 {
		t.Fatalf("%d notices queued for a limit-waiting worker", c)
	}
	if c := systemToControl(t, e, sp.ID, "at its usage limit"); c != 1 {
		t.Fatalf("control got %d limit messages, want 1", c)
	}
	// The message was written when the limit opened (the wrap-up record): the reclassification is the same window.
	l, _ := e.c.Messages.List(ctx, sp.ID, 200)
	for _, m := range l {
		if strings.Contains(m.Subject, "at its usage limit") && (!strings.Contains(m.Body, w1.ID) || !strings.Contains(m.Body, "unknown usage limit")) {
			t.Fatalf("limit message %q", m.Body)
		}
	}

	// The limit clears while the worker is idle: control is told once, and the held message is noticed now.
	cleared := &proto.RunLimit{Cleared: true, Since: since}
	for range 3 {
		report("idle", cleared)
	}
	eventually(t, "limit cleared", func() bool { return e.run1(w1.ID).Limit == nil })
	if err := e.c.Notifier.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if c := inputCommands(t, e, w1.ID); c != 1 {
		t.Fatalf("notices after the clear: %d, want 1", c)
	}
	eventually(t, "cleared message", func() bool { return systemToControl(t, e, sp.ID, "usage limit cleared") == 1 })
	time.Sleep(200 * time.Millisecond)
	if a, c := systemToControl(t, e, sp.ID, "at its usage limit"), systemToControl(t, e, sp.ID, "usage limit cleared"); a != 1 || c != 1 {
		t.Fatalf("control messages: %d limited, %d cleared (want 1 and 1)", a, c)
	}
	// Counted again, the worker fills the space's limit with the second one.
	if _, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, worker); apperr.CodeOf(err) != apperr.CodeRateLimited || !strings.Contains(err.Error(), w2.ID) {
		t.Fatalf("after the clear: %v", err)
	}
}

// ADR-0079 decision 3 (simulated: a scripted minor-12 node): a control run at its usage limit is waiting, not idle, so
// its idle stop isn't armed and it isn't told about its own limit; once the limit clears while it is idle, the idle
// stop is armed again and stops it.
func TestUsageLimitControlNotIdleStopped(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t, func(c *config.Config) { c.Limits.ControlIdleStopMinutes = 1 })
	e.enroll()
	e.c.Supervisor.IdleUnit = 300 * time.Millisecond // "1 minute" of idle time
	n, sp := hvNode(t, e, "ulctl", proto.MinorUsageLimit, true)
	ctrl, gen := launchGen(t, e, n, sp.ID, fleet.LaunchInput{Role: "control", Kind: "control", Harness: "claude-code", Prompt: "go"})
	report := func(status string, l *proto.RunLimit) {
		n.send(t, proto.TypeRunReport, proto.RunReport{RunID: ctrl.ID, SpaceID: sp.ID, Generation: gen, State: proto.RunRunning, Status: status,
			Limit: l, ObservedAt: time.Now().UTC()})
	}
	report("working", nil)
	eventually(t, "running", func() bool { return e.run1(ctrl.ID).Observed == proto.RunRunning })
	since := time.Now().UTC().Truncate(time.Second)
	report("idle", &proto.RunLimit{Kind: proto.LimitFiveHour, Since: since})
	eventually(t, "limit stored", func() bool { return e.run1(ctrl.ID).Limit != nil })
	time.Sleep(1500 * time.Millisecond) // five idle times
	if r := e.run1(ctrl.ID); r.Desired != "running" {
		t.Fatalf("a limit-waiting control was idle-stopped: %+v", r)
	}
	if c := systemToControl(t, e, sp.ID, "usage limit"); c != 0 {
		t.Fatalf("control told about its own limit: %d messages", c)
	}
	report("idle", &proto.RunLimit{Cleared: true, Since: since})
	eventually(t, "idle control stopped after the clear", func() bool { return e.run1(ctrl.ID).Desired == "stopped" })
}
