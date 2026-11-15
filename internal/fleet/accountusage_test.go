package fleet

import (
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

func (e *staleEnv) reportFull(rep proto.RunReport) {
	e.t.Helper()
	rep.Status, rep.ObservedAt = "idle", e.clk.Now()
	if _, err := e.svc.applyReport(e.ctx, "nod_1", rep); err != nil {
		e.t.Fatal(err)
	}
}

// Minor 13: a wrap-up limit (no kind, no reset) gets the reset the node read from the status line, clamped like any
// other; the hold's reset follows, its probe is dropped, and a reset the transcript gave is never replaced.
func TestStatusLineFillsWrapUpReset(t *testing.T) {
	e := ulEnv(t)
	e.runOn("run_a", "spc_a", "acc_1", "")
	e.report("run_a", "spc_a", 1, proto.RunRunning, nil)
	since := e.t0.Add(-time.Minute)
	rep := func(l *proto.RunLimit) proto.RunReport {
		return proto.RunReport{RunID: "run_a", SpaceID: "spc_a", Generation: 1, State: proto.RunRunning, Limit: l}
	}

	e.reportFull(rep(&proto.RunLimit{Kind: proto.LimitUnknown, Since: since}))
	run, _ := e.svc.GetRun(e.ctx, "run_a")
	if run.Limit == nil || run.Limit.ResetsAt != nil {
		t.Fatalf("wrap-up limit %+v", run.Limit)
	}
	if n := e.count(`SELECT count(*) FROM usage_limits WHERE resets_at IS NULL AND check_at IS NOT NULL`); n != 1 {
		t.Fatalf("%d probing holds", n)
	}

	// A fill past the clamp is dropped with its source: the limit stays unknown.
	far := e.t0.Add(proto.MaxLimitReset + time.Hour)
	e.reportFull(rep(&proto.RunLimit{Kind: proto.LimitUnknown, Since: since, ResetsAt: &far, Source: proto.LimitSourceStatusLine}))
	if run, _ = e.svc.GetRun(e.ctx, "run_a"); run.Limit.ResetsAt != nil {
		t.Fatalf("an out-of-bounds fill was stored: %+v", run.Limit)
	}

	reset := e.t0.Add(2 * time.Hour).UTC()
	e.reportFull(rep(&proto.RunLimit{Kind: proto.LimitUnknown, Since: since, ResetsAt: &reset, Source: proto.LimitSourceStatusLine}))
	run, _ = e.svc.GetRun(e.ctx, "run_a")
	if run.Limit == nil || run.Limit.ResetsAt == nil || !run.Limit.ResetsAt.Equal(reset) {
		t.Fatalf("filled limit %+v", run.Limit)
	}
	holds, err := e.svc.UsageLimits(e.ctx, "spc_a")
	if err != nil || len(holds) != 1 || holds[0].ResetsAt == nil || !holds[0].ResetsAt.Equal(reset) || holds[0].CheckAt != nil {
		t.Fatalf("hold %+v %v", holds, err)
	}
	if n := e.count(`SELECT count(*) FROM usage_limits WHERE source = 'transcript'`); n != 1 {
		t.Fatalf("hold source changed: %d transcript holds", n)
	}
	v := e.runVersion("run_a")
	// A repeat writes nothing, and a report that lost the fill (the snapshot gone) keeps the stored reset.
	e.reportFull(rep(&proto.RunLimit{Kind: proto.LimitUnknown, Since: since, ResetsAt: &reset, Source: proto.LimitSourceStatusLine}))
	e.reportFull(rep(&proto.RunLimit{Kind: proto.LimitUnknown, Since: since}))
	if got := e.runVersion("run_a"); got != v {
		t.Fatalf("a repeat moved the run version %d → %d", v, got)
	}
	if run, _ = e.svc.GetRun(e.ctx, "run_a"); run.Limit.ResetsAt == nil || !run.Limit.ResetsAt.Equal(reset) {
		t.Fatalf("stored reset lost: %+v", run.Limit)
	}

	// A reset the transcript gave stays when a status-line one arrives for the same limit.
	e.runOn("run_b", "spc_b", "acc_1", "")
	e.report("run_b", "spc_b", 1, proto.RunRunning, nil)
	tr := e.t0.Add(3 * time.Hour).UTC()
	repb := func(l *proto.RunLimit) proto.RunReport {
		return proto.RunReport{RunID: "run_b", SpaceID: "spc_b", Generation: 1, State: proto.RunRunning, Limit: l}
	}
	e.reportFull(repb(&proto.RunLimit{Kind: proto.LimitSevenDay, Since: since, ResetsAt: &tr}))
	other := e.t0.Add(5 * time.Hour).UTC()
	e.reportFull(repb(&proto.RunLimit{Kind: proto.LimitSevenDay, Since: since, ResetsAt: &other, Source: proto.LimitSourceStatusLine}))
	if runb, _ := e.svc.GetRun(e.ctx, "run_b"); runb.Limit == nil || runb.Limit.ResetsAt == nil || !runb.Limit.ResetsAt.Equal(tr) {
		t.Fatalf("a status-line reset replaced the transcript's: %+v", runb.Limit)
	}
}

// Minor 13: the failure class is stored on the run and shown, only from the allowlist; a report without one keeps it.
func TestRunFailureStored(t *testing.T) {
	e := ulEnv(t)
	e.runOn("run_a", "spc_a", "acc_1", "")
	e.report("run_a", "spc_a", 1, proto.RunRunning, nil)
	rep := func(f string) proto.RunReport {
		return proto.RunReport{RunID: "run_a", SpaceID: "spc_a", Generation: 1, State: proto.RunRunning, Failure: f}
	}
	for _, bad := range []string{"rate_limit", "Billing_Error", "you have run out of credit"} {
		e.reportFull(rep(bad))
		if run, _ := e.svc.GetRun(e.ctx, "run_a"); run.Failure != "" {
			t.Fatalf("%q stored as %q", bad, run.Failure)
		}
	}
	e.reportFull(rep("billing_error"))
	run, err := e.svc.GetRun(e.ctx, "run_a")
	if err != nil || run.Failure != "billing_error" {
		t.Fatalf("failure %q %v", run.Failure, err)
	}
	v := e.runVersion("run_a")
	e.reportFull(rep("billing_error"))
	e.reportFull(rep(""))
	if got := e.runVersion("run_a"); got != v {
		t.Fatalf("a repeat or an empty report moved the version %d → %d", v, got)
	}
	if run, _ = e.svc.GetRun(e.ctx, "run_a"); run.Failure != "billing_error" {
		t.Fatalf("an empty report cleared the class: %q", run.Failure)
	}
	e.reportFull(rep("authentication_failed"))
	if run, _ = e.svc.GetRun(e.ctx, "run_a"); run.Failure != "authentication_failed" {
		t.Fatalf("a newer class not stored: %q", run.Failure)
	}
	// An explicit clear (a new turn started) resets it, once; a clear never accompanies a class.
	clear := rep("")
	clear.FailureCleared = true
	e.reportFull(clear)
	if run, _ = e.svc.GetRun(e.ctx, "run_a"); run.Failure != "" {
		t.Fatalf("the clear was not applied: %q", run.Failure)
	}
	v = e.runVersion("run_a")
	e.reportFull(clear)
	if got := e.runVersion("run_a"); got != v {
		t.Fatalf("a repeated clear moved the version %d → %d", v, got)
	}
}

// The OpenAPI enum on Run.Failure is the proto allowlist.
func TestRunFailureEnumMatchesAllowlist(t *testing.T) {
	f, ok := reflect.TypeOf(Run{}).FieldByName("Failure")
	if !ok {
		t.Fatal("no Failure field")
	}
	got := strings.Split(f.Tag.Get("enum"), ",")
	if !slices.Equal(got, proto.FailureClasses) {
		t.Fatalf("enum tag %v, allowlist %v", got, proto.FailureClasses)
	}
}

// A minor-12 session keeps today's behaviour: the failure class and the limit source on a report never reach the apply.
func TestRunReportAccountUsageFilteredByMinor(t *testing.T) {
	e := newStaleEnv(t)
	var mu sync.Mutex
	var seen []proto.RunReport
	e.svc.onReport = func(r proto.RunReport) { mu.Lock(); seen = append(seen, r); mu.Unlock() }
	reset := e.t0.Add(time.Hour)
	rep := proto.RunReport{RunID: "run_x", SpaceID: "spc_x", Generation: 1, State: proto.RunExited, ObservedAt: e.t0, Failure: "billing_error",
		Limit: &proto.RunLimit{Kind: proto.LimitUnknown, ResetsAt: &reset, Since: e.t0, Source: proto.LimitSourceStatusLine}}
	b, err := proto.Encode("msg_rep", proto.TypeRunReport, e.t0, rep)
	if err != nil {
		t.Fatal(err)
	}
	env, err := proto.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, minor := range []int{proto.MinorUsageLimit, proto.MinorAccountUsage} {
		s := &session{nodeID: "nod_1", seq: 1, minor: minor, log: slog.New(slog.DiscardHandler)}
		mu.Lock()
		seen = nil
		mu.Unlock()
		_ = e.svc.hub.handle(e.ctx, s, env)
		mu.Lock()
		got := seen
		mu.Unlock()
		if len(got) != 1 || got[0].Limit == nil {
			t.Fatalf("minor %d: %+v", minor, got)
		}
		want := minor >= proto.MinorAccountUsage
		if (got[0].Failure != "") != want || (got[0].Limit.Source != "") != want {
			t.Errorf("minor %d: failure %q source %q", minor, got[0].Failure, got[0].Limit.Source)
		}
		if h := s.report(rep); (h.Failure != "") != want || (h.Limit.Source != "") != want {
			t.Errorf("minor %d: hello filter kept failure %q source %q", minor, h.Failure, h.Limit.Source)
		}
	}
}
