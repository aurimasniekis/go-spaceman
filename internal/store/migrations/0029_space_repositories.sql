-- 0029_space_repositories: central repository configuration of a space (SM-REPO-001, SM-REPO-003, SM-SPACE-001, ADR-0044).
--
-- A space lists its repositories: a name, the folder in the space folder on the node, the remote URL (never with
-- credentials: the node's own Git authentication is used, SM-REPO-005), the default branch, a role (code or kb) and a
-- clone policy. The node materializes them (clone on the lease, at first use or on request), fetches them on request
-- and reports their state. Writer worktrees, knowledge bases and relocation use the list.
--
-- Knowledge bases keep their own configuration and name their repository by its folder (kb_repos.repo). Every folder a
-- knowledge base names becomes a listed repository here (role kb, clone policy manual, no URL), so existing KB setups
-- keep working unchanged and appear in the list.
--
-- The node's last report per space is runtime state: it is stored apart from the configuration and never changes a
-- repository's version.

-- +goose Up
CREATE TABLE space_repositories (
  id             TEXT PRIMARY KEY,
  space_id       TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  -- Unique handle in the space, fixed after creation.
  name           TEXT NOT NULL,
  -- Folder relative to the space folder, fixed after creation.
  path           TEXT NOT NULL,
  description    TEXT NOT NULL DEFAULT '',
  -- Clone URL without credentials; empty for a repository SpaceMan never clones (clone_policy manual).
  url            TEXT NOT NULL DEFAULT '',
  default_branch TEXT NOT NULL DEFAULT 'main',
  role           TEXT NOT NULL DEFAULT 'code' CHECK (role IN ('code', 'kb')),
  -- auto: cloned while the node holds the space's lease; on_demand: cloned when a run needs it or on a sync request;
  -- manual: never cloned by SpaceMan (operators or agents put it there), only inspected.
  clone_policy   TEXT NOT NULL DEFAULT 'auto' CHECK (clone_policy IN ('auto', 'on_demand', 'manual')),
  version        INTEGER NOT NULL DEFAULT 1,
  created_at     INTEGER NOT NULL,
  created_by     TEXT NOT NULL,
  updated_at     INTEGER NOT NULL,
  updated_by     TEXT NOT NULL,
  UNIQUE (space_id, name),
  UNIQUE (space_id, path),
  CHECK (url <> '' OR clone_policy = 'manual')
) STRICT, WITHOUT ROWID;

-- The node's latest repository report for a space and the sync in flight.
CREATE TABLE space_repository_reports (
  space_id       TEXT PRIMARY KEY REFERENCES spaces (id) ON DELETE CASCADE,
  -- The node that sent report_json (a report from an earlier node is shown as stale after a relocation).
  node_id        TEXT NOT NULL DEFAULT '',
  report_json    TEXT NOT NULL DEFAULT '',
  -- When central received the report, and when the node made it (node clock; orders reports from one node).
  reported_at    INTEGER,
  checked_at     INTEGER,
  -- The last sync command queued (node_commands.id) and when.
  command_id     TEXT NOT NULL DEFAULT '',
  requested_at   INTEGER,
  requested_by   TEXT NOT NULL DEFAULT ''
) STRICT, WITHOUT ROWID;

-- Every folder a knowledge base names becomes a listed repository, with the ID derived from the first such knowledge
-- base. Names come from the folder ('/' becomes '-'); a name already taken in the space gets a suffix.
INSERT OR IGNORE INTO space_repositories (id, space_id, name, path, description, url, default_branch, role, clone_policy, created_at, created_by,
  updated_at, updated_by)
SELECT 'rep_' || substr(min(k.id), 5), k.space_id, lower(substr(replace(k.repo, '/', '-'), 1, 63)), k.repo,
  'Knowledge base repository (registered by migration 0029)', '', min(k.base_branch), 'kb', 'manual', min(k.created_at), 'system:migration', min(k.created_at),
  'system:migration'
FROM kb_repos k GROUP BY k.space_id, k.repo;

INSERT OR IGNORE INTO space_repositories (id, space_id, name, path, description, url, default_branch, role, clone_policy, created_at, created_by,
  updated_at, updated_by)
SELECT 'rep_' || substr(min(k.id), 5), k.space_id, lower(substr(replace(k.repo, '/', '-'), 1, 50)) || '-' || lower(substr(min(k.id), -6)), k.repo,
  'Knowledge base repository (registered by migration 0029)', '', min(k.base_branch), 'kb', 'manual', min(k.created_at), 'system:migration', min(k.created_at),
  'system:migration'
FROM kb_repos k
WHERE NOT EXISTS (SELECT 1 FROM space_repositories r WHERE r.space_id = k.space_id AND r.path = k.repo)
GROUP BY k.space_id, k.repo;

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
