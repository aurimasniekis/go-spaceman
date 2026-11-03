package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/usage"
)

// UsageRules are the authorization rules of the usage and transcript operations. Transcripts are as sensitive as
// recordings: reading needs recording:read, deleting recording:delete with a recent re-authentication and MFA.
var UsageRules = map[string]Rule{
	"get-run-usage":   InSpace(authz.RunRead),
	"get-space-usage": InSpace(authz.RunRead),
	// Deleting a transcript is as irreversible as deleting a recording, so it needs the same step-up (SR-L06).
	"delete-transcript": {Perm: authz.RecordingDelete, Space: true, Reauth: true, MFA: true},
}

// RegisterUsage registers the usage operations.
func RegisterUsage(svc *usage.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"usage"}
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
