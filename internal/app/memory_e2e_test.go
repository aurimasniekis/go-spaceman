package app_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
)

// SM-MEM-001…005, ADR-0025 end to end: a run gets its role's memory checkout (and is told where it is); what it writes
// there is checkpointed when the run ends, published to the node's shared history and synced to central over the node
// connection. A second run of the same role starts from that history.
func TestMemorySyncedToCentral(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := newFleetEnv(t)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	admin := audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "memory", Name: "Memory", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { x, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return x.Lease != nil && x.Lease.Valid })
	run, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor-backend", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "run running", func() bool { return e.run1(run.ID).Observed == "running" })
	r := e.run1(run.ID)
	dir := filepath.Join(e.agentDir, "memory", sp.ID, "implementor-backend", "instances", r.InstanceID)
	// The harness was told where its memory is.
	var argv []string
	if procs, err := e.run.List(ctx, map[string]string{"spaceman.run": run.ID}, true); err == nil && len(procs) == 1 {
		argv = procs[0].Argv
	}
	if !slices.Contains(argv, "--add-dir") || !strings.Contains(strings.Join(argv, " "), "Role memory") {
		t.Fatalf("the run was not given its memory: %v", argv)
	}
	if err := os.WriteFile(filepath.Join(dir, "decisions.md"), []byte("- settle in batches of 500\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Fleet.StopRun(ctx, admin, run.ID, "kill", "done"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "memory acknowledged by central", func() bool {
		files, err := e.c.Memory.Files(ctx, sp.ID, "implementor-backend")
		return err == nil && slices.Contains(files, "decisions.md")
	})
	content, err := e.c.Memory.File(ctx, sp.ID, "implementor-backend", "decisions.md")
	if err != nil || content != "- settle in batches of 500\n" {
		t.Fatalf("central copy: %q %v", content, err)
	}
	eventually(t, "node status reported", func() bool {
		c, err := e.c.Memory.Get(ctx, sp.ID, "implementor-backend")
		return err == nil && c.Node != nil && !c.Node.Pending && c.Node.Acked == c.Head && c.Node.Instances == 1
	})

	// A new instance of the role starts from the shared history.
	run2, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor-backend", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "second run running", func() bool { return e.run1(run2.ID).Observed == "running" })
	dir2 := filepath.Join(e.agentDir, "memory", sp.ID, "implementor-backend", "instances", e.run1(run2.ID).InstanceID)
	if b, err := os.ReadFile(filepath.Join(dir2, "decisions.md")); err != nil || string(b) != "- settle in batches of 500\n" {
		t.Fatalf("second instance: %q %v", b, err)
	}
}
