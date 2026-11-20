package usage

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// The read side of the usage ledger (ADR-0090, plan T7): usage over time per space and per account, and the latest
// rate-limit windows. Everything here is identifiers, names and numbers; no credential value is ever read.

// Bucket sizes and the longest range each one may ask for (a range bounds the rows a request reads).
const (
	maxHourSpan = 31 * 24 * time.Hour
	maxDaySpan  = 800 * 24 * time.Hour

	// CostBasis labels every cost figure: it is the harness's own list-price estimate, not a bill (Q12). Tokens are the
	// primary unit.
	CostBasis = "list-price estimate"
)

// Series groupings.
var (
	// SpaceGroups are the group_by values of a space's usage.
	SpaceGroups = []string{"role", "account", "model", "harness"}
	// AccountGroups are the group_by values of an account's usage.
	AccountGroups = []string{"space", "role", "model"}
)

// SeriesQuery selects a usage series.
type SeriesQuery struct {
	From, To time.Time
	Bucket   string
	GroupBy  string
}

// Row is the usage of one group in one bucket (or, in Series.Totals, in the whole range).
type Row struct {
	BucketStart      *time.Time `json:"bucket_start,omitempty" doc:"Start of the bucket, UTC"`
	Group            string     `json:"group" doc:"The value of the group_by field (an ID or name; empty when the usage had none, such as a run without an account)"`
	InputTokens      int64      `json:"input_tokens"`
	OutputTokens     int64      `json:"output_tokens"`
	CacheWriteTokens int64      `json:"cache_write_tokens"`
	CacheReadTokens  int64      `json:"cache_read_tokens"`
	ReasoningTokens  int64      `json:"reasoning_tokens"`
	TotalTokens      int64      `json:"total_tokens" doc:"Input, output, cache write and cache read tokens (reasoning tokens are reported separately)"`
	Requests         int64      `json:"requests"`
	TotalCostUSD     float64    `json:"total_cost_usd" doc:"The harness's own list-price estimate in USD, not a bill; tokens are the primary unit"`
}

func (r *Row) add(b db.UsageBucket) {
	r.InputTokens += b.InputTokens
	r.OutputTokens += b.OutputTokens
	r.CacheWriteTokens += b.CacheWriteTokens
	r.CacheReadTokens += b.CacheReadTokens
	r.ReasoningTokens += b.ReasoningTokens
	r.Requests += b.Requests
	r.TotalCostUSD += b.CostUsd
	r.TotalTokens = r.InputTokens + r.OutputTokens + r.CacheWriteTokens + r.CacheReadTokens
}

func (r *Row) addRow(o Row) {
	r.InputTokens += o.InputTokens
	r.OutputTokens += o.OutputTokens
	r.CacheWriteTokens += o.CacheWriteTokens
	r.CacheReadTokens += o.CacheReadTokens
	r.ReasoningTokens += o.ReasoningTokens
	r.Requests += o.Requests
	r.TotalCostUSD += o.TotalCostUSD
	r.TotalTokens = r.InputTokens + r.OutputTokens + r.CacheWriteTokens + r.CacheReadTokens
}

