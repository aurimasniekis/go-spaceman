package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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

// The cross-space series (ADR-0103) needs only a session; it filters every row to the spaces with run:read.
func TestUsageSeriesRule(t *testing.T) {
	t.Parallel()
	if got, ok := app.SpecDeps().RuleFor("get-usage-series"); !ok || got != api.FilteredRead() {
		t.Fatalf("get-usage-series: %+v", got)
	}
}

// Over HTTP, a viewer of space A gets no row, total, label or not-counted run of space B, for every group_by and every
// filter; a global reader gets both; a viewer of neither gets nothing; the filter bound is enforced.
func TestUsageSeriesAPIIsolation(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	spA, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "sera", Name: "Series A", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	spB, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "serb", Name: "Series B", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	spC, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "serc", Name: "Series C", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	mkAcc := func(name string) string {
		a, err := e.c.Accounts.Create(ctx, admin, accounts.Input{Name: name, Kind: "claude_oauth_token", Secret: "sk-ant-oat01-" + name, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	accA, accB, accS := mkAcc("acc-only-a"), mkAcc("acc-only-b"), mkAcc("acc-shared")
	if _, err := e.c.Accounts.SetBindings(ctx, admin, spA.ID, []accounts.Binding{{AccountID: accA, Default: true}, {AccountID: accS}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Accounts.SetBindings(ctx, admin, spB.ID, []accounts.Binding{{AccountID: accB, Default: true}, {AccountID: accS}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	hour := usage.HourStart(now.Add(-time.Hour))
	// Space A: 10 input tokens per row; space B: 1,000,000, so any B row shows in a total.
	type row struct {
		space, account, role, harness, model string
		in                                   int64
	}
	rows := []row{
		{spA.ID, accA, "role-a", "claude-code", "model-a", 10}, {spA.ID, accS, "shared", "claude-code", "model-s", 10}, {spA.ID, "", "shared", "claude-code", "model-s", 10},
		{spB.ID, accB, "role-b", "claude-code", "model-b", 1_000_000}, {spB.ID, accS, "shared", "claude-code", "model-s", 1_000_000},
		{spB.ID, accS, "shared", "codex", "model-s", 1_000_000}, {spB.ID, "", "shared", "claude-code", "model-s", 1_000_000},
	}
	if err := e.c.Store.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
		for _, r := range rows {
			if _, err := tx.SQL.ExecContext(ctx, `INSERT INTO usage_buckets (space_id, account_id, role, harness, model, granularity, bucket_start, input_tokens, requests, cost_usd)
				VALUES (?, ?, ?, ?, ?, 'hour', ?, ?, 1, 0.5)`, r.space, r.account, r.role, r.harness, r.model, hour, r.in); err != nil {
				return err
			}
		}
		for i, r := range []struct{ id, space string }{{"run_01serieaaaaaaaaaaaaaaaaaa", spA.ID}, {"run_01seriebbbbbbbbbbbbbbbbbb", spB.ID}, {"run_01seriecccccccccccccccccc", spB.ID}} {
			at := now.Add(-time.Duration(i+1) * time.Minute).UnixMicro()
			if _, err := tx.SQL.ExecContext(ctx, `INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, session_id, desired, observed,
				launch_json, manifest_json, created_at, created_by, updated_at, ended_at) VALUES (?, ?, ?, 1, 'worker', 'shared', 'codex', 'ins_1', '', 'stopped', 'exited',
				'{}', '{}', ?, 't', ?, ?)`, r.id, r.space, nodes[0].ID, at, at, at); err != nil {
				return err
			}
		}
		return nil
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
	viewerA := mkToken("sera@example.com", []auth.GrantInput{{Role: authz.RoleViewer, Domain: spA.ID}})
	viewerC := mkToken("serc@example.com", []auth.GrantInput{{Role: authz.RoleViewer, Domain: spC.ID}})
	adminAll := mkToken("seradm@example.com", []auth.GrantInput{{Role: authz.RoleAdmin, Domain: authz.Global}})

	type page struct {
		GroupBy string `json:"group_by"`
		Totals  struct {
			In int64 `json:"input_tokens"`
		} `json:"totals"`
		Items []struct {
			Group string `json:"group"`
			In    int64  `json:"input_tokens"`
		} `json:"items"`
		Labels     map[string]string  `json:"labels"`
		NotCounted []usage.NotCounted `json:"not_counted"`
	}
	get := func(tok string, q url.Values) (int, page, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+api.Prefix+"/usage/series?"+q.Encode(), nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		var raw json.RawMessage
		_ = json.NewDecoder(res.Body).Decode(&raw)
		var p page
		if res.StatusCode == http.StatusOK {
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatal(err)
			}
		}
		return res.StatusCode, p, string(raw)
	}
	filters := map[string]url.Values{
		"none":            {},
		"space B":         {"space_id": {spB.ID}},
		"spaces A,B":      {"space_id": {spA.ID + "," + spB.ID}},
		"spaces A and B":  {"space_id": {spA.ID, spB.ID}},
		"account B":       {"account_id": {accB}},
		"shared account":  {"account_id": {accS}},
		"model B":         {"model": {"model-b"}},
		"shared model":    {"model": {"model-s"}},
		"role B":          {"role": {"role-b"}},
		"shared role":     {"role": {"shared"}},
		"harness codex":   {"harness": {"codex"}},
		"every dimension": {"space_id": {spB.ID, spA.ID}, "account_id": {accS, accB}, "model": {"model-s", "model-b"}, "role": {"shared", "role-b"}},
	}
	bOnly := []string{spB.ID, accB, "model-b", "role-b", "Series B", "acc-only-b"}
	for _, group := range usage.GlobalGroups {
		for fname, f := range filters {
			name := group + "/" + fname
			q := url.Values{"group_by": {group}}
			for k, v := range f {
				q[k] = v
			}
			st, p, body := get(viewerA, q)
			if st != http.StatusOK || p.GroupBy != group {
				t.Fatalf("%s: %d %s", name, st, body)
			}
			if p.Totals.In >= 1_000_000 {
				t.Fatalf("%s: totals hold space B: %s", name, body)
			}
			var sum int64
			for _, it := range p.Items {
				sum += it.In
				if it.Group == "codex" { // only space B booked Codex usage
					t.Fatalf("%s: a Codex row of space B: %s", name, body)
				}
			}
			if sum != p.Totals.In {
				t.Fatalf("%s: rows %d, totals %d: %s", name, sum, p.Totals.In, body)
			}
			for _, b := range bOnly {
				if strings.Contains(body, b) {
					t.Fatalf("%s: response names %q of space B: %s", name, b, body)
				}
			}
			for _, nc := range p.NotCounted {
				if nc.Runs != 1 {
					t.Fatalf("%s: not counted %+v (space A has one Codex run)", name, p.NotCounted)
				}
			}
			// The admin sees space B's usage under the same filter wherever it matches.
			if _, all, body := get(adminAll, q); all.Totals.In < p.Totals.In {
				t.Fatalf("%s: admin totals %s", name, body)
			}
		}
	}
	// Viewer A's full picture: three rows of 10 tokens, labels for A and its accounts, one Codex run.
	if _, p, body := get(viewerA, url.Values{"group_by": {"space"}}); p.Totals.In != 30 || len(p.Items) != 1 || p.Labels[spA.ID] != "Series A" ||
		len(p.NotCounted) != 1 || p.NotCounted[0].Harness != "codex" || p.NotCounted[0].Runs != 1 {
		t.Fatalf("viewer A by space: %s", body)
	}
	if _, p, body := get(viewerA, url.Values{"group_by": {"account"}}); p.Labels[accA] != "acc-only-a" || p.Labels[accS] != "acc-shared" || len(p.Items) != 3 {
		t.Fatalf("viewer A by account: %s", body)
	}
	// The global reader sees both spaces and every Codex run.
	if _, p, body := get(adminAll, url.Values{"group_by": {"space"}}); p.Totals.In != 30+4_000_000 || len(p.Items) != 2 || p.Labels[spB.ID] != "Series B" ||
		len(p.NotCounted) != 1 || p.NotCounted[0].Runs != 3 {
		t.Fatalf("admin by space: %s", body)
	}
	// A viewer of neither space reads an empty series.
	if st, p, body := get(viewerC, url.Values{"space_id": {spA.ID, spB.ID}}); st != http.StatusOK || p.Totals.In != 0 || len(p.Items) != 0 || len(p.Labels) != 0 ||
		len(p.NotCounted) != 0 {
		t.Fatalf("viewer C: %d %s", st, body)
	}
	// Paging: one row per page, the pages join to the whole series.
	var seen []string
	q := url.Values{"group_by": {"model"}, "limit": {"1"}}
	for i := 0; ; i++ {
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+api.Prefix+"/usage/series?"+q.Encode(), nil)
		req.Header.Set("Authorization", "Bearer "+adminAll)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var pg struct {
			Items []struct {
				Group string `json:"group"`
			} `json:"items"`
			Next string `json:"next_cursor"`
		}
		_ = json.NewDecoder(res.Body).Decode(&pg)
		_ = res.Body.Close()
		for _, it := range pg.Items {
			seen = append(seen, it.Group)
		}
		if pg.Next == "" || i > 10 {
			break
		}
		q.Set("cursor", pg.Next)
	}
	if fmt.Sprint(seen) != "[model-a model-b model-s]" {
		t.Fatalf("pages %v", seen)
	}
	// Bounds: 50 values are accepted, 51 refused; a bad group_by and a bad cursor are refused.
	vals := make([]string, 51)
	for i := range vals {
		vals[i] = fmt.Sprint("m", i)
	}
	if st, _, body := get(viewerA, url.Values{"model": vals[:50]}); st != http.StatusOK {
		t.Fatalf("50 values: %d %s", st, body)
	}
	for name, q := range map[string]url.Values{
		"51 repeated":       {"model": vals},
		"51 comma":          {"account_id": {strings.Join(vals, ",")}},
		"51 spaces":         {"space_id": vals},
		"51 roles":          {"role": vals},
		"51 harnesses":      {"harness": vals},
		"group_by":          {"group_by": {"node"}},
		"cursor":            {"cursor": {"bogus"}},
		"hours over 31days": {"from": {now.Add(-40 * 24 * time.Hour).Format(time.RFC3339)}},
	} {
		if st, _, body := get(viewerA, q); st != http.StatusUnprocessableEntity && st != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, st, body)
		}
	}
	// 90 days of days is accepted.
	if st, _, body := get(viewerA, url.Values{"bucket": {"day"}, "from": {now.Add(-90 * 24 * time.Hour).Format(time.RFC3339)}}); st != http.StatusOK {
		t.Fatalf("90 days: %d %s", st, body)
	}
}
