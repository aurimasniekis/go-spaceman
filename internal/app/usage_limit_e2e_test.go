package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// ADR-0079 (simulated: a scripted node connection): a node agent that speaks up to minor 11 still connects to a
// minor-12 central at minor 11. Central sends it nothing of minor 12 (its launch carries no limit field), and a limit
// on a report from that node is not stored. A minor-12 node negotiates minor 12.
func TestUsageLimitMinor11Node(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	ctx := context.Background()
	n, sp := hvNode(t, e, "ulold", proto.MinorUsageLimit-1, true) // fails unless the welcome says minor 11
	r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Role: "free", Kind: "worker", Harness: "claude-code", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	var gen int64
	for {
		env := n.next(t, proto.TypeCommand)
		if strings.Contains(string(env.Body), `"limit"`) || strings.Contains(string(env.Body), `"resets_at"`) {
			t.Fatalf("minor-12 field sent to a minor-11 node: %s", env.Body)
		}
		var c proto.Command
		if err := env.Decode(&c); err != nil {
			t.Fatal(err)
		}
		if c.RunID == r.ID && c.Kind == proto.CmdLaunch {
			gen = c.Generation
			break
		}
	}
	reset := time.Now().Add(time.Hour).UTC()
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: r.ID, SpaceID: sp.ID, Generation: gen, State: proto.RunRunning, Status: "idle",
		Limit: &proto.RunLimit{Kind: proto.LimitFiveHour, ResetsAt: &reset, Since: time.Now().UTC()}, ObservedAt: time.Now().UTC()})
	eventually(t, "running", func() bool { return e.run1(r.ID).Observed == "running" })
	var kind string
	if err := e.c.Store.ReadDB().QueryRowContext(ctx, `SELECT limit_kind FROM runs WHERE id = ?`, r.ID).Scan(&kind); err != nil || kind != "" {
		t.Fatalf("limit from a minor-11 node stored: %q %v", kind, err)
	}

	hvNode(t, e, "ulnew", proto.MinorUsageLimit, true) // fails unless the welcome says minor 12
}
