-- name: InsertSkill :exec
INSERT INTO skills (id, slug, name, description, source, source_ref, created_at, created_by, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetSkill :one
SELECT * FROM skills WHERE id = ?;

-- name: GetSkillBySlug :one
SELECT * FROM skills WHERE slug = ?;

-- name: ListSkills :many
SELECT * FROM skills ORDER BY slug;

-- name: InsertSkillVersion :exec
INSERT INTO skill_versions (id, skill_id, number, state, files_json, notes, created_at, created_by, updated_at)
VALUES (?, ?, 0, 'draft', ?, ?, ?, ?, ?);

-- name: GetSkillVersion :one
SELECT * FROM skill_versions WHERE id = ?;

-- name: ListSkillVersions :many
SELECT * FROM skill_versions WHERE skill_id = ? ORDER BY state DESC, number DESC;

-- name: GetSkillDraft :one
SELECT * FROM skill_versions WHERE skill_id = ? AND state = 'draft';

-- name: LatestSkillVersion :one
SELECT * FROM skill_versions WHERE skill_id = ? AND state = 'published' ORDER BY number DESC LIMIT 1;

-- name: UpdateSkillDraft :execrows
UPDATE skill_versions SET files_json = ?, notes = ?, version = version + 1, updated_at = ? WHERE id = ? AND state = 'draft' AND version = ?;

-- name: PublishSkillVersion :execrows
UPDATE skill_versions SET state = 'published', number = sqlc.arg(number), published_at = sqlc.arg(now), published_by = sqlc.arg(by),
  version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state = 'draft' AND version = sqlc.arg(version);

