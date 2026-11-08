package harborpilot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/harborpilot/hptest"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// TestRealHarborPilotConnection validates connections and space settings against a real `harbor-pilot serve --demo`
// v0.1.3 (opt-in: SPACEMAN_HARBOR_PILOT=1, make test-harbor-pilot). Checked:
//   - a parent with tokens:mint connects, and the probe learns its client binding and leaves no live credential;
//   - a parent without tokens:mint is refused as "parent token lacks tokens:mint" (Harbor Pilot's real 403);
//   - a service-principal token reads budget status and projects; a gateway token in its place is refused;
//   - a space's project is checked: an allowed project id passes, a project name is refused with its id, an unknown
//     project is refused, and without a service-principal token Harbor Pilot's own mint check refuses a project the
//     binding doesn't allow.
func TestRealHarborPilotConnection(t *testing.T) {
	g := hptest.Start(t)
	svc, _ := newSvc(t)
	ctx := context.Background()

	_, noMint, err := g.IssueCredential(ctx, "spaceman-parent-no-mint", []string{ScopeInvoke, ScopeModels})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.PutConnection(ctx, admin, 0, ConnectionInput{BaseURL: g.URL, AllowInsecureHTTP: true, ParentToken: noMint})
	fieldMsg(t, err, "parent_token", "parent token lacks tokens:mint")
	_, err = svc.PutConnection(ctx, admin, 0, ConnectionInput{BaseURL: g.URL, AllowInsecureHTTP: true, ParentToken: g.ParentToken, ServiceToken: g.ParentToken})
	fieldMsg(t, err, "service_token", "is a gateway token")

	c, err := svc.PutConnection(ctx, admin, 0, ConnectionInput{BaseURL: g.URL, AllowInsecureHTTP: true, ParentToken: g.ParentToken, ServiceToken: g.ServiceToken})
	if err != nil {
		t.Fatal(err)
	}
	if c.BindingID != g.BindingID || !c.HasServiceToken || c.ParentExpiresAt != nil {
		t.Fatalf("connection %+v (binding want %s)", c, g.BindingID)
	}

	// The probe credentials were revoked: none of the parent's children is left active.
	assertNoChildren := func() {
		t.Helper()
		if live, err := g.ActiveChildren(ctx); err != nil || len(live) != 0 {
			t.Fatalf("active children after validation: %v %v", live, err)
		}
	}
	assertNoChildren()

	sp, err := svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{Enabled: true, ProjectID: g.AltProjectID}, true)
	if err != nil || !sp.Enabled || sp.ProjectID != g.AltProjectID {
		t.Fatalf("space %+v %v", sp, err)
	}
	_, err = svc.PutSpace(ctx, admin, "spc_b", 0, SpaceInput{Enabled: true, ProjectID: "web-app"}, true)
	fieldMsg(t, err, "project_id", "use its id "+g.ProjectID)
	_, err = svc.PutSpace(ctx, admin, "spc_b", 0, SpaceInput{Enabled: true, ProjectID: "prj_doesnotexist"}, true)
	fieldMsg(t, err, "project_id", "has no project")

	// Without a service-principal token, the mint probe is the check: Harbor Pilot refuses a name (it allows ids only).
	c, err = svc.PutConnection(ctx, admin, c.Version, ConnectionInput{BaseURL: g.URL, AllowInsecureHTTP: true, RemoveServiceToken: true})
	if err != nil || c.HasServiceToken {
		t.Fatalf("remove service token %+v %v", c, err)
	}
	_, err = svc.PutSpace(ctx, admin, "spc_b", 0, SpaceInput{Enabled: true, ProjectID: "web-app"}, true)
	fieldMsg(t, err, "project_id", "refused project")

	// Deleting the connection is refused while spc_a uses it.
	if err := svc.DeleteConnection(ctx, admin, c.Version); err == nil || !strings.Contains(err.Error(), "route runs through Harbor Pilot") {
		t.Fatalf("delete while enabled: %v", err)
	}
	code(t, svc.DeleteConnection(ctx, admin, c.Version), apperr.CodePrecondition)
	assertNoChildren()
}

