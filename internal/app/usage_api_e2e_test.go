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
	}

	// A space's series: its viewer reads it; another space's viewer and a caller without access are refused.
	var a series
	if st, body := get(viewerA, "/spaces/"+spA.ID+"/usage/series?group_by=account", &a); st != http.StatusOK || a.Totals.Total != 110 || len(a.Items) != 1 ||
		a.Items[0].Group != acc.ID || a.CostNote != usage.CostBasis || a.Totals.Cost != 0.5 {
		t.Fatalf("viewer A: %d %s", st, body)
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
