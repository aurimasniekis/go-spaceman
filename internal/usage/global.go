package usage

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// The cross-space usage series (get-usage-series, ADR-0103, plan UI-T1): the ledger summed in SQL per bucket and group,
// over the spaces the caller may read. A range across every space can hold hundreds of thousands of bucket rows, so
// nothing here loads them: SQL returns one page of (bucket, group) sums and the totals of the range.

// GlobalGroups are the group_by values of the cross-space series.
var GlobalGroups = []string{"space", "account", "model", "role", "harness"}

// Filter bounds.
const (
	// MaxFilterValues is the most values one filter list may hold.
	MaxFilterValues = 50
	// maxFilterValueLen is the longest value a filter may name (IDs, model, role and harness names are far shorter).
	maxFilterValueLen = 200
)

// Filter narrows a cross-space series. An empty list does not filter; the lists are ANDed, the values in a list ORed.
type Filter struct {
	Spaces, Accounts, Models, Roles, Harnesses []string
}

// Scope is the set of spaces a caller may read usage of: every space (All), or only Spaces. An empty, not All scope
// reads nothing.
type Scope struct {
	All    bool
	Spaces []string
}

// After is a page position: the last (bucket start, group) a previous page returned.
type After struct {
	Bucket time.Time
	Group  string
}

// NotCounted is the number of runs of one harness, active in the range, whose usage the ledger does not hold.
type NotCounted struct {
	Harness         string `json:"harness"`
	Runs            int64  `json:"runs" doc:"Runs active in the range whose usage is not in these figures"`
	HarborPilotRuns int64  `json:"harbor_pilot_runs" doc:"Of those, runs whose usage was read back from Harbor Pilot (per run only, not in the ledger)"`
}

// GlobalPage is one page of a cross-space series, with the totals of the whole range.
type GlobalPage struct {
	Series
	// More is true when rows follow the last one in Rows.
	More bool
	// Labels names the space and account groups of Rows (only spaces in the scope, and accounts bound to one of them).
	Labels map[string]string
	// NotCounted lists, per harness, the runs in the range whose usage is not in the ledger (in the scope and filters).
	NotCounted []NotCounted
}

