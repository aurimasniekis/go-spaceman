package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/harborpilot"
	"github.com/aurimasniekis/go-spaceman/internal/harborpilot/hpfake"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// hpBudget is one budget status item for the fake.
func hpBudget(name, scope, scopeID, enforcement, state string, limit, spent int64) string {
	b, _ := json.Marshal(map[string]any{"budget_id": "bdg_" + name, "name": name, "scope": scope, "scope_id": scopeID, "period": "day",
		"period_start": "2026-11-08T00:00:00Z", "period_end": "2026-11-09T00:00:00Z", "currency": "USD", "enforcement": enforcement,
		"enabled": true, "limit_e10": limit, "spent_e10": spent, "reserved_e10": 0, "fraction": float64(spent) / float64(limit), "state": state})
	return "[" + string(b) + "]"
}

// SM-GW-006 (simulated: hpfake): a used-up blocking budget covering the space refuses a launch of a routed harness at
// once, naming the budget, its spend and its reset time, before anything is minted; a used-up warn-only budget doesn't.
func TestHarborPilotBudgetRefusesLaunch(t *testing.T) {
	t.Parallel()
	f := hpfake.New(t)
	h := newHPRunEnv(t, f.URL(), f.Parent, f.Service, "prj_web")
	ctx := context.Background()
	mints := func() int { n := 0; f.Set(func(x *hpfake.HP) { n = len(x.Bodies) }); return n }
	f.Set(func(x *hpfake.HP) {
		x.Budgets = hpBudget("soft", harborpilot.ScopeProject, "prj_web", "warn", "exceeded", 1e9, 2e9)
	})
	r, err := h.e.c.Fleet.LaunchRun(ctx, admin, h.sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: "codex", Prompt: "go"})
	if err != nil {
		t.Fatalf("a warn-only budget refused the launch: %v", err)
	}
	h.launchCmd(t, r.ID)

	// Budget status is reused for 30 s; the test drops the cached read instead of waiting.
	f.Set(func(x *hpfake.HP) {
		x.Budgets = hpBudget("daily", harborpilot.ScopeProject, "prj_web", "block", "exceeded", 1e9, 12e8)
	})
	h.e.c.HarborPilotBudgets.Invalidate()
	base := mints()
	_, err = h.e.c.Fleet.LaunchRun(ctx, admin, h.sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: "claude-code", Prompt: "go"})
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.CodeRateLimited {
		t.Fatalf("launch over a used-up budget: %v", err)
	}
	for _, want := range []string{`Harbor Pilot budget "daily" (project prj_web)`, "0.12 of 0.10 USD spent", "resets at 2026-11-09T00:00:00Z"} {
		if !strings.Contains(ae.Message, want) {
			t.Fatalf("refusal %q lacks %q", ae.Message, want)
		}
	}
	if mints() != base {
		t.Fatal("a refused launch minted a token")
	}
}

