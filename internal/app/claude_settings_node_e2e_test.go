package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/catalog"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// ADR-0059 (simulated: a scripted node connection): a run whose profile has Claude Code settings is never sent to a node
// that negotiated a minor below proto.MinorClaudeSettings, which would start it without them (deny rules included).
// The dispatch loop fails the run as node_too_old and cancels its launch instead of retrying it (this used to leave the
// run pending: ending it wrote a NULL session ID).
func TestClaudeSettingsLaunchFailsOnOldNode(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	nodeID := nodes[0].ID
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "oldnode", Name: "Old node", NodeID: nodeID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	n := dialRawNode(t, e, proto.MinorClaudeSettings-1)
	var w proto.Welcome
	if err := n.next(t, proto.TypeWelcome).Decode(&w); err != nil || w.Minor != proto.MinorClaudeSettings-1 {
		t.Fatalf("welcome %+v %v", w, err)
	}
	var grant proto.LeaseGrant
	if err := n.next(t, proto.TypeLeaseGrant).Decode(&grant); err != nil {
		t.Fatal(err)
	}
	eventually(t, "lease", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })

	prof, err := e.c.Catalog.SaveProfile(ctx, admin, "", 0, catalog.Profile{Slug: "claude-deny", Name: "Claude with deny rules", Harness: "claude-code",
		ClaudeSettings: proto.ClaudeSettings{"permissions": map[string]any{"deny": []any{"Bash(rm:*)"}}}})
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := e.c.Catalog.SaveTemplate(ctx, admin, "", 0, catalog.RoleTemplate{Slug: "denied", Name: "Denied", Kind: "worker", ProfileID: prof.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Catalog.SaveSpaceRole(ctx, admin, sp.ID, catalog.SpaceRole{Name: "denied", TemplateID: tpl.ID}); err != nil {
		t.Fatal(err)
	}
	r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Role: "denied", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "failed as node_too_old", func() bool { x := e.run1(r.ID); return x.Observed == "failed" && x.EndReason == "node_too_old" })
	var state string
	if err := e.c.Store.ReadDB().QueryRowContext(ctx, `SELECT state FROM node_commands WHERE run_id = ?`, r.ID).Scan(&state); err != nil || state != "cancelled" {
		t.Fatalf("launch command %q %v", state, err)
	}
	// The launch was cancelled, not sent: nothing reaches the node for this run.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case env, ok := <-n.ch:
			if !ok {
				t.Fatal("connection closed")
			}
			var c proto.Command
			if env.Type == proto.TypeCommand && env.Decode(&c) == nil && c.RunID == r.ID {
				t.Fatalf("the run reached the old node: %+v", c.Kind)
			}
		case <-deadline:
			return
		}
	}
}
