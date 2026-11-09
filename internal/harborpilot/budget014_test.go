package harborpilot

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/harborpilot/hpfake"
)

// onceJSON builds a Harbor Pilot 0.1.4 budget status item: periodEnd "" is null (a one-time budget without an end).
func onceJSON(id, name, scope, scopeID, period, periodStart, periodEnd, state string, enabled bool, limit, spent int64) string {
	m := map[string]any{"budget_id": id, "name": name, "scope": scope, "scope_id": scopeID, "period": period,
		"period_start": periodStart, "period_end": nil, "currency": "USD", "enforcement": EnforceBlock, "enabled": enabled,
		"limit_e10": limit, "spent_e10": spent, "reserved_e10": 0, "remaining_e10": max(limit-spent, 0), "fraction": float64(spent) / float64(limit), "state": state}
	if periodEnd != "" {
		m["period_end"] = periodEnd
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// The v0.1.4 fixture (testdata/budget_status_v014.json) is GET /api/v1/budgets/status?project_id=…&binding_id=… from a
// real `harbor-pilot serve --demo` v0.1.4 (TestRealHarborPilotOneTimeBudgets, SPACEMAN_HP_FIXTURE_OUT_014): web-app's
// budgets with one-time budgets that are scheduled, expired, open-ended and used up, and open-ended within limits
// (the demo's trial-credit), a disabled used-up daily one, and the workspace's monthly one. Every item parses, null
// period_end stays absent, and only the used-up open-ended one blocks.
func TestCoveringBudgetsOnV014Fixture(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("testdata/budget_status_v014.json")
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Items []Budget `json:"items"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	project := ""
	for _, b := range st.Items {
		if b.Scope == ScopeProject {
			project = b.ScopeID
		}
	}
	cov := coveringFiltered(st.Items, project, "cbn_spaceman")
	if len(cov) != len(st.Items) {
		t.Fatalf("covering %d of %d", len(cov), len(st.Items))
	}
	by := map[string]Covering{}
	for _, c := range cov {
		by[c.Name] = c
	}
	want := map[string]struct {
		state    string
		blocking bool
		end      bool
	}{
		"spaceman-once-open":      {BudgetExceeded, true, false},
		"spaceman-once-scheduled": {BudgetScheduled, false, false},
		"spaceman-once-expired":   {BudgetExpired, false, true},
		"spaceman-daily-disabled": {BudgetDisabled, false, true},
		"trial-credit":            {BudgetOK, false, false},
		"acme-monthly":            {BudgetOK, false, true},
	}
	for name, w := range want {
		c, ok := by[name]
		if !ok || c.State != w.state || c.Blocking != w.blocking || (c.PeriodEnd != nil) != w.end {
			t.Fatalf("%s: %+v, want %+v", name, c, w)
		}
	}
	if cov[0].Name != "spaceman-once-open" || by["trial-credit"].Remaining == "0.00" || by["spaceman-once-open"].Remaining != "0.00" {
		t.Fatalf("order or remaining: %+v", cov)
	}
	r := Refusal(cov[0])
	if !strings.Contains(r, `one-time budget "spaceman-once-open"`) || !strings.Contains(r, "never resets") || strings.Contains(r, "0001-") {
		t.Fatalf("refusal %q", r)
	}

	var sys System
	if raw, err = os.ReadFile("testdata/system_v014.json"); err != nil || json.Unmarshal(raw, &sys) != nil || sys.Version != "0.1.4" || sys.SchemaVersion == 0 {
		t.Fatalf("system fixture %+v %v", sys, err)
	}
}

// 0.1.4-shaped status over the 0.1.3 path (every budget, client-side selection): scheduled, expired, disabled and
// unknown states with null period_end never refuse a launch under either on_budget_status_unavailable policy, and
// they leave the space's overall state ok.
func TestInactiveBudgetsNeverRefuse(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"", "0.1.4"} {
		svc, _ := newSvc(t)
		f := hpfake.New(t)
		gatewaySpace(t, svc, f, SpaceInput{OnBudgetStatusUnavailable: BudgetRefuse})
		ctx := context.Background()
		f.Set(func(h *hpfake.HP) {
			h.Version = version
			h.Budgets = items(
				onceJSON("bdg_s", "later", ScopeProject, "prj_web", PeriodOnce, "2027-01-01T00:00:00Z", "", BudgetScheduled, true, 1e10, 0),
				onceJSON("bdg_e", "ended", ScopeGlobal, "", PeriodOnce, "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z", BudgetExpired, true, 1e10, 9e10),
				onceJSON("bdg_d", "off", ScopeProject, "prj_web", "day", "2026-11-08T00:00:00Z", "2026-11-09T00:00:00Z", BudgetDisabled, false, 1e10, 9e10),
				onceJSON("bdg_u", "future", ScopeProject, "prj_web", PeriodOnce, "2026-01-01T00:00:00Z", "", "paused", true, 1e10, 9e10))
		})
		l := NewLifecycle(svc, LifecycleOptions{Budget: newBudgets(svc)})
		if err := l.PreLaunch(ctx, "spc_a", "codex"); err != nil {
			t.Fatalf("%q: an inactive budget refused the launch: %v", version, err)
		}
		st, err := newBudgets(svc).SpaceStatus(ctx, "spc_a")
		if err != nil || st.State != StatusOK || len(st.Budgets) != 4 {
			t.Fatalf("%q: status %+v %v", version, st, err)
		}
		for _, c := range st.Budgets {
			if c.Blocking || (c.Name == "future" && c.State != "unknown") || (c.Name == "later" && c.PeriodEnd != nil) {
				t.Fatalf("%q: %+v", version, c)
			}
		}
		// The scheduled budget's future period start is not the usage window.
		if st.Usage == nil || st.Usage.Window != "last_30_days" {
			t.Fatalf("%q: usage window %+v", version, st.Usage)
		}
	}
}

// On a 0.1.4 gateway, budget status is read filtered by the space's project and the connection's binding; Harbor
// Pilot resolves the workspace, so projects are never looked up, and the version is read once (cached). On a 0.1.3
// gateway (GET /api/v1/system is 404) every budget is read and the workspace looked up, as before.
func TestBudgetLookupByGatewayVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ws := items(budgetJSON("bdg_ws", "team", ScopeWorkspace, "wsp_9", EnforceBlock, BudgetExceeded, true, 1e10, 1e10),
		budgetJSON("bdg_o", "other", ScopeProject, "prj_data", EnforceBlock, BudgetExceeded, true, 1e10, 1e10),
		budgetJSON("bdg_wo", "other-team", ScopeWorkspace, "wsp_other", EnforceBlock, BudgetExceeded, true, 1e10, 1e10))
	for _, c := range []struct {
		version, query, source string
		projectReads           int
	}{
		{"0.1.4", "binding_id=bnd_fake&project_id=prj_web", "system", 0},
		{"", "", "legacy", 1},
	} {
		svc, _ := newSvc(t)
		f := hpfake.New(t)
		gatewaySpace(t, svc, f, SpaceInput{})
		var reads0 int
		f.Set(func(h *hpfake.HP) {
			h.Version, h.Workspace, h.Budgets = c.version, "wsp_9", ws
			h.BudgetQueries, reads0 = nil, h.ProjectReads
		})
		b := newBudgets(svc)
		for range 2 {
			ae := appErr(t, b.CheckBudget(ctx, "spc_a", &SpaceSettings{ProjectID: "prj_web"}), apperr.CodeRateLimited)
			if !strings.Contains(ae.Message, `"team"`) {
				t.Fatalf("%q: %s", c.version, ae.Message)
			}
		}
		st, err := b.SpaceStatus(ctx, "spc_a")
		if err != nil || len(st.Budgets) != 1 || st.Budgets[0].BudgetID != "bdg_ws" {
			t.Fatalf("%q: status %+v %v", c.version, st, err)
		}
		gi, err := b.Gateway(ctx)
		if err != nil || gi.Source != c.source || gi.ProjectFilter != (c.version != "") || gi.MinVersion != MinVersion || gi.Recommended != RecommendedVersion {
			t.Fatalf("%q: gateway %+v %v", c.version, gi, err)
		}
		f.Set(func(h *hpfake.HP) {
			if len(h.BudgetQueries) != 1 || h.BudgetQueries[0] != c.query || h.ProjectReads-reads0 != c.projectReads {
				t.Fatalf("%q: queries %q, project reads %d", c.version, h.BudgetQueries, h.ProjectReads-reads0)
			}
			if c.version != "" && (h.SystemReads != 1 || gi.Version != "0.1.4" || gi.SchemaVersion == 0 || gi.StartedAt == nil) {
				t.Fatalf("system reads %d, %+v", h.SystemReads, gi)
			}
		})
	}
}

// The version can't be read: without a service-principal token (unknown, with the reason), or with Harbor Pilot
// refusing the token. Budget status then takes the 0.1.3 path, which fails or works on its own.
func TestGatewayUnknown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, _ := newSvc(t)
	gi, err := newBudgets(svc).Gateway(ctx)
	if err != nil || gi.Source != "unknown" || !strings.Contains(gi.Unavailable, "no Harbor Pilot connection") {
		t.Fatalf("no connection: %+v %v", gi, err)
	}
	f := hpfake.New(t)
	gatewaySpace(t, svc, f, SpaceInput{})
	f.Set(func(h *hpfake.HP) { h.Version = "0.1.4"; h.ServicePerms = []string{"usage.read"} })
	b := newBudgets(svc)
	gi, err = b.Gateway(ctx)
	if err != nil || gi.Source != "unknown" || gi.ProjectFilter || !strings.Contains(gi.Unavailable, "system information") {
		t.Fatalf("no config.read: %+v %v", gi, err)
	}
	if err := b.CheckBudget(ctx, "spc_a", &SpaceSettings{ProjectID: "prj_web"}); err != nil {
		t.Fatal(err)
	}
	f.Set(func(h *hpfake.HP) {
		if len(h.BudgetQueries) == 0 || h.BudgetQueries[len(h.BudgetQueries)-1] != "" {
			t.Fatalf("queries %q", h.BudgetQueries)
		}
	})
}

// Refusal texts per period: a one-time budget with an end names its end, a recurring one without an end time doesn't
// print a zero time.
func TestRefusalPeriods(t *testing.T) {
	t.Parallel()
	end := time.Date(2027, 3, 31, 0, 0, 0, 0, time.UTC)
	c := Covering{Name: "q1", Scope: ScopeGlobal, Period: PeriodOnce, PeriodEnd: &end, Spent: "2.00", Limit: "1.00", Currency: "USD"}
	if r := Refusal(c); !strings.Contains(r, "(global)") || !strings.Contains(r, "window ends at 2027-03-31T00:00:00Z") || strings.Contains(r, "resets") {
		t.Fatalf("once with end: %q", r)
	}
	c = Covering{Name: "d", Scope: ScopeProject, ScopeID: "prj_web", Period: "day", Spent: "2.00", Limit: "1.00", Currency: "USD"}
	if r := Refusal(c); !strings.Contains(r, "until it resets") || strings.Contains(r, "0001-") {
		t.Fatalf("recurring without end: %q", r)
	}
}

// The gateway at the connection's address is downgraded to 0.1.3 while its 0.1.4 version is still cached: the
// filtered read then answers every budget (0.1.3 ignores the filters). Another project's used-up budget in that answer
// must not refuse this space's launch: the answer is taken as unfiltered, the cached version dropped, and every budget
// is evaluated client-side. The next read asks GET /api/v1/system again and stays on the old path.
func TestFilteredAnswerIgnoredAfterDowngrade(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, _ := newSvc(t)
	f := hpfake.New(t)
	gatewaySpace(t, svc, f, SpaceInput{OnBudgetStatusUnavailable: BudgetRefuse})
	f.Set(func(h *hpfake.HP) {
		h.Version, h.BudgetQueries = "0.1.4", nil
		h.Budgets = items(budgetJSON("bdg_w", "web", ScopeProject, "prj_web", EnforceBlock, BudgetOK, true, 1e10, 1),
			budgetJSON("bdg_o", "other", ScopeProject, "prj_data", EnforceBlock, BudgetExceeded, true, 1e10, 1e10))
	})
	b := newBudgets(svc)
	set := &SpaceSettings{ProjectID: "prj_web", OnBudgetStatusUnavailable: BudgetRefuse}
	if gi, err := b.Gateway(ctx); err != nil || !gi.ProjectFilter { // cached as 0.1.4
		t.Fatalf("gateway %+v %v", gi, err)
	}
	f.Set(func(h *hpfake.HP) { h.Version = "" }) // downgraded: 0.1.3 at the same address
	for range 2 {
		if err := b.CheckBudget(ctx, "spc_a", set); err != nil {
			t.Fatalf("another project's budget refused the launch: %v", err)
		}
		b.mu.Lock()
		b.status = nil
		b.mu.Unlock()
	}
	st, err := b.SpaceStatus(ctx, "spc_a")
	if err != nil || st.State != StatusOK || len(st.Budgets) != 1 || st.Budgets[0].BudgetID != "bdg_w" {
		t.Fatalf("status %+v %v", st, err)
	}
	gi, err := b.Gateway(ctx)
	if err != nil || gi.Source != "legacy" || gi.ProjectFilter {
		t.Fatalf("gateway after the downgrade %+v %v", gi, err)
	}
	f.Set(func(h *hpfake.HP) {
		// The filtered read once, then the old path only.
		want := []string{"binding_id=bnd_fake&project_id=prj_web", "", "", ""}
		if !slices.Equal(h.BudgetQueries, want) {
			t.Fatalf("queries %q", h.BudgetQueries)
		}
	})

	rem := int64(1)
	for _, c := range []struct {
		name string
		bu   []Budget
		want bool
	}{
		{"filtered", []Budget{{Scope: ScopeGlobal, RemainingE10: &rem}, {Scope: ScopeWorkspace, ScopeID: "wsp_1", RemainingE10: &rem},
			{Scope: ScopeProject, ScopeID: "prj_web", RemainingE10: &rem}, {Scope: ScopeBinding, ScopeID: "bnd_fake", RemainingE10: &rem}}, false},
		{"empty", nil, false},
		{"other project", []Budget{{Scope: ScopeProject, ScopeID: "prj_data", RemainingE10: &rem}}, true},
		{"other binding", []Budget{{Scope: ScopeBinding, ScopeID: "bnd_other", RemainingE10: &rem}}, true},
		{"two workspaces", []Budget{{Scope: ScopeWorkspace, ScopeID: "wsp_1", RemainingE10: &rem}, {Scope: ScopeWorkspace, ScopeID: "wsp_2", RemainingE10: &rem}}, true},
		{"account", []Budget{{Scope: "account", ScopeID: "acc_1", RemainingE10: &rem}}, true},
		{"0.1.3 item", []Budget{{Scope: ScopeWorkspace, ScopeID: "wsp_other"}}, true},
	} {
		if got := unfiltered(c.bu, "prj_web", "bnd_fake"); got != c.want {
			t.Fatalf("%s: unfiltered %v", c.name, got)
		}
	}
}

// A filtered read answering 404 (the space's project, or the binding, was deleted in Harbor Pilot) falls back to the
// old path, which still refuses on a used-up global budget enforced with block, even under proceed.
func TestFilteredNotFoundFallsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, _ := newSvc(t)
	f := hpfake.New(t)
	gatewaySpace(t, svc, f, SpaceInput{})
	f.Set(func(h *hpfake.HP) {
		h.Version, h.BudgetQueries = "0.1.4", nil
		h.Budgets = items(budgetJSON("bdg_g", "everyone", ScopeGlobal, "", EnforceBlock, BudgetExceeded, true, 1e10, 1e10))
		delete(h.Projects, "prj_web")
	})
	ae := appErr(t, newBudgets(svc).CheckBudget(ctx, "spc_a", &SpaceSettings{ProjectID: "prj_web", OnBudgetStatusUnavailable: BudgetProceed}), apperr.CodeRateLimited)
	if !strings.Contains(ae.Message, `"everyone"`) {
		t.Fatal(ae.Message)
	}
	f.Set(func(h *hpfake.HP) {
		if !slices.Equal(h.BudgetQueries, []string{"binding_id=bnd_fake&project_id=prj_web", ""}) {
			t.Fatalf("queries %q", h.BudgetQueries)
		}
	})
}

// A hung gateway delays a launch check by CheckTimeout in all, not by the client timeout once per read; the status is
// then unavailable and the space's policy decides.
func TestCheckBudgetBoundedByOneDeadline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, _ := newSvc(t)
	f := hpfake.New(t)
	gatewaySpace(t, svc, f, SpaceInput{})
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	f.Set(func(h *hpfake.HP) {
		h.Version = "0.1.4"
		h.Hook = func(r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/v1/budgets/") || r.URL.Path == "/api/v1/system" {
				<-hang
			}
		}
	})
	b := NewBudgets(svc, BudgetsOptions{CheckTimeout: 200 * time.Millisecond})
	start := time.Now()
	ae := appErr(t, b.CheckBudget(ctx, "spc_a", &SpaceSettings{ProjectID: "prj_web", OnBudgetStatusUnavailable: BudgetRefuse}), apperr.CodeUnavailable)
	if !strings.Contains(ae.Message, "did not answer within 200ms") {
		t.Fatal(ae.Message)
	}
	if err := b.CheckBudget(ctx, "spc_a", &SpaceSettings{ProjectID: "prj_web", OnBudgetStatusUnavailable: BudgetProceed}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("two checks took %s", d)
	}
}
