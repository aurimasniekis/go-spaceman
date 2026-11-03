-- Space export (ADR-0045): every query reads one space's rows in id order, a page at a time, inside one read
-- transaction (a consistent snapshot). The *Stats queries estimate the archive's uncompressed size before it starts.

-- name: ExportAttachmentStats :one
SELECT CAST(count(*) AS INTEGER) AS n, CAST(coalesce(sum(size + length(name) + 300), 0) AS INTEGER) AS bytes FROM attachments WHERE space_id = ?;

-- name: ExportInboxStats :one
SELECT CAST(count(*) AS INTEGER) AS n,
  CAST(coalesce(sum(length(title) + length(body) + length(trusted_json) + length(status_reason) + 600), 0) AS INTEGER) AS bytes
FROM inbox_items WHERE space_id = ?;

-- name: ExportTaskStats :one
SELECT CAST(count(*) AS INTEGER) AS n,
  CAST(coalesce(sum(length(title) + length(objective) + length(acceptance) + length(result) + length(status_reason) + 700), 0) AS INTEGER) AS bytes
FROM tasks WHERE space_id = ?;

-- name: ExportHumanStats :one
SELECT CAST(count(*) AS INTEGER) AS n,
  CAST(coalesce(sum(length(title) + length(body) + length(request_json) + length(response_json) + length(status_reason) + 900), 0) AS INTEGER) AS bytes
FROM human_actions WHERE space_id = ?;

-- name: ExportCommentStats :one
SELECT CAST(count(*) AS INTEGER) AS n, CAST(coalesce(sum(length(c.body) + 300), 0) AS INTEGER) AS bytes
FROM human_comments c JOIN human_actions h ON h.id = c.action_id WHERE h.space_id = ?;

-- name: ExportMessageStats :one
SELECT CAST(count(*) AS INTEGER) AS n, CAST(coalesce(sum(length(subject) + length(body) + length(resolution) + 600), 0) AS INTEGER) AS bytes
FROM messages WHERE space_id = ?;

-- name: ExportInboxItems :many
SELECT * FROM inbox_items WHERE space_id = sqlc.arg(space_id) AND id > sqlc.arg(after_id) ORDER BY id LIMIT sqlc.arg(lim);

-- name: ExportTasks :many
SELECT * FROM tasks WHERE space_id = sqlc.arg(space_id) AND id > sqlc.arg(after_id) ORDER BY id LIMIT sqlc.arg(lim);

-- name: ExportTaskDependencies :many
SELECT d.task_id, d.depends_on FROM task_dependencies d JOIN tasks t ON t.id = d.task_id
WHERE t.space_id = sqlc.arg(space_id) ORDER BY d.task_id, d.depends_on;

-- name: ExportTaskBlockers :many
SELECT b.* FROM task_blockers b JOIN tasks t ON t.id = b.task_id WHERE t.space_id = sqlc.arg(space_id) ORDER BY b.id;

-- name: ExportHumanActions :many
SELECT * FROM human_actions WHERE space_id = sqlc.arg(space_id) AND id > sqlc.arg(after_id) ORDER BY id LIMIT sqlc.arg(lim);

-- name: ExportHumanComments :many
SELECT c.* FROM human_comments c JOIN human_actions h ON h.id = c.action_id
WHERE h.space_id = sqlc.arg(space_id) AND c.id > sqlc.arg(after_id) ORDER BY c.id LIMIT sqlc.arg(lim);

-- name: ExportMessages :many
SELECT * FROM messages WHERE space_id = sqlc.arg(space_id) AND id > sqlc.arg(after_id) ORDER BY id LIMIT sqlc.arg(lim);

-- name: ExportAttachments :many
SELECT * FROM attachments WHERE space_id = sqlc.arg(space_id) AND id > sqlc.arg(after_id) ORDER BY id LIMIT sqlc.arg(lim);

-- name: ExportAttachmentRefs :many
SELECT r.* FROM attachment_refs r JOIN attachments a ON a.id = r.attachment_id WHERE a.space_id = sqlc.arg(space_id)
ORDER BY r.attachment_id, r.owner_kind, r.owner_id;
