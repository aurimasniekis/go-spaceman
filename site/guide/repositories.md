# Repositories and worktrees

A space lists its **repositories**: the Git repositories its agents work in. Each one has a name, a folder in the space
folder on the node, a clone URL, a default branch, a role (code or knowledge base) and a clone policy. The space's node
clones them, writer runs get their own worktrees of them, and the space's **Repositories** page shows what the node last
reported about each: the remote, the default branch locally and on the remote, the last fetch, changed and untracked
files, unpushed commits and every worktree with the run that owns it.

<Shot name="space-repositories" alt="A space's Repositories page: each configured repository with its remote, default branch, state, changes, unpushed commits, worktrees and last fetch" caption="A space's repositories as the node reports them" />

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

### Git authentication

By default the node clones and fetches with its own Git setup: SSH keys and agents, credential helpers, Hallpass Git
roles or whatever the operator configured. A URL with credentials in it (`https://user:token@…`) is refused, as are:
- Git's remote-helper URLs (`ext::…`, which run programs);
- plain `http://` and `git://`;
- local paths.

In development mode (`mode: development`), `http://`, `git://` and `file://` URLs and absolute paths are allowed for local
remotes. Agents use the same node setup inside their terminals.

Public repositories need no credentials: use their `https://` URL.

### SSH keys held by SpaceMan

For a private repository, SpaceMan can hold the credential instead of the node's user, so agents never see it:

1. Give the repository an SSH URL (`git@github.com:acme/api.git` or `ssh://…`).
2. Open the repository and choose **Generate SSH key** (or run `spaceman repo ssh-key --space <space> <repository>`).
   SpaceMan creates an Ed25519 key, keeps the private half sealed in its database and shows only the public key.
3. Add the public key to the Git host as a **read-only deploy key** (GitHub: the repository's *Settings → Deploy
   keys*, with *Allow write access* off).
4. Sync. The node clones and fetches with that key.

How the key is handled:
- It travels only in the message of a clone or fetch, over the node's authenticated connection. The node never writes
  it to disk, to the environment or to its journal. It keeps the key in memory and offers it to that one git command
  on a private agent socket, which signs but never hands out the key.
- ssh runs without the node's SSH configuration, and accepts only pinned host keys. SpaceMan has GitHub's keys built
  in. For any other host, paste its keys into the repository's **Known hosts** (known_hosts lines, for example from
  `ssh-keyscan gitlab.example.com`, checked against the host's published fingerprints).
- **Rotate** replaces the key at once; add the new public key and remove the old one on the host. **Remove** returns
  the repository to the node's own Git authentication.

#### One key for many repositories

A deploy key opens one repository (GitHub accepts it on one repository only). To clone many private repositories
with a single key, an administrator can generate a **shared Git SSH key**:

1. Open **Git SSH keys** (sidebar, Integrations), choose **Generate key**, and pick the spaces that may use it, or
   every space.
2. Add its public key to the Git account that can read the repositories. GitHub: the account's *Settings → SSH and
   GPG keys*.
3. On each repository, choose the key under **Shared Git SSH key** (Edit), or with
   `spaceman repo update --git-key gky_…`.

<Shot name="git-ssh-keys" alt="Git SSH keys page with the shared key github-bot open: its public key to copy, its fingerprint and the space allowed to use it" caption="Git SSH keys" />

Prefer a **read-only machine user** for this account. A key on a person's account can also push to everything that
person can, and SpaceMan never needs to push. A repository uses either its own key or a shared key, never both. A key
can't be deleted while a repository uses it, and a space can't be taken off its list while one of the space's
repositories uses it. **Replace key** rotates it: add the new public key to the account and remove the old one. The key
reaches the node exactly like a repository's own key: in memory, for one clone or fetch, with pinned host keys.

Agents work in their own worktrees and never get the key. To push, they need their own means, as before. The key
needs the node agent from release 0.1.0-rc.6 or later; an older agent shows a note on the key and keeps using its own
Git setup.

### Clone policies

| Policy | The node clones it |
|---|---|
| `auto` (default with a URL) | as soon as it holds the space's lease: when the repository is added, when the node connects, and before a run needs it |
| `on_demand` | when a run first needs it (a worker launched in a worktree of it) or on **Sync now** |
| `manual` (default without a URL) | never: operators or agents put it in the folder; the node only reports on it |

A folder that already exists but is not a Git repository is never overwritten; the report says so. The first launch of
a repository that is still being cloned waits for the clone.

<Shot name="space-repository-form" alt="The Add repository dialog: name, folder, clone URL, default branch, role and clone policy" caption="Adding a repository" />

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

<Shot name="space-repository-detail" alt="A repository's detail: branches with unpushed commits, and worktrees with the runs that own them" caption="A repository's branches and worktrees" />

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
configured repository with a URL on the destination, besides the ones the operator lists. A repository with a
SpaceMan SSH key is cloned on the destination by the first sync after the move.

## API, CLI and Terraform

- `GET`/`POST /api/v1/spaces/{space_id}/repositories`; `GET`/`PUT`/`DELETE /api/v1/spaces/{space_id}/repositories/{id or name}`
  (`PUT` needs `If-Match`; `DELETE` checks it when given); `POST /api/v1/spaces/{space_id}/repositories/sync` (202);
  `POST`/`DELETE /api/v1/spaces/{space_id}/repositories/{id or name}/ssh-key` generates (or rotates) and removes the SSH
  key.
  Reading needs `space_config:read`, changes and syncs `space_config:write`. Every change is audited
  (`space_repository.*`).