// TestRealHarborPilotBudgetAndUsage checks HP-T5 against a real `harbor-pilot serve --demo` v0.1.3 (opt-in:
// SPACEMAN_HARBOR_PILOT=1, make test-harbor-pilot):
//   - a run's token (minted with check_budget) sends demo traffic on web-app, and the run-end read-back
//     (usage/summary?ref=spaceman.run:<run>&binding_id=…) fills the run's usage with source harbor-pilot;
//   - a tiny blocking budget on web-app is then used up by that project's traffic: the space's status shows it
//     exceeded with its spend, the project's usage over all clients is shown, PreLaunch refuses naming the budget, the
//     spend and the reset time, and the dispatch mint (check_budget) is refused by Harbor Pilot itself.
//
// With SPACEMAN_HP_FIXTURE_OUT set, the budget status body is written there (testdata/budget_status_demo.json).
func TestRealHarborPilotBudgetAndUsage(t *testing.T) {
	g := hptest.Start(t)
	svc, s := newSvc(t)
	ctx := context.Background()
	if _, err := svc.PutConnection(ctx, admin, 0, ConnectionInput{BaseURL: g.URL, AllowInsecureHTTP: true, ParentToken: g.ParentToken, ServiceToken: g.ServiceToken}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{Enabled: true, ProjectID: g.ProjectID}, true); err != nil {
		t.Fatal(err)
	}
	budgets := newBudgets(svc)
	l := NewLifecycle(svc, LifecycleOptions{Budget: budgets, MintRetry: []time.Duration{0, 0, 0}})
	if err := l.PreLaunch(ctx, "spc_a", "codex"); err != nil {
		t.Fatalf("pre-launch before any budget is used up: %v", err)
	}
	run := realRun(t, s, "run_01hpt5usage00000000000000", g.ProjectID)
	sec, err := l.Token(ctx, run, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		code, _, err := g.Do(ctx, http.MethodPost, "/v1/messages", sec[proto.SecretGatewayToken], map[string]any{
			"model": "coding/default", "max_tokens": 16, "messages": []map[string]any{{"role": "user", "content": "hello"}}})
		if err != nil || code != http.StatusOK {
			t.Fatalf("messages: %d %v", code, err)
		}
	}
	// The run ends: its end path queues the read-back (as fleet does); reads are made due at once here.
	exec(t, s, `INSERT INTO run_gateway_usage (run_id, space_id, ended_at, next_attempt_at, updated_at) VALUES (?, 'spc_a', ?, 0, 0)`, run.ID, time.Now().UnixMicro())
	deadline := time.Now().Add(20 * time.Second)
	var gu db.RunGatewayUsage
	for {
		exec(t, s, `UPDATE run_gateway_usage SET next_attempt_at = 0, state = 'pending', reads = 0 WHERE run_id = ?`, run.ID)
		if _, err := l.ReadDueUsage(ctx); err != nil {
			t.Fatal(err)
		}
		if gu, err = s.R().GetRunGatewayUsage(ctx, run.ID); err != nil {
			t.Fatal(err)
		}
		if gu.Attempts >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if gu.Attempts != 2 || gu.LastError != "" || !gu.ReadAt.Valid {
		t.Fatalf("read-back %+v", gu)
	}
	ru, err := s.R().GetRunUsage(ctx, run.ID)
	if err != nil || ru.Source != proto.UsageHarborPilot || !strings.Contains(ru.ModelsJson, `"requests":2`) {
		t.Fatalf("run usage %+v %v", ru, err)
	}

	// Use up a tiny blocking budget on web-app: the demo's seeded traffic and the two requests above spent more.
	const name = "spaceman-hpt5-tiny"
	if _, err := g.CreateBudget(ctx, name, g.ProjectID, "0.000001"); err != nil {
		t.Fatal(err)
	}
	if out := os.Getenv("SPACEMAN_HP_FIXTURE_OUT"); out != "" {
		code, body, err := g.Do(ctx, http.MethodGet, "/api/v1/budgets/status", g.ServiceToken, nil)
		if err != nil || code != http.StatusOK {
			t.Fatalf("budget status: %d %v", code, err)
		}
		if err := os.WriteFile(out, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	budgets = newBudgets(svc)
	st, err := budgets.SpaceStatus(ctx, "spc_a")
	if err != nil || st.State != StatusExceeded || st.Usage == nil || st.Usage.Clients != "all" || st.Usage.Requests < 2 {
		t.Fatalf("status %+v %v", st, err)
	}
	var tiny *Covering
	for i := range st.Budgets {
		if st.Budgets[i].Name == name {
			tiny = &st.Budgets[i]
		}
		if st.Budgets[i].Scope == ScopeProject && st.Budgets[i].ScopeID != g.ProjectID {
			t.Fatalf("another project's budget covers the space: %+v", st.Budgets[i])
		}
	}
	if tiny == nil || !tiny.Blocking || tiny.Limit != "0.000001" || tiny.Spent == "0.00" || tiny.PeriodEnd.IsZero() {
		t.Fatalf("tiny budget %+v in %+v", tiny, st.Budgets)
	}
	l = NewLifecycle(svc, LifecycleOptions{Budget: budgets, MintRetry: []time.Duration{0, 0, 0}})
	ae := appErr(t, l.PreLaunch(ctx, "spc_a", "claude-code"), apperr.CodeRateLimited)
	for _, want := range []string{`"` + name + `"`, tiny.Spent + " of 0.000001 USD", "resets at " + tiny.PeriodEnd.Format(time.RFC3339)} {
		if !strings.Contains(ae.Message, want) {
			t.Fatalf("refusal %q lacks %q", ae.Message, want)
		}
	}
	// Without the pre-check, Harbor Pilot's own check at the dispatch mint refuses the run.
	l = NewLifecycle(svc, LifecycleOptions{MintRetry: []time.Duration{0, 0, 0}})
	_, err = l.Token(ctx, realRun(t, s, "run_01hpt5refused000000000000", g.ProjectID), "s")
	if !errors.Is(err, fleet.ErrGatewayUnavailable) || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("dispatch mint over a used-up budget: %v", err)
	}
}

// realRun inserts a running gateway run of spc_a charged to project.
func realRun(t *testing.T, s *store.Store, id, project string) *fleet.Run {
	t.Helper()
	spec := fleet.LaunchSpec{Gateway: &fleet.GatewaySpec{Kind: fleet.GatewayKindHarborPilot, ProjectID: project}}
	b, _ := json.Marshal(spec)
	exec(t, s, `INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at, created_by, updated_at)
		VALUES (?, 'spc_a', 'nod_1', 1, 'worker', 'implementor', 'codex', 'ins_'||?, 'running', 'pending', ?, ?, 't', 0)`, id, id, string(b), time.Now().UnixMicro())
	return &fleet.Run{ID: id, SpaceID: "spc_a", NodeID: "nod_1", Harness: "codex", InstanceID: "ins_" + id, Launch: spec}
}
