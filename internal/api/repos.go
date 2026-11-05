package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/repos"
)

// RepositoryRules are the authorization rules of a space's repository configuration (ADR-0044). Repositories are space
// configuration: reading them needs space_config:read, changing them or asking the node for a sync space_config:write.
var RepositoryRules = map[string]Rule{
	"list-space-repositories": InSpace(authz.SpaceConfigRead),
	"get-space-repository":    InSpace(authz.SpaceConfigRead),
	"create-space-repository": InSpace(authz.SpaceConfigWrite),
	"update-space-repository": InSpace(authz.SpaceConfigWrite),
	"delete-space-repository": InSpace(authz.SpaceConfigWrite),
	"sync-space-repositories": InSpace(authz.SpaceConfigWrite),
	// The private key never leaves central through the API; generating and removing one changes how the node clones.
	"generate-repository-ssh-key": InSpace(authz.SpaceConfigWrite),
	"remove-repository-ssh-key":   InSpace(authz.SpaceConfigWrite),
}

// RepositoryPath identifies a repository of a space (by ID or name).
type RepositoryPath struct {
	SpacePath
	RepositoryID string `path:"repository_id" maxLength:"64" doc:"The repository's ID or name"`
}

type repositoryOut struct {
	ETag string `header:"ETag"`
	Body *repos.Repository
}

