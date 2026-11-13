package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/catalog"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// pinnedRole configures a space role whose claude-code profile pins pin.
func pinnedRole(t *testing.T, e *fleetEnv, spaceID, slug, pin string) {
	t.Helper()
	ctx := context.Background()
	prof, err := e.c.Catalog.SaveProfile(ctx, admin, "", 0, catalog.Profile{Slug: slug, Name: slug, Harness: "claude-code", HarnessVersion: pin})
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := e.c.Catalog.SaveTemplate(ctx, admin, "", 0, catalog.RoleTemplate{Slug: slug, Name: slug, Kind: "worker", ProfileID: prof.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Catalog.SaveSpaceRole(ctx, admin, spaceID, catalog.SpaceRole{Name: slug, TemplateID: tpl.ID}); err != nil {
		t.Fatal(err)
	}
}

// hvNode connects a scripted node at maxMinor that reports a Claude Code version and channel (managed) or only its
// detected version, and a Codex version, and waits for its lease on a new space.
func hvNode(t *testing.T, e *fleetEnv, slug string, maxMinor int, managed bool) (*rawNode, *fleet.Space) {
	t.Helper()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: slug, Name: slug, NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	n := dialRawNodeHello(t, e, proto.Hello{AgentVersion: "test", MinMinor: proto.MinMinor, MaxMinor: maxMinor, Runs: []proto.RunReport{}, Leases: []proto.LeaseState{},
		Capabilities: proto.Capabilities{Harnesses: []proto.HarnessInfo{{Kind: "claude-code", Version: "2.1.322", Available: true, Capabilities: []string{},
			Managed: managed, Channel: map[bool]string{true: "latest"}[managed]}, {Kind: "codex", Version: "0.50.0", Available: true, Capabilities: []string{}}}}})
	var w proto.Welcome
	if err := n.next(t, proto.TypeWelcome).Decode(&w); err != nil || w.Minor != maxMinor {
		t.Fatalf("welcome %+v %v", w, err)
	}
	n.next(t, proto.TypeLeaseGrant)
	eventually(t, "lease", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	return n, sp
}

// ADR-0077 (simulated: a scripted node connection): a run whose profile pins a Claude Code version is never sent to a
// node below minor 11, which would ignore the pin. It fails as node_too_old with a clear error naming the node; the
// launch is cancelled, not sent. An unpinned run still reaches that node, and its manifest keeps the plain version.
func TestHarnessPinFailsOnOldNode(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	ctx := context.Background()
	n, sp := hvNode(t, e, "oldpin", proto.MinorHarnessVersion-1, true)
	pinnedRole(t, e, sp.ID, "pinned", "2.1.317")
	r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Role: "pinned", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "failed as node_too_old", func() bool { x := e.run1(r.ID); return x.Observed == "failed" && x.EndReason == "node_too_old" })
	if x := e.run1(r.ID); !strings.Contains(x.Error, "doesn't support pinned Claude Code versions; upgrade it") {
		t.Fatalf("error %q", x.Error)
	}
	var state string
	if err := e.c.Store.ReadDB().QueryRowContext(ctx, `SELECT state FROM node_commands WHERE run_id = ?`, r.ID).Scan(&state); err != nil || state != "cancelled" {
		t.Fatalf("launch command %q %v", state, err)
	}

	plain, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Role: "free", Kind: "worker", Harness: "claude-code", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	for {
		var c proto.Command
		if err := n.next(t, proto.TypeCommand).Decode(&c); err != nil {
			t.Fatal(err)
		}
		if c.RunID == r.ID {
			t.Fatal("the pinned run reached the old node")
		}
		if c.RunID == plain.ID && c.Kind == proto.CmdLaunch {
			break
		}
	}
	var m struct {
		Dependencies map[string]any `json:"dependencies"`
	}
	if err := json.Unmarshal(e.run1(plain.ID).Manifest, &m); err != nil || m.Dependencies["claude-code"] != "2.1.322" {
		t.Fatalf("old node's manifest dependency %v %v", m.Dependencies["claude-code"], err)
	}
}

// ADR-0077 / SM-HARN-007 (simulated): a minor-11 node gets the pin on the launch; its report of the version the run
// launched is stored on the run row and shown with the run; the manifest records {requested, node_default}.
func TestHarnessPinReachesNodeAndRunShowsVersion(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	ctx := context.Background()
	n, sp := hvNode(t, e, "newpin", proto.MinorHarnessVersion, true)
	pinnedRole(t, e, sp.ID, "pinned", "2.1.317")
	r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Role: "pinned", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	var c proto.Command
	for c.RunID != r.ID {
		if err := n.next(t, proto.TypeCommand).Decode(&c); err != nil {
			t.Fatal(err)
		}
	}
	var p proto.LaunchPayload
	if err := json.Unmarshal(c.Payload, &p); err != nil || p.HarnessVersion != "2.1.317" {
		t.Fatalf("launch payload pin %q %v", p.HarnessVersion, err)
	}
	var m struct {
		Dependencies   map[string]any `json:"dependencies"`
		HarnessVersion string         `json:"harness_version"`
	}
	if err := json.Unmarshal(e.run1(r.ID).Manifest, &m); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(m.Dependencies["claude-code"]); got != "map[node_default:2.1.322 requested:2.1.317]" || m.HarnessVersion != "2.1.317" {
		t.Fatalf("manifest %v %q", m.Dependencies["claude-code"], m.HarnessVersion)
	}

	// The first starting report before the version is known carries none; the one after does.
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: r.ID, SpaceID: sp.ID, Generation: c.Generation, State: proto.RunStarting, ObservedAt: time.Now()})
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: r.ID, SpaceID: sp.ID, Generation: c.Generation, State: proto.RunStarting, ObservedAt: time.Now(),
		HarnessVersion: "2.1.317", HarnessRequested: "2.1.317"})
	eventually(t, "the run's version", func() bool { return e.run1(r.ID).HarnessVersion == "2.1.317" })
	// A later report without the version keeps it; the API run carries it.
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: r.ID, SpaceID: sp.ID, Generation: c.Generation, State: proto.RunRunning, ObservedAt: time.Now()})
	eventually(t, "running", func() bool { return e.run1(r.ID).Observed == proto.RunRunning })
	x := e.run1(r.ID)
	if x.HarnessVersion != "2.1.317" || x.HarnessRequested != "2.1.317" || x.HarnessFallback != "" {
		t.Fatalf("run %+v", x)
	}
	b, _ := json.Marshal(x)
	if !strings.Contains(string(b), `"harness_version":"2.1.317"`) || !strings.Contains(string(b), `"harness_requested":"2.1.317"`) {
		t.Fatalf("API run %s", b)
	}

	// A channel run with a fallback: what ran, the channel and why are all kept; a malformed version is ignored.
	r2, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Role: "free", Kind: "worker", Harness: "claude-code", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(e.run1(r2.ID).Manifest, &m); err != nil || fmt.Sprint(m.Dependencies["claude-code"]) != "map[node_default:2.1.322 requested:latest]" {
		t.Fatalf("channel manifest %v %v", m.Dependencies["claude-code"], err)
	}
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: r2.ID, SpaceID: sp.ID, Generation: c.Generation, State: proto.RunStarting, ObservedAt: time.Now(),
		HarnessVersion: "../2.1.0", HarnessRequested: "latest"})
	eventually(t, "starting", func() bool { return e.run1(r2.ID).Observed == proto.RunStarting })
	if x := e.run1(r2.ID); x.HarnessVersion != "" || x.HarnessRequested != "" {
		t.Fatalf("malformed version stored: %+v", x)
	}
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: r2.ID, SpaceID: sp.ID, Generation: c.Generation, State: proto.RunRunning, ObservedAt: time.Now(),
		HarnessVersion: "2.1.320", HarnessRequested: "latest", HarnessFallback: "previous_target"})
	eventually(t, "the channel run's version", func() bool { return e.run1(r2.ID).HarnessVersion == "2.1.320" })
	if x := e.run1(r2.ID); x.HarnessRequested != "latest" || x.HarnessFallback != "previous_target" {
		t.Fatalf("channel run %+v", x)
	}
	// Review S2: a request that isn't a valid one and a fallback that isn't a known reason are dropped, not truncated;
	// the version itself is still stored.
	r3, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Role: "free", Kind: "worker", Harness: "claude-code", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: r3.ID, SpaceID: sp.ID, Generation: c.Generation, State: proto.RunRunning, ObservedAt: time.Now(),
		HarnessVersion: "2.1.321", HarnessRequested: strings.Repeat("nightly", 20), HarnessFallback: "because\nI said so"})
	eventually(t, "the third run's version", func() bool { return e.run1(r3.ID).HarnessVersion == "2.1.321" })
	if x := e.run1(r3.ID); x.HarnessRequested != "" || x.HarnessFallback != "" {
		t.Fatalf("bad report fields stored: requested %q, fallback %q", x.HarnessRequested, x.HarnessFallback)
	}
}

