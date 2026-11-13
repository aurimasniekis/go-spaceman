package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/accounts"
	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/app"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
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

// ADR-0079 UL-4 (simulated: a scripted minor-12 node): a run's limit report is stored on the run and opens a hold that
// the API shows to the space's viewers, and, as a shared hold, to the viewers of another space bound to the same
// account; the account view needs account:read; a space the caller can't read is not found. A report from a stale
// generation stores nothing.
func TestUsageLimitReportVisibleThroughAPI(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	ctx := context.Background()
	n, sp := hvNode(t, e, "ula", proto.MinorUsageLimit, true)
	spB, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "ulb", Name: "ulb", NodeID: sp.NodeID})
	if err != nil {
		t.Fatal(err)
	}
	bs, err := e.c.Accounts.Bindings(ctx, sp.ID)
	if err != nil || len(bs) == 0 {
		t.Fatalf("bindings %v %v", bs, err)
	}
	var acc string
	for _, b := range bs {
		if b.Harness == "claude-code" {
			acc = b.AccountID
		}
	}
	if _, err := e.c.Accounts.SetBindings(ctx, admin, spB.ID, []accounts.Binding{{AccountID: acc, Default: true}}); err != nil {
		t.Fatal(err)
	}
	r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Role: "free", Kind: "worker", Harness: "claude-code", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	var gen int64
	for {
		var c proto.Command
		if err := n.next(t, proto.TypeCommand).Decode(&c); err != nil {
			t.Fatal(err)
		}
		if c.RunID == r.ID && c.Kind == proto.CmdLaunch {
			gen = c.Generation
			break
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(3 * time.Hour)
	limit := &proto.RunLimit{Kind: proto.LimitSevenDay, ResetsAt: &reset, Since: now}
	// A stale generation first: fenced, nothing stored.
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: r.ID, SpaceID: sp.ID, Generation: gen - 1, State: proto.RunRunning, Status: "idle",
		Limit: limit, ObservedAt: now})
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: r.ID, SpaceID: sp.ID, Generation: gen, State: proto.RunRunning, Status: "working", ObservedAt: now})
	eventually(t, "running", func() bool { return e.run1(r.ID).Observed == "running" })
	var holds int
	if err := e.c.Store.ReadDB().QueryRowContext(ctx, `SELECT count(*) FROM usage_limits`).Scan(&holds); err != nil || holds != 0 || e.run1(r.ID).Limit != nil {
		t.Fatalf("a stale generation's limit was stored: %d holds, %v", holds, err)
	}
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: r.ID, SpaceID: sp.ID, Generation: gen, State: proto.RunRunning, Status: "idle",
		Limit: limit, ObservedAt: now})
	eventually(t, "limit stored", func() bool { return e.run1(r.ID).Limit != nil })

	sys := audit.System
	mkToken := func(email string, grants []auth.GrantInput) string {
		t.Helper()
		u, err := e.c.Auth.Create(ctx, sys, auth.CreateInput{Email: email, DisplayName: email, Grants: grants})
		if err != nil {
			t.Fatal(err)
		}
		u, _ = e.c.Auth.Get(ctx, u.ID)
		_, secret, err := e.c.Auth.CreateAPIToken(ctx, &auth.Principal{User: u}, "ul", grants, 24*time.Hour, false)
		if err != nil {
			t.Fatal(err)
		}
		return secret
	}
	viewerA := mkToken("va@example.com", []auth.GrantInput{{Role: authz.RoleViewer, Domain: sp.ID}})
	viewerB := mkToken("vb@example.com", []auth.GrantInput{{Role: authz.RoleViewer, Domain: spB.ID}})
	adminAll := mkToken("adm@example.com", []auth.GrantInput{{Role: authz.RoleAdmin, Domain: authz.Global}})
	get := func(tok, path string, out any) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+api.Prefix+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		if out != nil && res.StatusCode == http.StatusOK {
			if err := json.NewDecoder(res.Body).Decode(out); err != nil {
				t.Fatal(err)
			}
		}
		return res.StatusCode
	}
	type list struct {
		Items []fleet.UsageLimit `json:"items"`
	}

	var a list
	if st := get(viewerA, "/spaces/"+sp.ID+"/usage-limits", &a); st != http.StatusOK || len(a.Items) != 1 {
		t.Fatalf("space A: %d %+v", st, a)
	}
	if h := a.Items[0]; h.Shared || h.SpaceID != sp.ID || h.AccountID != acc || h.Kind != proto.LimitSevenDay || h.FirstRunID != r.ID ||
		h.ResetsAt == nil || !h.ResetsAt.Equal(reset) {
		t.Fatalf("space A hold %+v", h)
	}
	var run fleet.Run
	if st := get(viewerA, "/spaces/"+sp.ID+"/runs/"+r.ID, &run); st != http.StatusOK || run.Limit == nil || run.Limit.Kind != proto.LimitSevenDay {
		t.Fatalf("run: %d %+v", st, run.Limit)
	}
	var waits struct {
		Items []fleet.LimitWait `json:"items"`
	}
	if st := get(viewerA, "/spaces/"+sp.ID+"/limit-waits", &waits); st != http.StatusOK || len(waits.Items) != 0 {
		t.Fatalf("waits: %d %+v", st, waits)
	}
	var b list
	if st := get(viewerB, "/spaces/"+spB.ID+"/usage-limits", &b); st != http.StatusOK || len(b.Items) != 1 || !b.Items[0].Shared ||
		b.Items[0].SpaceID != "" || b.Items[0].FirstRunID != "" || b.Items[0].AccountID != acc {
		t.Fatalf("space B: %d %+v", st, b)
	}
	// Authorization: another space's list is not found; the account view needs account:read.
	if st := get(viewerB, "/spaces/"+sp.ID+"/usage-limits", nil); st != http.StatusNotFound {
		t.Fatalf("viewer of B reading A's holds: %d", st)
	}
	if st := get(viewerB, "/spaces/"+sp.ID+"/limit-waits", nil); st != http.StatusNotFound {
		t.Fatalf("viewer of B reading A's waits: %d", st)
	}
	if st := get(viewerA, "/accounts/"+acc+"/usage-limits", nil); st != http.StatusForbidden {
		t.Fatalf("space viewer reading the account view: %d", st)
	}
	var all list
	if st := get(adminAll, "/accounts/"+acc+"/usage-limits", &all); st != http.StatusOK || len(all.Items) != 1 || all.Items[0].SpaceID != sp.ID {
		t.Fatalf("account view: %d %+v", st, all)
	}
}

// The usage-limit reads' authorization rules (ADR-0079): space viewers read a space's holds and waits; the account view,
// which names every space, needs account:read.
func TestUsageLimitRules(t *testing.T) {
	t.Parallel()
	for op, want := range map[string]api.Rule{
		"list-space-usage-limits":   api.InSpace(authz.RunRead),
		"list-space-limit-waits":    api.InSpace(authz.RunRead),
		"list-account-usage-limits": api.Global(authz.AccountRead),
	} {
		if got, ok := app.SpecDeps().RuleFor(op); !ok || got != want {
			t.Errorf("%s: %+v, want %+v", op, got, want)
		}
	}
	if !authz.RoleHas(authz.RoleViewer, authz.RunRead) || authz.RoleHas(authz.RoleOperator, authz.AccountRead) {
		t.Error("viewers must read runs; space operators must not read accounts")
	}
}
