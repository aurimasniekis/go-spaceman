package agent

import (
	"context"
	"os"
	"path/filepath"
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
		if len(rs) == 0 {
			t.Fatalf("minor %d: no report after the hooks", minor)
		}
		last := rs[len(rs)-1]
		if last.Limit == nil || last.Limit.Kind != proto.LimitUnknown {
			t.Fatalf("minor %d: limit %+v", minor, last.Limit)
		}
		if minor < proto.MinorAccountUsage {
			for _, r := range rs {
				if r.Failure != "" || r.Limit != nil && (r.Limit.Source != "" || r.Limit.ResetsAt != nil) {
					t.Fatalf("minor %d: report carries minor-13 data: %+v limit %+v", minor, r, r.Limit)
				}
			}
			continue
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
