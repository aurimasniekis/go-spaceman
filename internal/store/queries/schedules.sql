-- name: InsertSchedule :exec
INSERT INTO space_schedules (id, space_id, name, description, enabled, kind, cron, interval_seconds, anchor_at, timezone, missed, catch_up_max,
  skip_if_open, target, title, body, priority, assignee_role, next_at, created_at, created_by, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateSchedule :execrows
UPDATE space_schedules SET name = sqlc.arg(name), description = sqlc.arg(description), enabled = sqlc.arg(enabled), kind = sqlc.arg(kind),
  cron = sqlc.arg(cron), interval_seconds = sqlc.arg(interval_seconds), anchor_at = sqlc.arg(anchor_at), timezone = sqlc.arg(timezone),
  missed = sqlc.arg(missed), catch_up_max = sqlc.arg(catch_up_max), skip_if_open = sqlc.arg(skip_if_open), target = sqlc.arg(target),
  title = sqlc.arg(title), body = sqlc.arg(body), priority = sqlc.arg(priority), assignee_role = sqlc.arg(assignee_role),
  next_at = sqlc.arg(next_at), updated_at = sqlc.arg(now), version = version + 1
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version);

-- name: GetSchedule :one
SELECT * FROM space_schedules WHERE id = ?;

-- name: ListSchedules :many
SELECT * FROM space_schedules WHERE space_id = ? ORDER BY name;

-- name: DeleteSchedule :execrows
DELETE FROM space_schedules WHERE id = ? AND space_id = ?;

-- name: NextScheduleAt :one
SELECT CAST(COALESCE(MIN(s.next_at), 0) AS INTEGER) FROM space_schedules s JOIN spaces p ON p.id = s.space_id
WHERE s.enabled = 1 AND s.next_at IS NOT NULL AND p.state <> 'archived';

-- name: ListDueSchedules :many
SELECT s.* FROM space_schedules s JOIN spaces p ON p.id = s.space_id
WHERE s.enabled = 1 AND s.next_at IS NOT NULL AND s.next_at <= ? AND p.state <> 'archived'
ORDER BY s.next_at LIMIT 100;

-- name: AdvanceSchedule :execrows
UPDATE space_schedules SET next_at = sqlc.arg(next_at) WHERE id = sqlc.arg(id) AND next_at = sqlc.arg(expected_next);

-- name: SetScheduleLast :exec
UPDATE space_schedules SET last_at = ?, last_outcome = ?, last_target_id = ? WHERE id = ?;

-- name: InsertScheduleOccurrence :execrows
INSERT INTO schedule_occurrences (schedule_id, occurrence_at, space_id, outcome, late, target_kind, target_id, detail, at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING;

-- name: ListScheduleOccurrences :many
SELECT * FROM schedule_occurrences WHERE schedule_id = ? ORDER BY occurrence_at DESC LIMIT ?;
