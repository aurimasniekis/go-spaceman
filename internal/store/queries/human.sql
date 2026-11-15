-- name: InsertHumanAction :exec
INSERT INTO human_actions (id, space_id, type, type_version, type_version_id, type_slug, title, body, request_json, requester_kind, requester_id,
  requester_role, requester_run, requester_session, requester_label, task_id, inbox_item_id, correlation_id, dedup_key, blocks_task, priority, rank,
  due_at, expires_at, follow_up_of, created_at, created_by, updated_at, next_reminder_at, escalate_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

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

-- name: CountPendingHumanByType :many
SELECT space_id, type, type_slug, count(*) AS n FROM human_actions WHERE status IN ('open', 'in_progress') GROUP BY space_id, type, type_slug;

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
INSERT INTO human_comments (id, action_id, body, author, author_label, created_at, kind) VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: ListHumanComments :many
SELECT * FROM human_comments WHERE action_id = ? ORDER BY created_at;

-- name: InsertHumanNotification :exec
INSERT INTO human_notifications (id, action_id, space_id, target_run, target_session, target_role, kind, created_at, target_kind, target_instance,
  hold_until, wake, original_run, routed_from, route_reason, routed_at, route_count)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

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

-- name: GetHumanNotification :one
SELECT * FROM human_notifications WHERE id = ?;

-- name: RouteHumanNotification :execrows
UPDATE human_notifications SET target_kind = sqlc.arg(target_kind), target_run = sqlc.arg(target_run), target_role = sqlc.arg(target_role),
  target_instance = sqlc.arg(target_instance), hold_until = sqlc.arg(hold_until), routed_from = sqlc.arg(routed_from), route_reason = sqlc.arg(route_reason),
  routed_at = sqlc.arg(now), route_count = route_count + 1, state = 'pending', delivered_at = NULL, notice_command = '', noticed_at = NULL
WHERE id = sqlc.arg(id) AND state IN ('pending', 'delivered') AND target_kind = sqlc.arg(from_kind) AND target_run = sqlc.arg(from_run);

-- name: PinHumanNotification :execrows
UPDATE human_notifications SET target_kind = 'run', target_run = sqlc.arg(run_id), hold_until = NULL
WHERE id = sqlc.arg(id) AND state = 'pending' AND target_kind = 'instance';

-- name: ListStrandedHumanNotifications :many
SELECT n.* FROM human_notifications n LEFT JOIN runs r ON r.id = n.target_run
WHERE n.state IN ('pending', 'delivered') AND n.target_kind = 'run'
  AND (r.id IS NULL OR r.space_id <> n.space_id OR r.observed IN ('exited', 'failed', 'lost') OR (r.observed = 'unknown' AND r.desired = 'stopped'))
ORDER BY n.created_at LIMIT ?;

-- name: ListHeldHumanNotifications :many
SELECT * FROM human_notifications WHERE state = 'pending' AND target_kind = 'instance' ORDER BY created_at LIMIT ?;

-- name: ListHumanActionIDsNotifiedTo :many
SELECT DISTINCT action_id FROM human_notifications WHERE space_id = ? AND target_run = ?;

-- name: LiveRunOfInstance :one
SELECT id FROM runs WHERE space_id = ? AND instance_id = ? AND desired = 'running' AND observed IN ('pending', 'starting', 'running')
ORDER BY created_at DESC LIMIT 1;

-- name: LiveRunOfRole :one
SELECT id FROM runs WHERE space_id = sqlc.arg(space_id) AND role = sqlc.arg(role) AND kind <> 'control' AND id <> sqlc.arg(not_run)
  AND desired = 'running' AND observed IN ('pending', 'starting', 'running')
ORDER BY created_at DESC LIMIT 1;

-- name: CountWantedControlRuns :one
SELECT count(*) FROM runs WHERE space_id = ? AND kind = 'control' AND desired = 'running';

-- name: SpaceHasControl :one
-- A space control auto-start can start control in: it had a control run, or it has a space role of kind control (ADR-0056).
SELECT CAST(EXISTS (SELECT 1 FROM runs WHERE runs.space_id = sqlc.arg(space_id) AND runs.kind = 'control')
  OR EXISTS (SELECT 1 FROM space_roles sr JOIN role_templates t ON t.id = sr.template_id WHERE sr.space_id = sqlc.arg(space_id) AND t.kind = 'control')
  AS INTEGER) AS has;

-- name: CountHumanActionsByRunSince :one
SELECT count(*) FROM human_actions WHERE requester_run = ? AND type = ? AND created_at > ?;

-- name: CountAgentHumanActionsInSpaceSince :one
SELECT count(*) FROM human_actions WHERE space_id = ? AND requester_kind = 'agent' AND type = ? AND created_at > ?;

-- name: CountPendingHumanByRun :one
SELECT count(*) FROM human_actions WHERE requester_run = ? AND status IN ('open', 'in_progress');

-- name: DueHumanReminders :many
SELECT * FROM human_actions WHERE next_reminder_at IS NOT NULL AND next_reminder_at <= ? AND status IN ('open', 'in_progress')
ORDER BY next_reminder_at LIMIT ?;

-- name: DueHumanEscalations :many
SELECT * FROM human_actions WHERE escalate_at IS NOT NULL AND escalate_at <= ? AND status IN ('open', 'in_progress')
ORDER BY escalate_at LIMIT ?;

-- name: RemindHumanAction :execrows
UPDATE human_actions SET reminder_count = reminder_count + 1, reminded_at = sqlc.arg(now), next_reminder_at = sqlc.arg(next_reminder_at), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND reminder_count = sqlc.arg(reminder_count) AND status IN ('open', 'in_progress');

-- name: EscalateHumanAction :execrows
UPDATE human_actions SET priority = sqlc.arg(priority), assignee = sqlc.arg(assignee), status = sqlc.arg(status), escalated_at = sqlc.arg(now),
  escalated_from = sqlc.arg(escalated_from), escalate_at = NULL, version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version) AND status IN ('open', 'in_progress');

