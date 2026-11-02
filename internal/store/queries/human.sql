-- name: InsertHumanAction :exec
INSERT INTO human_actions (id, space_id, type, type_version, type_version_id, type_slug, title, body, request_json, requester_kind, requester_id,
  requester_role, requester_run, requester_session, requester_label, task_id, inbox_item_id, correlation_id, dedup_key, blocks_task, priority, rank,
  due_at, expires_at, follow_up_of, created_at, created_by, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetHumanAction :one
SELECT * FROM human_actions WHERE id = ?;

-- name: GetHumanActionByDedup :one
SELECT * FROM human_actions WHERE space_id = ? AND dedup_key = ?;

-- name: ListHumanActions :many
SELECT * FROM human_actions WHERE space_id = ? ORDER BY CASE WHEN status IN ('open', 'in_progress') THEN 0 ELSE 1 END, rank, created_at DESC LIMIT ?;

-- name: ListPendingHumanActions :many
SELECT * FROM human_actions WHERE status IN ('open', 'in_progress') ORDER BY created_at LIMIT ?;

-- name: MaxHumanRank :one
SELECT CAST(coalesce(max(rank), 0) AS INTEGER) AS r FROM human_actions WHERE space_id = ?;

-- name: CountPendingHuman :many
SELECT space_id, count(*) AS n FROM human_actions WHERE status IN ('open', 'in_progress') GROUP BY space_id;

-- name: UpdateHumanAssignment :execrows
UPDATE human_actions SET assignee = sqlc.arg(assignee), status = sqlc.arg(status), version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version) AND status IN ('open', 'in_progress');

-- name: FinalizeHumanAction :execrows
UPDATE human_actions SET status = sqlc.arg(status), outcome = sqlc.arg(outcome), response_json = sqlc.arg(response_json),
  responded_by = sqlc.arg(responded_by), responded_at = sqlc.arg(now), status_reason = sqlc.arg(status_reason), version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version) AND status IN ('open', 'in_progress');

-- name: SetHumanRank :exec
UPDATE human_actions SET rank = ?, updated_at = ? WHERE id = ?;

-- name: ExpiredHumanActions :many
SELECT * FROM human_actions WHERE expires_at IS NOT NULL AND expires_at <= ? AND status IN ('open', 'in_progress');

-- name: InsertHumanComment :exec
INSERT INTO human_comments (id, action_id, body, author, author_label, created_at) VALUES (?, ?, ?, ?, ?, ?);

-- name: ListHumanComments :many
SELECT * FROM human_comments WHERE action_id = ? ORDER BY created_at;

-- name: InsertHumanNotification :exec
INSERT INTO human_notifications (id, action_id, space_id, target_run, target_session, target_role, kind, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListHumanNotifications :many
SELECT * FROM human_notifications WHERE action_id = ? ORDER BY created_at;

-- name: MarkHumanNotificationDelivered :execrows
UPDATE human_notifications SET state = 'delivered', delivered_at = ? WHERE id = ? AND state = 'pending';

-- name: ListPendingNotifications :many
SELECT * FROM human_notifications WHERE space_id = ? AND state = 'pending' ORDER BY created_at LIMIT ?;

-- name: InsertHumanType :exec
INSERT INTO human_types (id, slug, name, description, created_at, created_by, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetHumanType :one
SELECT * FROM human_types WHERE id = ?;

-- name: GetHumanTypeBySlug :one
SELECT * FROM human_types WHERE slug = ?;

-- name: ListHumanTypes :many
SELECT * FROM human_types ORDER BY slug;

-- name: InsertHumanTypeVersion :exec
INSERT INTO human_type_versions (id, type_id, number, state, request_schema, response_schema, ui_json, outcomes_json, default_priority,
  default_expiry_minutes, notes, created_at, created_by, updated_at)
VALUES (?, ?, 0, 'draft', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetHumanTypeVersion :one
SELECT * FROM human_type_versions WHERE id = ?;

-- name: ListHumanTypeVersions :many
SELECT * FROM human_type_versions WHERE type_id = ? ORDER BY state DESC, number DESC;

-- name: GetHumanTypeDraft :one
SELECT * FROM human_type_versions WHERE type_id = ? AND state = 'draft';

-- name: LatestHumanTypeVersion :one
SELECT * FROM human_type_versions WHERE type_id = ? AND state = 'published' ORDER BY number DESC LIMIT 1;

-- name: UpdateHumanTypeDraft :execrows
UPDATE human_type_versions SET request_schema = ?, response_schema = ?, ui_json = ?, outcomes_json = ?, default_priority = ?,
  default_expiry_minutes = ?, notes = ?, version = version + 1, updated_at = ?
WHERE id = ? AND state = 'draft' AND version = ?;

-- name: PublishHumanTypeVersion :execrows
UPDATE human_type_versions SET state = 'published', number = sqlc.arg(number), published_at = sqlc.arg(now), published_by = sqlc.arg(by),
  version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state = 'draft' AND version = sqlc.arg(version);

-- name: UpsertSpaceHumanType :exec
INSERT INTO space_human_types (space_id, type_id, version_pin, label, default_priority, default_expiry_minutes, created_at, created_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (space_id, type_id) DO UPDATE SET version_pin = excluded.version_pin, label = excluded.label, default_priority = excluded.default_priority,
  default_expiry_minutes = excluded.default_expiry_minutes;

-- name: DeleteSpaceHumanType :execrows
DELETE FROM space_human_types WHERE space_id = ? AND type_id = ?;

-- name: ListSpaceHumanTypes :many
SELECT * FROM space_human_types WHERE space_id = ? ORDER BY type_id;

-- name: GetSpaceHumanType :one
SELECT * FROM space_human_types WHERE space_id = ? AND type_id = ?;
