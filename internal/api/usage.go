package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/usage"
)

// UsageRules are the authorization rules of the usage and transcript operations. Transcripts are as sensitive as
// recordings: reading needs recording:read, deleting recording:delete with a recent re-authentication and MFA.
var UsageRules = map[string]Rule{
	"get-run-usage":   InSpace(authz.RunRead),
	"get-space-usage": InSpace(authz.RunRead),
	// The cross-space series (ADR-0103) filters every row to the spaces with run:read, so it needs only a session.
	"get-usage-series": FilteredRead(),
	// The usage ledger (ADR-0090). A space's views need run:read there. An account's views are global (account:read) and
	// show only what the caller can also read in a space: usage of the spaces they have run:read in, and windows only
	// of an account bound to such a space.
	"get-space-usage-series":     InSpace(authz.RunRead),
	"list-space-account-windows": InSpace(authz.RunRead),
	// The cross-space list is filtered per account by canSeeAccount (run:read in a space the account is bound to).
	"list-account-windows": FilteredRead(),
	"get-account-usage":    Global(authz.AccountRead),
	"get-account-windows":  Global(authz.AccountRead),
	// Deleting a transcript is as irreversible as deleting a recording, so it needs the same step-up (SR-L06).
	"delete-transcript": {Perm: authz.RecordingDelete, Space: true, Reauth: true, MFA: true},
}

// UsageSeriesQuery is the range and grouping of a usage series.
type UsageSeriesQuery struct {
	From   time.Time `query:"from" format:"date-time" doc:"Start of the range, inclusive (RFC 3339). Default: 7 days (bucket=hour) or 30 days (bucket=day) before to"`
	To     time.Time `query:"to" format:"date-time" doc:"End of the range, exclusive (RFC 3339). Default: now"`
	Bucket string    `query:"bucket" enum:"hour,day" default:"hour" doc:"Bucket size, UTC. Hourly rows are kept 90 days, so older ranges need bucket=day; a daily series covers whole UTC days; a range is at most 31 days of hours or 800 days"`
	Page
}

