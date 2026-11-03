package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/retention"
	"github.com/aurimasniekis/go-spaceman/internal/spaceexport"
)

// RetentionRules are the authorization rules of retention and space export (ADR-0045). Running a pass deletes data,
// so it needs a recent step-up like other destructive administration; an export holds a space's data in plain text,
// so only space admins (space:export) may make one.
var RetentionRules = map[string]Rule{
	"get-retention":    Global(authz.SystemRead),
	"run-retention":    StepUp(authz.SystemWrite),
	"get-space-export": InSpace(authz.SpaceExport),
}

// RegisterRetention registers the retention and space export operations.
func RegisterRetention(ret *retention.Service, exp *spaceexport.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		huma.Register(api, huma.Operation{OperationID: "get-retention", Method: http.MethodGet, Path: "/retention",
			Summary: "Retention: installation defaults, audit retention, the next daily pass and recent passes",
			Description: "Per-space retention periods are limits (retention_*_days; see a space's limits for its effective values). " +
				"One daily pass deletes what is older; it never deletes what an open item uses, and it does not run while a restore is unacknowledged.",
			Tags: []string{"retention"}},
			func(ctx context.Context, _ *struct{}) (*struct{ Body *retention.Status }, error) {
				st, err := ret.Status(ctx)
				if err != nil {
					return nil, d.fail(ctx, "retention.status", err)
				}
				return &struct{ Body *retention.Status }{Body: st}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "run-retention", Method: http.MethodPost, Path: "/retention/run",
			Summary:     "Run a retention pass now and return it when it finished",
			Description: "Deletions are audited and recorded in the change feed. Refused (409) while a pass runs or a restore is unacknowledged.",
			Tags:        []string{"retention"}},
			func(ctx context.Context, _ *struct{}) (*struct{ Body *retention.Pass }, error) {
				p, err := ret.Run(ctx, Actor(ctx))
				if err != nil && p == nil {
					return nil, d.fail(ctx, "retention.run", err)
				}
				// A pass that failed part-way is still returned: its state and error say what happened.
				return &struct{ Body *retention.Pass }{Body: p}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-space-export", Method: http.MethodGet, Path: "/spaces/{space_id}/export/summary",
			Summary: "What a space export would hold now: counts, estimated size and the space's export limit",
			Description: "The archive itself is GET /spaces/{space_id}/export (a ZIP of JSON Lines and attachment files). It is NOT encrypted; " +
				"see warning.", Tags: []string{"spaces"}},
			func(ctx context.Context, in *SpacePath) (*struct{ Body *spaceexport.Summary }, error) {
				s, err := exp.Summarize(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "export.summary", err)
				}
				return &struct{ Body *spaceexport.Summary }{Body: s}, nil
			})
	}
}

// SpaceExportDownload is the raw route that streams a space's export archive (application/zip).
func SpaceExportDownload(exp *spaceexport.Service) RawRoute {
	return RawRoute{Pattern: "GET " + Prefix + "/spaces/{space_id}/export", Perm: authz.SpaceExport, Handler: func(d *Deps) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			spaceID := r.PathValue("space_id")
			slug, sum, err := exp.Prepare(ctx, Actor(ctx), spaceID)
			if err != nil {
				writeProblem(w, problemFor(err))
				return
			}
			name := fmt.Sprintf("spaceman-export-%s-%s.zip", slug, d.now().UTC().Format("20060102T150405Z"))
			h := w.Header()
			h.Set("Content-Type", "application/zip")
			h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
			h.Set("Cache-Control", "no-store")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Spaceman-Export-Encrypted", "false")
			if _, err := exp.Write(ctx, w, Actor(ctx), spaceID, sum.MaxBytes); err != nil && !errors.Is(err, context.Canceled) {
				// Headers are sent: the client sees a truncated archive without manifest.json.
				d.log().WarnContext(ctx, "space export stopped", slog.String("space.id", spaceID), slog.String("error.message", err.Error()))
			}
		}
	}}
}
