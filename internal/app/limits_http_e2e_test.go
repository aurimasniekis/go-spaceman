package app_test

import (
	"context"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/limits"
	"github.com/aurimasniekis/go-spaceman/internal/schedule"
)

// SM-LIMIT-001/003 over HTTP: definitions, the effective limits read with sources, versioned overrides (If-Match once
// stored), bounds, installation defaults from the configuration, and schedule management with a timing preview;
// viewers read, only space configuration writers change.
func TestLimitsAndSchedulesOverHTTP(t *testing.T) {
	t.Parallel()
	h := newHarnessWith(t, func(c *config.Config) { c.Limits.MaxConcurrentRuns = 5; c.Limits.BudgetTokens24h = 1_000_000 })
	ctx := context.Background()
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	en, _ := h.c.Fleet.CreateNode(ctx, admin, fleet.CreateNodeInput{Name: "node-a"})
	sp, err := h.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "alpha", Name: "Alpha", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	var defs struct{ Items []limits.Def }
	root.do("GET", "/limits/definitions", nil).json(t, &defs)
	if len(defs.Items) != len(limits.Defs) {
		t.Fatalf("definitions %d", len(defs.Items))
	}
	var eff limits.Effective
	r := root.do("GET", "/spaces/"+sp.ID+"/limits", nil)
	r.json(t, &eff)
	byKey := func() map[string]limits.Limit {
		m := map[string]limits.Limit{}
		for _, l := range eff.Limits {
			m[l.Key] = l
		}
		return m
	}
	if l := byKey()[limits.ConcurrentRuns]; l.Value != 5 || l.Source != "installation" || r.header.Get("ETag") != `"0"` {
		t.Fatalf("installation default %+v etag %s", l, r.header.Get("ETag"))
	}
	if eff.Budget.TokensLimit != 1_000_000 || eff.Budget.State != limits.BudgetOK || eff.Budget.Note == "" {
		t.Fatalf("budget %+v", eff.Budget)
	}
	body := map[string]any{"values": map[string]int64{limits.ConcurrentRuns: 2, limits.IngestPerMinute: 60}, "on_lower": "drain"}
	r = root.do("PUT", "/spaces/"+sp.ID+"/limits", body)
	expect(t, r, 200)
	r.json(t, &eff)
	if l := byKey()[limits.ConcurrentRuns]; l.Value != 2 || l.Source != "space" || eff.Version != 1 || r.header.Get("ETag") != `"1"` {
		t.Fatalf("override %+v", l)
	}
	expect(t, root.do("PUT", "/spaces/"+sp.ID+"/limits", body), 409) // stored: If-Match is needed
	expect(t, root.do("PUT", "/spaces/"+sp.ID+"/limits", body, "If-Match", `"7"`), 409)
	expect(t, root.do("PUT", "/spaces/"+sp.ID+"/limits", map[string]any{"values": map[string]int64{limits.ConcurrentRuns: 999}}, "If-Match", `"1"`), 422)
	expect(t, root.do("PUT", "/spaces/"+sp.ID+"/limits", map[string]any{"values": map[string]int64{limits.TopLevelTasks: 2}}, "If-Match", `"1"`), 422)
	expect(t, root.do("PUT", "/spaces/"+sp.ID+"/limits", map[string]any{"values": map[string]int64{}, "on_lower": "stop"}, "If-Match", `"1"`), 200)

	// Viewers read limits and schedules but change neither.
	expect(t, root.do("POST", "/users", map[string]any{"email": "vic@example.com", "display_name": "Vic", "password": "correct horse battery staple",
		"grants": []map[string]string{{"role": "viewer", "domain": sp.ID}}}), 201)
	viewer := h.client()
	viewer.bearer = loginAndIssueToken(t, h, "vic@example.com", []map[string]string{{"role": "viewer", "domain": sp.ID}})
	expect(t, viewer.do("GET", "/spaces/"+sp.ID+"/limits", nil), 200)
	expect(t, viewer.do("PUT", "/spaces/"+sp.ID+"/limits", body, "If-Match", `"2"`), 403)

	sched := map[string]any{"name": "weekly review", "kind": "cron", "cron": "0 9 * * 1", "timezone": "Europe/Vilnius", "missed": "run_once",
		"target": "inbox", "title": "Weekly review", "body": "Look at the queue."}
	var pv struct{ Occurrences []string }
	root.do("POST", "/spaces/"+sp.ID+"/schedules/preview", sched).json(t, &pv)
	if len(pv.Occurrences) != 5 {
		t.Fatalf("preview %+v", pv)
	}
	expect(t, root.do("POST", "/spaces/"+sp.ID+"/schedules/preview", map[string]any{"kind": "cron", "cron": "bad", "timezone": "UTC"}), 422)
	expect(t, viewer.do("POST", "/spaces/"+sp.ID+"/schedules", sched), 403)
	r = root.do("POST", "/spaces/"+sp.ID+"/schedules", sched)
	expect(t, r, 201)
	var s schedule.Schedule
	r.json(t, &s)
	if s.NextAt == nil || s.Timezone != "Europe/Vilnius" || s.Missed != "run_once" || len(s.Upcoming) != 3 {
		t.Fatalf("schedule %+v", s)
	}
	var list struct{ Items []schedule.Schedule }
	viewer.do("GET", "/spaces/"+sp.ID+"/schedules", nil).json(t, &list)
	if len(list.Items) != 1 {
		t.Fatalf("list %+v", list)
	}
	var detail api.ScheduleDetail
	viewer.do("GET", "/spaces/"+sp.ID+"/schedules/"+s.ID, nil).json(t, &detail)
	if detail.Schedule == nil || detail.ID != s.ID || detail.Occurrences == nil {
		t.Fatalf("detail %+v", detail)
	}
	sched["kind"], sched["interval_seconds"], sched["cron"] = "interval", 3600, ""
	expect(t, root.do("PUT", "/spaces/"+sp.ID+"/schedules/"+s.ID, sched), 428)
	r = root.do("PUT", "/spaces/"+sp.ID+"/schedules/"+s.ID, sched, "If-Match", `"1"`)
	expect(t, r, 200)
	r.json(t, &s)
	if s.Kind != "interval" || s.Version != 2 {
		t.Fatalf("updated %+v", s)
	}
	expect(t, root.do("DELETE", "/spaces/"+sp.ID+"/schedules/"+s.ID, nil), 204)
	expect(t, root.do("GET", "/spaces/"+sp.ID+"/schedules/"+s.ID, nil), 404)
}

