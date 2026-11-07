package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/catalog"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/kb"
	"github.com/aurimasniekis/go-spaceman/internal/limits"
)

// SM-LIMIT-001/002 end to end with a real agent and central: the space's run concurrency limit refuses launches beyond
// it (control, through its tools, gets the same answer); KB support keeps its own pool while workers are at their
// limit; a KB session cannot start further support (depth guard); lowering the limit lets running runs finish unless
// the space's on_lower policy is stop, which stops the newest run; agent tool calls stop at the per-run limit while
// the run keeps running.
func TestLimitsEndToEnd(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	admin := audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "limited", Name: "Limited", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	if _, err := e.c.Limits.Set(ctx, admin, sp.ID, 0, limits.Input{Values: map[string]int64{limits.ConcurrentRuns: 2, limits.ToolCallsPerRun: 3}}); err != nil {
		t.Fatal(err)
	}
	launch := func(kind, role string) (*fleet.Run, error) {
		return e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: kind, Role: role, Harness: hns.KindClaude})
	}
	var workers []*fleet.Run
	for range 2 {
		r, err := launch("worker", "implementor")
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, "worker running", func() bool { return e.run1(r.ID).Observed == "running" })
		workers = append(workers, r)
	}
	if _, err := launch("worker", "implementor"); apperr.CodeOf(err) != apperr.CodeRateLimited || !strings.Contains(err.Error(), "limit 2") {
		t.Fatalf("third worker: %v", err)
	}

	// Support has its own pool: a KB session starts while workers are at their limit.
	prof, err := e.c.Catalog.SaveProfile(ctx, admin, "", 0, catalog.Profile{Slug: "kb-claude", Name: "KB", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := e.c.Catalog.SaveTemplate(ctx, admin, "", 0, catalog.RoleTemplate{Slug: "kb-keeper", Name: "KB keeper", Kind: "kb", ProfileID: prof.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"payments", "billing"} {
		if _, err := e.c.Catalog.SaveSpaceRole(ctx, admin, sp.ID, catalog.SpaceRole{Name: "kb-" + slug, TemplateID: tpl.ID}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.c.KB.Create(ctx, admin, sp.ID, kb.Input{Slug: slug, Name: slug, Repo: "repos/" + slug, BaseBranch: "main", SpaceRole: "kb-" + slug}); err != nil {
			t.Fatal(err)
		}
	}
	w := workers[0]
	req, err := e.c.KB.Ask(ctx, &fleet.RunPrincipal{RunID: w.ID, SpaceID: sp.ID, Kind: "worker", Role: "implementor", InstanceID: w.InstanceID},
		kb.AskInput{KB: "payments", Kind: "context", Question: "What did we decide?"})
	if err != nil {
		t.Fatal(err)
	}
	e.c.KB.Step(ctx)
	var kbRun string
	eventually(t, "KB session launched despite the worker limit", func() bool {
		d, err := e.c.KB.Detail(ctx, sp.ID, req.KBID)
		if err != nil || len(d.Sessions) != 1 {
			return false
		}
		kbRun = d.Sessions[0].RunID
		return kbRun != ""
	})
	kr := e.run1(kbRun)
	if kr.Kind != "kb" {
		t.Fatalf("session run %+v", kr)
	}
	// The KB session cannot start support of its own (depth 1): no recursion through another knowledge base.
	_, err = e.c.KB.Ask(ctx, &fleet.RunPrincipal{RunID: kr.ID, SpaceID: sp.ID, Kind: "kb", Role: "kb-payments", InstanceID: kr.InstanceID},
		kb.AskInput{KB: "billing", Kind: "context", Question: "Ask the other KB"})
	if apperr.CodeOf(err) != apperr.CodeForbidden || !strings.Contains(err.Error(), "support depth 1") {
		t.Fatalf("recursive support: %v", err)
	}
	if d, _ := e.c.KB.Detail(ctx, sp.ID, req.KBID); len(d.Sessions) != 1 {
		t.Fatalf("a refused request created a session: %+v", d.Sessions)
	}

	// Tool calls: the per-run limit refuses further calls; the run keeps running.
	tok := e.runToken(w.ID)
	for i := range 3 {
		if r := e.bridgeCall(w.ID, tok, "spaceman_inbox_list", map[string]any{}); !r.OK {
			t.Fatalf("call %d: %+v", i, r)
		}
	}
	if r := e.bridgeCall(w.ID, tok, "spaceman_inbox_list", map[string]any{}); r.OK || r.Code != string(apperr.CodeRateLimited) {
		t.Fatalf("call beyond the limit: %+v", r)
	}
	if got := e.run1(w.ID); got.Desired != "running" || got.Observed != "running" {
		t.Fatalf("tool-call limit stopped the run: %+v", got)
	}

	// Lowering with drain (default) stops nothing.
	cur, _ := e.c.Limits.Effective(ctx, sp.ID)
	if _, err := e.c.Limits.Set(ctx, admin, sp.ID, cur.Version, limits.Input{Values: map[string]int64{limits.ConcurrentRuns: 1}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	for _, r := range workers {
		if got := e.run1(r.ID); got.Desired != "running" || got.Observed != "running" {
			t.Fatalf("drain stopped %s: %+v", r.ID, got)
		}
	}
	// With on_lower=stop, lowering to 1 stops the newest of the two.
	cur, _ = e.c.Limits.Effective(ctx, sp.ID)
	if _, err := e.c.Limits.Set(ctx, admin, sp.ID, cur.Version, limits.Input{Values: map[string]int64{limits.ConcurrentRuns: 2}, OnLower: limits.Stop}); err != nil {
		t.Fatal(err)
	}
	cur, _ = e.c.Limits.Effective(ctx, sp.ID)
	if _, err := e.c.Limits.Set(ctx, admin, sp.ID, cur.Version, limits.Input{Values: map[string]int64{limits.ConcurrentRuns: 1}, OnLower: limits.Stop}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "newest worker stopped", func() bool { return e.run1(workers[1].ID).Desired == "stopped" })
	if got := e.run1(workers[0].ID); got.Desired != "running" {
		t.Fatalf("the oldest worker was stopped: %+v", got)
	}
}
