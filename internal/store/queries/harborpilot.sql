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
INSERT INTO gateway_revocations (credential_id, run_id, space_id, reason, expires_at, next_attempt_at, requested_at, parent_fp)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (credential_id) DO NOTHING;

-- name: CountLiveRunGatewayCredentials :one
SELECT count(*) FROM run_gateway_credentials WHERE token_enc IS NOT NULL AND expires_at > sqlc.arg(now);

-- name: CountPendingGatewayRevocations :one
SELECT count(*) FROM gateway_revocations WHERE state = 'pending' AND expires_at > sqlc.arg(now);

-- name: GetRunGatewayCredential :one
SELECT * FROM run_gateway_credentials WHERE run_id = ?;

-- name: InsertRunGatewayCredential :exec
INSERT INTO run_gateway_credentials (run_id, space_id, credential_id, token_enc, expires_at, created_at, parent_fp) VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: ReplaceRunGatewayCredential :execrows
UPDATE run_gateway_credentials SET credential_id = sqlc.arg(credential_id), token_enc = sqlc.arg(token_enc), expires_at = sqlc.arg(expires_at),
  created_at = sqlc.arg(created_at), parent_fp = sqlc.arg(parent_fp)
WHERE run_id = sqlc.arg(run_id) AND credential_id = sqlc.arg(old_credential_id) AND token_enc IS NOT NULL;

-- Queue a revocation of a run's live child token (in the end path's own transaction); ClearRunGatewayToken then drops
-- the sealed token, which is never needed again.
-- name: QueueRunGatewayRevocation :execrows
INSERT INTO gateway_revocations (credential_id, run_id, space_id, reason, expires_at, next_attempt_at, requested_at, parent_fp)
SELECT c.credential_id, c.run_id, c.space_id, sqlc.arg(reason), c.expires_at, sqlc.arg(now), sqlc.arg(now), c.parent_fp
FROM run_gateway_credentials c WHERE c.run_id = sqlc.arg(run_id) AND c.token_enc IS NOT NULL
ON CONFLICT (credential_id) DO NOTHING;

-- name: ClearRunGatewayToken :execrows
UPDATE run_gateway_credentials SET token_enc = NULL WHERE run_id = ? AND token_enc IS NOT NULL;

-- name: QueueSpaceGatewayRevocations :execrows
INSERT INTO gateway_revocations (credential_id, run_id, space_id, reason, expires_at, next_attempt_at, requested_at, parent_fp)
SELECT c.credential_id, c.run_id, c.space_id, sqlc.arg(reason), c.expires_at, sqlc.arg(now), sqlc.arg(now), c.parent_fp
FROM run_gateway_credentials c WHERE c.space_id = sqlc.arg(space_id) AND c.token_enc IS NOT NULL
ON CONFLICT (credential_id) DO NOTHING;

-- name: ClearSpaceGatewayTokens :execrows
UPDATE run_gateway_credentials SET token_enc = NULL WHERE space_id = ? AND token_enc IS NOT NULL;

-- Every credential a parent token may still have to revoke (parent rotation): live run tokens and pending queued
-- revocations minted by that parent, or by an unknown one (parent_fp NULL).
-- name: ListParentGatewayCredentials :many
SELECT c.credential_id, c.run_id, c.space_id, c.expires_at, CAST(coalesce(c.parent_fp, '') AS TEXT) AS fp
FROM run_gateway_credentials c WHERE c.token_enc IS NOT NULL AND (c.parent_fp = sqlc.arg(parent_fp) OR c.parent_fp IS NULL)
UNION
SELECT g.credential_id, g.run_id, g.space_id, g.expires_at, CAST(coalesce(g.parent_fp, '') AS TEXT) AS fp
FROM gateway_revocations g WHERE g.state = 'pending' AND (g.parent_fp = sqlc.arg(parent_fp) OR g.parent_fp IS NULL)
ORDER BY 1;

