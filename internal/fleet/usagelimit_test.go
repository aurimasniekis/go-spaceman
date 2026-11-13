package fleet

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// ADR-0079: a usage limit on a run report from a session negotiated below minor 12 never reaches the report's apply,
// whatever the node sent; from minor 12 on it does. Hello reports go through the same session filter (session.report).
func TestRunReportLimitFilteredByMinor(t *testing.T) {
	e := newStaleEnv(t)
	var mu sync.Mutex
	var seen []proto.RunReport
	e.svc.onReport = func(r proto.RunReport) { mu.Lock(); seen = append(seen, r); mu.Unlock() }
	reset := e.t0.Add(time.Hour)
	rep := proto.RunReport{RunID: "run_x", SpaceID: "spc_x", Generation: 1, State: proto.RunExited, ObservedAt: e.t0,
		Limit: &proto.RunLimit{Kind: proto.LimitFiveHour, ResetsAt: &reset, Since: e.t0}}
	b, err := proto.Encode("msg_rep", proto.TypeRunReport, e.t0, rep)
	if err != nil {
		t.Fatal(err)
	}
	env, err := proto.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, minor := range []int{proto.MinorUsageLimit - 1, proto.MinorUsageLimit} {
		s := &session{nodeID: "nod_1", seq: 1, minor: minor, log: slog.New(slog.DiscardHandler)}
		mu.Lock()
		seen = nil
		mu.Unlock()
		_ = e.svc.hub.handle(e.ctx, s, env) // an unknown run: fenced, which doesn't matter here
		mu.Lock()
		got := seen
		mu.Unlock()
		if len(got) != 1 {
			t.Fatalf("minor %d: %d reports applied", minor, len(got))
		}
		if want := minor >= proto.MinorUsageLimit; (got[0].Limit != nil) != want {
			t.Errorf("minor %d: applied limit %+v", minor, got[0].Limit)
		}
		if want := minor >= proto.MinorUsageLimit; (s.report(rep).Limit != nil) != want {
			t.Errorf("minor %d: hello report filter kept %+v", minor, s.report(rep).Limit)
		}
	}
}
