-- name: InsertPrompt :exec
INSERT INTO prompts (id, slug, name, kind, description, forked_from, created_at, created_by, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetPrompt :one
SELECT * FROM prompts WHERE id = ?;

-- name: GetPromptBySlug :one
SELECT * FROM prompts WHERE slug = ?;

-- name: ListPrompts :many
SELECT * FROM prompts ORDER BY slug;

-- name: UpdatePrompt :execrows
UPDATE prompts SET name = ?, description = ?, version = version + 1, updated_at = ? WHERE id = ? AND version = ?;

-- name: InsertPromptVersion :exec
INSERT INTO prompt_versions (id, prompt_id, number, state, body, extends, notes, created_at, created_by, updated_at)
VALUES (?, ?, 0, 'draft', ?, ?, ?, ?, ?, ?);

-- name: GetPromptVersion :one
SELECT * FROM prompt_versions WHERE id = ?;

-- name: ListPromptVersions :many
SELECT * FROM prompt_versions WHERE prompt_id = ? ORDER BY state DESC, number DESC;

-- name: GetPromptDraft :one
SELECT * FROM prompt_versions WHERE prompt_id = ? AND state = 'draft';

-- name: LatestPromptVersion :one
SELECT * FROM prompt_versions WHERE prompt_id = ? AND state = 'published' ORDER BY number DESC LIMIT 1;

-- name: UpdatePromptDraft :execrows
UPDATE prompt_versions SET body = ?, extends = ?, notes = ?, version = version + 1, updated_at = ? WHERE id = ? AND state = 'draft' AND version = ?;

-- name: PublishPromptVersion :execrows
UPDATE prompt_versions SET state = 'published', number = sqlc.arg(number), published_at = sqlc.arg(now), published_by = sqlc.arg(by),
  version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state = 'draft' AND version = sqlc.arg(version);

-- name: DeletePromptDraft :execrows
DELETE FROM prompt_versions WHERE id = ? AND state = 'draft';

-- name: InsertProfile :exec
INSERT INTO harness_profiles (id, slug, name, harness, model, settings_json, claude_settings_json, harness_version, description, created_at, created_by,
  updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetProfile :one
SELECT * FROM harness_profiles WHERE id = ?;

-- name: ListProfiles :many
SELECT * FROM harness_profiles ORDER BY slug;

-- name: UpdateProfile :execrows
UPDATE harness_profiles SET name = ?, model = ?, settings_json = ?, claude_settings_json = ?, harness_version = ?, description = ?, revision = revision + 1, version = version + 1, updated_at = ?
WHERE id = ? AND version = ?;

-- name: InsertRoleTemplate :exec
INSERT INTO role_templates (id, slug, name, kind, description, profile_id, prompt_id, prompt_pin, memory_conventions, settings_json, skills_json, created_at,
  created_by, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetRoleTemplate :one
SELECT * FROM role_templates WHERE id = ?;

-- name: GetRoleTemplateBySlug :one
SELECT * FROM role_templates WHERE slug = ?;

-- name: ListRoleTemplates :many
SELECT * FROM role_templates ORDER BY slug;

-- name: UpdateRoleTemplate :execrows
UPDATE role_templates SET name = ?, description = ?, profile_id = ?, prompt_id = ?, prompt_pin = ?, memory_conventions = ?, settings_json = ?,
  skills_json = ?, revision = revision + 1, version = version + 1, updated_at = ?
WHERE id = ? AND version = ?;

-- name: UpsertSpaceRole :exec
INSERT INTO space_roles (space_id, name, template_id, profile_id, prompt_id, prompt_pin, model, settings_json, instructions, skills_json, created_at,
  created_by, updated_at)
VALUES (sqlc.arg(space_id), sqlc.arg(name), sqlc.arg(template_id), sqlc.arg(profile_id), sqlc.arg(prompt_id), sqlc.arg(prompt_pin), sqlc.arg(model),
  sqlc.arg(settings_json), sqlc.arg(instructions), sqlc.arg(skills_json), sqlc.arg(now), sqlc.arg(by), sqlc.arg(now))
ON CONFLICT (space_id, name) DO UPDATE SET template_id = excluded.template_id, profile_id = excluded.profile_id, prompt_id = excluded.prompt_id,
  prompt_pin = excluded.prompt_pin, model = excluded.model, settings_json = excluded.settings_json, instructions = excluded.instructions,
  skills_json = excluded.skills_json,
  revision = space_roles.revision + 1, version = space_roles.version + 1, updated_at = excluded.updated_at;

-- name: GetSpaceRole :one
SELECT * FROM space_roles WHERE space_id = ? AND name = ?;

-- name: ListSpaceRoles :many
SELECT * FROM space_roles WHERE space_id = ? ORDER BY name;

-- name: DeleteSpaceRole :execrows
DELETE FROM space_roles WHERE space_id = ? AND name = ?;

-- name: DeleteRoleTemplate :execrows
DELETE FROM role_templates WHERE id = ?;

-- name: SpaceRolesUsingTemplate :many
SELECT space_id, name FROM space_roles WHERE template_id = ? ORDER BY space_id, name LIMIT 5;

-- name: DeleteProfile :execrows
DELETE FROM harness_profiles WHERE id = ?;

-- name: TemplatesUsingProfile :many
SELECT slug FROM role_templates WHERE profile_id = ? ORDER BY slug LIMIT 5;

-- name: SpaceRolesUsingProfile :many
SELECT space_id, name FROM space_roles WHERE profile_id = ? ORDER BY space_id, name LIMIT 5;

-- Space role renames (ADR-0093). Every rewrite is scoped to one space and one name; the rename transaction runs them.

-- name: InsertSpaceRoleRename :exec
INSERT INTO space_role_renames (id, space_id, from_name, to_name, renamed_at, renamed_by, node_id, node_state)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetSpaceRoleRename :one
SELECT * FROM space_role_renames WHERE id = ?;

-- name: ListSpaceRoleRenames :many
SELECT * FROM space_role_renames WHERE space_id = ? ORDER BY renamed_at DESC, id DESC;

-- The latest rename away from a name: where a former name went last (a name can be renamed away, reused and renamed
-- away again).
-- name: LatestSpaceRoleRenameFrom :one
SELECT * FROM space_role_renames WHERE space_id = ? AND from_name = ? ORDER BY renamed_at DESC, id DESC LIMIT 1;

-- The first rename away from a name after a given rename (renamed_at, id): the next step of a chain. A rename before
-- it belongs to an earlier holder of the name.
-- name: NextSpaceRoleRenameFrom :one
SELECT * FROM space_role_renames
WHERE space_id = sqlc.arg(space_id) AND from_name = sqlc.arg(from_name)
  AND (renamed_at > sqlc.arg(after_at) OR (renamed_at = sqlc.arg(after_at) AND id > sqlc.arg(after_id)))
ORDER BY renamed_at, id LIMIT 1;

-- name: ListSpaceRoleRenamesTo :many
SELECT * FROM space_role_renames WHERE space_id = ? AND to_name = ? ORDER BY renamed_at DESC, id DESC;

-- The open renames of a space: the node has not moved the memory folder yet (pending) or could not (failed).
-- name: ListOpenSpaceRoleRenames :many
SELECT * FROM space_role_renames WHERE space_id = ? AND node_state IN ('pending', 'failed') ORDER BY renamed_at, id;

-- name: ListOpenRoleRenamesByNode :many
SELECT * FROM space_role_renames WHERE node_id = ? AND node_state IN ('pending', 'failed') ORDER BY renamed_at, id;

-- The node's memory.rename commands of one rename: the first one's key is role-rename:<id>, a re-queued one's
-- role-rename:<id>:<n> (the range covers both and uses the (node_id, idempotency_key) index).
-- name: ListRoleRenameCommands :many
SELECT id, state, idempotency_key FROM node_commands
WHERE node_id = sqlc.arg(node_id) AND idempotency_key >= sqlc.arg(key) AND idempotency_key < sqlc.arg(key_end) AND kind = 'memory.rename'
ORDER BY created_at, id;

-- The node's outcome of a rename's memory move. Only an open rename changes: a done one is final.
-- name: SetRoleRenameNodeState :execrows
UPDATE space_role_renames SET node_state = sqlc.arg(node_state), node_message = sqlc.arg(node_message), node_done_at = sqlc.arg(node_done_at)
WHERE id = sqlc.arg(id) AND node_state IN ('pending', 'failed');

-- Closes a space's open renames without the node's report (a source_unavailable relocation: the node is gone).
-- name: CloseOpenSpaceRoleRenames :execrows
UPDATE space_role_renames SET node_state = 'not_needed', node_message = sqlc.arg(node_message), node_done_at = sqlc.arg(node_done_at)
WHERE space_id = sqlc.arg(space_id) AND node_state IN ('pending', 'failed');

-- An admin closes an open rename: the node move is given up (ADR-0093 addendum). The close time goes in node_done_at.
-- name: CloseRoleRename :execrows
UPDATE space_role_renames SET node_state = 'abandoned', node_message = sqlc.arg(node_message), node_done_at = sqlc.arg(node_done_at)
WHERE id = sqlc.arg(id) AND node_state IN ('pending', 'failed');

-- An admin retries an open rename: a failed one is pending again (its message stays until the node reports).
-- name: ReopenRoleRename :execrows
UPDATE space_role_renames SET node_state = 'pending' WHERE id = ? AND node_state IN ('pending', 'failed');

-- The rename's memory.rename commands the node has not journaled (pending, sent, or rejected, which the connect re-queue
-- would send again) are cancelled when an admin forgets the node (close with forget_node): none is delivered again.
-- name: CancelRoleRenameCommands :execrows
UPDATE node_commands SET state = 'cancelled', reason = sqlc.arg(reason)
WHERE node_id = sqlc.arg(node_id) AND kind = 'memory.rename' AND state IN ('pending', 'sent', 'rejected')
  AND idempotency_key >= sqlc.arg(key) AND idempotency_key < sqlc.arg(key_end);

-- Closed renames of a space whose node has not journaled them yet: the newest memory.rename of the rename (keys
-- role-rename:<id> and role-rename:<id>:<n>, as catalog.RenameCommandKey) is pending, sent, or rejected (queued again
-- at the node's next connect). Until the node journals the move, a later rename of either name could reach the node
-- first, so they keep blocking renames of both names (ADR-0093 addendum, ordering).
-- name: ListUndeliveredClosedRoleRenames :many
SELECT r.* FROM space_role_renames r
WHERE r.space_id = sqlc.arg(space_id) AND r.node_state = 'abandoned'
  AND (SELECT c.state FROM node_commands c
       WHERE c.node_id = r.node_id AND c.kind = 'memory.rename'
         AND c.idempotency_key >= 'role-rename:' || r.id AND c.idempotency_key < 'role-rename:' || r.id || ';'
       ORDER BY c.created_at DESC, c.id DESC LIMIT 1) IN ('pending', 'sent', 'rejected')
ORDER BY r.renamed_at, r.id;

-- Closed renames of a node whose newest memory.rename the node refused: queued again at its next connect, like an
-- open rename's, so the node still journals the rename (ADR-0093 addendum, ordering).
-- name: ListRejectedClosedRoleRenamesByNode :many
SELECT r.* FROM space_role_renames r
WHERE r.node_id = sqlc.arg(node_id) AND r.node_state = 'abandoned'
  AND (SELECT c.state FROM node_commands c
       WHERE c.node_id = r.node_id AND c.kind = 'memory.rename'
         AND c.idempotency_key >= 'role-rename:' || r.id AND c.idempotency_key < 'role-rename:' || r.id || ';'
       ORDER BY c.created_at DESC, c.id DESC LIMIT 1) = 'rejected'
ORDER BY r.renamed_at, r.id;

-- name: SetRoleRenameNodeMessage :execrows
UPDATE space_role_renames SET node_message = ? WHERE id = ? AND node_state IN ('pending', 'failed');

-- Renames whose central repository has not moved yet, oldest first (a chain a->b->c moves in order).
-- name: ListUnmovedSpaceRoleRenames :many
SELECT * FROM space_role_renames WHERE space_id = ? AND central_moved_at IS NULL ORDER BY renamed_at, id;

-- name: ListUnmovedRoleRenames :many
SELECT * FROM space_role_renames WHERE central_moved_at IS NULL ORDER BY space_id, renamed_at, id;

-- name: SetRoleRenameCentralMoved :execrows
UPDATE space_role_renames SET central_moved_at = ? WHERE id = ? AND central_moved_at IS NULL;

-- Whether memory under a role name must be refused because of a rename: the node has not moved its folders yet (an
-- open rename from or to the name: until then the node's folder of either name is not the renamed role's memory under
-- that name), or the name was renamed away and no role has it again.
-- name: RoleNameRenamedAway :one
SELECT count(*) FROM space_role_renames r
WHERE r.space_id = sqlc.arg(space_id)
  AND ((r.from_name = sqlc.arg(name)
      AND (r.node_state IN ('pending', 'failed')
        OR NOT EXISTS (SELECT 1 FROM space_roles s WHERE s.space_id = r.space_id AND s.name = r.from_name)))
    OR (r.to_name = sqlc.arg(name) AND r.node_state IN ('pending', 'failed')));

-- name: RenameSpaceRoleRow :execrows
UPDATE space_roles SET name = sqlc.arg(to_name), revision = revision + 1, version = version + 1, updated_at = sqlc.arg(now)
WHERE space_id = sqlc.arg(space_id) AND name = sqlc.arg(from_name);

-- Blockers: what keeps a role busy. A live run is one not observed ended (fleet.Live); desired is not consulted,
-- because a run that ended on its own keeps desired = 'running'.
-- name: RoleRenameLiveRuns :many
SELECT id, observed FROM runs
WHERE space_id = sqlc.arg(space_id) AND role = sqlc.arg(role) AND observed IN ('pending', 'starting', 'running', 'stopping', 'unknown')
ORDER BY created_at, id;

-- name: RoleRenameOpenLimitWaits :many
SELECT id, state FROM limit_waits WHERE space_id = ? AND role = ? AND state IN ('waiting', 'resuming') ORDER BY created_at, id;

-- name: RoleRenameOpenAccountSwitches :many
SELECT id, state FROM account_switches WHERE space_id = ? AND role = ? AND state IN ('pending', 'parked', 'resumed') ORDER BY created_at, id;

-- name: RoleRenameQueuedLaunches :many
SELECT id, state FROM node_commands
WHERE space_id = sqlc.arg(space_id) AND kind = 'run.launch' AND state IN ('pending', 'sent')
  AND json_extract(payload_json, '$.role') = sqlc.arg(role)
ORDER BY created_at, id;

-- name: RoleRenameOpenDreamRuns :many
SELECT id, state FROM dream_runs WHERE space_id = ? AND state IN ('running', 'reported') ORDER BY started_at, id;

-- name: RoleRenamePendingDreamCommits :many
SELECT id, state FROM node_commands WHERE space_id = ? AND kind = 'dream.commit' AND state IN ('pending', 'sent') ORDER BY created_at, id;

-- name: GetMemoryCollectionByRole :one
SELECT * FROM memory_collections WHERE space_id = ? AND role = ?;

-- Collisions with leftovers of a deleted role of the target name (a grant on the same server, a pool entry for the same
-- account): the rename refuses them rather than merging.
-- name: RoleRenameGrantCollisions :many
SELECT a.server_id FROM mcp_grants a JOIN mcp_grants b ON b.server_id = a.server_id AND b.space_id = a.space_id AND b.role = sqlc.arg(to_name)
WHERE a.space_id = sqlc.arg(space_id) AND a.role = sqlc.arg(from_name) ORDER BY a.server_id;

-- name: RoleRenamePoolCollisions :many
SELECT a.account_id FROM space_role_accounts a
JOIN space_role_accounts b ON b.space_id = a.space_id AND b.account_id = a.account_id AND b.role = sqlc.arg(to_name)
WHERE a.space_id = sqlc.arg(space_id) AND a.role = sqlc.arg(from_name) ORDER BY a.account_id;

-- Live references: plain columns.

-- name: RenameRoleMCPGrants :many
UPDATE mcp_grants SET role = sqlc.arg(to_name) WHERE space_id = sqlc.arg(space_id) AND role = sqlc.arg(from_name) RETURNING id, server_id;

-- name: RenameRoleKBRepos :many
UPDATE kb_repos SET space_role = sqlc.arg(to_name), version = version + 1, updated_at = sqlc.arg(now)
WHERE space_id = sqlc.arg(space_id) AND space_role = sqlc.arg(from_name) RETURNING id, version;

-- name: RenameRoleDreamConfigs :many
UPDATE dream_configs SET space_role = sqlc.arg(to_name), version = version + 1, updated_at = sqlc.arg(now)
WHERE space_id = sqlc.arg(space_id) AND space_role = sqlc.arg(from_name) RETURNING id, version;

-- name: RenameRoleSchedules :many
UPDATE space_schedules SET assignee_role = sqlc.arg(to_name), version = version + 1, updated_at = sqlc.arg(now)
WHERE space_id = sqlc.arg(space_id) AND assignee_role = sqlc.arg(from_name) RETURNING id, version;

-- name: RenameRoleHumanPolicies :many
UPDATE human_policies SET successor_role = sqlc.arg(to_name), version = version + 1, updated_at = sqlc.arg(now), updated_by = sqlc.arg(by)
WHERE space_id = sqlc.arg(space_id) AND successor_role = sqlc.arg(from_name) RETURNING id, version;

-- name: RenameRoleAccounts :execrows
UPDATE space_role_accounts SET role = sqlc.arg(to_name) WHERE space_id = sqlc.arg(space_id) AND role = sqlc.arg(from_name);

-- name: RenameRoleMemoryCollection :many
UPDATE memory_collections SET role = sqlc.arg(to_name), updated_at = sqlc.arg(now)
WHERE space_id = sqlc.arg(space_id) AND role = sqlc.arg(from_name) RETURNING id;

-- Open tasks only: a done, failed or cancelled task keeps the name it was worked under.
-- name: RenameRoleOpenTasks :many
UPDATE tasks SET assignee_role = sqlc.arg(to_name), version = version + 1, updated_at = sqlc.arg(now)
WHERE space_id = sqlc.arg(space_id) AND assignee_role = sqlc.arg(from_name) AND status NOT IN ('done', 'failed', 'cancelled')
RETURNING id, version;

-- Messages to the role not yet acknowledged (including a future deliver_at).
-- name: RenameRolePendingMessages :many
UPDATE messages SET to_id = sqlc.arg(to_name), version = version + 1
WHERE space_id = sqlc.arg(space_id) AND to_kind = 'role' AND to_id = sqlc.arg(from_name) AND state IN ('stored', 'available')
RETURNING id, version;

-- name: RenameRoleOpenKBRequests :execrows
UPDATE kb_requests SET requester_role = sqlc.arg(to_name), updated_at = sqlc.arg(now)
WHERE space_id = sqlc.arg(space_id) AND requester_role = sqlc.arg(from_name) AND state IN ('queued', 'dispatched')
  AND xspace_request_id = '';

-- name: RenameRolePendingHumanNotifications :execrows
UPDATE human_notifications SET target_role = sqlc.arg(to_name)
WHERE space_id = sqlc.arg(space_id) AND target_role = sqlc.arg(from_name) AND state = 'pending';

-- Live references: JSON arrays of role names, rewritten in Go (catalog.renameInList) so they stay byte-identical to
-- what a save writes.

-- name: RoleListsKBRepos :many
SELECT id, allowed_roles_json FROM kb_repos WHERE space_id = ? ORDER BY id;

-- name: SetKBRepoAllowedRoles :one
UPDATE kb_repos SET allowed_roles_json = ?, version = version + 1, updated_at = ? WHERE id = ? RETURNING version;

-- name: RoleListsDreamConfigs :many
SELECT id, memory_roles_json, transcript_roles_json FROM dream_configs WHERE space_id = ? ORDER BY id;

-- name: SetDreamConfigRoles :one
UPDATE dream_configs SET memory_roles_json = ?, transcript_roles_json = ?, version = version + 1, updated_at = ? WHERE id = ? RETURNING version;

-- name: RoleListsHumanTypePermissions :many
SELECT action_type, agent_roles_json FROM space_human_type_permissions WHERE space_id = ? ORDER BY action_type;

-- name: SetHumanTypePermissionAgentRoles :execrows
UPDATE space_human_type_permissions SET agent_roles_json = ?, version = version + 1, updated_at = ?, updated_by = ?
WHERE space_id = ? AND action_type = ?;

-- The launch hold (ADR-0093): an open rename (pending or failed) holds launches of its new name, of its old name (the
-- node's directory of that name is the one still to move) and dream launches in the space.
-- name: OpenRoleRenameHold :one
SELECT * FROM space_role_renames
WHERE space_id = sqlc.arg(space_id) AND node_state IN ('pending', 'failed') AND (to_name = sqlc.arg(role) OR from_name = sqlc.arg(role) OR CAST(sqlc.arg(dream) AS INTEGER) = 1)
ORDER BY renamed_at, id LIMIT 1;