// RegisterRepositories registers the space repository operations.
func RegisterRepositories(svc *repos.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"repositories"}
		huma.Register(api, huma.Operation{OperationID: "list-space-repositories", Method: http.MethodGet, Path: "/spaces/{space_id}/repositories",
			Summary:     "A space's repositories with the node's latest report",
			Description: "The report (remote, default branch, last fetch, changes, unpushed commits, worktrees) is runtime state from the node and the repositories: untrusted data, as of reported_at.",
			Tags:        tags},
			func(ctx context.Context, in *SpacePath) (*struct{ Body *repos.Overview }, error) {
				ov, err := svc.List(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "repos.list", err)
				}
				return &struct{ Body *repos.Overview }{Body: ov}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-space-repository", Method: http.MethodGet, Path: "/spaces/{space_id}/repositories/{repository_id}",
			Summary: "A repository with the node's latest report", Tags: tags},
			func(ctx context.Context, in *RepositoryPath) (*repositoryOut, error) {
				r, err := svc.Get(ctx, in.SpaceID, in.RepositoryID)
				if err != nil {
					return nil, d.fail(ctx, "repos.get", err)
				}
				return &repositoryOut{ETag: ETag(r.Version), Body: r}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "create-space-repository", Method: http.MethodPost, Path: "/spaces/{space_id}/repositories",
			Summary: "Add a repository to a space",
			Description: "The URL never carries credentials: the node's own Git authentication is used, or the repository's SpaceMan SSH key once " +
				"one is generated (POST …/ssh-key). With the auto clone policy the node clones it as soon as it holds the space's lease.",
			Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				SpacePath
				Body repos.Input
			}) (*repositoryOut, error) {
				r, err := svc.Create(ctx, Actor(ctx), in.SpaceID, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "repos.create", err)
				}
				return &repositoryOut{ETag: ETag(r.Version), Body: r}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "update-space-repository", Method: http.MethodPut, Path: "/spaces/{space_id}/repositories/{repository_id}",
			Summary: "Edit a repository", Description: "Name and path are fixed. The folder's own remotes on the node are never rewritten.", Tags: tags},
			func(ctx context.Context, in *struct {
				RepositoryPath
				IfMatch
				Body repos.Input
			}) (*repositoryOut, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "repos.update", err)
				}
				cur, err := svc.Get(ctx, in.SpaceID, in.RepositoryID)
				if err != nil {
					return nil, d.fail(ctx, "repos.update", err)
				}
				r, err := svc.Update(ctx, Actor(ctx), in.SpaceID, cur.ID, v, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "repos.update", err)
				}
				return &repositoryOut{ETag: ETag(r.Version), Body: r}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "generate-repository-ssh-key", Method: http.MethodPost,
			Path:    "/spaces/{space_id}/repositories/{repository_id}/ssh-key",
			Summary: "Generate (or rotate) the repository's SSH key",
			Description: "SpaceMan generates an Ed25519 key, keeps the private half sealed and returns only the public key: add it to the Git host as a " +
				"read-only deploy key. The node clones and fetches this repository with it over SSH, against the pinned host keys, and agents never " +
				"receive it (ADR-0057). Rotating replaces the key at once; remove the old public key from the host. The URL must be an SSH URL.",
			Tags: tags},
			func(ctx context.Context, in *struct {
				RepositoryPath
				IfMatch
			}) (*repositoryOut, error) {
				v, err := optionalVersion(in.IfMatch)
				if err != nil {
					return nil, d.fail(ctx, "repos.ssh_key", err)
				}
				cur, err := svc.Get(ctx, in.SpaceID, in.RepositoryID)
				if err != nil {
					return nil, d.fail(ctx, "repos.ssh_key", err)
				}
				r, err := svc.GenerateSSHKey(ctx, Actor(ctx), in.SpaceID, cur.ID, v)
				if err != nil {
					return nil, d.fail(ctx, "repos.ssh_key", err)
				}
				return &repositoryOut{ETag: ETag(r.Version), Body: r}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "remove-repository-ssh-key", Method: http.MethodDelete,
			Path:        "/spaces/{space_id}/repositories/{repository_id}/ssh-key",
			Summary:     "Remove the repository's SSH key",
			Description: "The node clones and fetches with its own Git authentication again. Remove the public key from the Git host too.",
			Tags:        tags},
			func(ctx context.Context, in *struct {
				RepositoryPath
				IfMatch
			}) (*repositoryOut, error) {
				v, err := optionalVersion(in.IfMatch)
				if err != nil {
					return nil, d.fail(ctx, "repos.ssh_key_remove", err)
				}
				cur, err := svc.Get(ctx, in.SpaceID, in.RepositoryID)
				if err != nil {
					return nil, d.fail(ctx, "repos.ssh_key_remove", err)
				}
				r, err := svc.RemoveSSHKey(ctx, Actor(ctx), in.SpaceID, cur.ID, v)
				if err != nil {
					return nil, d.fail(ctx, "repos.ssh_key_remove", err)
				}
				return &repositoryOut{ETag: ETag(r.Version), Body: r}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "delete-space-repository", Method: http.MethodDelete, Path: "/spaces/{space_id}/repositories/{repository_id}",
			Summary:     "Remove a repository from a space",
			Description: "Only the configuration is removed: the folder on the node and the upstream repository stay. A repository a knowledge base uses cannot be removed.",
			Tags:        tags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *struct {
				RepositoryPath
				IfMatch
			}) (*struct{}, error) {
				v, err := optionalVersion(in.IfMatch)
				if err != nil {
					return nil, d.fail(ctx, "repos.delete", err)
				}
				cur, err := svc.Get(ctx, in.SpaceID, in.RepositoryID)
				if err != nil {
					return nil, d.fail(ctx, "repos.delete", err)
				}
				if err := svc.Delete(ctx, Actor(ctx), in.SpaceID, cur.ID, v); err != nil {
					return nil, d.fail(ctx, "repos.delete", err)
				}
				return &struct{}{}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "sync-space-repositories", Method: http.MethodPost, Path: "/spaces/{space_id}/repositories/sync",
			Summary: "Ask the node to clone, fetch and report a space's repositories",
			Description: "Missing auto and on_demand repositories are cloned, the named repositories (default all with a URL) are fetched, and every " +
				"repository is inspected; the report arrives asynchronously. Nothing is reset, merged, pushed or deleted. The node must be connected.",
			Tags: tags, DefaultStatus: http.StatusAccepted},
			func(ctx context.Context, in *struct {
				SpacePath
				Body repos.SyncInput
			}) (*struct{ Body *repos.SyncResult }, error) {
				res, err := svc.Sync(ctx, Actor(ctx), in.SpaceID, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "repos.sync", err)
				}
				return &struct{ Body *repos.SyncResult }{Body: res}, nil
			})
	}
}