-- name: SetHumanSchedule :exec
UPDATE human_actions SET next_reminder_at = sqlc.arg(next_reminder_at), escalate_at = sqlc.arg(escalate_at) WHERE id = sqlc.arg(id);

-- name: NextHumanExpiry :one
SELECT CAST(coalesce(min(expires_at), 0) AS INTEGER) AS t FROM human_actions WHERE expires_at IS NOT NULL AND status IN ('open', 'in_progress');

-- name: NextHumanReminder :one
SELECT CAST(coalesce(min(next_reminder_at), 0) AS INTEGER) AS t FROM human_actions WHERE next_reminder_at IS NOT NULL AND status IN ('open', 'in_progress');

-- name: NextHumanEscalation :one
SELECT CAST(coalesce(min(escalate_at), 0) AS INTEGER) AS t FROM human_actions WHERE escalate_at IS NOT NULL AND status IN ('open', 'in_progress');

-- name: NextHumanHold :one
SELECT CAST(coalesce(min(hold_until), 0) AS INTEGER) AS t FROM human_notifications WHERE hold_until IS NOT NULL AND state = 'pending' AND target_kind = 'instance';

-- name: ListHumanPoliciesFor :many
SELECT * FROM human_policies WHERE (space_id = sqlc.arg(space_id) OR space_id IS NULL) AND action_type IN (sqlc.arg(action_type), 'all');

-- name: ListDefaultHumanPolicies :many
SELECT * FROM human_policies WHERE space_id IS NULL ORDER BY action_type;

-- name: ListSpaceHumanPolicies :many
SELECT * FROM human_policies WHERE space_id = ? ORDER BY action_type;

-- name: GetDefaultHumanPolicy :one
SELECT * FROM human_policies WHERE space_id IS NULL AND action_type = ?;

-- name: GetSpaceHumanPolicy :one
SELECT * FROM human_policies WHERE space_id = ? AND action_type = ?;

-- name: InsertHumanPolicy :exec
INSERT INTO human_policies (id, space_id, action_type, remind_after_minutes, remind_every_minutes, max_reminders, escalate_after_minutes,
  escalate_priority, escalate_assignee, requester_ended, hold_minutes, successor_role, wake_idle, rate_per_run_hour, rate_per_space_hour,
  created_at, created_by, updated_at, updated_by, response_inbox)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateHumanPolicy :execrows
UPDATE human_policies SET remind_after_minutes = ?, remind_every_minutes = ?, max_reminders = ?, escalate_after_minutes = ?, escalate_priority = ?,
  escalate_assignee = ?, requester_ended = ?, hold_minutes = ?, successor_role = ?, wake_idle = ?, rate_per_run_hour = ?, rate_per_space_hour = ?,
  response_inbox = ?, updated_at = ?, updated_by = ?, version = version + 1
WHERE id = ? AND version = ?;

-- name: DeleteHumanPolicy :execrows
DELETE FROM human_policies WHERE id = ?;

-- name: ListSpaceHumanTypePermissions :many
SELECT * FROM space_human_type_permissions WHERE space_id = ? ORDER BY action_type;

-- name: GetSpaceHumanTypePermission :one
SELECT * FROM space_human_type_permissions WHERE space_id = ? AND action_type = ?;

-- name: InsertSpaceHumanTypePermission :exec
INSERT INTO space_human_type_permissions (space_id, action_type, view_role, create_role, respond_role, agents, agent_roles_json, updated_at, updated_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateSpaceHumanTypePermission :execrows
UPDATE space_human_type_permissions SET view_role = sqlc.arg(view_role), create_role = sqlc.arg(create_role), respond_role = sqlc.arg(respond_role),
  agents = sqlc.arg(agents), agent_roles_json = sqlc.arg(agent_roles_json), version = version + 1, updated_at = sqlc.arg(now), updated_by = sqlc.arg(by)
WHERE space_id = sqlc.arg(space_id) AND action_type = sqlc.arg(action_type) AND version = sqlc.arg(version);

-- name: DeleteSpaceHumanTypePermission :execrows
DELETE FROM space_human_type_permissions WHERE space_id = ? AND action_type = ?;
