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
	"github.com/aurimasniekis/go-spaceman/internal/catalog"
	"github.com/aurimasniekis/go-spaceman/internal/dream"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
)

// SM-DREAM-001…004, ADR-0027 end to end: a dream reads the new part of other roles' transcripts (never its own), edits
// role memory through an ordinary checkout and reports; only then are its memory changes checkpointed (and synced) and
// its evidence advanced. A dream that ends without reporting advances nothing.
func TestDreamConsolidates(t *testing.T) {
	t.Parallel()
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
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "dreaming", Name: "Dreaming", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	space := filepath.Join(e.agentDir, "spaces", sp.Workdir)

	// A worker of the implementor role: its memory exists and it left a transcript.
	wrk, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "worker running", func() bool { return e.run1(wrk.ID).Observed == "running" })
	inst := e.run1(wrk.ID).InstanceID
	transcript := filepath.Join(space, "instances", inst, "claude", "projects", "work", "s1.jsonl")
	write := func(p, s string, appendTo bool) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
		if appendTo {
			flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
		}
		f, err := os.OpenFile(p, flag, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(s)
		_ = f.Close()
	}
	write(transcript, `{"type":"user","text":"the bank window is 01:00-03:00 UTC"}`+"\n", false)
	if _, err := e.c.Fleet.StopRun(ctx, admin, wrk.ID, "kill", "done"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "worker memory synced", func() bool { _, err := e.c.Memory.Files(ctx, sp.ID, "implementor"); return err == nil })

	prof, err := e.c.Catalog.SaveProfile(ctx, admin, "", 0, catalog.Profile{Slug: "dreamer-claude", Name: "Dreamer", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := e.c.Catalog.SaveTemplate(ctx, admin, "", 0, catalog.RoleTemplate{Slug: "dreamer", Name: "Dreamer", Kind: "dreamer", ProfileID: prof.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Catalog.SaveSpaceRole(ctx, admin, sp.ID, catalog.SpaceRole{Name: "dreamer", TemplateID: tpl.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Catalog.SaveSpaceRole(ctx, admin, sp.ID, catalog.SpaceRole{Name: "not-a-dreamer", TemplateID: tpl.ID}); err != nil {
		t.Fatal(err)
	}
	cfg, err := e.c.Dreams.Create(ctx, admin, sp.ID, dream.Input{Slug: "nightly", Name: "Nightly", SpaceRole: "dreamer", MemoryRoles: []string{"implementor"}})
	if err != nil {
		t.Fatal(err)
	}
	// The dreamer's own transcripts are excluded: a transcript in the dream configuration's own instance is never offered.
	full, _ := e.c.Store.R().GetDreamConfig(ctx, cfg.ID)
	write(filepath.Join(space, "instances", full.InstanceID, "claude", "projects", "x", "own.jsonl"), `{"own":true}`+"\n", false)

	startDream := func() (*dream.Run, string) {
		t.Helper()
		r, err := e.c.Dreams.Start(ctx, admin, sp.ID, cfg.ID)
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, "dreamer running", func() bool { return e.run1(r.RunID).Observed == "running" })
		return r, filepath.Join(space, "dreams", r.RunID)
	}
	d1, dir := startDream()
	if _, err := e.c.Dreams.Start(ctx, admin, sp.ID, cfg.ID); err == nil {
		t.Fatal("a second concurrent dream started")
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "DREAM.md"))
	if err != nil || !strings.Contains(string(manifest), "memory/implementor/") || !strings.Contains(string(manifest), "s1.jsonl") || strings.Contains(string(manifest), "own.jsonl") {
		t.Fatalf("DREAM.md: %s %v", manifest, err)
	}
	ev, _ := filepath.Glob(filepath.Join(dir, "evidence", "implementor", "*.jsonl"))
	if len(ev) != 1 {
		t.Fatalf("evidence: %v", ev)
	}
	if e.run1(d1.RunID).Kind != "dreamer" {
		t.Fatal("the dream did not run as a dreamer")
	}

	// A dream that ends without reporting advances nothing.
	if _, err := e.c.Fleet.StopRun(ctx, admin, d1.RunID, "kill", "crash"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "unreported dream failed", func() bool {
		runs, _ := e.c.Dreams.Runs(ctx, sp.ID, cfg.ID)
		return len(runs) == 1 && runs[0].State == "failed"
	})

	// The next dream gets the same evidence, consolidates and reports.
	d2, dir2 := startDream()
	if m, _ := os.ReadFile(filepath.Join(dir2, "DREAM.md")); !strings.Contains(string(m), "s1.jsonl") {
		t.Fatalf("evidence was advanced by a failed dream: %s", m)
	}
	write(filepath.Join(dir2, "memory", "implementor", "bank-window.md"), "# Bank window\n\n01:00-03:00 UTC (from a worker transcript, 2026-11-02).\n", false)
	tok := e.runToken(d2.RunID)
	if r := e.bridgeCall(d2.RunID, tok, "spaceman_dream_report", map[string]any{"summary": "Recorded the bank window.", "changes": []string{"bank-window.md"},
		"findings": []map[string]string{{"kind": "insight", "summary": "Retries must avoid the bank window."}}}); !r.OK {
		t.Fatalf("report: %+v", r)
	}
	if r := e.bridgeCall(d2.RunID, tok, "spaceman_dream_report", map[string]any{"summary": "again"}); r.OK {
		t.Fatal("a dream reported twice")
	}
	eventually(t, "dream succeeded", func() bool {
		runs, _ := e.c.Dreams.Runs(ctx, sp.ID, cfg.ID)
		return len(runs) == 2 && runs[0].State == "succeeded" && len(runs[0].Findings) == 1
	})
	eventually(t, "dream memory synced to central", func() bool {
		files, err := e.c.Memory.Files(ctx, sp.ID, "implementor")
		return err == nil && slices.Contains(files, "bank-window.md")
	})
	e.c.Dreams.Step(ctx)
	eventually(t, "finished dream stopped", func() bool { o := e.run1(d2.RunID).Observed; return o == "exited" || o == "failed" })

	// Evidence advanced: the next dream sees only what is new.
	d3, dir3 := startDream()
	if m, _ := os.ReadFile(filepath.Join(dir3, "DREAM.md")); !strings.Contains(string(m), "Nothing new") {
		t.Fatalf("old evidence offered again: %s", m)
	}
	tok3 := e.runToken(d3.RunID)
	if r := e.bridgeCall(d3.RunID, tok3, "spaceman_dream_report", map[string]any{"summary": "Nothing to do.", "no_change": true}); !r.OK {
		t.Fatalf("no-change report: %+v", r)
	}
	eventually(t, "no-change dream succeeded", func() bool {
		runs, _ := e.c.Dreams.Runs(ctx, sp.ID, cfg.ID)
		return len(runs) == 3 && runs[0].State == "succeeded" && runs[0].NoChange
	})
	e.c.Dreams.Step(ctx)
	eventually(t, "third dream stopped", func() bool { o := e.run1(d3.RunID).Observed; return o == "exited" || o == "failed" })
	write(transcript, `{"type":"user","text":"retries: 5 per batch"}`+"\n", true)
	_, dir4 := startDream()
	m4, _ := os.ReadFile(filepath.Join(dir4, "DREAM.md"))
	ev4, _ := filepath.Glob(filepath.Join(dir4, "evidence", "implementor", "*.jsonl"))
	if len(ev4) != 1 {
		t.Fatalf("new evidence: %s %v", m4, ev4)
	}
	if b, _ := os.ReadFile(ev4[0]); strings.Contains(string(b), "bank window") || !strings.Contains(string(b), "5 per batch") {
		t.Fatalf("offered bytes: %q", b)
	}
	// Only dreamer-kind roles can dream; workers cannot report dreams.
	if _, err := e.c.Dreams.Create(ctx, admin, sp.ID, dream.Input{Slug: "bad", Name: "Bad", SpaceRole: "nope"}); err == nil {
		t.Fatal("a dream with an unknown role was accepted")
	}
}
