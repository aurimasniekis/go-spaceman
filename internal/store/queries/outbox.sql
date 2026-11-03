-- Outbound event outbox (internal/outbox, ADR-0030). Derived from go-hallpass queries/events.sql (Apache-2.0, same author).

-- name: InsertEventOutbox :exec
INSERT INTO event_outbox (id, subject, type, space_id, correlation_id, causation_id, payload_json, created_at, next_attempt_at, depth)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: DueEventOutbox :many
SELECT id, subject, type, correlation_id, causation_id, payload_json, created_at, attempts, depth
FROM event_outbox WHERE published_at IS NULL AND dead_at IS NULL AND next_attempt_at <= sqlc.arg(now)
ORDER BY next_attempt_at, id LIMIT sqlc.arg(lim);

-- name: NextEventOutboxAttempt :one
SELECT CAST(coalesce(min(next_attempt_at), 0) AS INTEGER) AS next_at FROM event_outbox WHERE published_at IS NULL AND dead_at IS NULL;

-- name: MarkEventPublished :execrows
UPDATE event_outbox SET published_at = ?, attempts = attempts + 1, last_error = NULL WHERE id = ? AND published_at IS NULL;

-- name: MarkEventRetry :execrows
UPDATE event_outbox SET attempts = attempts + 1, next_attempt_at = ?, last_error = ? WHERE id = ? AND published_at IS NULL;

-- name: MarkEventDead :execrows
UPDATE event_outbox SET attempts = attempts + 1, dead_at = ?, last_error = ? WHERE id = ? AND published_at IS NULL;

-- name: EventOutboxStats :one
SELECT
  CAST(coalesce(sum(published_at IS NULL AND dead_at IS NULL), 0) AS INTEGER) AS pending,
  CAST(coalesce(sum(published_at IS NULL AND dead_at IS NULL AND attempts > 0), 0) AS INTEGER) AS retrying,
  CAST(coalesce(sum(dead_at IS NOT NULL), 0) AS INTEGER) AS dead,
  CAST(coalesce(sum(published_at IS NOT NULL), 0) AS INTEGER) AS published,
  CAST(coalesce(min(CASE WHEN published_at IS NULL AND dead_at IS NULL THEN created_at END), 0) AS INTEGER) AS oldest_pending_at,
  CAST(coalesce(max(published_at), 0) AS INTEGER) AS last_published_at
FROM event_outbox;

-- name: GetEventOutbox :one
SELECT * FROM event_outbox WHERE id = ?;

-- name: ListRecentEventFailures :many
SELECT id, type, attempts, last_error, created_at, dead_at FROM event_outbox
WHERE last_error IS NOT NULL AND published_at IS NULL ORDER BY created_at DESC LIMIT ?;

-- name: PurgePublishedEventOutbox :execrows
DELETE FROM event_outbox WHERE (published_at IS NOT NULL AND published_at < sqlc.arg(before)) OR (dead_at IS NOT NULL AND dead_at < sqlc.arg(before));

-- name: PurgeStaleEventOutbox :execrows
DELETE FROM event_outbox WHERE published_at IS NULL AND dead_at IS NULL AND created_at < ?;
