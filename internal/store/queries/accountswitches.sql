-- Account switches (ADR-0090, plan T3). Keep this file ASCII (sqlc offsets).

-- name: InsertAccountSwitch :exec
INSERT INTO account_switches (id, space_id, instance_id, role, run_kind, harness, from_run_id, from_account_id, to_account_id, cause, limit_kind,
  limit_resets_at, limit_since, usage_limit_id, state, reason, armed_until, created_at, updated_at, origin_account_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetAccountSwitch :one
SELECT * FROM account_switches WHERE id = ?;

-- The switch armed for a run that is waiting for its idle point.
-- name: GetPendingAccountSwitchByRun :one
SELECT * FROM account_switches WHERE from_run_id = ? AND state = 'pending';

-- The newest switch decided for a run (any state).
-- name: GetLatestAccountSwitchByRun :one
SELECT * FROM account_switches WHERE from_run_id = ? ORDER BY created_at DESC, id DESC LIMIT 1;

-- The switch that started a run, while it is not yet verified.
-- name: GetResumedAccountSwitchByNewRun :one
SELECT * FROM account_switches WHERE new_run_id = ? AND state = 'resumed';

-- name: GetActiveAccountSwitch :one
SELECT * FROM account_switches WHERE space_id = ? AND instance_id = ? AND state IN ('pending', 'parked', 'resumed');

-- name: UpdateAccountSwitch :execrows
UPDATE account_switches SET state = sqlc.arg(state), to_account_id = sqlc.arg(to_account_id), new_run_id = sqlc.arg(new_run_id),
  limit_wait_id = sqlc.arg(limit_wait_id), reason = sqlc.arg(reason), version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version);

-- The switches an instance used since a time (the daily budget): every switch that moved, or was set to move, a run. A
-- switch-back that fell back at the claim counts too: it had already stopped the run, which was restarted where it was.
-- name: CountAccountSwitchesSince :one
SELECT count(*) FROM account_switches WHERE space_id = ? AND instance_id = ? AND created_at > ?
  AND (state IN ('pending', 'parked', 'resumed', 'succeeded', 'failed') OR (cause = 'switch_back' AND state = 'fell_back'));

-- name: ListAccountSwitchesBySpace :many
SELECT * FROM account_switches WHERE space_id = ? ORDER BY created_at DESC, id DESC LIMIT ?;

-- Armed switches whose idle-point wait ran out: the run is parked now, busy or not.
-- name: ListArmedAccountSwitchesDue :many
SELECT * FROM account_switches WHERE state = 'pending' AND armed_until <= ? ORDER BY armed_until, id LIMIT ?;

-- name: NextArmedAccountSwitch :one
SELECT CAST(coalesce(min(armed_until), 0) AS INTEGER) AS at FROM account_switches WHERE state = 'pending';

-- The newest switch of an instance that moved, or was set to move, a run (fell_back and cancelled ones moved nothing):
-- the switch that put the instance on its account, or the one moving it now (ADR-0105).
-- name: GetNewestAccountSwitchOfInstance :one
SELECT * FROM account_switches WHERE space_id = ? AND instance_id = ? AND state NOT IN ('fell_back', 'cancelled')
ORDER BY created_at DESC, id DESC LIMIT 1;

-- The newest switch-back of an instance that moved, or was set to move, its run (the ping-pong guard, ADR-0105). One that
-- fell back at the claim counts: it had already stopped the run, which was restarted where it was.
-- name: GetNewestSwitchBackOfInstance :one
SELECT * FROM account_switches WHERE space_id = ? AND instance_id = ? AND cause = 'switch_back' AND state <> 'cancelled'
ORDER BY created_at DESC, id DESC LIMIT 1;

-- Live runs between turns whose instance's newest switch moved it at a usage limit: the runs that may go back to their
-- chain's origin account (ADR-0105), after the run id given, a page at a time. The rest of the decision is made per run.
-- name: ListSwitchBackCandidates :many
SELECT r.id AS run_id, s.origin_account_id FROM runs r
JOIN account_switches s ON s.id = (SELECT s2.id FROM account_switches s2 WHERE s2.space_id = r.space_id AND s2.instance_id = r.instance_id
    AND s2.state NOT IN ('fell_back', 'cancelled') ORDER BY s2.created_at DESC, s2.id DESC LIMIT 1)
WHERE r.id > ? AND r.observed = 'running' AND r.desired = 'running' AND r.status IN ('idle', 'done') AND r.limit_kind = ''
  AND s.cause = 'usage_limit' AND s.state IN ('succeeded', 'failed') AND s.origin_account_id <> '' AND s.origin_account_id <> s.to_account_id
ORDER BY r.id LIMIT ?;