- `spaceman repo list|get|create|update|delete|ssh-key|ssh-key-remove|sync` ([CLI reference](../reference/cli/repo)) and
  `spaceman git-key list|get|create|update|rotate|delete|for-space` for shared keys (`/api/v1/git-ssh-keys`, administrators;
  `/api/v1/spaces/{id}/git-ssh-keys` lists a space's keys).
- Terraform: `spaceman_space_repository` manages the configuration, including `known_hosts` and `ssh_key` (its computed
  `ssh_public_key` can feed the host's deploy key resource). The node's report is runtime state and is not part of the
  resource ([Terraform](../operators/terraform)).

## Removing worktrees

Worktrees are removed only when control (or a person's tool call) asks. Control has three tools ([Agent tools](./agent-tools)):

- `spaceman_worktrees_list` joins the node's last report with SpaceMan's own records: the run that owns each worktree (the
  newest run launched with that repository and branch), that run's task, the owning run's last activity, and whether the branch
  was pushed as of the repository's last fetch. A worktree no run created is listed as unowned. Pull request state is always
  `unknown`: SpaceMan knows no event tied to a branch and asks no forge per worktree. The `last_change` is the run's activity,
  because the node's report carries no folder times.
- `spaceman_worktree_remove` removes one worktree. SpaceMan first declines a worktree a live run holds (`live_run`), then asks
  the space's node (node protocol 1.16 or later; an older node is refused with an upgrade hint). The node removes it only
  when the worktree has no change of its own, its HEAD is on the remote after a fresh fetch, no run uses it and it lies directly
  inside `worktrees/`; otherwise it refuses with a code (`invalid`, `outside`, `not_worktree`, `locked`, `no_remote`,
  `fetch_failed`, `live_run`, `dirty`, `untracked`, `unpushed`, `git_error`) that is passed on unchanged. Nothing forces a
  removal and the branch is kept. An answer that takes longer than a minute comes back `pending` with a `command_id`.
- `spaceman_worktrees_prune` tries the worktrees of finished work (every task of the owning run done or cancelled and the run ended, optionally
  at least `older_than_days` after the later of the run's end and the tasks' latest finish time), oldest first, up to `limit` (default 10, at most 25), each through the same safe path.
  It only lists them unless `apply` is set. Unowned worktrees and those of failed or open tasks are never picked. There is no
  separate node command that only forgets stale worktree entries: the node already does that after every removal.

With more than 1000 runs in a space, a worktree whose owning run ended outside the newest 1000 is listed as unowned. That fails
safe: an unowned worktree is never picked by the prune tool.

Every request and every answer is an audit event of the target `worktree_removal` (the command ID).

### The lifecycle of a worktree

1. A writer run launched with a repository gets `worktrees/<branch>` (see above). It stays while the work goes on and after
   the run ends: SpaceMan never deletes a checkout by itself unless you turn the automatic cleanup on.
2. When the owning run has ended and every task of it is done or cancelled, the worktree counts as **finished**. Its age
   is counted from the later of the run's end and the tasks' latest finish time.
3. Once a week, control gets a message (see below) listing how many worktrees are finished. Control decides what to
   remove with the three tools above.
4. If you set `worktree_retention_days`, the daily retention pass also removes finished worktrees that are at least that
   old, without anyone asking.

Either way the node refuses a worktree that is dirty, has untracked files, has commits no remote has, is locked or is in
use, and the branch is kept, so a later task on that branch gets a fresh checkout of the same commit.

### Turning on the automatic cleanup

The automatic cleanup is off (`0`) until you set the space limit `worktree_retention_days` (0 to 36,500 days). It needs
the `retention:write` permission, because it lets SpaceMan remove worktrees:

```sh
spaceman limits set --space payments --set worktree_retention_days=30
spaceman limits set --space payments --unset worktree_retention_days   # back to the installation default
```

The installation default is `limits.worktree_retention_days` in the configuration file
([Configuration](../reference/configuration)). The daily pass removes at most 10 worktrees per space and 50 per pass; see
[Retention and export](./retention) for the exact rule.

### The weekly cleanup message

Once every `worktree_cleanup_message_days` days (7 by default), the daily pass sends the space's control a message that
asks it to tidy the repositories. It says how many worktrees there are, how many of them are finished, how old the oldest
finished one is and how many no run created, and points at `spaceman_worktrees_list` and `spaceman_worktrees_prune`.
It names no worktree and removes nothing. It is skipped when no worktree is finished, and sent once per period even
when central restarts. The counts come from the node's last repository report. To turn it off for a space:

```sh
spaceman limits set --space payments --set worktree_cleanup_message_days=0
```

## Limitations

See [Limitations](../reference/limitations#control-and-repositories): reports follow events, not a clock; a repository
with Git LFS installed per repository is refused; worktrees are removed automatically only when the space's
`worktree_retention_days` is set (control can also remove them, see [Removing worktrees](#removing-worktrees)). When a
worktree is removed (by a control tool call or the daily retention pass), the files the committed `.gitignore` ignores,
such as `.env` or build output, are deleted with it: SpaceMan only checks tracked and untracked-but-not-ignored work.
Keep anything you need out of ignored files in a worktree.
