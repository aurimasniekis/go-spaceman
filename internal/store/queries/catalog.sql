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