// Series is a usage series: every row of the range in order (bucket start, then group), and the totals of the range.
type Series struct {
	Bucket   string    `json:"bucket" enum:"hour,day"`
	GroupBy  string    `json:"group_by"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	CostNote string    `json:"cost_note" doc:"What the cost figures are: a list-price estimate, not a bill"`
	Totals   Row       `json:"totals"`
	Rows     []Row     `json:"-"`
}

// normalize validates the query and fills the defaults: the last 7 days (hour) or 30 days (day) up to now.
func (q SeriesQuery) normalize(now time.Time, groups []string) (SeriesQuery, error) {
	if q.Bucket == "" {
		q.Bucket = GranHour
	}
	if q.Bucket != GranHour && q.Bucket != GranDay {
		return q, apperr.New(apperr.CodeInvalid, "bucket must be hour or day")
	}
	if !slices.Contains(groups, q.GroupBy) {
		return q, apperr.New(apperr.CodeInvalid, "group_by must be one of %s", joinComma(groups))
	}
	if q.To.IsZero() {
		q.To = now
	}
	if q.From.IsZero() {
		if q.Bucket == GranDay {
			q.From = q.To.Add(-30 * 24 * time.Hour)
		} else {
			q.From = q.To.Add(-7 * 24 * time.Hour)
		}
	}
	q.From, q.To = q.From.UTC(), q.To.UTC()
	if q.Bucket == GranDay {
		// Whole UTC days, so a day rolled up from hours and a day not yet rolled up count the same hours.
		q.From = q.From.Truncate(24 * time.Hour)
		if t := q.To.Truncate(24 * time.Hour); t.Before(q.To) {
			q.To = t.Add(24 * time.Hour)
		}
	}
	if !q.From.Before(q.To) {
		return q, apperr.New(apperr.CodeInvalid, "from must be before to")
	}
	span := maxHourSpan
	if q.Bucket == GranDay {
		span = maxDaySpan
	}
	if q.To.Sub(q.From) > span {
		return q, apperr.New(apperr.CodeInvalid, "the range is too long for bucket=%s", q.Bucket)
	}
	return q, nil
}

func joinComma(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ", "
		}
		out += v
	}
	return out
}

// groupOf is the value a bucket is grouped under.
func groupOf(b db.UsageBucket, by string) string {
	switch by {
	case "space":
		return b.SpaceID
	case "role":
		return b.Role
	case "account":
		return b.AccountID
	case "model":
		return b.Model
	}
	return b.Harness
}

// fold sums buckets into rows per (bucket start, group). Hourly rows are the only source of an hourly series; a daily
// series adds the hourly rows (not yet rolled up) into their UTC day and the rolled-up daily rows.
func fold(rows []db.UsageBucket, q SeriesQuery, keep func(db.UsageBucket) bool) *Series {
	type key struct {
		start int64
		group string
	}
	sums := map[key]*Row{}
	s := &Series{Bucket: q.Bucket, GroupBy: q.GroupBy, From: q.From, To: q.To, CostNote: CostBasis}
	for _, b := range rows {
		if keep != nil && !keep(b) {
			continue
		}
		start := b.BucketStart
		if q.Bucket == GranDay && b.Granularity == GranHour {
			start = time.UnixMicro(start).UTC().Truncate(24 * time.Hour).UnixMicro()
		}
		g := groupOf(b, q.GroupBy)
		r := sums[key{start, g}]
		if r == nil {
			t := time.UnixMicro(start).UTC()
			r = &Row{BucketStart: &t, Group: g}
			sums[key{start, g}] = r
		}
		r.add(b)
		s.Totals.add(b)
	}
	for _, r := range sums {
		s.Rows = append(s.Rows, *r)
	}
	slices.SortFunc(s.Rows, func(a, b Row) int {
		if c := a.BucketStart.Compare(*b.BucketStart); c != 0 {
			return c
		}
		return compareStr(a.Group, b.Group)
	})
	return s
}

func compareStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func granularities(bucket string) []string {
	if bucket == GranDay {
		return []string{GranHour, GranDay}
	}
	return []string{GranHour}
}

// SpaceSeries is a space's usage over the range, grouped by role, account, model or harness.
func (svc *Service) SpaceSeries(ctx context.Context, spaceID string, q SeriesQuery) (*Series, error) {
	q, err := q.normalize(svc.s.Now(), SpaceGroups)
	if err != nil {
		return nil, err
	}
	var all []db.UsageBucket
	for _, g := range granularities(q.Bucket) {
		rows, err := svc.s.R().ListUsageBuckets(ctx, db.ListUsageBucketsParams{SpaceID: spaceID, Granularity: g, BucketStart: q.From.UnixMicro(), BucketStart_2: q.To.UnixMicro()})
		if err != nil {
			return nil, err
		}
		all = append(all, rows...)
	}
	return fold(all, q, nil), nil
}

// AccountSeries is an account's usage over the range, grouped by space, role or model. Only buckets of spaces for which
// readable is true are counted, so the totals never include a space the caller may not see. The account must exist.
func (svc *Service) AccountSeries(ctx context.Context, accountID string, q SeriesQuery, readable func(spaceID string) bool) (*Series, error) {
	q, err := q.normalize(svc.s.Now(), AccountGroups)
	if err != nil {
		return nil, err
	}
	if _, err := svc.s.R().GetAccount(ctx, accountID); errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.New(apperr.CodeNotFound, "account not found")
	} else if err != nil {
		return nil, err
	}
	var all []db.UsageBucket
	for _, g := range granularities(q.Bucket) {
		rows, err := svc.s.R().ListAccountUsageBuckets(ctx, db.ListAccountUsageBucketsParams{AccountID: accountID, Granularity: g, BucketStart: q.From.UnixMicro(), BucketStart_2: q.To.UnixMicro()})
		if err != nil {
			return nil, err
		}
		all = append(all, rows...)
	}
	seen := map[string]bool{}
	return fold(all, q, func(b db.UsageBucket) bool {
		ok, known := seen[b.SpaceID]
		if !known {
			ok = readable(b.SpaceID)
			seen[b.SpaceID] = ok
		}
		return ok
	}), nil
}

// Window is an account's latest rate-limit window.
type Window struct {
	Kind       string     `json:"kind" enum:"five_hour,seven_day"`
	UsedPct    float64    `json:"used_pct" doc:"Percent of the window used when it was observed"`
	ResetsAt   *time.Time `json:"resets_at,omitempty"`
	ObservedAt time.Time  `json:"observed_at" doc:"When a run last reported it; an old observation may be out of date"`
}

// AccountWindows is one account's latest windows.
type AccountWindows struct {
	AccountID string   `json:"account_id"`
	Name      string   `json:"name"`
	Harness   string   `json:"harness,omitempty"`
	Windows   []Window `json:"windows"`
}

func windowOf(kind string, used float64, resets sql.NullInt64, observed int64) Window {
	w := Window{Kind: kind, UsedPct: used, ObservedAt: time.UnixMicro(observed).UTC()}
	if resets.Valid {
		t := time.UnixMicro(resets.Int64).UTC()
		w.ResetsAt = &t
	}
	return w
}

// AccountSpaces returns the IDs of the spaces an account is bound to.
func (svc *Service) AccountSpaces(ctx context.Context, accountID string) ([]string, error) {
	return svc.s.R().ListAccountBindingSpaces(ctx, accountID)
}

// WindowsOf returns an account's latest windows (not found when the account does not exist).
func (svc *Service) WindowsOf(ctx context.Context, accountID string) (*AccountWindows, error) {
	a, err := svc.s.R().GetAccount(ctx, accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.New(apperr.CodeNotFound, "account not found")
	} else if err != nil {
		return nil, err
	}
	ws, err := svc.s.R().ListAccountWindows(ctx, accountID)
	if err != nil {
		return nil, err
	}
	out := &AccountWindows{AccountID: a.ID, Name: a.Name, Windows: []Window{}}
	for _, w := range ws {
		out.Windows = append(out.Windows, windowOf(w.Kind, w.UsedPct, w.ResetsAt, w.ObservedAt))
	}
	return out, nil
}

// SpaceWindows returns the latest windows of the accounts bound to a space, one entry per bound account (an account
// with no observation has no windows).
func (svc *Service) SpaceWindows(ctx context.Context, spaceID string) ([]AccountWindows, error) {
	bs, err := svc.s.R().ListSpaceAccounts(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	rows, err := svc.s.R().ListSpaceAccountWindows(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := []AccountWindows{}
	idx := map[string]int{}
	for _, b := range bs {
		if _, dup := idx[b.AccountID]; dup {
			continue
		}
		idx[b.AccountID] = len(out)
		out = append(out, AccountWindows{AccountID: b.AccountID, Harness: b.Harness, Windows: []Window{}})
	}
	for _, r := range rows {
		i, ok := idx[r.AccountID]
		if !ok {
			continue
		}
		out[i].Name = r.AccountName
		out[i].Windows = append(out[i].Windows, windowOf(r.Kind, r.UsedPct, r.ResetsAt, r.ObservedAt))
	}
	for i := range out {
		if out[i].Name == "" {
			a, err := svc.s.R().GetAccount(ctx, out[i].AccountID)
			if err != nil {
				return nil, err
			}
			out[i].Name = a.Name
		}
	}
	return out, nil
}

// CursorKey is the position of a row, for pagination: the bucket start and the group.
func (r Row) CursorKey() (string, string) {
	return strconv.FormatInt(r.BucketStart.UnixMicro(), 10), "g:" + r.Group
}

// Limit is an open usage limit on an account (ADR-0079): the hold applies in every space bound to the account.
type Limit struct {
	Kind     string     `json:"kind"`
	ResetsAt *time.Time `json:"resets_at,omitempty"`
	SeenAt   time.Time  `json:"seen_at" doc:"When a run first reported the limit"`
}

// AccountState is one account's windows and open limits, for the cross-space view.
type AccountState struct {
	AccountID string   `json:"account_id"`
	Name      string   `json:"name"`
	Harness   string   `json:"harness,omitempty"`
	Windows   []Window `json:"windows" doc:"The latest 5-hour and 7-day windows; empty when no run has reported one"`
	Limits    []Limit  `json:"limits" doc:"Open usage limits, oldest first; empty when none"`
}

// AllAccountStates returns every account bound (to a space or a role pool) to at least one space that visible accepts,
// once each, with its latest windows and open limits. It reads stored state only: no network and no probe. An account
// whose bindings are all in other spaces is absent, so its name is never disclosed.
func (svc *Service) AllAccountStates(ctx context.Context, visible func(spaceID string) bool) ([]AccountState, error) {
	r := svc.s.R()
	bs, err := r.ListAllAccountBindings(ctx)
	if err != nil {
		return nil, err
	}
	out := []AccountState{}
	idx := map[string]int{}
	for _, b := range bs {
		if !visible(b.SpaceID) {
			continue
		}
		i, ok := idx[b.AccountID]
		if !ok {
			i = len(out)
			idx[b.AccountID] = i
			out = append(out, AccountState{AccountID: b.AccountID, Name: b.AccountName, Windows: []Window{}, Limits: []Limit{}})
		}
		if out[i].Harness == "" {
			out[i].Harness = b.Harness
		}
	}
	if len(out) == 0 {
		return out, nil
	}
	ws, err := r.ListAllAccountWindows(ctx)
	if err != nil {
		return nil, err
	}
	for _, w := range ws {
		if i, ok := idx[w.AccountID]; ok {
			out[i].Windows = append(out[i].Windows, windowOf(w.Kind, w.UsedPct, w.ResetsAt, w.ObservedAt))
		}
	}
	ls, err := r.ListAllOpenAccountUsageLimits(ctx)
	if err != nil {
		return nil, err
	}
	for _, l := range ls {
		i, ok := idx[l.AccountID]
		if !ok {
			continue
		}
		lim := Limit{Kind: l.Kind, SeenAt: time.UnixMicro(l.SeenAt).UTC()}
		if l.ResetsAt.Valid {
			t := time.UnixMicro(l.ResetsAt.Int64).UTC()
			lim.ResetsAt = &t
		}
		// A limit is stored once per space that saw it (ADR-0079), but the hold is the account's: list each kind once,
		// so the count of spaces (some of them perhaps unreadable to the caller) isn't disclosed. Rows come oldest first,
		// so the first sighting is kept, with the latest known reset.
		if j := slices.IndexFunc(out[i].Limits, func(x Limit) bool { return x.Kind == lim.Kind }); j >= 0 {
			if have := out[i].Limits[j].ResetsAt; lim.ResetsAt != nil && (have == nil || lim.ResetsAt.After(*have)) {
				out[i].Limits[j].ResetsAt = lim.ResetsAt
			}
			continue
		}
		out[i].Limits = append(out[i].Limits, lim)
	}
	return out, nil
}
