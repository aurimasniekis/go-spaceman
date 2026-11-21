package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
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
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/usage"
)

// The usage rules (ADR-0090, plan T7): a space's views need run:read in that space; an account's views need account:read.
func TestUsageLedgerRules(t *testing.T) {
	t.Parallel()
	for op, want := range map[string]api.Rule{
		"get-space-usage-series":     api.InSpace(authz.RunRead),
		"list-space-account-windows": api.InSpace(authz.RunRead),
		"get-account-usage":          api.Global(authz.AccountRead),
		"get-account-windows":        api.Global(authz.AccountRead),
		"list-account-windows":       api.FilteredRead(),
	} {
		if got, ok := app.SpecDeps().RuleFor(op); !ok || got != want {
			t.Errorf("%s: %+v, want %+v", op, got, want)
		}
	}
}

// Usage per space against usage per account: a space's viewer reads that space's series and the windows of the accounts
// bound to it, never another space's and never the account-level views (account:read is global); an administrator reads
// the account views, which count every space. No response carries a credential value.
func TestUsageAPIAuthorization(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	spA, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "usa", Name: "usa", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	spB, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "usb", Name: "usb", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	spC, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "usc", Name: "usc", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	const secret = "sk-ant-oat01-usage-api-secret-value"
	acc, err := e.c.Accounts.Create(ctx, admin, accounts.Input{Name: "usage-acc", Kind: "claude_oauth_token", Secret: secret, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Accounts.SetBindings(ctx, admin, spA.ID, []accounts.Binding{{AccountID: acc.ID, Default: true}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	hour := usage.HourStart(now)
	if err := e.c.Store.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
		for _, sp := range []struct {
			id string
			in int64
		}{{spA.ID, 100}, {spB.ID, 7000}} {
			if _, err := tx.SQL.ExecContext(ctx, `INSERT INTO usage_buckets (space_id, account_id, role, harness, model, granularity, bucket_start, input_tokens, output_tokens, requests, cost_usd)
				VALUES (?, ?, 'worker', 'claude-code', 'm', 'hour', ?, ?, 10, 1, 0.5)`, sp.id, acc.ID, hour, sp.in); err != nil {
				return err
			}
		}
		// A Codex run in each of A and B: their usage is not in the ledger, so the series says so, per space.
		for i, sp := range []string{spA.ID, spB.ID} {
			if _, err := tx.SQL.ExecContext(ctx, `INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, session_id, desired, observed,
				launch_json, manifest_json, created_at, created_by, updated_at, ended_at) VALUES (?, ?, ?, 1, 'worker', 'worker', 'codex', 'ins_1', '', 'stopped', 'exited',
				'{}', '{}', ?, 't', ?, ?)`, fmt.Sprintf("run_01codex%019d", i), sp, nodes[0].ID, now.Add(-time.Hour).UnixMicro(), now.UnixMicro(), now.UnixMicro()); err != nil {
				return err
			}
		}
		_, err := tx.SQL.ExecContext(ctx, `INSERT INTO account_windows (account_id, kind, used_pct, resets_at, observed_at, run_id, updated_at) VALUES (?, 'five_hour', 61, ?, ?, '', 0)`,
			acc.ID, now.Add(time.Hour).UnixMicro(), now.UnixMicro())
		return err
	}); err != nil {
		t.Fatal(err)
	}

	mkToken := func(email string, grants []auth.GrantInput) string {
		t.Helper()
		u, err := e.c.Auth.Create(ctx, audit.System, auth.CreateInput{Email: email, DisplayName: email, Grants: grants})
		if err != nil {
			t.Fatal(err)
		}
		u, _ = e.c.Auth.Get(ctx, u.ID)
		_, tok, err := e.c.Auth.CreateAPIToken(ctx, &auth.Principal{User: u}, "us", grants, 24*time.Hour, false)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	viewerA := mkToken("usa@example.com", []auth.GrantInput{{Role: authz.RoleViewer, Domain: spA.ID}})
	viewerB := mkToken("usb@example.com", []auth.GrantInput{{Role: authz.RoleViewer, Domain: spB.ID}})
	nobody := mkToken("usn@example.com", []auth.GrantInput{{Role: authz.RoleViewer, Domain: spC.ID}})
	adminAll := mkToken("usadm@example.com", []auth.GrantInput{{Role: authz.RoleAdmin, Domain: authz.Global}})
	get := func(tok, path string, out any) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+api.Prefix+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		var raw json.RawMessage
		_ = json.NewDecoder(res.Body).Decode(&raw)
		if out != nil && res.StatusCode == http.StatusOK {
			if err := json.Unmarshal(raw, out); err != nil {
				t.Fatal(err)
			}
		}
		return res.StatusCode, string(raw)
	}
	type series struct {
		CostNote string `json:"cost_note"`
		Totals   struct {
			Total int64   `json:"total_tokens"`
			Cost  float64 `json:"total_cost_usd"`
		} `json:"totals"`
		Items []struct {
			Group string `json:"group"`
		} `json:"items"`
		NotCounted []usage.NotCounted `json:"not_counted"`
	}

	// A space's series: its viewer reads it; another space's viewer and a caller without access are refused.
	var a series
	if st, body := get(viewerA, "/spaces/"+spA.ID+"/usage/series?group_by=account", &a); st != http.StatusOK || a.Totals.Total != 110 || len(a.Items) != 1 ||
		a.Items[0].Group != acc.ID || a.CostNote != usage.CostBasis || a.Totals.Cost != 0.5 {
		t.Fatalf("viewer A: %d %s", st, body)
	}
	// The Codex run of space A is reported as not counted, and space B's is not mixed in.
	if want := []usage.NotCounted{{Harness: "codex", Runs: 1}}; !reflect.DeepEqual(a.NotCounted, want) {
		t.Fatalf("viewer A not_counted %+v, want %+v", a.NotCounted, want)
	}
	for who, tok := range map[string]string{"viewer of B": viewerB, "viewer of C": nobody} {
		if st, _ := get(tok, "/spaces/"+spA.ID+"/usage/series", nil); st != http.StatusNotFound && st != http.StatusForbidden {
			t.Errorf("%s reading space A's series: %d", who, st)
		}
		if st, _ := get(tok, "/spaces/"+spA.ID+"/account-windows", nil); st != http.StatusNotFound && st != http.StatusForbidden {
			t.Errorf("%s reading space A's windows: %d", who, st)
		}
	}
	if st, _ := get(viewerA, "/spaces/"+spA.ID+"/usage/series?group_by=bogus", nil); st != http.StatusUnprocessableEntity && st != http.StatusBadRequest {
		t.Errorf("bad group_by: %d", st)
	}

	// The bound account's windows through the space; ids, names and numbers only.
	var ws api.SpaceAccountWindows
	if st, body := get(viewerA, "/spaces/"+spA.ID+"/account-windows", &ws); st != http.StatusOK || len(ws.Items) != 1 || ws.Items[0].AccountID != acc.ID ||
		len(ws.Items[0].Windows) != 1 || ws.Items[0].Windows[0].UsedPct != 61 {
		t.Fatalf("space windows: %d %s", st, body)
	}
	// Space B has no bound account: an empty list, not A's.
	if st, body := get(viewerB, "/spaces/"+spB.ID+"/account-windows", &ws); st != http.StatusOK || len(ws.Items) != 0 {
		t.Fatalf("space B windows: %d %s", st, body)
	}

	// The account views need account:read: space viewers are refused, the administrator reads them and sees every space.
	for who, tok := range map[string]string{"viewer A": viewerA, "viewer B": viewerB, "viewer of C": nobody} {
		for _, p := range []string{"/accounts/" + acc.ID + "/usage", "/accounts/" + acc.ID + "/windows"} {
			if st, _ := get(tok, p, nil); st != http.StatusForbidden {
				t.Errorf("%s reading %s: %d", who, p, st)
			}
		}
	}
	var all series
	if st, body := get(adminAll, "/accounts/"+acc.ID+"/usage?group_by=space", &all); st != http.StatusOK || all.Totals.Total != 110+7010 || len(all.Items) != 2 {
		t.Fatalf("admin account usage: %d %s", st, body)
	}
	var w struct {
		AccountID string `json:"account_id"`
		Name      string `json:"name"`
	}
	if st, body := get(adminAll, "/accounts/"+acc.ID+"/windows", &w); st != http.StatusOK || w.AccountID != acc.ID || w.Name != "usage-acc" {
		t.Fatalf("admin account windows: %d %s", st, body)
	}
	// Nothing returned names the credential.
	for _, p := range []string{"/spaces/" + spA.ID + "/usage/series?group_by=account", "/spaces/" + spA.ID + "/account-windows"} {
		if _, body := get(adminAll, p, nil); strings.Contains(body, secret) || strings.Contains(body, "secret") {
			t.Errorf("%s mentions a secret: %s", p, body)
		}
	}
	if st, _ := get(adminAll, "/accounts/acc_missing/usage", nil); st != http.StatusNotFound {
		t.Errorf("missing account: %d", st)
	}
	if st, _ := get(adminAll, "/accounts/acc_missing/windows", nil); st != http.StatusNotFound {
		t.Errorf("missing account windows: %d", st)
	}

	// An existing account bound to no space is not found even for the global administrator (the rule is binding to a
	// readable space, not account:read); once it is bound to a space the administrator can read, it is served.
	loose, err := e.c.Accounts.Create(ctx, admin, accounts.Input{Name: "usage-loose", Kind: "claude_oauth_token", Secret: secret + "-2", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := get(adminAll, "/accounts/"+loose.ID+"/windows", nil); st != http.StatusNotFound {
		t.Errorf("unbound account windows: %d, want 404", st)
	}
	if _, err := e.c.Accounts.SetBindings(ctx, admin, spC.ID, []accounts.Binding{{AccountID: loose.ID, Default: true}}); err != nil {
		t.Fatal(err)
	}
	if st, body := get(adminAll, "/accounts/"+loose.ID+"/windows", nil); st != http.StatusOK {
		t.Errorf("bound account windows: %d %s, want 200", st, body)
	}
}

// The cross-space account view (plan UI-T2): each account bound to a space the caller can read, once, with its windows
// and open limits; an account bound only to spaces the caller cannot read is absent, name and all. A role-pool binding
// counts like a space binding. Viewers, operators and administrators all read it, each their own spaces' accounts.
func TestListAccountWindowsPerSpace(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	mkSpace := func(slug string) string {
		sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: slug, Name: slug, NodeID: nodes[0].ID})
		if err != nil {
			t.Fatal(err)
		}
		return sp.ID
	}
	spA, spB, spC := mkSpace("wla"), mkSpace("wlb"), mkSpace("wlc")
	mkAcc := func(name string) string {
		a, err := e.c.Accounts.Create(ctx, admin, accounts.Input{Name: name, Kind: "claude_oauth_token", Secret: "sk-ant-oat01-" + name + "-secret", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	shared, onlyB, pooled, loose := mkAcc("acct-shared"), mkAcc("acct-only-b"), mkAcc("acct-pooled"), mkAcc("acct-unbound")
	bind := func(space string, ids ...string) {
		var bs []accounts.Binding
		for _, id := range ids {
			bs = append(bs, accounts.Binding{AccountID: id})
		}
		if _, err := e.c.Accounts.SetBindings(ctx, admin, space, bs); err != nil {
			t.Fatal(err)
		}
	}
	bind(spA, shared)
	bind(spB, shared, onlyB)
	bind(spC, pooled)
	now := time.Now().UTC()
	if err := e.c.Store.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
		for _, acc := range []string{shared, onlyB} {
			if _, err := tx.SQL.ExecContext(ctx, `INSERT INTO account_windows (account_id, kind, used_pct, resets_at, observed_at, run_id, updated_at) VALUES (?, 'five_hour', 42, ?, ?, '', 0)`,
				acc, now.Add(time.Hour).UnixMicro(), now.UnixMicro()); err != nil {
				return err
			}
		}
		if _, err := tx.SQL.ExecContext(ctx, `INSERT INTO account_windows (account_id, kind, used_pct, observed_at, run_id, updated_at) VALUES (?, 'seven_day', 80, ?, '', 0)`, shared, now.UnixMicro()); err != nil {
			return err
		}
		// An open limit and a cleared one on the shared account: only the open one is listed.
		for i, cleared := range []any{nil, now.UnixMicro()} {
			if _, err := tx.SQL.ExecContext(ctx, `INSERT INTO usage_limits (id, space_id, scope, account_id, kind, resets_at, source, seen_at, cleared_at, created_at, updated_at)
				VALUES (?, ?, 'account', ?, 'five_hour', ?, 'transcript', ?, ?, 0, 0)`, "ul_wl_"+string(rune('a'+i)), spA, shared, now.Add(2*time.Hour).UnixMicro(), now.UnixMicro(), cleared); err != nil {
				return err
			}
		}
		// The same window seen exhausted in space B (one row per space): still one limit, with the later reset.
		if _, err := tx.SQL.ExecContext(ctx, `INSERT INTO usage_limits (id, space_id, scope, account_id, kind, resets_at, source, seen_at, created_at, updated_at)
			VALUES ('ul_wl_c', ?, 'account', ?, 'five_hour', ?, 'transcript', ?, 0, 0)`, spB, shared, now.Add(3*time.Hour).UnixMicro(), now.Add(time.Minute).UnixMicro()); err != nil {
			return err
		}
		// A role pool binding of the pooled account to space B (space C's own binding is what the viewer of C reads).
		_, err := tx.SQL.ExecContext(ctx, `INSERT INTO space_role_accounts (space_id, role, account_id, priority, created_at, created_by) VALUES (?, 'worker', ?, 100, 0, 'test')`, spB, pooled)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	mkToken := func(email string, role authz.Role, domain string) string {
		t.Helper()
		grants := []auth.GrantInput{{Role: role, Domain: domain}}
		u, err := e.c.Auth.Create(ctx, audit.System, auth.CreateInput{Email: email, DisplayName: email, Grants: grants})
		if err != nil {
			t.Fatal(err)
		}
		u, _ = e.c.Auth.Get(ctx, u.ID)
		_, tok, err := e.c.Auth.CreateAPIToken(ctx, &auth.Principal{User: u}, "wl", grants, 24*time.Hour, false)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	list := func(tok string) (int, map[string]usage.AccountState, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+api.Prefix+"/account-windows", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		var raw json.RawMessage
		_ = json.NewDecoder(res.Body).Decode(&raw)
		var body api.AccountWindowsList
		_ = json.Unmarshal(raw, &body)
		m := map[string]usage.AccountState{}
		for _, it := range body.Items {
			if _, dup := m[it.AccountID]; dup {
				t.Errorf("account %s listed twice", it.AccountID)
			}
			m[it.AccountID] = it
		}
		return res.StatusCode, m, string(raw)
	}
	keys := func(m map[string]usage.AccountState) []string {
		var ids []string
		for id := range m {
			ids = append(ids, id)
		}
		return ids
	}

	// A viewer of A sees the shared account only, with both windows and the one open limit; never B's own account or its name.
	st, m, body := list(mkToken("wla@example.com", authz.RoleViewer, spA))
	if st != http.StatusOK || len(m) != 1 {
		t.Fatalf("viewer A: %d %s", st, body)
	}
	sh := m[shared]
	if sh.Name != "acct-shared" || sh.Harness != "claude-code" || len(sh.Windows) != 2 || len(sh.Limits) != 1 || sh.Limits[0].Kind != "five_hour" || sh.Limits[0].ResetsAt == nil {
		t.Errorf("shared account: %+v", sh)
	} else if !sh.Limits[0].SeenAt.Equal(time.UnixMicro(now.UnixMicro()).UTC()) || !sh.Limits[0].ResetsAt.Equal(time.UnixMicro(now.Add(3*time.Hour).UnixMicro()).UTC()) {
		t.Errorf("shared limit: %+v, want the first sighting with the latest reset", sh.Limits[0])
	}
	for _, leak := range []string{"acct-only-b", "acct-pooled", "acct-unbound", onlyB, pooled, loose} {
		if strings.Contains(body, leak) {
			t.Errorf("viewer A's list mentions %q: %s", leak, body)
		}
	}
	// A viewer of C sees only what is bound to C: the pooled account (and not the shared one, bound to A and B only).
	if st, m, body := list(mkToken("wlc@example.com", authz.RoleViewer, spC)); st != http.StatusOK || len(m) != 1 || m[pooled].Name != "acct-pooled" || len(m[pooled].Windows) != 0 || len(m[pooled].Limits) != 0 {
		t.Errorf("viewer C: %d %s", st, body)
	}
	// An operator of B sees B's bindings, including the role-pool one, each once.
	if st, m, body := list(mkToken("wlb@example.com", authz.RoleOperator, spB)); st != http.StatusOK || len(m) != 3 || m[shared].AccountID == "" || m[onlyB].AccountID == "" || m[pooled].AccountID == "" {
		t.Errorf("operator B: %d %v %s", st, keys(m), body)
	}
	// The administrator reads every space, so every bound account; an unbound one is in no space and so absent.
	if st, m, body := list(mkToken("wladm@example.com", authz.RoleAdmin, authz.Global)); st != http.StatusOK || len(m) != 3 || m[loose].AccountID != "" {
		t.Errorf("admin: %d %v %s", st, keys(m), body)
	}
	// A user who reads only a space with no bound account sees an empty list, not an error; no token is refused.
	if st, m, body := list(mkToken("wlz@example.com", authz.RoleViewer, mkSpace("wld"))); st != http.StatusOK || len(m) != 0 {
		t.Errorf("no readable space: %d %s", st, body)
	}
	// A token narrowed to A by a user who also views B reads A's accounts only: B's own account stays hidden.
	{
		u, err := e.c.Auth.Create(ctx, audit.System, auth.CreateInput{Email: "wlab@example.com", DisplayName: "wlab",
			Grants: []auth.GrantInput{{Role: authz.RoleViewer, Domain: spA}, {Role: authz.RoleViewer, Domain: spB}}})
		if err != nil {
			t.Fatal(err)
		}
		u, _ = e.c.Auth.Get(ctx, u.ID)
		_, tok, err := e.c.Auth.CreateAPIToken(ctx, &auth.Principal{User: u}, "wl-narrow", []auth.GrantInput{{Role: authz.RoleViewer, Domain: spA}}, 24*time.Hour, false)
		if err != nil {
			t.Fatal(err)
		}
		st, m, body := list(tok)
		if st != http.StatusOK || len(m) != 1 || m[shared].AccountID == "" || strings.Contains(body, "acct-only-b") || strings.Contains(body, "acct-pooled") {
			t.Errorf("narrowed token: %d %v %s", st, keys(m), body)
		}
	}
	if st, _, _ := list(""); st != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", st)
	}
	// Nothing returned carries a credential value.
	if strings.Contains(body, "secret") {
		t.Errorf("response mentions a secret: %s", body)
	}
}
