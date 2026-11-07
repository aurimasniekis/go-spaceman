-- name: GetHarborPilotConnection :one
SELECT * FROM harbor_pilot_connection WHERE singleton = 1;

-- name: InsertHarborPilotConnection :exec
INSERT INTO harbor_pilot_connection (id, base_url, run_base_url, allow_insecure_http, parent_token_enc, parent_token_version,
  service_token_enc, service_token_version, binding_id, parent_expires_at, validated_at, created_at, created_by, updated_at, updated_by)
VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateHarborPilotConnection :execrows
UPDATE harbor_pilot_connection SET base_url = sqlc.arg(base_url), run_base_url = sqlc.arg(run_base_url),
  allow_insecure_http = sqlc.arg(allow_insecure_http), parent_token_enc = sqlc.arg(parent_token_enc),
  parent_token_version = sqlc.arg(parent_token_version), service_token_enc = sqlc.arg(service_token_enc),
  service_token_version = sqlc.arg(service_token_version), binding_id = sqlc.arg(binding_id),
  parent_expires_at = sqlc.arg(parent_expires_at), validated_at = sqlc.arg(now), updated_at = sqlc.arg(now),
  updated_by = sqlc.arg(updated_by), version = version + 1
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version);

-- name: DeleteHarborPilotConnection :execrows
DELETE FROM harbor_pilot_connection WHERE id = sqlc.arg(id) AND version = sqlc.arg(version);

-- name: GetSpaceHarborPilot :one
SELECT * FROM space_harbor_pilot WHERE space_id = ?;

-- name: ListEnabledSpaceHarborPilot :many
SELECT * FROM space_harbor_pilot WHERE enabled = 1 ORDER BY space_id;

-- name: InsertSpaceHarborPilot :exec
INSERT INTO space_harbor_pilot (space_id, enabled, project_id, harnesses_json, ttl_seconds, on_budget_status_unavailable,
  allowed_model_aliases_json, created_at, created_by, updated_at, updated_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateSpaceHarborPilot :execrows
UPDATE space_harbor_pilot SET enabled = sqlc.arg(enabled), project_id = sqlc.arg(project_id), harnesses_json = sqlc.arg(harnesses_json),
  ttl_seconds = sqlc.arg(ttl_seconds), on_budget_status_unavailable = sqlc.arg(on_budget_status_unavailable),
  allowed_model_aliases_json = sqlc.arg(allowed_model_aliases_json), updated_at = sqlc.arg(now), updated_by = sqlc.arg(updated_by),
  version = version + 1
WHERE space_id = sqlc.arg(space_id) AND version = sqlc.arg(version);

-- name: DeleteSpaceHarborPilot :execrows
DELETE FROM space_harbor_pilot WHERE space_id = sqlc.arg(space_id) AND version = sqlc.arg(version);

-- name: InsertGatewayRevocation :exec
INSERT INTO gateway_revocations (credential_id, run_id, space_id, reason, expires_at, next_attempt_at, requested_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (credential_id) DO NOTHING;

-- name: CountLiveRunGatewayCredentials :one
SELECT count(*) FROM run_gateway_credentials WHERE token_enc IS NOT NULL AND expires_at > sqlc.arg(now);

-- name: CountPendingGatewayRevocations :one
SELECT count(*) FROM gateway_revocations WHERE state = 'pending' AND expires_at > sqlc.arg(now);
