-- Attention summary (SM-UI-003): one grouped query per category. Rows are per space; the API keeps the spaces the
-- caller may read.

-- name: AttentionHuman :many
SELECT space_id,
  count(*) AS pending,
  CAST(coalesce(sum(CASE WHEN due_at IS NOT NULL AND due_at < sqlc.arg(now) THEN 1 ELSE 0 END), 0) AS INTEGER) AS overdue,
  CAST(coalesce(sum(CASE WHEN escalated_at IS NOT NULL THEN 1 ELSE 0 END), 0) AS INTEGER) AS escalated,
  CAST(coalesce(sum(CASE WHEN assignee = '' THEN 1 ELSE 0 END), 0) AS INTEGER) AS unassigned
FROM human_actions WHERE status IN ('open', 'in_progress') GROUP BY space_id;

-- name: AttentionTasks :many
SELECT space_id,
  CAST(coalesce(sum(CASE WHEN status = 'blocked' THEN 1 ELSE 0 END), 0) AS INTEGER) AS blocked,
  CAST(coalesce(sum(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0) AS INTEGER) AS failed
FROM tasks WHERE status IN ('blocked', 'failed') GROUP BY space_id;

-- name: AttentionQuarantine :many
SELECT s.space_id, count(*) AS open
FROM event_quarantine q JOIN event_subscriptions s ON s.id = q.subscription_id
WHERE q.status = 'open' GROUP BY s.space_id;

-- name: AttentionRuns :many
-- unknown: the node has not reported the run's state (disconnected or lost contact) while it should run; failed and
-- lost: runs that ended that way since the given time.
SELECT space_id,
  CAST(coalesce(sum(CASE WHEN observed = 'unknown' THEN 1 ELSE 0 END), 0) AS INTEGER) AS unknown,
  CAST(coalesce(sum(CASE WHEN observed IN ('failed', 'lost') THEN 1 ELSE 0 END), 0) AS INTEGER) AS failed
FROM runs
WHERE observed = 'unknown' OR (observed IN ('failed', 'lost') AND coalesce(observed_at, created_at) >= sqlc.arg(since))
GROUP BY space_id;

-- name: AttentionBudgets :many
SELECT space_id,
  CAST(coalesce(sum(CASE WHEN level = 'warning' THEN 1 ELSE 0 END), 0) AS INTEGER) AS warnings,
  CAST(coalesce(sum(CASE WHEN level = 'exceeded' THEN 1 ELSE 0 END), 0) AS INTEGER) AS exceeded
FROM budget_notices WHERE at >= sqlc.arg(since) GROUP BY space_id;

-- name: AttentionAttachmentBytes :many
SELECT space_id, CAST(coalesce(sum(size), 0) AS INTEGER) AS bytes FROM attachments GROUP BY space_id;

-- name: AttentionOutbox :one
SELECT
  CAST(coalesce(sum(CASE WHEN dead_at IS NOT NULL THEN 1 ELSE 0 END), 0) AS INTEGER) AS dead,
  CAST(coalesce(sum(CASE WHEN dead_at IS NULL AND published_at IS NULL AND attempts > 0 THEN 1 ELSE 0 END), 0) AS INTEGER) AS retrying
FROM event_outbox WHERE published_at IS NULL;

-- name: AttentionBackups :one
SELECT
  CAST(coalesce(max(CASE WHEN state IN ('succeeded', 'deleted') THEN started_at END), 0) AS INTEGER) AS last_succeeded,
  CAST(coalesce(max(CASE WHEN state IN ('failed', 'interrupted') THEN started_at END), 0) AS INTEGER) AS last_failed,
  CAST(coalesce(max(CASE WHEN verify_state = 'failed' THEN verified_at END), 0) AS INTEGER) AS last_verify_failed
FROM backups;
