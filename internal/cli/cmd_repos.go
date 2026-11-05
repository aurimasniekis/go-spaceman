package cli

import (
	"github.com/spf13/cobra"
)

var repoCols = []col{c("NAME", "name"), c("PATH", "path"), c("ROLE", "role"), c("CLONE", "clone_policy"), c("STATE", "status.state"),
	c("DIRTY", "status.dirty"), c("UNTRACKED", "status.untracked"), c("UNPUSHED", "status.unpushed"), c("FETCHED", "status.last_fetch_at"), c("URL", "url")}

// repoCmd is `spaceman repo`: a space's configured repositories (ADR-0044).
func (a *App) repoCmd() *cobra.Command {
	repoForm := func(f *form) {
		f.str("url", "url", "clone URL without credentials (https://, ssh:// or git@host:path; the node's own Git authentication is used unless the repository has an SSH key)").
			str("path", "path", "folder in the space folder (default the name; fixed after creation)").
			str("default-branch", "default_branch", "default branch (default main)").str("role", "role", "code or kb").
			str("clone-policy", "clone_policy", "auto (clone while the node holds the lease), on_demand (when a run needs it) or manual (never)").
			str("description", "description", "description").
			textFile("known-hosts", "known_hosts", "known_hosts lines pinning the Git host's SSH keys for the repository's SSH key (empty: the built-in pins, github.com)")
	}
	return group("repo", "A space's repositories: list, show, add, edit, remove, SSH keys and sync (--space)", []string{"repos", "repository", "repositories"},
		a.cmd(opts{use: "list", short: "A space's repositories with the node's latest report", op: "list-space-repositories", alias: []string{"ls"}, scoped: true,
			view: view{items: "items", cols: repoCols, empty: "No repositories."}}),
		a.cmd(opts{use: "get", short: "A repository (ID or name) with the node's latest report", op: "get-space-repository", scoped: true, args: pos("repository")}),
		a.cmd(opts{use: "create", short: "Add a repository (the node clones auto repositories at once)", op: "create-space-repository", scoped: true,
			bodyArgs: []string{"name"}, form: repoForm,
			example: "spaceman repo create --space payments api --url git@github.com:acme/api.git --path repos/api --default-branch main"}),
		a.cmd(opts{use: "update", short: "Edit a repository (only the flags given change; name and path are fixed)", op: "update-space-repository",
			get: "get-space-repository", merge: true, scoped: true, args: pos("repository"), form: repoForm}),
		a.cmd(opts{use: "delete", short: "Remove a repository from the space (its folder on the node stays)", op: "delete-space-repository",
			get: "get-space-repository", scoped: true, args: pos("repository"), done: "Repository removed; its folder on the node was left as it is."}),
		a.cmd(opts{use: "ssh-key", short: "Generate (or rotate) the repository's SSH key and print its public key", op: "generate-repository-ssh-key",
			get: "get-space-repository", scoped: true, args: pos("repository"),
			long: "SpaceMan generates an Ed25519 key and keeps the private half; add the printed public key to the Git host as a read-only " +
				"deploy key. The node then clones and fetches the repository with it over SSH, and agents never receive it. Running it again " +
				"replaces the key at once.",
			example: "spaceman repo ssh-key --space payments api",
			view:    view{first: []string{"ssh_key"}}}),
		a.cmd(opts{use: "ssh-key-remove", short: "Remove the repository's SSH key (the node's own Git authentication applies again)",
			op: "remove-repository-ssh-key", get: "get-space-repository", scoped: true, args: pos("repository"), confirm: "remove the repository's SSH key"}),
		a.cmd(opts{use: "sync", short: "Ask the node to clone what is missing, fetch and report", op: "sync-space-repositories", scoped: true,
			long: "Missing auto and on_demand repositories are cloned, the named repositories (default every repository with a URL) are fetched " +
				"from their configured URL, and every repository is inspected. The report arrives asynchronously: see `spaceman repo list`. " +
				"Nothing is reset, merged, pushed or deleted.",
			example: "spaceman repo sync --space payments --repository api",
			form: func(f *form) {
				f.list("repository", "repositories", "repository (name or ID) to fetch (repeatable; default all)").
					boolean("fetch", "fetch", "fetch from the remotes (default true)")
			}}),
	)
}