func cleanList(name string, in []string) ([]string, error) {
	var out []string
	for _, v := range in {
		if v == "" && name != "account" {
			continue
		}
		if len(v) > maxFilterValueLen {
			return nil, apperr.New(apperr.CodeInvalid, "a %s filter value is too long", name)
		}
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	if len(out) > MaxFilterValues {
		return nil, apperr.New(apperr.CodeInvalid, "at most %d %s filter values", MaxFilterValues, name)
	}
	return out, nil
}

// normalize deduplicates the filter lists and enforces the bounds.
func (f Filter) normalize() (Filter, error) {
	var err error
	out := Filter{}
	for _, l := range []struct {
		name string
		in   []string
		out  *[]string
	}{{"space", f.Spaces, &out.Spaces}, {"account", f.Accounts, &out.Accounts}, {"model", f.Models, &out.Models}, {"role", f.Roles, &out.Roles},
		{"harness", f.Harnesses, &out.Harnesses}} {
		if *l.out, err = cleanList(l.name, l.in); err != nil {
			return Filter{}, err
		}
	}
	return out, nil
}

// jsonList is a filter list as the queries take it: a JSON array, or nil (no filter) when the list is empty.
func jsonList(l []string) any {
	if len(l) == 0 {
		return nil
	}
	b, _ := json.Marshal(l)
	return string(b)
}

// spaceList is the space filter of the queries: nil (every space) only when the caller reads every space and the filter
// names none, else the readable spaces the filter keeps. ok is false when that leaves no space, so nothing is read.
func spaceList(sc Scope, named []string) (any, bool) {
	if sc.All {
		return jsonList(named), true
	}
	keep := sc.Spaces
	if len(named) > 0 {
		keep = nil
		for _, s := range named {
			if slices.Contains(sc.Spaces, s) {
				keep = append(keep, s)
			}
		}
	}
	if len(keep) == 0 {
		return nil, false
	}
	b, _ := json.Marshal(keep)
	return string(b), true
}

// GlobalSeries is one page of the usage of the spaces in scope, grouped by space, account, model, role or harness. A
// space outside the scope never contributes a row, a total, a label or a not-counted run, whatever the filter names.
func (svc *Service) GlobalSeries(ctx context.Context, q SeriesQuery, f Filter, sc Scope, after *After, limit int) (*GlobalPage, error) {
	q, err := q.normalize(svc.s.Now(), GlobalGroups)
	if err != nil {
		return nil, err
	}
	if f, err = f.normalize(); err != nil {
		return nil, err
	}
	if limit < 1 {
		limit = 1
	}
	out := &GlobalPage{Series: Series{Bucket: q.Bucket, GroupBy: q.GroupBy, From: q.From, To: q.To, CostNote: CostBasis}, Labels: map[string]string{}, NotCounted: []NotCounted{}}
	spaces, ok := spaceList(sc, f.Spaces)
	if !ok {
		return out, nil
	}
	grans, _ := json.Marshal(granularities(q.Bucket))
	bucketUS := int64(1)
	if q.Bucket == GranDay {
		bucketUS = (24 * time.Hour).Microseconds()
	}
	from, to := q.From.UnixMicro(), q.To.UnixMicro()
	p := db.SumUsageSeriesParams{BucketUs: bucketUS, GroupBy: q.GroupBy, Grans: string(grans), Spaces: spaces, Accounts: jsonList(f.Accounts),
		Models: jsonList(f.Models), Roles: jsonList(f.Roles), Harnesses: jsonList(f.Harnesses), AfterBucket: -1}
	lo := from
	if after != nil {
		p.AfterBucket, p.AfterGroup = after.Bucket.UnixMicro(), after.Group
		// Rows before the cursor's bucket only fall into earlier buckets, so the scan starts at that bucket.
		lo = max(lo, p.AfterBucket-((p.AfterBucket%bucketUS)+bucketUS)%bucketUS)
	}
	r := svc.s.R()
	// The totals of the range are summed while the page is read.
	totals := svc.startTotals(ctx, db.SumUsageTotalsParams{Grans: p.Grans, FromUs: from, ToUs: to, Spaces: p.Spaces, Accounts: p.Accounts,
		Models: p.Models, Roles: p.Roles, Harnesses: p.Harnesses})
	// A page is read in windows of whole buckets that double in length until it is full: SQLite sorts each window's
	// groups in a temporary B-tree, so a page sorts about as many rows as it returns, not the whole range's.
	var rows []db.SumUsageSeriesRow
	for step := seriesWindow(q.Bucket); lo < to; step *= 2 {
		hi := min(lo+step, to)
		p.FromUs, p.ToUs, p.Lim = lo, hi, int64(limit-len(rows))+1
		got, err := r.SumUsageSeries(ctx, p)
		if err != nil {
			_, _ = totals()
			return nil, err
		}
		rows = append(rows, got...)
		if len(rows) > limit {
			break
		}
		lo = hi
	}
	if len(rows) > limit {
		rows, out.More = rows[:limit], true
	}
	for _, g := range rows {
		t := time.UnixMicro(g.Bucket).UTC()
		row := Row{BucketStart: &t, Group: g.Grp, InputTokens: g.InputTokens, OutputTokens: g.OutputTokens, CacheWriteTokens: g.CacheWriteTokens,
			CacheReadTokens: g.CacheReadTokens, ReasoningTokens: g.ReasoningTokens, Requests: g.Requests, TotalCostUSD: g.CostUsd}
		row.TotalTokens = row.InputTokens + row.OutputTokens + row.CacheWriteTokens + row.CacheReadTokens
		out.Rows = append(out.Rows, row)
	}
	if out.Totals, err = totals(); err != nil {
		return nil, err
	}
	if err := svc.labels(ctx, out, spaces); err != nil {
		return nil, err
	}
	nc, err := r.CountUnledgeredRuns(ctx, db.CountUnledgeredRunsParams{ToUs: to, FromUs: from, Spaces: p.Spaces, Accounts: p.Accounts, Roles: p.Roles,
		Harnesses: p.Harnesses})
	if err != nil {
		return nil, err
	}
	for _, n := range nc {
		out.NotCounted = append(out.NotCounted, NotCounted{Harness: n.Harness, Runs: n.Runs, HarborPilotRuns: n.HarborPilotRuns})
	}
	return out, nil
}

// totalSlices is how many parallel reads sum a range's totals. Every ledger row of the range is read (index only), so
// on a large range the sum is the slowest part of a page; reader connections run in parallel.
const totalSlices = 4

// startTotals starts summing the totals of the range, in totalSlices parallel reads over consecutive parts of it, and
// returns a function that waits for them. The caller must call it before returning.
func (svc *Service) startTotals(ctx context.Context, p db.SumUsageTotalsParams) func() (Row, error) {
	var (
		wg   sync.WaitGroup
		res  [totalSlices]db.SumUsageTotalsRow
		errs [totalSlices]error
	)
	span := p.ToUs - p.FromUs
	for i := range totalSlices {
		q := p
		q.FromUs, q.ToUs = p.FromUs+span*int64(i)/totalSlices, p.FromUs+span*int64(i+1)/totalSlices
		wg.Go(func() { res[i], errs[i] = svc.s.R().SumUsageTotals(ctx, q) })
	}
	return func() (Row, error) {
		wg.Wait()
		var out Row
		for i := range totalSlices {
			if errs[i] != nil {
				return Row{}, errs[i]
			}
			t := res[i]
			out.addRow(Row{InputTokens: t.InputTokens, OutputTokens: t.OutputTokens, CacheWriteTokens: t.CacheWriteTokens, CacheReadTokens: t.CacheReadTokens,
				ReasoningTokens: t.ReasoningTokens, Requests: t.Requests, TotalCostUSD: t.CostUsd})
		}
		return out, nil
	}
}

// seriesWindow is the first window a page reads: a day of hourly buckets, or 8 daily ones.
func seriesWindow(bucket string) int64 {
	if bucket == GranDay {
		return (8 * 24 * time.Hour).Microseconds()
	}
	return (24 * time.Hour).Microseconds()
}

// labels names the page's space or account groups. The rows only hold spaces in scope; an account is named only when it
// is bound to a space in scope (the get-account-windows rule), so a name never reaches a caller through usage alone.
func (svc *Service) labels(ctx context.Context, p *GlobalPage, spaces any) error {
	if p.GroupBy != "space" && p.GroupBy != "account" {
		return nil
	}
	var ids []string
	for _, r := range p.Rows {
		if r.Group != "" && !slices.Contains(ids, r.Group) {
			ids = append(ids, r.Group)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	b, _ := json.Marshal(ids)
	if p.GroupBy == "space" {
		rows, err := svc.s.R().ListSpaceNamesByID(ctx, string(b))
		if err != nil {
			return err
		}
		for _, r := range rows {
			p.Labels[r.ID] = r.Name
		}
		return nil
	}
	rows, err := svc.s.R().ListBoundAccountNames(ctx, db.ListBoundAccountNamesParams{Ids: string(b), Spaces: spaces})
	if err != nil {
		return err
	}
	for _, r := range rows {
		p.Labels[r.ID] = r.Name
	}
	return nil
}
