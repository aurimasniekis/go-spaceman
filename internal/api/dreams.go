package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/dream"
)

// DreamRules are the authorization rules of the dreamer operations (dreams change shared memory: memory:write).
var DreamRules = map[string]Rule{
	"list-dreams":  InSpace(authz.MemoryRead),
	"create-dream": InSpace(authz.MemoryWrite),
	"update-dream": InSpace(authz.MemoryWrite),
	"start-dream":  InSpace(authz.MemoryWrite),
	"dream-runs":   InSpace(authz.MemoryRead),
}

// DreamPath identifies a dream configuration.
type DreamPath struct {
	SpacePath
	DreamID string `path:"dream_id" maxLength:"64"`
}

type dreamOut struct {
	ETag string `header:"ETag"`
	Body *dream.Config
}

// RegisterDreams registers the dreamer operations.
func RegisterDreams(svc *dream.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"dreams"}
		huma.Register(api, huma.Operation{OperationID: "list-dreams", Method: http.MethodGet, Path: "/spaces/{space_id}/dreams", Summary: "A space's dream configurations", Tags: tags},
			func(ctx context.Context, in *SpacePath) (*struct {
				Body struct {
					Items []*dream.Config `json:"items"`
				}
			}, error) {
				l, err := svc.List(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "dream.list", err)
				}
				out := &struct {
					Body struct {
						Items []*dream.Config `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "create-dream", Method: http.MethodPost, Path: "/spaces/{space_id}/dreams", Summary: "Add a dream configuration",
			Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				SpacePath
				Body dream.Input
			}) (*dreamOut, error) {
				c, err := svc.Create(ctx, Actor(ctx), in.SpaceID, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "dream.create", err)
				}
				return &dreamOut{ETag: ETag(c.Version), Body: c}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "update-dream", Method: http.MethodPut, Path: "/spaces/{space_id}/dreams/{dream_id}", Summary: "Edit a dream configuration", Tags: tags},
			func(ctx context.Context, in *struct {
				DreamPath
				IfMatch
				Body dream.Input
			}) (*dreamOut, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "dream.update", err)
				}
				c, err := svc.Update(ctx, Actor(ctx), in.SpaceID, in.DreamID, v, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "dream.update", err)
				}
				return &dreamOut{ETag: ETag(c.Version), Body: c}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "start-dream", Method: http.MethodPost, Path: "/spaces/{space_id}/dreams/{dream_id}/start", Summary: "Dream now",
			Description: "Starts a dreamer run unless one of this configuration is still going.", Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *DreamPath) (*struct{ Body *dream.Run }, error) {
				r, err := svc.Start(ctx, Actor(ctx), in.SpaceID, in.DreamID)
				if err != nil {
					return nil, d.fail(ctx, "dream.start", err)
				}
				return &struct{ Body *dream.Run }{Body: r}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "dream-runs", Method: http.MethodGet, Path: "/spaces/{space_id}/dreams/{dream_id}/runs", Summary: "Recent dreams of a configuration", Tags: tags},
			func(ctx context.Context, in *DreamPath) (*struct {
				Body struct {
					Items []*dream.Run `json:"items"`
				}
			}, error) {
				l, err := svc.Runs(ctx, in.SpaceID, in.DreamID)
				if err != nil {
					return nil, d.fail(ctx, "dream.runs", err)
				}
				out := &struct {
					Body struct {
						Items []*dream.Run `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
	}
}
