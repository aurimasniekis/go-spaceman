package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/control"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/limits"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// ADR-0079 Q5, UL-9 (simulated: a scripted minor-12 node, central on a fake clock, the deadline loop running as in
// Serve with the limit-wait and people-notice jobs). A worker's 5-hour limit (3 h away, under the 12 h default) raises
// no human action. Its weekly limit (3 days away) raises exactly one in the space: an informational message from
// SpaceMan, normal priority, naming the account, the reset and what waits on it; more reports, parking and later
// rounds raise no second one.
func TestUsageLimitPeopleNotice(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	e := newFleetEnvClock(t, clk)
	e.enroll()
	ctx := context.Background()
	n, sp := hvNode(t, e, "ulpeople", proto.MinorUsageLimit, false)
	dctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go e.c.Human.Run(dctx, human.Deadline{Name: "limit_waits", Next: e.c.Fleet.NextLimitWait, Due: e.c.Fleet.LimitWaitsDue},
		human.Deadline{Name: "usage_limit_people", Due: e.c.Supervisor.TellLongHolds})
	w1, gen, task := assignedWorker(t, e, n, sp.ID, "Wait for me", "")
	notices := func() []*human.Action {
		t.Helper()
		l, err := e.c.Human.List(ctx, sp.ID, human.ListFilter{})
		if err != nil {
			t.Fatal(err)
		}
		var out []*human.Action
		for _, a := range l {
			if strings.HasPrefix(a.DedupKey, "usage-limit-human:") {
				out = append(out, a)
			}
		}
		return out
	}
	report := func(l *proto.RunLimit) {
		n.send(t, proto.TypeRunReport, proto.RunReport{RunID: w1.ID, SpaceID: sp.ID, Generation: gen, State: proto.RunRunning, Status: "idle", Turn: 2,
			SessionID: "ses-people", Limit: l, ObservedAt: time.Now().UTC()})
	}

	// The 5-hour limit: kept alive, no notice.
	since := clk.Now()
	short := since.Add(3 * time.Hour)
	report(&proto.RunLimit{Kind: proto.LimitFiveHour, ResetsAt: &short, Since: since})
	eventually(t, "5-hour hold", func() bool {
		hs, _ := e.c.Fleet.UsageLimits(ctx, sp.ID)
		return len(hs) == 1 && hs[0].Kind == proto.LimitFiveHour
	})
	e.c.Human.Kick()
	time.Sleep(300 * time.Millisecond)
	if l := notices(); len(l) != 0 {
		t.Fatalf("a 3 h wait raised %d notices", len(l))
	}

	// A weekly limit (a new window): one notice.
	clk.Advance(time.Minute)
	since2 := clk.Now()
	long := since2.Add(3 * 24 * time.Hour)
	weekly := &proto.RunLimit{Kind: proto.LimitSevenDay, ResetsAt: &long, Since: since2}
	report(weekly)
	eventually(t, "people told", func() bool { return len(notices()) == 1 })
	var hold fleet.UsageLimit
	hs, _ := e.c.Fleet.UsageLimits(ctx, sp.ID)
	for _, h := range hs {
		if h.Kind == proto.LimitSevenDay && h.ClearedAt == nil {
			hold = h
		}
	}
	a := notices()[0]
	if a.DedupKey != control.LongHoldDedup(hold.ID) || a.CorrelationID != hold.ID || a.Type != human.TypeMessage || a.Priority != "normal" ||
		a.RequesterKind != "system" || a.RequesterLabel != "Usage limits" || a.BlocksTask {
		t.Fatalf("notice %+v, hold %+v", a, hold)
	}
	if !strings.Contains(a.Title, "weekly usage limit until "+long.UTC().Format("2006-01-02 15:04 UTC")) ||
		!strings.Contains(a.Body, "In ulpeople, account ") || !strings.Contains(a.Body, "1 run is waiting on it (1 with a task)") ||
		!strings.Contains(a.Body, "usage_limit_failover") || !strings.Contains(a.Body, "longer than 12 hours") {
		t.Fatalf("notice text %q / %q", a.Title, a.Body)
	}

	// Parked (central stops it), reported again, later rounds: still one.
	n.command(t, w1.ID, proto.CmdStop)
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: w1.ID, SpaceID: sp.ID, Generation: gen, State: proto.RunExited, Reason: "stopped", Turn: 2,
		SessionID: "ses-people", Limit: weekly, ObservedAt: time.Now().UTC()})
	eventually(t, "parked", func() bool { w, _ := e.c.Fleet.LimitWaitForRun(ctx, w1.ID); return w != nil && w.TaskID == task.ID })
	clk.Advance(time.Hour)
	e.c.Human.Kick()
	if _, err := e.c.Supervisor.TellLongHolds(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if l := notices(); len(l) != 1 {
		t.Fatalf("%d notices for one account window", len(l))
	}
}

// The usage_limit space settings (ADR-0079 Q1, Q2, Q3, Q5): space limits shown with their bounds and validated on set.
func TestUsageLimitSettingsValidated(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "ulset", Name: "ULSet", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	eff, err := e.c.Limits.Effective(ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][4]int64{ // value, min, max, zero means none
		limits.UsageLimitFailover: {0, 0, 1, 0}, limits.UsageLimitKeepAliveMax: {360, 1, 1440, 0},
		limits.UsageLimitMaxWait: {168, 1, 720, 0}, limits.UsageLimitNotifyAfter: {12, 0, 720, 1},
	}
	for _, l := range eff.Limits {
		w, ok := want[l.Key]
		if !ok {
			continue
		}
		delete(want, l.Key)
		if l.Value != w[0] || l.Min != w[1] || l.Max != w[2] || l.ZeroNone != (w[3] == 1) || !l.Overridable || l.Source != limits.SourceInstallation {
			t.Fatalf("%s: %+v", l.Key, l)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing settings %v", want)
	}
	for key, bad := range map[string][]int64{limits.UsageLimitFailover: {-1, 2}, limits.UsageLimitKeepAliveMax: {0, 1441},
		limits.UsageLimitMaxWait: {0, 721}, limits.UsageLimitNotifyAfter: {-1, 721}} {
		for _, v := range bad {
			_, err := e.c.Limits.Set(ctx, admin, sp.ID, 0, limits.Input{Values: map[string]int64{key: v}})
			if apperr.CodeOf(err) != apperr.CodeInvalid || !strings.Contains(err.Error(), key) {
				t.Fatalf("%s=%d: %v", key, v, err)
			}
		}
	}
	eff, err = e.c.Limits.Set(ctx, admin, sp.ID, 0, limits.Input{Values: map[string]int64{limits.UsageLimitNotifyAfter: 0, limits.UsageLimitFailover: 1}})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range eff.Limits {
		if (l.Key == limits.UsageLimitNotifyAfter && l.Value != 0) || (l.Key == limits.UsageLimitFailover && l.Value != 1) {
			t.Fatalf("%s after set: %+v", l.Key, l)
		}
	}
}
