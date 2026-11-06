package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/gitkeys"
)

// GitKeyRules are the authorization rules of shared Git SSH keys (ADR-0058). A key opens every repository its Git
// account can read, so managing keys and the spaces that may use them needs account:write, like harness accounts; a
// space's operators only see the keys their space may use (to choose one for a repository).
var GitKeyRules = map[string]Rule{
	"list-git-ssh-keys":       Global(authz.AccountRead),
	"get-git-ssh-key":         Global(authz.AccountRead),
	"create-git-ssh-key":      Global(authz.AccountWrite),
	"update-git-ssh-key":      Global(authz.AccountWrite),
	"rotate-git-ssh-key":      Global(authz.AccountWrite),
	"delete-git-ssh-key":      Global(authz.AccountWrite),
	"list-space-git-ssh-keys": InSpace(authz.SpaceConfigRead),
}

// GitKeyPath identifies a shared Git SSH key.
type GitKeyPath struct {
	KeyID string `path:"key_id" maxLength:"64" doc:"The key's ID (gky_…)"`
}

type gitKeyOut struct {
	ETag string `header:"ETag"`
	Body *gitkeys.Key
}

// RegisterGitKeys registers the shared Git SSH key operations.
func RegisterGitKeys(svc *gitkeys.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"git-ssh-keys"}
		huma.Register(api, huma.Operation{OperationID: "list-git-ssh-keys", Method: http.MethodGet, Path: "/git-ssh-keys",
			Summary: "Shared Git SSH keys (never their private halves)", Tags: tags},
			func(ctx context.Context, _ *struct{}) (*struct {
				Body struct {
					Items []*gitkeys.Key `json:"items"`
				}
			}, error) {
				l, err := svc.List(ctx)
				if err != nil {
					return nil, d.fail(ctx, "gitkeys.list", err)
				}
				out := &struct {
					Body struct {
						Items []*gitkeys.Key `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-git-ssh-key", Method: http.MethodGet, Path: "/git-ssh-keys/{key_id}", Summary: "A shared Git SSH key", Tags: tags},
			func(ctx context.Context, in *GitKeyPath) (*gitKeyOut, error) {
				k, err := svc.Get(ctx, in.KeyID)
				if err != nil {
					return nil, d.fail(ctx, "gitkeys.get", err)
				}
				return &gitKeyOut{ETag: ETag(k.Version), Body: k}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "create-git-ssh-key", Method: http.MethodPost, Path: "/git-ssh-keys",
			Summary: "Generate a shared Git SSH key",
			Description: "SpaceMan generates an Ed25519 key, keeps the private half sealed and returns only the public key. Add it to the Git account " +
				"whose repositories the listed spaces may clone (prefer a read-only machine user: a key on a person's account can also push) " +
				"or as a deploy key. Agents never receive it (ADR-0058).",
			Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct{ Body gitkeys.Input }) (*gitKeyOut, error) {
				k, err := svc.Create(ctx, Actor(ctx), in.Body)
				if err != nil {
					return nil, d.fail(ctx, "gitkeys.create", err)
				}
				return &gitKeyOut{ETag: ETag(k.Version), Body: k}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "update-git-ssh-key", Method: http.MethodPut, Path: "/git-ssh-keys/{key_id}",
			Summary: "Rename a shared Git SSH key or change the spaces that may use it", Tags: tags},
			func(ctx context.Context, in *struct {
				GitKeyPath
				IfMatch
				Body gitkeys.Input
			}) (*gitKeyOut, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "gitkeys.update", err)
				}
				k, err := svc.Update(ctx, Actor(ctx), in.KeyID, v, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "gitkeys.update", err)
				}
				return &gitKeyOut{ETag: ETag(k.Version), Body: k}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "rotate-git-ssh-key", Method: http.MethodPost, Path: "/git-ssh-keys/{key_id}/rotate",
			Summary: "Replace a shared Git SSH key", Description: "The old key stops being used at once: add the new public key where the old one was and remove the old one.",
			Tags: tags},
			func(ctx context.Context, in *struct {
				GitKeyPath
				IfMatch
			}) (*gitKeyOut, error) {
				v, err := optionalVersion(in.IfMatch)
				if err != nil {
					return nil, d.fail(ctx, "gitkeys.rotate", err)
				}
				k, err := svc.Rotate(ctx, Actor(ctx), in.KeyID, v)
				if err != nil {
					return nil, d.fail(ctx, "gitkeys.rotate", err)
				}
				return &gitKeyOut{ETag: ETag(k.Version), Body: k}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "delete-git-ssh-key", Method: http.MethodDelete, Path: "/git-ssh-keys/{key_id}",
			Summary: "Delete a shared Git SSH key no repository uses", Tags: tags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *GitKeyPath) (*struct{}, error) {
				if err := svc.Delete(ctx, Actor(ctx), in.KeyID); err != nil {
					return nil, d.fail(ctx, "gitkeys.delete", err)
				}
				return &struct{}{}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-space-git-ssh-keys", Method: http.MethodGet, Path: "/spaces/{space_id}/git-ssh-keys",
			Summary: "The shared Git SSH keys a space's repositories may use", Tags: tags},
			func(ctx context.Context, in *SpacePath) (*struct {
				Body struct {
					Items []*gitkeys.Key `json:"items"`
				}
			}, error) {
				l, err := svc.ForSpace(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "gitkeys.for_space", err)
				}
				out := &struct {
					Body struct {
						Items []*gitkeys.Key `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
	}
}