// SM-GW-006, SM-CTRL-002: when control's auto-start is refused because a blocking Harbor Pilot budget is used up, the
// space gets "Control could not start" with that reason.
func TestHarborPilotBudgetRefusalRaisesControlCouldNotStart(t *testing.T) {
	t.Parallel()
	w := newWakeEnv(t, 6)
	f := hpfake.New(t)
	f.Set(func(x *hpfake.HP) {
		x.Budgets = hpBudget("daily", harborpilot.ScopeProject, "prj_web", "block", "exceeded", 1e9, 12e8)
	})
	if _, err := w.c.HarborPilot.PutConnection(w.ctx, admin, 0, harborpilot.ConnectionInput{BaseURL: f.URL(), AllowInsecureHTTP: true, ParentToken: f.Parent,
		ServiceToken: f.Service}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.c.HarborPilot.PutSpace(w.ctx, admin, w.spaceID, 0, harborpilot.SpaceInput{Enabled: true, ProjectID: "prj_web"}, true); err != nil {
		t.Fatal(err)
	}
	w.answer(t) // work for control, which is not running: the supervisor tries to start it
	var got *human.Action
	eventually(t, "control could not start", func() bool {
		as, err := w.c.Human.List(w.ctx, w.spaceID, human.ListFilter{Query: "Control could not start"})
		if err != nil || len(as) == 0 {
			return false
		}
		got = as[0]
		return true
	})
	for _, want := range []string{`Harbor Pilot budget "daily"`, "0.12 of 0.10 USD spent", "resets at 2026-11-09T00:00:00Z"} {
		if !strings.Contains(got.Body, want) {
			t.Fatalf("attention body %q lacks %q", got.Body, want)
		}
	}
	if len(w.controls()) != 0 {
		t.Fatal("control was started over a used-up budget")
	}
}

// SM-GW-008 (simulated: hpfake and a scripted node): a gateway Codex run whose harness reports no usage gets its usage
// read back from Harbor Pilot after it ends (source harbor-pilot), shown with the run's usage as gateway-observed.
func TestHarborPilotRunUsageReadBack(t *testing.T) {
	t.Parallel()
	f := hpfake.New(t)
	h := newHPRunEnv(t, f.URL(), f.Parent, f.Service, "prj_web")
	ctx := context.Background()
	r, err := h.e.c.Fleet.LaunchRun(ctx, admin, h.sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: "codex", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	h.launchCmd(t, r.ID)
	f.Set(func(x *hpfake.HP) {
		x.Usage = map[string]string{"spaceman.run:" + r.ID: `{"group_by":"model","groups":[{"key":"coding/default","attempts":2,` +
			`"tokens":{"input":50,"output":7}}],"totals":[{"kind":"estimated_api_charge","currency":"USD","amount":"0.002"}]}`}
	})
	h.report(t, r, proto.RunRunning, "")
	h.n.send(t, proto.TypeUsage, proto.UsageReport{RunID: r.ID, Source: proto.UsageUnavailable, Models: []proto.ModelUsage{}, Final: true})
	h.report(t, r, proto.RunExited, "exited")
	eventually(t, "read-back queued", func() bool {
		var n int
		_ = h.e.c.Store.ReadDB().QueryRowContext(ctx, `SELECT count(*) FROM run_gateway_usage WHERE run_id = ?`, r.ID).Scan(&n)
		return n == 1
	})
	// The deadline timer's round, due at once here.
	if err := h.e.c.Store.W().TxNamed(ctx, "test", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `UPDATE run_gateway_usage SET next_attempt_at = 1 WHERE run_id = ?`, r.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := h.e.c.HarborPilotRuns.ReadDueUsage(ctx); err != nil || n != 1 {
		t.Fatalf("read round %d %v", n, err)
	}
	eventually(t, "usage reported unavailable, then filled", func() bool {
		u, err := h.e.c.Usage.ForRun(ctx, h.sp.ID, r.ID)
		return err == nil && u.Source == proto.UsageHarborPilot
	})
	u, _ := h.e.c.Usage.ForRun(ctx, h.sp.ID, r.ID)
	if !u.Known || len(u.Models) != 1 || *u.Models[0].Input != 50 || u.Estimate == nil || u.Estimate.Source != "harbor-pilot" ||
		u.Gateway == nil || u.Gateway.Requests != 2 || u.Gateway.State != "pending" || len(u.Gateway.Costs) != 1 {
		b, _ := json.Marshal(u)
		t.Fatalf("usage %s", b)
	}
}

// SM-GW-008 (simulated): a usage read-back still pending when central stops is done by the next central (the queue
// is durable, and the deadline timer re-arms from the database), and stops after its final read.
func TestHarborPilotUsageReadBackAfterRestart(t *testing.T) {
	// Not parallel: restarts central, which must finish within the node lease.
	f := hpfake.New(t)
	h := newHPRunEnv(t, f.URL(), f.Parent, f.Service, "prj_web")
	ctx := context.Background()
	r, err := h.e.c.Fleet.LaunchRun(ctx, admin, h.sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: "codex", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	h.launchCmd(t, r.ID)
	f.Set(func(x *hpfake.HP) {
		x.Usage = map[string]string{"spaceman.run:" + r.ID: `{"group_by":"model","groups":[{"key":"coding/default","attempts":3,"tokens":{"input":9,"output":1}}],"totals":[]}`}
	})
	h.report(t, r, proto.RunExited, "exited")
	exec := func(q string, args ...any) {
		t.Helper()
		if err := h.e.c.Store.W().TxNamed(ctx, "test", func(tx *store.Tx) error {
			_, err := tx.SQL.ExecContext(ctx, q, args...)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	state := func() (st string, reads int) {
		_ = h.e.c.Store.ReadDB().QueryRowContext(ctx, `SELECT state, reads FROM run_gateway_usage WHERE run_id = ?`, r.ID).Scan(&st, &reads)
		return
	}
	eventually(t, "read-back queued", func() bool { st, _ := state(); return st == "pending" })
	// One read before the restart; two remain.
	exec(`UPDATE run_gateway_usage SET next_attempt_at = 1 WHERE run_id = ?`, r.ID)
	if n, err := h.e.c.HarborPilotRuns.ReadDueUsage(ctx); err != nil || n != 1 {
		t.Fatalf("first read %d %v", n, err)
	}
	h.e.restartCentral()
	if next, err := h.e.c.HarborPilotRuns.NextUsageRead(ctx); err != nil || next.IsZero() {
		t.Fatalf("the restarted central has nothing due: %v %v", next, err)
	}
	for range harborpilot.UsageReads[1:] {
		exec(`UPDATE run_gateway_usage SET next_attempt_at = 1 WHERE run_id = ? AND state = 'pending'`, r.ID)
		if n, err := h.e.c.HarborPilotRuns.ReadDueUsage(ctx); err != nil || n != 1 {
			t.Fatalf("read after restart %d %v", n, err)
		}
	}
	if st, reads := state(); st != "final" || reads != len(harborpilot.UsageReads) {
		t.Fatalf("after the restart: %s after %d reads", st, reads)
	}
	if next, _ := h.e.c.HarborPilotRuns.NextUsageRead(ctx); !next.IsZero() {
		t.Fatal("still due after the final read")
	}
	u, err := h.e.c.Usage.ForRun(ctx, h.sp.ID, r.ID)
	if err != nil || u.Source != proto.UsageHarborPilot || !u.Final || u.Gateway == nil || u.Gateway.State != "final" || u.Gateway.Requests != 3 {
		t.Fatalf("usage %+v %v", u, err)
	}
}
