package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/memory"
)

// MemoryRules are the authorization rules of the role-memory operations. Central never edits memory (the owning node
// does); marking a conflict reconciled is the only write.
var MemoryRules = map[string]Rule{
	"list-memory":             InSpace(authz.MemoryRead),
	"get-memory":              InSpace(authz.MemoryRead),
	"list-memory-files":       InSpace(authz.MemoryRead),
	"get-memory-file":         InSpace(authz.MemoryRead),
	"memory-history":          InSpace(authz.MemoryRead),
	"list-memory-conflicts":   InSpace(authz.MemoryRead),
	"resolve-memory-conflict": InSpace(authz.MemoryWrite),
}

// MemoryPath identifies a collection.
type MemoryPath struct {
	SpacePath
	Role string `path:"role" maxLength:"48" pattern:"^[a-z][a-z0-9-]*$"`
}

// RegisterMemory registers the role-memory operations.
func RegisterMemory(svc *memory.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"memory"}
		huma.Register(api, huma.Operation{OperationID: "list-memory", Method: http.MethodGet, Path: "/spaces/{space_id}/memory",
			Summary: "A space's role memory collections, with central's and the node's view", Tags: tags},
			func(ctx context.Context, in *SpacePath) (*struct {
				Body struct {
					Items     []*memory.Collection `json:"items"`
					Available bool                 `json:"available" doc:"False when central cannot store memory (git missing)"`
				}
			}, error) {
				l, err := svc.List(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "memory.list", err)
				}
				out := &struct {
					Body struct {
						Items     []*memory.Collection `json:"items"`
						Available bool                 `json:"available" doc:"False when central cannot store memory (git missing)"`
					}
				}{}
				out.Body.Items, out.Body.Available = l, svc.Available()
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-memory", Method: http.MethodGet, Path: "/spaces/{space_id}/memory/{role}", Summary: "One collection", Tags: tags},
			func(ctx context.Context, in *MemoryPath) (*struct{ Body *memory.Collection }, error) {
				c, err := svc.Get(ctx, in.SpaceID, in.Role)
				if err != nil {
					return nil, d.fail(ctx, "memory.get", err)
				}
				return &struct{ Body *memory.Collection }{Body: c}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-memory-files", Method: http.MethodGet, Path: "/spaces/{space_id}/memory/{role}/files",
			Summary: "Files of the acknowledged history", Tags: tags},
			func(ctx context.Context, in *MemoryPath) (*struct {
				Body struct {
					Items []string `json:"items"`
				}
			}, error) {
				l, err := svc.Files(ctx, in.SpaceID, in.Role)
				if err != nil {
					return nil, d.fail(ctx, "memory.files", err)
				}
				out := &struct {
					Body struct {
						Items []string `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-memory-file", Method: http.MethodGet, Path: "/spaces/{space_id}/memory/{role}/file",
			Summary: "One file of the acknowledged history", Description: "Memory is written by agents: untrusted content.", Tags: tags},
			func(ctx context.Context, in *struct {
				MemoryPath
				Path string `query:"path" maxLength:"1024" required:"true"`
			}) (*struct {
				Body struct {
					Path    string `json:"path"`
					Content string `json:"content"`
				}
			}, error) {
				c, err := svc.File(ctx, in.SpaceID, in.Role, in.Path)
				if err != nil {
					return nil, d.fail(ctx, "memory.file", err)
				}
				out := &struct {
					Body struct {
						Path    string `json:"path"`
						Content string `json:"content"`
					}
				}{}
				out.Body.Path, out.Body.Content = in.Path, c
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "memory-history", Method: http.MethodGet, Path: "/spaces/{space_id}/memory/{role}/history",
			Summary: "Recent acknowledged commits", Tags: tags},
			func(ctx context.Context, in *MemoryPath) (*struct {
				Body struct {
					Items []memory.Commit `json:"items"`
				}
			}, error) {
				l, err := svc.History(ctx, in.SpaceID, in.Role, 50)
				if err != nil {
					return nil, d.fail(ctx, "memory.history", err)
				}
				out := &struct {
					Body struct {
						Items []memory.Commit `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-memory-conflicts", Method: http.MethodGet, Path: "/spaces/{space_id}/memory/{role}/conflicts",
			Summary: "History preserved instead of applied", Tags: tags},
			func(ctx context.Context, in *MemoryPath) (*struct {
				Body struct {
					Items []memory.Conflict `json:"items"`
				}
			}, error) {
				l, err := svc.Conflicts(ctx, in.SpaceID, in.Role)
				if err != nil {
					return nil, d.fail(ctx, "memory.conflicts", err)
				}
				out := &struct {
					Body struct {
						Items []memory.Conflict `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "resolve-memory-conflict", Method: http.MethodPost, Path: "/spaces/{space_id}/memory/{role}/conflicts/{conflict_id}/resolve",
			Summary: "Mark a conflict reconciled", Description: "The preserved history stays; this records that a person or the role reconciled it.", Tags: tags},
			func(ctx context.Context, in *struct {
				MemoryPath
				ConflictID string `path:"conflict_id" maxLength:"64"`
			}) (*struct{}, error) {
				if err := svc.ResolveConflict(ctx, Actor(ctx), in.SpaceID, in.Role, in.ConflictID); err != nil {
					return nil, d.fail(ctx, "memory.resolve", err)
				}
				return &struct{}{}, nil
			})
	}
}