// Review N2: only managed Claude Code gets the {requested, node_default} dependency. Unmanaged Claude Code and other
// harnesses keep the plain version string, also on a minor-11 node.
func TestHarnessDependencyPlainUnlessManaged(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	ctx := context.Background()
	_, sp := hvNode(t, e, "unmanaged", proto.MinorHarnessVersion, false)
	for harness, want := range map[string]string{"claude-code": "2.1.322", "codex": "0.50.0"} {
		r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Role: "free-" + harness, Kind: "worker", Harness: harness, Prompt: "go"})
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Dependencies map[string]any `json:"dependencies"`
		}
		if err := json.Unmarshal(e.run1(r.ID).Manifest, &m); err != nil || m.Dependencies[harness] != want {
			t.Fatalf("%s dependency %#v %v, want the plain %q", harness, m.Dependencies[harness], err, want)
		}
	}
}

// SM-HARN-008 (simulated: a scripted node connection): a hello whose managed Claude Code entry carries a version of
// the wrong shape is still welcomed; central drops that version from the entry and stores the rest.
func TestHelloWithBadHarnessEntryAccepted(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	ctx := context.Background()
	n := dialRawNodeHello(t, e, proto.Hello{AgentVersion: "test", MinMinor: proto.MinMinor, MaxMinor: proto.MaxMinor, Runs: []proto.RunReport{}, Leases: []proto.LeaseState{},
		Capabilities: proto.Capabilities{OS: "linux", Harnesses: []proto.HarnessInfo{
			{Kind: "claude-code", Version: "2.1.322-beta", Available: true, Capabilities: []string{}, Managed: true, Channel: "latest", Cached: []string{"2.1.321", "../x"}},
			{Kind: "codex", Version: "0.50.0", Available: true, Capabilities: []string{}}}}})
	n.next(t, proto.TypeWelcome)
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	var node *fleet.Node
	eventually(t, "node connected", func() bool { node, _ = e.c.Fleet.GetNode(ctx, nodes[0].ID); return node.Connected })
	hs := node.Capabilities.Harnesses
	if len(hs) != 2 || hs[0].Version != "" || !hs[0].Managed || hs[0].Channel != "latest" || fmt.Sprint(hs[0].Cached) != "[2.1.321]" || hs[1].Version != "0.50.0" {
		t.Fatalf("stored harnesses %+v", hs)
	}
}
