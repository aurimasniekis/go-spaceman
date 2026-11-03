package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
)

// RelocationRules are the authorization rules of space relocation (ADR-0037). Moving a space between nodes is global
// administration with step-up (sessions re-authenticate with MFA; API tokens, such as Terraform's, do not).
var RelocationRules = map[string]Rule{
	"get-relocation":     InSpace(authz.SpaceRead),
	"list-relocations":   InSpace(authz.SpaceRead),
	"start-relocation":   StepUp(authz.SpaceMove),
	"retry-relocation":   StepUp(authz.SpaceMove),
	"cancel-relocation":  Global(authz.SpaceMove),
	"release-relocation": StepUp(authz.SpaceMove),
}

type relocationOut struct {
	Body *fleet.Relocation
}

// RegisterRelocations registers the relocation operations.
func RegisterRelocations(f *fleet.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"relocation"}
		huma.Register(api, huma.Operation{OperationID: "get-relocation", Method: http.MethodGet, Path: "/spaces/{space_id}/relocation",
			Summary: "The space's relocation in progress, or its latest one", Tags: tags},
			func(ctx context.Context, in *SpacePath) (*relocationOut, error) {
				r, err := f.GetRelocation(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "relocations.get", err)
				}
				return &relocationOut{Body: r}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-relocations", Method: http.MethodGet, Path: "/spaces/{space_id}/relocations",
			Summary: "The space's relocations, newest first", Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				Limit int `query:"limit" minimum:"1" maximum:"200" default:"50"`
			}) (*struct {
				Body struct {
					Items []*fleet.Relocation `json:"items"`
				}
			}, error) {
				rs, err := f.ListRelocations(ctx, in.SpaceID, in.Limit)
				if err != nil {
					return nil, d.fail(ctx, "relocations.list", err)
				}
				out := &struct {
					Body struct {
						Items []*fleet.Relocation `json:"items"`
					}
				}{}
				out.Body.Items = rs
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "start-relocation", Method: http.MethodPost, Path: "/spaces/{space_id}/relocation",
			Summary: "Relocate a stopped space to another node, or inspect it first (precheck)",
			Description: "An explicit operator action (SM-INV-003). A clean relocation checks the source (memory synced, uploads finished, repositories " +
				"pushed), refuses unpushed, changed or untracked state unless dirty_state is export, prepares the destination and then moves the " +
				"assignment and the execution lease in one step. source_unavailable relocates from central's copies only and requires an " +
				"acknowledgement that local state may be lost. The space stays paused at the destination.",
			Tags: tags, DefaultStatus: http.StatusAccepted},
			func(ctx context.Context, in *struct {
				SpacePath
				IdempotencyKey
				Body fleet.StartRelocationInput
			}) (*relocationOut, error) {
				r, _, err := Idempotent(ctx, d, "start-relocation:"+in.SpaceID, in.IdempotencyKey, in.Body, false, func() (*fleet.Relocation, error) {
					return f.StartRelocation(ctx, Actor(ctx), in.SpaceID, in.Body)
				})
				if err != nil {
					return nil, d.fail(ctx, "relocations.start", err)
				}
				return &relocationOut{Body: r}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "retry-relocation", Method: http.MethodPost, Path: "/spaces/{space_id}/relocation/retry",
			Summary: "Resume a blocked relocation, optionally exporting the reported state", Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				Body fleet.RetryRelocationInput
			}) (*relocationOut, error) {
				r, err := f.RetryRelocation(ctx, Actor(ctx), in.SpaceID, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "relocations.retry", err)
				}
				return &relocationOut{Body: r}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "cancel-relocation", Method: http.MethodPost, Path: "/spaces/{space_id}/relocation/cancel",
			Summary: "Cancel a relocation before it finalizes (the space stays on its source, paused)", Tags: tags},
			func(ctx context.Context, in *SpacePath) (*relocationOut, error) {
				r, err := f.CancelRelocation(ctx, Actor(ctx), in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "relocations.cancel", err)
				}
				return &relocationOut{Body: r}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "release-relocation", Method: http.MethodPost, Path: "/spaces/{space_id}/relocation/release",
			Summary: "Delete the source folder the latest relocation retained", Tags: tags},
			func(ctx context.Context, in *SpacePath) (*relocationOut, error) {
				r, err := f.ReleaseRelocationSource(ctx, Actor(ctx), in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "relocations.release", err)
				}
				return &relocationOut{Body: r}, nil
			})
	}
}