// UsageSeriesPage is a page of a usage series with the totals of the whole range.
type UsageSeriesPage struct {
	Bucket     string      `json:"bucket" enum:"hour,day"`
	GroupBy    string      `json:"group_by"`
	From       time.Time   `json:"from"`
	To         time.Time   `json:"to"`
	CostNote   string      `json:"cost_note" doc:"What the cost figures are: a list-price estimate, not a bill. Tokens are the primary unit"`
	Totals     usage.Row   `json:"totals" doc:"Over the whole range, not only this page"`
	Items      []usage.Row `json:"items" doc:"One row per bucket and group, oldest first"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// SpaceUsageSeriesPage is a page of a space's series, with the runs of the space the ledger cannot count.
type SpaceUsageSeriesPage struct {
	UsageSeriesPage
	NotCounted []usage.NotCounted `json:"not_counted" doc:"Per harness, runs of this space active in the range whose usage the ledger does not hold, such as Codex and Harbor Pilot runs. Their usage is missing from these figures, not zero"`
}

func (q UsageSeriesQuery) series(groupBy string) usage.SeriesQuery {
	return usage.SeriesQuery{From: q.From, To: q.To, Bucket: q.Bucket, GroupBy: groupBy}
}

// pageSeries cuts one page out of a series (cursors are positions after a row, so a page never repeats or skips rows).
func pageSeries(s *usage.Series, p Page) (*UsageSeriesPage, error) {
	k, id, err := DecodeCursor(p.Cursor)
	if err != nil {
		return nil, err
	}
	out := &UsageSeriesPage{Bucket: s.Bucket, GroupBy: s.GroupBy, From: s.From, To: s.To, CostNote: s.CostNote, Totals: s.Totals, Items: []usage.Row{}}
	var afterStart int64
	if p.Cursor != "" {
		if afterStart, err = strconv.ParseInt(k, 10, 64); err != nil || len(id) < 2 || id[:2] != "g:" {
			return nil, apperr.New(apperr.CodeInvalid, "invalid cursor")
		}
	}
	limit := p.limit()
	for _, r := range s.Rows {
		if p.Cursor != "" {
			rk := r.BucketStart.UnixMicro()
			if rk < afterStart || rk == afterStart && "g:"+r.Group <= id {
				continue
			}
		}
		if len(out.Items) == limit {
			last := out.Items[len(out.Items)-1]
			ck, cid := last.CursorKey()
			out.NextCursor = EncodeCursor(ck, cid)
			break
		}
		out.Items = append(out.Items, r)
	}
	return out, nil
}

// AccountWindowsList is the accounts bound to the spaces the caller can read, with their windows and open limits.
type AccountWindowsList struct {
	Items []usage.AccountState `json:"items"`
}

// SpaceAccountWindows is the latest windows of a space's bound accounts.
type SpaceAccountWindows struct {
	Items []usage.AccountWindows `json:"items"`
}

// RegisterUsage registers the usage operations.
func RegisterUsage(svc *usage.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"usage"}
		registerUsageSeries(svc, api, d)
		huma.Register(api, huma.Operation{OperationID: "get-run-usage", Method: http.MethodGet, Path: "/spaces/{space_id}/runs/{run_id}/usage",
			Summary: "A run's usage", Description: "Only what the harness recorded; unknown usage is reported as unknown, never zero.", Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				RunID string `path:"run_id" maxLength:"64"`
			}) (*struct{ Body *usage.Usage }, error) {
				u, err := svc.ForRun(ctx, in.SpaceID, in.RunID)
				if err != nil {
					return nil, d.fail(ctx, "usage.run", err)
				}
				return &struct{ Body *usage.Usage }{Body: u}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-space-usage", Method: http.MethodGet, Path: "/spaces/{space_id}/usage",
			Summary: "A space's usage", Description: "Totals cover runs with known usage only; runs with unknown usage are counted separately.", Tags: tags},
			func(ctx context.Context, in *SpacePath) (*struct{ Body *usage.Summary }, error) {
				s, err := svc.Space(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "usage.space", err)
				}
				return &struct{ Body *usage.Summary }{Body: s}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-space-usage-series", Method: http.MethodGet, Path: "/spaces/{space_id}/usage/series",
			Summary: "A space's usage over time",
			Description: "Tokens per bucket and group (role, account, model or harness) from the usage ledger, with the cost as total_cost_usd: the " +
				"harness's own list-price estimate, not a bill. Tokens are the primary unit. Counts only what runs reported; runs whose usage the ledger " +
				"does not hold are counted per harness in not_counted.", Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				UsageSeriesQuery
				GroupBy string `query:"group_by" enum:"role,account,model,harness" default:"role"`
			}) (*struct{ Body *SpaceUsageSeriesPage }, error) {
				s, err := svc.SpaceSeries(ctx, in.SpaceID, in.series(in.GroupBy))
				if err != nil {
					return nil, d.fail(ctx, "usage.series", err)
				}
				pg, err := pageSeries(s, in.Page)
				if err != nil {
					return nil, d.fail(ctx, "usage.series", err)
				}
				nc, err := svc.SpaceNotCounted(ctx, in.SpaceID, in.series(in.GroupBy))
				if err != nil {
					return nil, d.fail(ctx, "usage.series", err)
				}
				return &struct{ Body *SpaceUsageSeriesPage }{Body: &SpaceUsageSeriesPage{UsageSeriesPage: *pg, NotCounted: nc}}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-account-usage", Method: http.MethodGet, Path: "/accounts/{account_id}/usage",
			Summary: "An account's usage over time, in the spaces you can read",
			Description: "Like a space's series, per space, role or model. Accounts are shared by spaces, so only the spaces the caller can read " +
				"(run:read) are counted: the rows and the totals never include another space.", Tags: []string{"accounts", "usage"}},
			func(ctx context.Context, in *struct {
				AccountPath
				UsageSeriesQuery
				GroupBy string `query:"group_by" enum:"space,role,model" default:"space"`
			}) (*struct{ Body *UsageSeriesPage }, error) {
				p := PrincipalFrom(ctx)
				s, err := svc.AccountSeries(ctx, in.AccountID, in.series(in.GroupBy), func(spaceID string) bool { return d.canReadRuns(p, spaceID) })
				if err != nil {
					return nil, d.fail(ctx, "usage.account_series", err)
				}
				pg, err := pageSeries(s, in.Page)
				if err != nil {
					return nil, d.fail(ctx, "usage.account_series", err)
				}
				return &struct{ Body *UsageSeriesPage }{Body: pg}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-account-windows", Method: http.MethodGet, Path: "/accounts/{account_id}/windows",
			Summary: "An account's latest rate-limit windows",
			Description: "The 5-hour and 7-day windows as the last run on the account reported them. Not found unless the account is bound to a " +
				"space the caller can read (run:read).", Tags: []string{"accounts", "usage"}},
			func(ctx context.Context, in *AccountPath) (*struct{ Body *usage.AccountWindows }, error) {
				spaces, err := svc.AccountSpaces(ctx, in.AccountID)
				if err != nil {
					return nil, d.fail(ctx, "usage.windows", err)
				}
				p := PrincipalFrom(ctx)
				visible := false
				for _, sp := range spaces {
					if d.canReadRuns(p, sp) {
						visible = true
						break
					}
				}
				if !visible {
					return nil, d.fail(ctx, "usage.windows", apperr.New(apperr.CodeNotFound, "account not found"))
				}
				w, err := svc.WindowsOf(ctx, in.AccountID)
				if err != nil {
					return nil, d.fail(ctx, "usage.windows", err)
				}
				return &struct{ Body *usage.AccountWindows }{Body: w}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-space-account-windows", Method: http.MethodGet, Path: "/spaces/{space_id}/account-windows",
			Summary: "The latest rate-limit windows of the accounts bound to a space", Tags: tags},
			func(ctx context.Context, in *SpacePath) (*struct{ Body SpaceAccountWindows }, error) {
				l, err := svc.SpaceWindows(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "usage.space_windows", err)
				}
				return &struct{ Body SpaceAccountWindows }{Body: SpaceAccountWindows{Items: l}}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-account-windows", Method: http.MethodGet, Path: "/account-windows",
			Summary: "The usage windows and open limits of the accounts bound to the spaces you can read",
			Description: "Each account bound (to a space or a role pool) to at least one space the caller can read (run:read), once: its name, " +
				"harness, latest 5-hour and 7-day windows and open usage limits. Stored state only: nothing is fetched from a provider. " +
				"An account bound only to other spaces is absent.", Tags: []string{"accounts", "usage"}},
			func(ctx context.Context, _ *struct{}) (*struct{ Body AccountWindowsList }, error) {
				p := PrincipalFrom(ctx)
				l, err := svc.AllAccountStates(ctx, func(spaceID string) bool { return d.canSeeAccount(p, spaceID) })
				if err != nil {
					return nil, d.fail(ctx, "usage.account_windows", err)
				}
				return &struct{ Body AccountWindowsList }{Body: AccountWindowsList{Items: l}}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "delete-transcript", Method: http.MethodDelete, Path: "/spaces/{space_id}/runs/{run_id}/transcript",
			Summary: "Delete a run's stored transcript", Tags: tags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *struct {
				SpacePath
				RunID string `path:"run_id" maxLength:"64"`
			}) (*struct{}, error) {
				if err := svc.DeleteTranscript(ctx, Actor(ctx), in.SpaceID, in.RunID); err != nil {
					return nil, d.fail(ctx, "usage.transcript_delete", err)
				}
				return &struct{}{}, nil
			})
	}
}

// TranscriptDownload serves a run's stored native transcript (may contain secrets; download only, never rendered).
func TranscriptDownload(svc *usage.Service) RawRoute {
	return RawRoute{Pattern: "GET " + Prefix + "/spaces/{space_id}/runs/{run_id}/transcript", Perm: authz.RecordingRead,
		Telegram: TelegramExcluded,
		Handler: func(d *Deps) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				f, t, err := svc.OpenTranscript(r.Context(), r.PathValue("space_id"), r.PathValue("run_id"))
				if err != nil {
					writeProblem(w, newProblem(http.StatusNotFound, "not_found", "no stored transcript", nil))
					return
				}
				defer func() { _ = f.Close() }()
				h := w.Header()
				h.Set("Content-Type", "application/octet-stream")
				h.Set("Content-Disposition", "attachment; filename=\""+r.PathValue("run_id")+"-transcript.jsonl\"")
				h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
				h.Set("X-Content-Type-Options", "nosniff")
				h.Set("Cache-Control", "private, no-store")
				h.Set("ETag", `"`+t.SHA256+`"`)
				http.ServeContent(w, r, "", f.Info().ModTime, f)
			}
		}}
}

// canReadRuns reports whether the principal may read runs (and so their usage) in a space.
func (d *Deps) canReadRuns(p *auth.Principal, spaceID string) bool {
	return d.Authz != nil && p != nil && d.Authz.AllowedAll(p.Subjects(), spaceID, authz.RunRead)
}

// canSeeAccount is the one visibility rule of the cross-space account view: an account, its name and its limit state
// are shown when it is bound to a space where the caller may read runs. Making the view admin-only is a change to this
// function (and to the rule above), nothing else.
func (d *Deps) canSeeAccount(p *auth.Principal, boundSpaceID string) bool {
	return d.canReadRuns(p, boundSpaceID)
}
