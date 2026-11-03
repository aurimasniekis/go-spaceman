# Repositories and worktrees

A space lists its **repositories**: the Git repositories its agents work in. Each one has a name, a folder in the space
folder on the node, a clone URL, a default branch, a role (code or knowledge base) and a clone policy. The space's node
clones them, writer runs get their own worktrees of them, and the space's **Repositories** page shows what the node last
reported about each: the remote, the default branch locally and on the remote, the last fetch, changed and untracked
files, unpushed commits and every worktree with the run that owns it.

SpaceMan never merges, pushes, resets or deletes anything in a repository. Agents open pull requests with their own
tools; removing a repository from the list leaves its folder on the node, and deleting a space never touches the
upstream repositories.

## Adding a repository

On the space's **Repositories** page choose **Add repository**, or use the CLI, the API or Terraform:

```sh
spaceman repo create --space payments api --url git@github.com:acme/api.git --path repos/api --default-branch main
```

| Field | Meaning |
|---|---|
| Name | Unique in the space; lowercase letters, digits, `.`, `_` and `-`. Fixed after creation. |
| Folder (`path`) | Where the repository lives in the space folder, default the name, for example `repos/api`. Fixed after creation. Not inside `instances/`, `worktrees/`, `work/` or `dreams/`, which SpaceMan and the harnesses own. |
| Clone URL | `https://…`, `ssh://…` or an scp-like address such as `git@github.com:acme/api.git`. |
| Default branch | The branch the node checks out when it clones (default `main`) and reports on. |
| Role | `code` or `kb` (a knowledge base's repository). |
| Clone policy | When the node clones it; see below. |

A space has at most 50 repositories.

### Git authentication stays on the node

The node clones and fetches with its own Git setup: SSH keys and agents, credential helpers, Hallpass Git roles or
whatever the operator configured. SpaceMan does not store Git credentials, and a URL with credentials in it
(`https://user:token@…`) is refused, as are Git's remote-helper URLs (`ext::…`, which run programs), plain `http://` and
`git://`, and local paths. In development mode (`mode: development`) `http://`, `git://`, `file://` URLs and absolute
paths are allowed for local remotes. Agents use the same node setup inside their terminals.

### Clone policies

| Policy | The node clones it |
|---|---|
| `auto` (default with a URL) | as soon as it holds the space's lease: when the repository is added, when the node connects, and before a run needs it |
| `on_demand` | when a run first needs it (a worker launched in a worktree of it) or on **Sync now** |
| `manual` (default without a URL) | never: operators or agents put it in the folder; the node only reports on it |

A folder that already exists but is not a Git repository is never overwritten; the report says so. The first launch of
a repository that is still being cloned waits for the clone.

## Worktrees for writers

Control launches a worker for a task with `spaceman_worker_launch` and names a repository by its name or folder
(`"repo": "api"`). The worker gets its own worktree, `worktrees/<branch>` on the task's branch (default
`spaceman/<task id>`), created from the repository on the node; two writers never share a working tree. The run's page
shows its worktree, the task shows its worker and worktree, and the repository's report lists every worktree with the
run SpaceMan last launched in it. Folders in the space folder that are not listed still work as before; the report
lists them as **unlisted** so they can be added.

## What the node reports

The node reports after every change it is asked for and after events: a repository added or edited, the node
connecting, a run of the space ending, and **Sync now**. Nothing polls idle spaces. For each repository:

- **State**: present, missing (not cloned yet) or error, with the reason (unreachable remote, a folder in the way, a
  repository configuration that would run a program).
- **Remote**: whether the configured URL answered (`git ls-remote`) and where the default branch is there.
- **Default branch**: the local branch's commit and the remote-tracking branch's (`origin/<default>`), and the last fetch.
- **Changes**: changed and untracked files and stashes in the main folder; commits on local branches no remote has.
- **Worktrees**: each worktree's branch, changed and untracked files, unpushed commits and owning run.

**Sync now** (or `spaceman repo sync --space payments`) clones what is missing (auto and on_demand repositories), fetches
the repositories named (default every one with a URL) from their configured URL into their remote-tracking branches,
and reports. A fetch never moves a local branch or touches a working tree. The report is as of the time shown; after a
relocation it is marked as coming from the previous node until the new node reports.

Agents read the same view with the read-only tool `spaceman_repos_list`; they cannot change the list or start a sync.
Branch names, paths and remotes in a report come from the repositories, which agents can write: treat them as data.

The daemon runs Git with hooks and fsmonitor off and the `ext` transport disabled, and refuses to run Git in a repository
whose own `.git/config` would run a program (filter, merge or diff drivers, SSH or proxy commands, credential helpers,
includes, URL rewrites).

## Knowledge bases

A [knowledge base](./knowledge) names its repository's folder. That folder is always a listed repository: a folder no
repository uses yet is added (role `kb`, clone policy `manual`, no URL) when the knowledge base is created or edited,
and an upgrade adds the folders of existing knowledge bases the same way. To have the node clone a knowledge base's
repository, edit that repository and give it a URL and a clone policy. A repository a knowledge base uses cannot be
removed.

## Relocation

[Relocation](../operators/relocation) reads the list. A clean relocation carries what the source has, and the destination
also clones configured `auto` repositories the source never cloned. A relocation from an unavailable source clones every
configured repository with a URL on the destination, besides the ones the operator lists.

## API, CLI and Terraform

- `GET`/`POST /api/v1/spaces/{space_id}/repositories`; `GET`/`PUT`/`DELETE /api/v1/spaces/{space_id}/repositories/{id or name}`
  (`PUT` needs `If-Match`; `DELETE` checks it when given); `POST /api/v1/spaces/{space_id}/repositories/sync` (202).
  Reading needs `space_config:read`, changes and syncs `space_config:write`. Every change is audited
  (`space_repository.*`).
- `spaceman repo list|get|create|update|delete|sync` ([CLI reference](../reference/cli/repo)).
- Terraform: `spaceman_space_repository` manages the configuration; the node's report is runtime state and is not part
  of the resource ([Terraform](../operators/terraform)).

## Limitations

See [Limitations](../reference/limitations#control-and-repositories): reports follow events, not a clock; a repository
with Git LFS installed per repository is refused; worktrees are never cleaned up automatically.