// ADR-0107 over HTTP: worktree_retention_days is defined as a days limit, its installation default comes from the
// configuration, and a space override round-trips with its bounds checked.
func TestWorktreeRetentionLimitOverHTTP(t *testing.T) {
	t.Parallel()
	h := newHarnessWith(t, func(c *config.Config) { c.Limits.WorktreeRetentionDays = 30 })
	ctx := context.Background()
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	en, _ := h.c.Fleet.CreateNode(ctx, admin, fleet.CreateNodeInput{Name: "node-w"})
	sp, err := h.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "worktrees", Name: "Worktrees", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	var defs struct{ Items []limits.Def }
	root.do("GET", "/limits/definitions", nil).json(t, &defs)
	found := false
	for _, d := range defs.Items {
		if d.Key == limits.WorktreeRetention {
			found = d.Unit == "days" && d.ZeroNone && d.Min == 0 && d.Max == 36_500
		}
	}
	if !found {
		t.Fatalf("definition of %s missing or wrong: %+v", limits.WorktreeRetention, defs.Items)
	}
	var eff limits.Effective
	root.do("GET", "/spaces/"+sp.ID+"/limits", nil).json(t, &eff)
	var got limits.Limit
	for _, l := range eff.Limits {
		if l.Key == limits.WorktreeRetention {
			got = l
		}
	}
	if got.Value != 30 || got.Default != 30 || got.Source != "installation" {
		t.Fatalf("installation default %+v", got)
	}
	expect(t, root.do("PUT", "/spaces/"+sp.ID+"/limits", map[string]any{"values": map[string]int64{limits.WorktreeRetention: 36_501}}), 422)
	r := root.do("PUT", "/spaces/"+sp.ID+"/limits", map[string]any{"values": map[string]int64{limits.WorktreeRetention: 14}})
	expect(t, r, 200)
	r.json(t, &eff)
	got = limits.Limit{}
	for _, l := range eff.Limits {
		if l.Key == limits.WorktreeRetention {
			got = l
		}
	}
	if got.Value != 14 || got.Source != "space" || got.Override == nil || *got.Override != 14 {
		t.Fatalf("override %+v", got)
	}
}

// ADR-0107 decision 4: worktree_retention_days is changed like the retention limits, with retention:write. An operator
// (space_config:write) is refused; an admin of the space is allowed; the operator still changes other limits.
func TestWorktreeRetentionLimitNeedsRetentionWrite(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	en, _ := h.c.Fleet.CreateNode(ctx, admin, fleet.CreateNodeInput{Name: "node-p"})
	sp, err := h.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "wt-perm", Name: "Worktree permission", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	pw := "correct horse battery staple"
	expect(t, root.do("POST", "/users", map[string]any{"email": "ops-wt@example.com", "display_name": "Ops", "password": pw,
		"grants": []map[string]string{{"role": "operator", "domain": sp.ID}}}), 201)
	expect(t, root.do("POST", "/users", map[string]any{"email": "adm-wt@example.com", "display_name": "Adm", "password": pw,
		"grants": []map[string]string{{"role": "admin", "domain": sp.ID}}}), 201)
	ops := h.client()
	ops.bearer = loginAndIssueToken(t, h, "ops-wt@example.com", []map[string]string{{"role": "operator", "domain": sp.ID}})
	adm := h.client()
	adm.bearer = loginAndIssueToken(t, h, "adm-wt@example.com", []map[string]string{{"role": "admin", "domain": sp.ID}})
	expect(t, ops.do("PUT", "/spaces/"+sp.ID+"/limits", map[string]any{"values": map[string]int64{limits.WorktreeRetention: 14}}), 403)
	expect(t, ops.do("PUT", "/spaces/"+sp.ID+"/limits", map[string]any{"values": map[string]int64{limits.ConcurrentRuns: 3}}), 200)
	expect(t, adm.do("PUT", "/spaces/"+sp.ID+"/limits", map[string]any{"values": map[string]int64{limits.ConcurrentRuns: 3, limits.WorktreeRetention: 14}},
		"If-Match", `"1"`), 200)
}