-- Failed revocations whose tokens are still valid: a token past its expiry is dead whatever happened to its
-- revocation, so the count (the connection's warning and the gauge) drops back once it expires.
-- name: CountFailedGatewayRevocations :one
SELECT count(*) FROM gateway_revocations WHERE state = 'failed' AND expires_at > sqlc.arg(now);

-- Lease loss (HP-T3): runs left unknown by a node that never came back keep live child tokens; the node's lease guard
-- stops them once the lease and its grace pass. The earliest lease expiry among such runs.
-- name: NextGatewayLeaseSweep :one
SELECT CAST(coalesce(min(l.expires_at), 0) AS INTEGER) FROM execution_leases l
JOIN runs r ON r.space_id = l.space_id
JOIN run_gateway_credentials c ON c.run_id = r.id
WHERE r.observed = 'unknown' AND c.token_enc IS NOT NULL;

-- name: ListGatewayLeaseLostRuns :many
SELECT r.id FROM runs r
JOIN execution_leases l ON l.space_id = r.space_id
JOIN run_gateway_credentials c ON c.run_id = r.id
WHERE r.observed = 'unknown' AND c.token_enc IS NOT NULL AND l.expires_at <= sqlc.arg(before)
ORDER BY r.id;

-- When the next failed revocation's token expires (0: none), so the failed count drops back on time.
-- name: NextFailedGatewayRevocationExpiry :one
SELECT CAST(coalesce(min(expires_at), 0) AS INTEGER) FROM gateway_revocations WHERE state = 'failed' AND expires_at > sqlc.arg(now);

-- name: NextGatewayRevocation :one
SELECT CAST(coalesce(min(next_attempt_at), 0) AS INTEGER) FROM gateway_revocations WHERE state = 'pending';

-- name: ListDueGatewayRevocations :many
SELECT * FROM gateway_revocations WHERE state = 'pending' AND next_attempt_at <= sqlc.arg(now) ORDER BY next_attempt_at, credential_id LIMIT sqlc.arg(lim);

-- name: SetGatewayRevocationDone :execrows
UPDATE gateway_revocations SET state = sqlc.arg(state), attempts = attempts + 1, last_error = sqlc.arg(last_error), done_at = sqlc.arg(now)
WHERE credential_id = sqlc.arg(credential_id) AND state = 'pending';

-- name: SetGatewayRevocationRetry :execrows
UPDATE gateway_revocations SET attempts = attempts + 1, last_error = sqlc.arg(last_error), next_attempt_at = sqlc.arg(next_attempt_at)
WHERE credential_id = sqlc.arg(credential_id) AND state = 'pending';

-- name: GetGatewayRevocation :one
SELECT * FROM gateway_revocations WHERE credential_id = ?;

-- name: GetTaskIDForRun :one
SELECT id FROM tasks WHERE assignee_run = sqlc.arg(run_id) ORDER BY updated_at DESC, id LIMIT 1;

-- name: ClearRunGatewayTokenFor :execrows
UPDATE run_gateway_credentials SET token_enc = NULL WHERE run_id = ? AND credential_id = ? AND token_enc IS NOT NULL;

-- name: QueueRunGatewayUsage :execrows
INSERT INTO run_gateway_usage (run_id, space_id, ended_at, next_attempt_at, updated_at)
SELECT c.run_id, c.space_id, sqlc.arg(now), sqlc.arg(next_at), sqlc.arg(now) FROM run_gateway_credentials c WHERE c.run_id = sqlc.arg(run_id)
ON CONFLICT (run_id) DO NOTHING;

-- name: QueueSpaceGatewayUsage :execrows
INSERT INTO run_gateway_usage (run_id, space_id, ended_at, next_attempt_at, updated_at)
SELECT c.run_id, c.space_id, sqlc.arg(now), sqlc.arg(next_at), sqlc.arg(now) FROM run_gateway_credentials c
WHERE c.space_id = sqlc.arg(space_id) AND c.token_enc IS NOT NULL
ON CONFLICT (run_id) DO NOTHING;

-- name: NextGatewayUsageRead :one
SELECT CAST(COALESCE(MIN(next_attempt_at), 0) AS INTEGER) FROM run_gateway_usage WHERE state = 'pending';

-- name: ListDueGatewayUsage :many
SELECT u.*, r.created_at AS run_created_at FROM run_gateway_usage u JOIN runs r ON r.id = u.run_id
WHERE u.state = 'pending' AND u.next_attempt_at <= sqlc.arg(now) ORDER BY u.next_attempt_at LIMIT sqlc.arg(lim);

-- name: GetRunGatewayUsage :one
SELECT * FROM run_gateway_usage WHERE run_id = ?;

-- name: SetGatewayUsageRead :execrows
UPDATE run_gateway_usage SET state = sqlc.arg(state), reads = reads + 1, failures = 0, next_attempt_at = sqlc.arg(next_attempt_at),
  read_at = sqlc.arg(now), models_json = sqlc.arg(models_json), costs_json = sqlc.arg(costs_json), attempts = sqlc.arg(attempts),
  unknown_usage = sqlc.arg(unknown_usage), last_error = '', updated_at = sqlc.arg(now)
WHERE run_id = sqlc.arg(run_id) AND state = 'pending';

-- name: SetGatewayUsageFailure :execrows
UPDATE run_gateway_usage SET state = sqlc.arg(state), failures = failures + 1, next_attempt_at = sqlc.arg(next_attempt_at),
  last_error = sqlc.arg(last_error), updated_at = sqlc.arg(now)
WHERE run_id = sqlc.arg(run_id) AND state = 'pending';

-- name: RescheduleGatewayUsage :execrows
-- A pending read that was skipped (no call was made) moves to a later attempt without counting as a failure.
UPDATE run_gateway_usage SET next_attempt_at = sqlc.arg(next_attempt_at), last_error = sqlc.arg(last_error), updated_at = sqlc.arg(now)
WHERE run_id = sqlc.arg(run_id) AND state = 'pending';

-- name: FillRunUsageFromGateway :exec
-- Gateway-observed usage fills a run's usage only where its harness reported none (no row, or source unavailable), and
-- updates its own earlier fill. A harness's own usage is never overwritten.
INSERT INTO run_usage (run_id, space_id, source, models_json, estimate_json, transcript_bytes, final, note, updated_at)
VALUES (sqlc.arg(run_id), sqlc.arg(space_id), 'harbor-pilot', sqlc.arg(models_json), sqlc.arg(estimate_json), NULL, sqlc.arg(final), sqlc.arg(note), sqlc.arg(updated_at))
ON CONFLICT (run_id) DO UPDATE SET source = excluded.source, models_json = excluded.models_json, estimate_json = excluded.estimate_json,
  final = excluded.final, note = excluded.note, updated_at = excluded.updated_at
WHERE run_usage.source IN ('unavailable', 'harbor-pilot');
