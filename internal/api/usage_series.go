package api

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/usage"
)

// The cross-space usage series (ADR-0103, plan UI-T1). It needs only a session: every row, total, label and not-counted
// run is limited to the spaces where the caller has run:read, the get-account-usage rule.

// UsageFilterQuery is the optional filters of the cross-space series. Each takes repeated parameters or comma-separated
// values, at most usage.MaxFilterValues each.
type UsageFilterQuery struct {
	SpaceID   []string `query:"space_id,explode" doc:"Only these spaces (IDs; repeat or comma-separate, at most 50). A space you cannot read adds nothing"`
	AccountID []string `query:"account_id,explode" doc:"Only these accounts (IDs; repeat or comma-separate, at most 50)"`
	Model     []string `query:"model,explode" doc:"Only these models (repeat or comma-separate, at most 50)"`
	Role      []string `query:"role,explode" doc:"Only these roles (repeat or comma-separate, at most 50)"`
	Harness   []string `query:"harness,explode" doc:"Only these harnesses (repeat or comma-separate, at most 50)"`
}

func splitValues(in []string) []string {
	var out []string
	for _, v := range in {
		for _, p := range strings.Split(v, ",") {
			out = append(out, strings.TrimSpace(p))
		}
	}
	return out
}

func (q UsageFilterQuery) filter() usage.Filter {
	return usage.Filter{Spaces: splitValues(q.SpaceID), Accounts: splitValues(q.AccountID), Models: splitValues(q.Model), Roles: splitValues(q.Role),
		Harnesses: splitValues(q.Harness)}
}

// GlobalUsageSeriesPage is a page of the cross-space series.
type GlobalUsageSeriesPage struct {
	UsageSeriesPage
	Labels     map[string]string  `json:"labels" doc:"Display names of the space or account groups on this page: spaces you can read, accounts bound to one of them"`
	NotCounted []usage.NotCounted `json:"not_counted" doc:"Per harness, runs active in the range (in the same spaces and filters, except model) whose usage the ledger does not hold, such as Codex and Harbor Pilot runs. Their usage is missing from these figures, not zero"`
}

// readableScope is the set of spaces whose runs the principal may read: every space for a run:read grant in "*",
// else the spaces of its grants where it has run:read.
func (d *Deps) readableScope(p *auth.Principal) usage.Scope {
	if d.Authz == nil || p == nil {
		return usage.Scope{}
	}
	subs := p.Subjects()
	if d.Authz.AllowedAll(subs, authz.Global, authz.RunRead) {
		return usage.Scope{All: true}
	}
	var sc usage.Scope
	for _, s := range subs {
		for _, dom := range d.Authz.Domains(s) {
			if dom != authz.Global && !slices.Contains(sc.Spaces, dom) && d.Authz.AllowedAll(subs, dom, authz.RunRead) {
				sc.Spaces = append(sc.Spaces, dom)
			}
		}
	}
	return sc
}

// globalAfter decodes a series cursor (the positions pageSeries writes).
func globalAfter(c string) (*usage.After, error) {
	if c == "" {
		return nil, nil
	}
	k, id, err := DecodeCursor(c)
	if err != nil {
		return nil, err
	}
	start, err := strconv.ParseInt(k, 10, 64)
	if err != nil || !strings.HasPrefix(id, "g:") {
		return nil, apperr.New(apperr.CodeInvalid, "invalid cursor")
	}
	return &usage.After{Bucket: time.UnixMicro(start).UTC(), Group: id[2:]}, nil
}

func registerUsageSeries(svc *usage.Service, api huma.API, d *Deps) {
	huma.Register(api, huma.Operation{OperationID: "get-usage-series", Method: http.MethodGet, Path: "/usage/series",
		Summary: "Usage over time across the spaces you can read",
		Description: "Tokens per bucket and group (space, account, model, role or harness) from the usage ledger, summed over every space where " +
			"the caller has run:read; the rows, totals, labels and not-counted runs never include another space. Filters narrow it by space, " +
			"account, model, role and harness. The cost is the harness's own list-price estimate, not a bill; tokens are the primary unit. " +
			"Runs whose usage the ledger does not hold are counted per harness in not_counted.", Tags: []string{"usage"}},
		func(ctx context.Context, in *struct {
			UsageSeriesQuery
			UsageFilterQuery
			GroupBy string `query:"group_by" enum:"space,account,model,role,harness" default:"space"`
		}) (*struct{ Body *GlobalUsageSeriesPage }, error) {
			after, err := globalAfter(in.Cursor)
			if err != nil {
				return nil, d.fail(ctx, "usage.global_series", err)
			}
			limit := in.limit()
			s, err := svc.GlobalSeries(ctx, in.series(in.GroupBy), in.filter(), d.readableScope(PrincipalFrom(ctx)), after, limit)
			if err != nil {
				return nil, d.fail(ctx, "usage.global_series", err)
			}
			out := &GlobalUsageSeriesPage{UsageSeriesPage: UsageSeriesPage{Bucket: s.Bucket, GroupBy: s.GroupBy, From: s.From, To: s.To, CostNote: s.CostNote,
				Totals: s.Totals, Items: []usage.Row{}}, Labels: s.Labels, NotCounted: s.NotCounted}
			out.Items = append(out.Items, s.Rows...)
			if s.More && len(s.Rows) > 0 {
				out.NextCursor = EncodeCursor(s.Rows[len(s.Rows)-1].CursorKey())
			}
			return &struct{ Body *GlobalUsageSeriesPage }{Body: out}, nil
		})
}
