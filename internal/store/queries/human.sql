-- name: InsertHumanAction :exec
INSERT INTO human_actions (id, space_id, type, title, body, request_json, requester_kind, requester_id, requester_role, requester_run,
  requester_session, requester_label, task_id, inbox_item_id, correlation_id, dedup_key, blocks_task, priority, rank, due_at, expires_at,
  follow_up_of, created_at, created_by, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

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
