-- Attention summary (SM-UI-003): one query per category. Rows are per space; the API keeps the spaces the
-- caller may read.

-- name: AttentionHuman :many
SELECT space_id,
  count(*) AS pending,
  CAST(coalesce(sum(CASE WHEN due_at IS NOT NULL AND due_at < sqlc.arg(now) THEN 1 ELSE 0 END), 0) AS INTEGER) AS overdue,
  CAST(coalesce(sum(CASE WHEN escalated_at IS NOT NULL THEN 1 ELSE 0 END), 0) AS INTEGER) AS escalated,
  CAST(coalesce(sum(CASE WHEN assignee = '' THEN 1 ELSE 0 END), 0) AS INTEGER) AS unassigned
FROM human_actions WHERE status IN ('open', 'in_progress') GROUP BY space_id;

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

-- Attention items (UI-T4): the newest rows of a category per space (at most 20 each); the API keeps the spaces the
-- caller may read and caps each kind again after that. total is the space's full count of the group (the API's
-- counts come from it, so there is no separate count query).

-- name: AttentionTaskItems :many
SELECT id, space_id, title, status, updated_at AS since, total FROM (
  SELECT id, space_id, title, status, updated_at,
    row_number() OVER (PARTITION BY space_id, status ORDER BY updated_at DESC, id) AS rn,
    count(*) OVER (PARTITION BY space_id, status) AS total
  FROM tasks WHERE status IN ('blocked', 'failed')
) WHERE rn <= 20;

-- name: AttentionQuarantineItems :many
SELECT id, space_id, subscription_id, since, total FROM (
  SELECT q.id AS id, s.space_id AS space_id, q.subscription_id AS subscription_id, q.created_at AS since,
    row_number() OVER (PARTITION BY s.space_id ORDER BY q.created_at DESC, q.id) AS rn,
    count(*) OVER (PARTITION BY s.space_id) AS total
  FROM event_quarantine q JOIN event_subscriptions s ON s.id = q.subscription_id
  WHERE q.status = 'open'
) WHERE rn <= 20;

-- name: AttentionRunItems :many
-- unknown: the node has not reported the run's state (disconnected or lost contact) while it should run; failed and
-- lost: runs that ended that way since the given time. total is the space's full count of that group.
SELECT id, space_id, role, kind, observed, since, total FROM (
  SELECT id, space_id, role, kind, observed, coalesce(observed_at, created_at) AS since,
    row_number() OVER (PARTITION BY space_id, observed = 'unknown' ORDER BY coalesce(observed_at, created_at) DESC, id) AS rn,
    count(*) OVER (PARTITION BY space_id, observed = 'unknown') AS total
  FROM runs
  WHERE observed = 'unknown' OR (observed IN ('failed', 'lost') AND coalesce(observed_at, created_at) >= sqlc.arg(since))
) WHERE rn <= 20;

-- name: AttentionBudgetItems :many
SELECT id, space_id, scope, run_id, budget, level, since, total FROM (
  SELECT id, space_id, scope, run_id, budget, level, at AS since,
    row_number() OVER (PARTITION BY space_id, level ORDER BY at DESC, id) AS rn,
    count(*) OVER (PARTITION BY space_id, level) AS total
  FROM budget_notices WHERE at >= sqlc.arg(since)
) WHERE rn <= 20;

-- name: AttentionParkedRuns :many
-- Runs parked at a usage limit (or for an agent restart) that still wait to come back.
SELECT id, space_id, run_id, role, cause, since, total FROM (
  SELECT id, space_id, run_id, role, cause, created_at AS since,
    row_number() OVER (PARTITION BY space_id ORDER BY created_at DESC, id) AS rn,
    count(*) OVER (PARTITION BY space_id) AS total
  FROM limit_waits WHERE state = 'waiting'
) WHERE rn <= 20;

-- name: AttentionAccountLimits :many
-- Open account limits as one space sees them (gateway holds are not accounts).
SELECT id, space_id, kind, resets_at, since, total FROM (
  SELECT id, space_id, kind, resets_at, seen_at AS since,
    row_number() OVER (PARTITION BY space_id ORDER BY seen_at DESC, id) AS rn,
    count(*) OVER (PARTITION BY space_id) AS total
  FROM usage_limits WHERE cleared_at IS NULL AND scope = 'account'
) WHERE rn <= 20;

-- name: AttentionAccountSwitches :many
-- Switches that failed since the given time.
SELECT id, space_id, role, from_run_id, reason, since, total FROM (
  SELECT id, space_id, role, from_run_id, reason, updated_at AS since,
    row_number() OVER (PARTITION BY space_id ORDER BY updated_at DESC, id) AS rn,
    count(*) OVER (PARTITION BY space_id) AS total
  FROM account_switches WHERE state = 'failed' AND updated_at >= sqlc.arg(since)
) WHERE rn <= 20;

-- name: AttentionXSpaceIncoming :many
-- Cross-space requests waiting for the destination's decision, by destination space.
SELECT id, space_id, kind, title, since, total FROM (
  SELECT id, dest_space_id AS space_id, kind, title, created_at AS since,
    row_number() OVER (PARTITION BY dest_space_id ORDER BY created_at DESC, id) AS rn,
    count(*) OVER (PARTITION BY dest_space_id) AS total
  FROM xspace_requests WHERE state = 'pending'
) WHERE rn <= 20;
