-- name: InsertEventConnection :exec
INSERT INTO event_connections (id, name, kind, url, creds_enc, client_id, ca_pem, stream, enabled, created_at, created_by, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetEventConnection :one
SELECT * FROM event_connections WHERE id = ?;

-- name: ListEventConnections :many
SELECT * FROM event_connections ORDER BY name;

-- name: UpdateEventConnection :execrows
UPDATE event_connections SET name = sqlc.arg(name), url = sqlc.arg(url), creds_enc = sqlc.arg(creds_enc), client_id = sqlc.arg(client_id),
  ca_pem = sqlc.arg(ca_pem), stream = sqlc.arg(stream), enabled = sqlc.arg(enabled), version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version);

-- name: DeleteEventConnection :execrows
DELETE FROM event_connections WHERE event_connections.id = ? AND NOT EXISTS (SELECT 1 FROM event_subscriptions s WHERE s.connection_id = event_connections.id);

-- name: InsertEventTemplate :exec
INSERT INTO event_templates (id, slug, name, description, builtin, created_at, created_by, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetEventTemplate :one
SELECT * FROM event_templates WHERE id = ?;

-- name: GetEventTemplateBySlug :one
SELECT * FROM event_templates WHERE slug = ?;

-- name: ListEventTemplates :many
SELECT * FROM event_templates ORDER BY name;

-- name: InsertEventTemplateVersion :exec
INSERT INTO event_template_versions (id, template_id, version, status, definition_json, notes, created_at, created_by, published_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetEventTemplateVersion :one
SELECT * FROM event_template_versions WHERE id = ?;

-- name: ListEventTemplateVersions :many
SELECT * FROM event_template_versions WHERE template_id = ? ORDER BY version DESC;

-- name: LatestPublishedTemplateVersion :one
SELECT * FROM event_template_versions WHERE template_id = ? AND status = 'published' ORDER BY version DESC LIMIT 1;

-- name: MaxTemplateVersion :one
SELECT CAST(coalesce(max(version), 0) AS INTEGER) AS v FROM event_template_versions WHERE template_id = ?;

-- name: UpdateTemplateDraft :execrows
UPDATE event_template_versions SET definition_json = sqlc.arg(definition_json), notes = sqlc.arg(notes)
WHERE id = sqlc.arg(id) AND status = 'draft';

-- name: PublishTemplateVersion :execrows
UPDATE event_template_versions SET status = 'published', published_at = sqlc.arg(now) WHERE id = sqlc.arg(id) AND status = 'draft';

-- name: InsertEventSubscription :exec
INSERT INTO event_subscriptions (id, space_id, name, connection_id, template_id, template_version_id, follow, custom_json, vars_json,
  effective_subject, durable, enabled, start_policy, start_time, start_seq, created_at, created_by, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetEventSubscription :one
SELECT * FROM event_subscriptions WHERE id = ?;

-- name: ListEventSubscriptionsBySpace :many
SELECT * FROM event_subscriptions WHERE space_id = ? ORDER BY name;

-- name: ListEventSubscriptions :many
SELECT * FROM event_subscriptions ORDER BY space_id, name;

-- name: ListFollowingSubscriptions :many
SELECT * FROM event_subscriptions WHERE template_id = ? AND follow = 'latest';

-- name: UpdateEventSubscription :execrows
UPDATE event_subscriptions SET name = sqlc.arg(name), template_version_id = sqlc.arg(template_version_id), follow = sqlc.arg(follow),
  custom_json = sqlc.arg(custom_json), vars_json = sqlc.arg(vars_json), effective_subject = sqlc.arg(effective_subject), enabled = sqlc.arg(enabled),
  cursor_generation = sqlc.arg(cursor_generation), start_policy = sqlc.arg(start_policy), start_time = sqlc.arg(start_time),
  start_seq = sqlc.arg(start_seq), replay = sqlc.arg(replay), version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version);

-- name: MarkSubscriptionApplied :exec
UPDATE event_subscriptions SET applied_generation = ? WHERE id = ?;

-- name: TouchSubscription :exec
UPDATE event_subscriptions SET last_event_at = ? WHERE id = ?;

-- name: DeleteEventSubscription :execrows
DELETE FROM event_subscriptions WHERE id = ?;

-- name: InsertEventDelivery :exec
INSERT INTO event_deliveries (subscription_id, event_id, subject, type, stream_seq, outcome, inbox_item_id, detail, at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListEventDeliveries :many
SELECT * FROM event_deliveries WHERE subscription_id = ? ORDER BY id DESC LIMIT ?;

-- name: PurgeEventDeliveries :execrows
DELETE FROM event_deliveries WHERE at < ?;

-- name: InsertQuarantine :execrows
INSERT INTO event_quarantine (id, subscription_id, event_id, subject, type, source, event_time, content_type, payload, error, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (subscription_id, event_id) DO UPDATE SET error = excluded.error, status = 'open';

-- name: GetQuarantine :one
SELECT * FROM event_quarantine WHERE id = ?;

-- name: ListQuarantine :many
SELECT * FROM event_quarantine WHERE subscription_id = ? AND (sqlc.arg(status) = '' OR status = sqlc.arg(status)) ORDER BY created_at DESC LIMIT sqlc.arg(lim);

-- name: ResolveQuarantine :execrows
UPDATE event_quarantine SET status = sqlc.arg(status), resolved_at = sqlc.arg(now), resolved_by = sqlc.arg(by) WHERE id = sqlc.arg(id) AND status = 'open';

-- name: CountOpenQuarantine :one
SELECT count(*) FROM event_quarantine WHERE subscription_id = ? AND status = 'open';
