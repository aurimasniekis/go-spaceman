-- name: InsertMessage :exec
INSERT INTO messages (id, space_id, from_kind, from_id, from_label, to_kind, to_id, kind, subject, body, correlation_id, reply_to, dedup_key, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetMessage :one
SELECT * FROM messages WHERE id = ?;

-- name: GetMessageByDedup :one
SELECT * FROM messages WHERE space_id = ? AND from_kind = ? AND from_id = ? AND dedup_key = ?;

-- name: ListSpaceMessages :many
SELECT * FROM messages WHERE space_id = ? ORDER BY created_at DESC LIMIT ?;

-- name: ListMessagesFor :many
SELECT * FROM messages
WHERE space_id = sqlc.arg(space_id) AND state IN ('stored', 'available')
  AND ((to_kind = 'run' AND to_id = sqlc.arg(run_id)) OR (to_kind = 'role' AND to_id = sqlc.arg(role)) OR (to_kind = 'control' AND sqlc.arg(is_control) = 1))
ORDER BY created_at LIMIT sqlc.arg(lim);

-- name: MarkMessagesAvailable :execrows
UPDATE messages SET state = 'available', available_at = sqlc.arg(now), delivered_via = sqlc.arg(via), version = version + 1
WHERE id IN (sqlc.slice(ids)) AND state = 'stored';

-- name: MarkNoticeAccepted :execrows
UPDATE messages SET state = 'available', available_at = sqlc.arg(now), delivered_via = 'terminal_notice', version = version + 1
WHERE notice_command = sqlc.arg(command) AND state = 'stored';

-- name: SetMessageNotice :exec
UPDATE messages SET notice_command = sqlc.arg(command), noticed_at = sqlc.arg(now) WHERE id IN (sqlc.slice(ids));

-- name: AckMessage :execrows
UPDATE messages SET state = 'acknowledged', acknowledged_at = sqlc.arg(now), acknowledged_by = sqlc.arg(by),
  available_at = coalesce(available_at, sqlc.arg(now)), version = version + 1
WHERE id = sqlc.arg(id) AND state IN ('stored', 'available');

-- name: ResolveMessage :execrows
UPDATE messages SET state = 'resolved', resolved_at = sqlc.arg(now), resolution = sqlc.arg(resolution), version = version + 1
WHERE id = sqlc.arg(id) AND state <> 'resolved';

-- name: CountOperatorMessages :one
SELECT count(*) FROM messages WHERE space_id = ? AND to_kind = 'operators' AND state IN ('stored', 'available');

-- name: ListHumanNotificationsFor :many
SELECT n.id, n.action_id, n.kind, n.created_at, n.notice_command, n.noticed_at, n.wake, n.target_kind, n.original_run, n.routed_from, n.route_reason,
  a.title, a.type, a.outcome, a.status, a.requester_role
FROM human_notifications n JOIN human_actions a ON a.id = n.action_id
WHERE n.space_id = sqlc.arg(space_id) AND n.state IN ('pending', 'delivered')
  AND ((n.target_kind = 'run' AND n.target_run = sqlc.arg(run_id)) OR (n.target_kind = 'control' AND sqlc.arg(is_control) = 1))
ORDER BY n.created_at LIMIT sqlc.arg(lim);

-- name: MarkNotificationsDelivered :execrows
UPDATE human_notifications SET state = 'delivered', delivered_at = sqlc.arg(now) WHERE id IN (sqlc.slice(ids)) AND state = 'pending';

-- name: MarkNotificationNoticeAccepted :execrows
UPDATE human_notifications SET state = 'delivered', delivered_at = sqlc.arg(now) WHERE notice_command = sqlc.arg(command) AND state = 'pending';

-- name: SetNotificationNotice :exec
UPDATE human_notifications SET notice_command = sqlc.arg(command), noticed_at = sqlc.arg(now) WHERE id IN (sqlc.slice(ids));

-- name: AckNotification :execrows
UPDATE human_notifications SET state = 'acknowledged', acknowledged_at = sqlc.arg(now), delivered_at = coalesce(delivered_at, sqlc.arg(now))
WHERE id = sqlc.arg(id) AND space_id = sqlc.arg(space_id) AND state IN ('pending', 'delivered')
  AND ((target_kind = 'run' AND target_run = sqlc.arg(run_id)) OR (target_kind = 'control' AND sqlc.arg(is_control) = 1));

-- name: ListNoticeCandidates :many
SELECT id, space_id, kind, role, harness, status FROM runs
WHERE desired = 'running' AND observed = 'running' AND status IN ('idle', 'done');

-- name: ListMessagesByNotice :many
SELECT id, space_id, version FROM messages WHERE notice_command = ?;
