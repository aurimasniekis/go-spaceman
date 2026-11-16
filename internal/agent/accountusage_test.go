package agent

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	rt "github.com/aurimasniekis/go-spaceman/internal/runner/runnertest"
)

func TestStatusLineReset(t *testing.T) {
	five := &harness.Window{UsedPct: 100, ResetsAt: 2_000_000_000}
	seven := &harness.Window{UsedPct: 100, ResetsAt: 2_000_100_000}
	snap := harness.StatusSnapshot{FiveHour: five, SevenDay: seven}
	at := func(w *harness.Window) *time.Time { t := time.Unix(w.ResetsAt, 0).UTC(); return &t }
	for _, tc := range []struct {
		name string
		kind string
		snap harness.StatusSnapshot
		want *time.Time
	}{
		{"five hour kind", proto.LimitFiveHour, snap, at(five)},
		{"seven day kind", proto.LimitSevenDay, snap, at(seven)},
		{"wrap-up takes the later full window", proto.LimitUnknown, snap, at(seven)},
		{"wrap-up with one full window", proto.LimitUnknown, harness.StatusSnapshot{FiveHour: five, SevenDay: &harness.Window{UsedPct: 40, ResetsAt: 2_000_100_000}}, at(five)},
		{"window not full", proto.LimitFiveHour, harness.StatusSnapshot{FiveHour: &harness.Window{UsedPct: 99.9, ResetsAt: 2_000_000_000}}, nil},
		{"window without reset", proto.LimitFiveHour, harness.StatusSnapshot{FiveHour: &harness.Window{UsedPct: 100}}, nil},
		{"model-specific kind has no window", proto.LimitSevenDayOpus, snap, nil},
		{"gateway kind", proto.LimitGatewayQuota, snap, nil},
		{"no snapshot data", proto.LimitUnknown, harness.StatusSnapshot{}, nil},
	} {
		got := statusLineReset(tc.kind, tc.snap)
		if (got == nil) != (tc.want == nil) || got != nil && !got.Equal(*tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// withHookData fills a missing reset only, drops one outside the limit's bounds, and never changes the journal's limit.
func TestWithHookData(t *testing.T) {
	a, _ := ccAgent(t, t.TempDir(), nil, nil)
	now := time.Now().UTC().Truncate(time.Second)
	a.o.Clock = clock.NewFake(now)
	a.statusSnaps = map[string]StatusObservation{"run_1": {ObservedAt: now, StatusSnapshot: harness.StatusSnapshot{FiveHour: &harness.Window{UsedPct: 100, ResetsAt: now.Add(time.Hour).Unix()}}}}
	a.failureClasses = map[string]string{"run_1": "billing_error", "run_2": "free text"}

	lim := &proto.RunLimit{Kind: proto.LimitUnknown, Since: now.Add(-time.Minute)}
	got := a.withHookData(proto.RunReport{RunID: "run_1", Limit: lim})
	if got.Failure != "billing_error" || got.Limit.ResetsAt == nil || got.Limit.Source != proto.LimitSourceStatusLine || !got.Limit.ResetsAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("report %+v limit %+v", got, got.Limit)
	}
	if lim.ResetsAt != nil || lim.Source != "" {
		t.Fatalf("the journal's limit was changed: %+v", lim)
	}
	if got := a.withHookData(proto.RunReport{RunID: "run_2"}); got.Failure != "" {
		t.Fatalf("an unlisted class crossed: %q", got.Failure)
	}
	// A snapshot older than statusMaxAge is not used.
	a.statusSnaps["run_4"] = StatusObservation{ObservedAt: now.Add(-statusMaxAge - time.Second), StatusSnapshot: a.statusSnaps["run_1"].StatusSnapshot}
	if got := a.withHookData(proto.RunReport{RunID: "run_4", Limit: &proto.RunLimit{Kind: proto.LimitUnknown, Since: now.Add(-time.Minute)}}); got.Limit.ResetsAt != nil {
		t.Fatalf("a stale snapshot filled a reset: %+v", got.Limit)
	}
	// A transcript reset, a cleared limit and a reset before since are left alone.
	tr := now.Add(3 * time.Hour)
	if got := a.withHookData(proto.RunReport{RunID: "run_1", Limit: &proto.RunLimit{Kind: proto.LimitUnknown, Since: now, ResetsAt: &tr}}); !got.Limit.ResetsAt.Equal(tr) || got.Limit.Source != "" {
		t.Fatalf("transcript reset replaced: %+v", got.Limit)
	}
	if got := a.withHookData(proto.RunReport{RunID: "run_1", Limit: &proto.RunLimit{Cleared: true, Since: now}}); got.Limit.ResetsAt != nil {
		t.Fatalf("cleared limit got a reset: %+v", got.Limit)
	}
	a.statusSnaps["run_3"] = StatusObservation{ObservedAt: now, StatusSnapshot: harness.StatusSnapshot{FiveHour: &harness.Window{UsedPct: 100, ResetsAt: now.Add(-2 * time.Hour).Unix()}}}
	if got := a.withHookData(proto.RunReport{RunID: "run_3", Limit: &proto.RunLimit{Kind: proto.LimitUnknown, Since: now}}); got.Limit.ResetsAt != nil || got.Limit.Source != "" {
		t.Fatalf("a reset before since was kept: %+v", got.Limit)
	}
}

// A wrap-up limit's reset time arrives from the status line as a limit report with source statusline (minor 13), and a
// minor-12 central gets neither the source nor the failure class: its reports are what they were.
func TestStatusLineFillsWrapUpLimitOnTheWire(t *testing.T) {
	old := limitSettle
	limitSettle = 10 * time.Millisecond
	t.Cleanup(func() { limitSettle = old })
	for _, minor := range []int{proto.MinorAccountUsage, proto.MinorUsageLimit} {
		dir := t.TempDir()
		a, f := ccAgent(t, dir, nil, nil)
		fc := newWSCentral(t, minor)
		clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
		startAgent(t, a, fc, clk)
		fc.next(t, proto.TypeHello)
		ccLaunch(a, "run_wrap")
		a.rememberRunToken("run_wrap", "tok")
		t.Setenv("SPACEMAN_RUN_TOKEN", "tok")
		waitFor(t, "bridge socket", func() bool { _, err := os.Stat(a.o.BridgeSocket); return err == nil })
		row := getRun(t, a, "run_wrap")
		transcript := filepath.Join(dir, "spaces", "cc", "instances", "ins_cc", "claude", "projects", "-work", row.SessionID+".jsonl")
		if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
			t.Fatal(err)
		}
		fc.collect(t, "run_wrap", 300*time.Millisecond)
		at := clk.Now().Add(-time.Minute)
		f.SetStatus(row.TerminalID, "working")
		if err := rt.AppendTranscript(transcript, rt.TypedRecord(at), rt.WrapUpRecord(at), rt.AssistantRecord("grace", at), rt.NoticeRecord(at)); err != nil {
			t.Fatal(err)
		}
		f.SetStatus(row.TerminalID, "idle")
		fc.collect(t, "run_wrap", 300*time.Millisecond)

		reset := clk.Now().Add(2 * time.Hour)
		snap := harness.StatusSnapshot{FiveHour: &harness.Window{UsedPct: 100, ResetsAt: reset.Unix()}}
		ctx := context.Background()
		if err := sendHook(ctx, a.o.BridgeSocket, "run_wrap", ToolStatusLine, snap); err != nil {
			t.Fatal(err)
		}
		if err := sendHook(ctx, a.o.BridgeSocket, "run_wrap", ToolHookStopFailure, harness.StopFailure{Error: "billing_error"}); err != nil {
			t.Fatal(err)
		}
		rs := fc.collect(t, "run_wrap", 500*time.Millisecond)
		if minor < proto.MinorAccountUsage {
			// Today's behaviour: the hooks cause no report at all, and nothing of minor 13 is on any report.
			if len(rs) != 0 {
				t.Fatalf("minor %d: %d reports after the hooks", minor, len(rs))
			}
			continue
		}
		if len(rs) == 0 {
			t.Fatalf("minor %d: no report after the hooks", minor)
		}
		last := rs[len(rs)-1]
		if last.Limit == nil || last.Limit.Kind != proto.LimitUnknown {
			t.Fatalf("minor %d: limit %+v", minor, last.Limit)
		}
		if last.Failure != "billing_error" || last.Limit.Source != proto.LimitSourceStatusLine || last.Limit.ResetsAt == nil || !last.Limit.ResetsAt.Equal(reset) {
			t.Fatalf("report %+v limit %+v", last, last.Limit)
		}
		// The journal still holds the transcript's view: the fill is on the wire only.
		if j := getRun(t, a, "run_wrap").Limit; j.ResetsAt != nil {
			t.Fatalf("journal limit %+v", j)
		}
	}
}

// Every StopFailure class the hook accepts, but rate_limit (the transcript scan's), is on central's allowlist, so a class
// the node keeps is never dropped on the way.
func TestFailureClassesMatchTheHook(t *testing.T) {
	for _, e := range harness.StopFailureErrors {
		if (e != "rate_limit") != (proto.FailureClass(e) == e) {
			t.Errorf("%q: allowlist disagrees with the hook", e)
		}
	}
	if len(proto.FailureClasses) != len(harness.StopFailureErrors)-1 {
		t.Errorf("%d classes on the wire, %d accepted by the hook", len(proto.FailureClasses), len(harness.StopFailureErrors))
	}
}

func TestFillChanged(t *testing.T) {
	a, _ := ccAgent(t, t.TempDir(), nil, nil)
	now := time.Now().UTC().Truncate(time.Second)
	a.o.Clock = clock.NewFake(now)
	l := &proto.RunLimit{Kind: proto.LimitUnknown, Since: now.Add(-time.Minute)}
	snap := func(d time.Duration) harness.StatusSnapshot {
		return harness.StatusSnapshot{FiveHour: &harness.Window{UsedPct: 100, ResetsAt: now.Add(d).Unix()}}
	}
	if !a.fillChanged(l, harness.StatusSnapshot{}, snap(time.Hour)) {
		t.Error("a first fill is not a change")
	}
	if a.fillChanged(l, snap(time.Hour), snap(time.Hour)) {
		t.Error("an unchanged fill on the next tick is a change")
	}
	if !a.fillChanged(l, snap(time.Hour), snap(2*time.Hour)) {
		t.Error("a moved fill is not a change")
	}
	if a.fillChanged(l, harness.StatusSnapshot{}, snap(-time.Hour)) || a.fillChanged(l, snap(time.Hour), harness.StatusSnapshot{}) {
		t.Error("a fill outside the limit's bounds, or none, is a change")
	}
}

// A failure ends when the run starts a new turn: the node tells central once the class is gone, and a new failure
// replaces the clear.
func TestFailureEndedOnNewTurn(t *testing.T) {
	a, _ := ccAgent(t, t.TempDir(), nil, nil)
	if got := a.withHookData(proto.RunReport{RunID: "run_1"}); got.FailureCleared || got.Failure != "" {
		t.Fatalf("a run without a failure reports a clear: %+v", got)
	}
	a.failureClasses = map[string]string{"run_1": "overloaded"}
	if got := a.withHookData(proto.RunReport{RunID: "run_1"}); got.Failure != "overloaded" || got.FailureCleared {
		t.Fatalf("held failure: %+v", got)
	}
	a.failureEnded("run_1", true)
	for range 2 { // repeated on every report
		if got := a.withHookData(proto.RunReport{RunID: "run_1"}); got.Failure != "" || !got.FailureCleared {
			t.Fatalf("cleared failure: %+v", got)
		}
	}
	a.failureClasses["run_1"] = "server_error"
	if got := a.withHookData(proto.RunReport{RunID: "run_1"}); got.Failure != "server_error" || got.FailureCleared {
		t.Fatalf("a new failure: %+v", got)
	}
}

// After a node restart nothing is held, so the first working report of a run whose status was already working clears
// what central stored; a class that arrives late, while the status is already working, is kept.
func TestFailureEndedAfterRestart(t *testing.T) {
	a, _ := ccAgent(t, t.TempDir(), nil, nil)
	a.failureEnded("run_1", false) // working before and after the restart, nothing held
	for range 2 {
		if got := a.withHookData(proto.RunReport{RunID: "run_1"}); got.Failure != "" || !got.FailureCleared {
			t.Fatalf("no clear after a restart: %+v", got)
		}
	}
	a.failureClasses["run_2"] = "overloaded" // a late StopFailure while already working
	a.failureEnded("run_2", false)
	if got := a.withHookData(proto.RunReport{RunID: "run_2"}); got.Failure != "overloaded" || got.FailureCleared {
		t.Fatalf("a late class was cleared: %+v", got)
	}
}

// On the wire at minor 13, through observe: a node restarted while the run's status was already working holds no class,
// so the next working observe (no transition) sends a clear; a class stored late, while already working, survives the
// next working observe and no clear is sent.
func TestFailureClearedAfterRestartOnTheWire(t *testing.T) {
	a, fc, runID, _, f := startHookAgentMinor(t, proto.MinorAccountUsage, nil)
	ctx := context.Background()
	row := getRun(t, a, runID)
	f.SetStatus(row.TerminalID, "working")
	fc.collect(t, runID, 500*time.Millisecond)
	observe := func() []proto.RunReport {
		t.Helper()
		p, err := f.Get(ctx, row.TerminalID)
		if err != nil {
			t.Fatal(err)
		}
		cur := getRun(t, a, runID)
		if cur.Status != "working" {
			t.Fatalf("journal status %q, want working before the observe", cur.Status)
		}
		a.observe(ctx, cur, *p, false)
		return fc.collect(t, runID, 500*time.Millisecond)
	}

	a.mu.Lock()
	a.failureClasses = nil // the restart: nothing held, status working in the journal
	a.mu.Unlock()
	cleared := false
	for _, r := range observe() {
		cleared = cleared || r.FailureCleared && r.Failure == ""
	}
	if !cleared {
		t.Fatal("a working observe after a restart did not clear the stored failure")
	}

	a.mu.Lock()
	a.failureClasses = map[string]string{runID: "overloaded"} // a late StopFailure, status still working
	a.mu.Unlock()
	for _, r := range observe() {
		if r.FailureCleared {
			t.Fatalf("a late class was cleared by a working observe: %+v", r)
		}
	}
	a.mu.Lock()
	got := a.failureClasses[runID]
	a.mu.Unlock()
	if got != "overloaded" {
		t.Fatalf("late class = %q, want overloaded", got)
	}
}

// On the wire at minor 13: a failure is reported once (a repeat of the same class sends nothing), a new turn clears it,
// and a hello carries none of it (the minor isn't known yet; a central below 13 would misread it) but the data follows
// the welcome.
func TestFailureOnTheWire(t *testing.T) {
	var drops atomic.Int32
	a, fc, runID, socket, f := startHookAgentMinor(t, proto.MinorAccountUsage, func(env proto.Envelope) bool {
		var r proto.RunReport
		return env.Type == proto.TypeRunReport && env.Decode(&r) == nil && r.Failure == "billing_error" && drops.Add(1) == 1
	})
	ctx := context.Background()
	row := getRun(t, a, runID)
	fc.collect(t, runID, 300*time.Millisecond)
	send := func(class string) {
		t.Helper()
		if err := sendHook(ctx, socket, runID, ToolHookStopFailure, harness.StopFailure{Error: class}); err != nil {
			t.Fatal(err)
		}
	}
	// The first report with the class is lost with its connection; the reconnect's hello carries no hook data, and the
	// class follows the welcome.
	send("billing_error")
	waitFor(t, "connection dropped", func() bool { return drops.Load() >= 1 })
	var hello proto.RunReport
	deadline := time.After(15 * time.Second)
	for hello.RunID == "" {
		select {
		case env := <-fc.msgs:
			var h proto.Hello
			if env.Type == proto.TypeHello && env.Decode(&h) == nil {
				for _, r := range h.Runs {
					if r.RunID == runID {
						hello = r
					}
				}
			}
		case <-deadline:
			t.Fatal("no reconnect")
		}
	}
	if hello.Failure != "" || hello.FailureCleared {
		t.Fatalf("hello carries hook data: %+v", hello)
	}
	rs := fc.collect(t, runID, time.Second)
	if n := countFailure(rs, "billing_error"); n < 1 {
		t.Fatalf("the class did not follow the welcome: %+v", rs)
	}
	// The same class again sends no report.
	send("billing_error")
	if rs := fc.collect(t, runID, 500*time.Millisecond); countFailure(rs, "billing_error") != 0 {
		t.Fatalf("a repeated class was reported again: %+v", rs)
	}
	// A new turn clears it.
	f.SetStatus(row.TerminalID, "idle")
	fc.collect(t, runID, 300*time.Millisecond)
	f.SetStatus(row.TerminalID, "working")
	cleared := false
	for _, r := range fc.collect(t, runID, 500*time.Millisecond) {
		cleared = cleared || r.FailureCleared && r.Failure == ""
	}
	if !cleared {
		t.Fatal("a new turn did not clear the failure")
	}
}

func countFailure(rs []proto.RunReport, class string) int {
	n := 0
	for _, r := range rs {
		if r.Failure == class {
			n++
		}
	}
	return n
}

// A usage report carries the status line's windows (minor ≥ 13, snapshot not older than statusMaxAge) as typed numbers.
func TestUsageWindows(t *testing.T) {
	a, _ := ccAgent(t, t.TempDir(), nil, nil)
	now := time.Now().UTC().Truncate(time.Second)
	a.o.Clock = clock.NewFake(now)
	reset := now.Add(time.Hour)
	a.statusSnaps = map[string]StatusObservation{
		"run_1": {ObservedAt: now.Add(-time.Minute), StatusSnapshot: harness.StatusSnapshot{FiveHour: &harness.Window{UsedPct: 42.5, ResetsAt: reset.Unix()}, SevenDay: &harness.Window{UsedPct: 7}}},
		"run_2": {ObservedAt: now.Add(-statusMaxAge - time.Second), StatusSnapshot: harness.StatusSnapshot{FiveHour: &harness.Window{UsedPct: 1}}},
	}
	a.mu.Lock()
	a.connected, a.minor = true, proto.MinorAccountUsage
	a.mu.Unlock()
	got := a.usageWindows("run_1")
	if len(got) != 2 || got[0].Kind != proto.WindowFiveHour || got[0].UsedPct != 42.5 || got[0].ResetsAt == nil || !got[0].ResetsAt.Equal(reset) ||
		got[1].Kind != proto.WindowSevenDay || got[1].ResetsAt != nil || !got[1].ObservedAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("%+v", got)
	}
	if a.usageWindows("run_2") != nil || a.usageWindows("run_3") != nil {
		t.Fatal("a stale or missing snapshot gave windows")
	}
	a.mu.Lock()
	a.minor = proto.MinorAccountUsage - 1
	a.mu.Unlock()
	if a.usageWindows("run_1") != nil {
		t.Fatal("windows below minor 13")
	}
}
