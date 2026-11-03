-- name: InsertApprovalPolicy :exec
INSERT INTO approval_policies (id, space_id, operation, uses, valid_seconds, request_ttl_seconds, note, created_at, created_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetApprovalPolicy :one
SELECT * FROM approval_policies WHERE id = ?;

-- name: GetApprovalPolicyByKey :one
SELECT * FROM approval_policies WHERE space_id = ? AND operation = ?;

-- name: ListApprovalPolicies :many
SELECT * FROM approval_policies ORDER BY space_id, operation;

-- name: ListApprovalPoliciesFor :many
SELECT * FROM approval_policies WHERE space_id = '' OR space_id = ? ORDER BY space_id DESC, operation;

-- name: DeleteApprovalPolicy :execrows
DELETE FROM approval_policies WHERE id = ?;

-- name: InsertApproval :exec
INSERT INTO approvals (id, space_id, policy_id, operation, resource, args_digest, args_preview, requester_run, requester_role, state,
  uses_allowed, uses_left, valid_seconds, request_expires_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?, ?, ?, ?);

-- name: GetApproval :one
SELECT * FROM approvals WHERE id = ?;

-- name: GetApprovalByAction :one
SELECT * FROM approvals WHERE human_action_id = ?;

-- name: ListApprovalsByKey :many
SELECT * FROM approvals WHERE space_id = ? AND operation = ? AND args_digest = ? AND requester_run = ? ORDER BY created_at DESC;

-- name: ListApprovals :many
SELECT * FROM approvals WHERE space_id = ? ORDER BY created_at DESC LIMIT ?;

-- name: SetApprovalAction :exec
UPDATE approvals SET human_action_id = ?, updated_at = ? WHERE id = ?;

-- name: DecideApproval :execrows
UPDATE approvals SET state = sqlc.arg(state), state_reason = sqlc.arg(reason), decided_by = sqlc.arg(by), decided_by_type = sqlc.arg(by_type),
  decided_via = sqlc.arg(via), decided_at = sqlc.arg(now), valid_until = sqlc.arg(valid_until), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state = 'pending';

-- name: EndApproval :execrows
UPDATE approvals SET state = sqlc.arg(state), state_reason = sqlc.arg(reason), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state IN ('pending', 'approved');

-- name: UseApproval :execrows
UPDATE approvals SET uses_left = uses_left - 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state = 'approved' AND uses_left > 0 AND valid_until > sqlc.arg(now);

-- name: ExpireApprovals :many
UPDATE approvals SET state = 'expired', state_reason = 'the approval was not used in time', updated_at = sqlc.arg(now)
WHERE state = 'approved' AND uses_left > 0 AND valid_until <= sqlc.arg(now)
RETURNING id, space_id;

-- name: NextApprovalExpiry :one
SELECT CAST(coalesce(min(valid_until), 0) AS INTEGER) AS t FROM approvals WHERE state = 'approved' AND uses_left > 0 AND valid_until IS NOT NULL;

-- name: InsertApprovalExecution :exec
INSERT INTO approval_executions (id, approval_id, state, started_at) VALUES (?, ?, 'executing', ?);

-- name: FinishApprovalExecution :execrows
UPDATE approval_executions SET state = ?, error = ?, finished_at = ? WHERE id = ? AND state = 'executing';

-- name: InterruptApprovalExecutions :many
UPDATE approval_executions SET state = 'interrupted', error = 'SpaceMan restarted during the execution; its outcome is unknown and it was not retried',
  finished_at = ? WHERE state = 'executing'
RETURNING approval_id;

-- name: ListApprovalExecutions :many
SELECT * FROM approval_executions WHERE approval_id = ? ORDER BY started_at;
