package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/accounts"
	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/app"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// ADR-0079 decision 7 (UL-6, simulated: the fake runner; the limit is seeded, as node detection is UL-3):
//   - a control run that ends at a usage limit is neither restarted nor counted, and nobody is paged;
//   - control auto-start is deferred while its account is held, without asking people to look;
//   - a launch on the held account is refused through the API (429) with the reset time, in every space bound to it;
//   - "clear now": a space operator clears only their space's row (held elsewhere), an account admin clears every
//     space's rows; both need their permission, are audited, and each cleared space's control is told once;
//   - with the hold gone, auto-start starts control again, resuming its session.
func TestUsageLimitHoldControlAndClear(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t, func(c *config.Config) { c.Limits.ControlAutostart = 1 })
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	admin := audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}
	e.c.Supervisor.MaxRestarts = 1
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "ulh", Name: "ULH", NodeID: nodes[0].ID, StarterRoles: true})
	if err != nil {
		t.Fatal(err)
	}
	acc := bindStubAccounts(t, e.c, sp.ID)["claude_oauth_token"]
	spB, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "ulh-b", Name: "ULH B", NodeID: nodes[0].ID, StarterRoles: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Accounts.SetBindings(ctx, admin, spB.ID, []accounts.Binding{{AccountID: acc, Default: true}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{sp.ID, spB.ID} {
		eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, id); return s.Lease != nil && s.Lease.Valid })
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if err := e.c.Store.W().TxNamed(ctx, "seed", func(tx *store.Tx) error { _, err := tx.SQL.ExecContext(ctx, q, args...); return err }); err != nil {
			t.Fatal(err)
		}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := e.c.Store.ReadDB().QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	controls := func() int { return count(`SELECT count(*) FROM runs WHERE space_id = ? AND kind = 'control'`, sp.ID) }
	pending := func() int { l, _ := e.c.Human.List(ctx, sp.ID, human.ListFilter{Pending: true}); return len(l) }

	ctrl, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "control", Role: "control", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "control running", func() bool { return e.run1(ctrl.ID).Observed == "running" })
	e.run.SetStatus(e.run1(ctrl.ID).TerminalID, "working")
	e.run.SetStatus(e.run1(ctrl.ID).TerminalID, "idle")
	eventually(t, "control finished a turn", func() bool { return e.run1(ctrl.ID).Turn == 1 })

	// Control reaches its 5-hour limit and ends there. No hold is open yet, so only the exemption keeps it from being
	// restarted (a restart would otherwise go through).
	now := time.Now().UTC()
	reset := now.Add(3 * time.Hour)
	exec(`UPDATE runs SET limit_kind = 'five_hour', limit_resets_at = ?, limit_since = ? WHERE id = ?`, reset.UnixMicro(), now.UnixMicro(), ctrl.ID)
	e.run.Exit(e.run1(ctrl.ID).TerminalID, 1, "usage limit")
	eventually(t, "control ended at the limit", func() bool { r := e.run1(ctrl.ID); return r.EndReason == fleet.EndUsageLimit && r.Desired == "stopped" })
	time.Sleep(500 * time.Millisecond)
	if n := controls(); n != 1 {
		t.Fatalf("control restarted after a usage-limit end: %d control runs", n)
	}
	if n := pending(); n != 0 {
		t.Fatalf("%d human actions raised for a usage-limit end", n)
	}
	launches := func() int { return launchTxs(t, e.c) }
	// UL-7: control is parked; its wait brings it back at the reset, so auto-start leaves it alone. An operator gives the
	// wait up here, so control can only come back through auto-start, which the hold defers.
	w, err := e.c.Fleet.LimitWaitForRun(ctx, ctrl.ID)
	if err != nil || w == nil || w.RunKind != "control" || w.ReleaseAt != nil {
		t.Fatalf("control's limit wait: %+v %v", w, err)
	}
	before := launches()
	e.c.Supervisor.ControlWanted(ctx, sp.ID)
	if n := launches() - before; n != 0 {
		t.Fatalf("auto-start tried %d launches for a parked control", n)
	}
	if _, err := e.c.Fleet.ReleaseLimitWait(ctx, admin, sp.ID, w.ID); err != nil {
		t.Fatal(err)
	}

	// Its account is held in space A. Work waits for control: auto-start is deferred, silently, and only the first
	// sweep tries a launch (one refused launch transaction); later sweeps only read the hold.
	exec(`INSERT INTO usage_limits (id, space_id, scope, account_id, kind, resets_at, source, first_run_id, seen_at, created_at, updated_at)
		VALUES ('ulm_hold_a', ?, 'account', ?, 'five_hour', ?, 'transcript', ?, ?, ?, ?)`, sp.ID, acc, reset.UnixMicro(), ctrl.ID, now.UnixMicro(),
		now.UnixMicro(), now.UnixMicro())
	before = launches()
	for range 5 {
		e.c.Supervisor.ControlWanted(ctx, sp.ID)
	}
	if n := launches() - before; n != 1 {
		t.Fatalf("deferred auto-start tried %d launches", n)
	}
	if n := controls(); n != 1 {
		t.Fatalf("auto-start launched into the hold: %d control runs", n)
	}
	if n := pending(); n != 0 {
		t.Fatalf("%d human actions raised for a deferred auto-start", n)
	}

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
	opA := mkToken("opa@example.com", []auth.GrantInput{{Role: authz.RoleOperator, Domain: sp.ID}})
	opB := mkToken("opb@example.com", []auth.GrantInput{{Role: authz.RoleOperator, Domain: spB.ID}})
	viewerA := mkToken("va@example.com", []auth.GrantInput{{Role: authz.RoleViewer, Domain: sp.ID}})
	adminAll := mkToken("adm@example.com", []auth.GrantInput{{Role: authz.RoleAdmin, Domain: authz.Global}})
	post := func(tok, path string, body any, out any) (int, string) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, e.srv.URL+api.Prefix+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		var raw bytes.Buffer
		_, _ = raw.ReadFrom(res.Body)
		if out != nil && res.StatusCode == http.StatusOK {
			if err := json.Unmarshal(raw.Bytes(), out); err != nil {
				t.Fatal(err)
			}
		}
		return res.StatusCode, raw.String()
	}

	// Space B, bound to the same account, is refused through the API with the reset time (Q8).
	st, body := post(opB, "/spaces/"+spB.ID+"/runs", map[string]any{"kind": "worker", "role": "implementor", "harness": hns.KindClaude}, nil)
	if st != http.StatusTooManyRequests || !strings.Contains(body, "5-hour usage limit until "+reset.Format("2006-01-02 15:04")+" UTC") {
		t.Fatalf("launch in B: %d %s", st, body)
	}
	if strings.Contains(body, sp.ID) || strings.Contains(body, "ulm_hold_a") {
		t.Fatalf("the refusal names space A: %s", body)
	}
	// B's own hold on the account (opened by a run of B).
	exec(`INSERT INTO usage_limits (id, space_id, scope, account_id, kind, resets_at, source, first_run_id, seen_at, created_at, updated_at)
		VALUES ('ulm_hold_b', ?, 'account', ?, 'five_hour', ?, 'transcript', 'run_b', ?, ?, ?)`, spB.ID, acc, reset.UnixMicro(), now.UnixMicro(),
		now.UnixMicro(), now.UnixMicro())

	// Space-level clear: viewers can't, another space's operator doesn't find it, A's operator clears A's row only.
	if st, _ := post(viewerA, "/spaces/"+sp.ID+"/usage-limits/ulm_hold_a/clear", nil, nil); st != http.StatusForbidden {
		t.Fatalf("viewer clear: %d", st)
	}
	if st, _ := post(opB, "/spaces/"+spB.ID+"/usage-limits/ulm_hold_a/clear", nil, nil); st != http.StatusNotFound {
		t.Fatalf("B's operator clearing A's hold: %d", st)
	}
	var res fleet.UsageLimitClear
	if st, body := post(opA, "/spaces/"+sp.ID+"/usage-limits/ulm_hold_a/clear", nil, &res); st != http.StatusOK || len(res.Cleared) != 1 || !res.HeldElsewhere ||
		!strings.Contains(res.Note, "Other spaces still hold the account") {
		t.Fatalf("space clear: %d %s", st, body)
	}
	if n := count(`SELECT count(*) FROM usage_limits WHERE id = 'ulm_hold_b' AND cleared_at IS NULL`); n != 1 {
		t.Fatal("a space-level clear cleared another space's hold")
	}
	// A is still held by B's row: auto-start stays deferred.
	e.c.Supervisor.ControlWanted(ctx, sp.ID)
	if n := controls(); n != 1 {
		t.Fatalf("auto-start while B still holds the account: %d control runs", n)
	}

	// Account-level clear: a space operator lacks account:write; an admin clears every space's rows.
	if st, _ := post(opA, "/accounts/"+acc+"/usage-limits/clear", map[string]any{}, nil); st != http.StatusForbidden {
		t.Fatalf("space operator account clear: %d", st)
	}
	res = fleet.UsageLimitClear{}
	if st, body := post(adminAll, "/accounts/"+acc+"/usage-limits/clear", map[string]any{"kind": "five_hour"}, &res); st != http.StatusOK ||
		len(res.Cleared) != 1 || res.Cleared[0].SpaceID != spB.ID {
		t.Fatalf("account clear: %d %s", st, body)
	}
	if n := count(`SELECT count(*) FROM audit_events WHERE action IN ('usage_limit.cleared', 'account.usage_limits_cleared')`); n != 2 {
		t.Fatalf("%d clear audit events", n)
	}
	toldCleared := func(space string) int {
		l, _ := e.c.Messages.List(ctx, space, 100)
		n := 0
		for _, m := range l {
			if m.ToKind == "control" && strings.HasPrefix(m.Subject, "Usage-limit hold cleared") {
				n++
			}
		}
		return n
	}
	eventually(t, "both spaces' control told", func() bool { return toldCleared(sp.ID) == 1 && toldCleared(spB.ID) == 1 })

	// The hold is gone: auto-start starts control again, resuming its session.
	e.c.Supervisor.ControlWanted(ctx, sp.ID)
	if n := controls(); n != 2 {
		t.Fatalf("auto-start after the clear: %d control runs", n)
	}
	time.Sleep(200 * time.Millisecond)
	if n := toldCleared(sp.ID) + toldCleared(spB.ID); n != 2 {
		t.Fatalf("control told %d times", n)
	}
}

// The clear endpoints' rules (ADR-0079 decision 7): a space's own hold with the permission that launches runs there,
// every space's holds on an account with the permission that manages accounts.
func TestUsageLimitClearRules(t *testing.T) {
	t.Parallel()
	for op, want := range map[string]api.Rule{
		"clear-space-usage-limit":    api.InSpace(authz.RunControl),
		"clear-account-usage-limits": api.Global(authz.AccountWrite),
		"resume-limit-wait":          api.InSpace(authz.RunControl),
		"release-limit-wait":         api.InSpace(authz.RunControl),
	} {
		if got, ok := app.SpecDeps().RuleFor(op); !ok || got != want {
			t.Errorf("%s: %+v, want %+v", op, got, want)
		}
	}
	if authz.RoleHas(authz.RoleOperator, authz.AccountWrite) || !authz.RoleHas(authz.RoleOperator, authz.RunControl) || authz.RoleHas(authz.RoleViewer, authz.RunControl) {
		t.Error("space operators launch runs but don't manage accounts; viewers do neither")
	}
}

// launchTxs counts the launch transactions central ran, whatever their result (store_tx_total).
func launchTxs(t *testing.T, c *app.Central) int {
	t.Helper()
	rec := httptest.NewRecorder()
	c.Metrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	n := 0
	for line := range strings.SplitSeq(rec.Body.String(), "\n") {
		if strings.Contains(line, "store_tx_total{") && strings.Contains(line, `name="fleet.launch_run"`) {
			f := strings.Fields(line)
			v, err := strconv.ParseFloat(f[len(f)-1], 64)
			if err != nil {
				t.Fatal(err)
			}
			n += int(v)
		}
	}
	return n
}
