-- Usage limits (ADR-0079): a run's reported limit, account holds and parked runs' waits. Times are Unix microseconds.

-- A run's limit as its node reported it: limit_kind '' clears it (both times NULL).
-- name: SetRunLimit :execrows
UPDATE runs SET limit_kind = sqlc.arg(limit_kind), limit_resets_at = sqlc.narg(limit_resets_at), limit_since = sqlc.narg(limit_since),
  version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND generation = sqlc.arg(generation)
  AND (limit_kind != sqlc.arg(limit_kind) OR limit_resets_at IS NOT sqlc.narg(limit_resets_at) OR limit_since IS NOT sqlc.narg(limit_since));

-- name: InsertUsageLimit :exec
INSERT INTO usage_limits (id, space_id, scope, account_id, gateway_ref, kind, resets_at, check_at, source, first_run_id, seen_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetUsageLimit :one
SELECT * FROM usage_limits WHERE id = ?;

-- name: GetOpenUsageLimit :one
SELECT * FROM usage_limits WHERE space_id = ? AND scope = ? AND account_id = ? AND gateway_ref = ? AND kind = ? AND cleared_at IS NULL;

-- The hold on an account applies in every space bound to it.
-- name: ListOpenUsageLimitsByAccount :many
SELECT * FROM usage_limits WHERE scope = 'account' AND account_id = ? AND cleared_at IS NULL ORDER BY seen_at, id;

-- name: ListOpenUsageLimitsByGateway :many
SELECT * FROM usage_limits WHERE scope = 'gateway' AND gateway_ref = ? AND cleared_at IS NULL ORDER BY seen_at, id;

-- Every open gateway hold, in every space: the gateway re-check reads it on each round (ADR-0079 decision 9).
-- name: ListOpenGatewayUsageLimits :many
SELECT * FROM usage_limits WHERE scope = 'gateway' AND cleared_at IS NULL ORDER BY seen_at, id LIMIT sqlc.arg(lim);

-- Open account holds whose reset is unknown, soonest expiry first (seen_at plus the kind window, see holdMaxAge): the
-- expiry sweep ends the ones past their window (ADR-0079 amendment), so the row cap never hides a due hold.
-- name: ListOpenUnknownResetAccountLimits :many
SELECT * FROM usage_limits WHERE scope = 'account' AND cleared_at IS NULL AND resets_at IS NULL
ORDER BY seen_at + CASE kind WHEN 'five_hour' THEN 18000000000 WHEN 'seven_day' THEN 604800000000 WHEN 'seven_day_opus' THEN 604800000000 WHEN 'seven_day_sonnet' THEN 604800000000 WHEN 'seven_day_overage_included' THEN 604800000000 ELSE 21600000000 END, id LIMIT sqlc.arg(lim);

-- name: ListOpenUsageLimitsBySpace :many
SELECT * FROM usage_limits WHERE space_id = ? AND cleared_at IS NULL ORDER BY seen_at, id;

-- name: ListUsageLimitsBySpace :many
SELECT * FROM usage_limits WHERE space_id = ? ORDER BY seen_at DESC, id DESC LIMIT ?;

-- Open limits whose known reset has passed.
-- name: ListDueUsageLimits :many
SELECT * FROM usage_limits WHERE cleared_at IS NULL AND resets_at IS NOT NULL AND resets_at <= ? ORDER BY resets_at, id;

-- name: NextUsageLimitDue :one
SELECT CAST(coalesce(min(resets_at), 0) AS INTEGER) AS at FROM usage_limits WHERE cleared_at IS NULL AND resets_at IS NOT NULL;

-- The earliest known reset ahead of after among open account holds (0: none): a gateway hold's estimate and a reset that
-- has passed never wake the expiry job.
-- name: NextAccountResetAfter :one
SELECT CAST(coalesce(min(resets_at), 0) AS INTEGER) AS at FROM usage_limits
WHERE scope = 'account' AND cleared_at IS NULL AND resets_at IS NOT NULL AND resets_at > ?;

-- Open holds due for a check: an unknown reset's probe, a gateway hold's quota-board re-read (ADR-0079 decisions 5, 7, 9).
-- name: ListUsageLimitsToCheck :many
SELECT * FROM usage_limits WHERE cleared_at IS NULL AND check_at IS NOT NULL AND check_at <= ? ORDER BY check_at, id;

-- name: NextUsageLimitCheck :one
SELECT CAST(coalesce(min(check_at), 0) AS INTEGER) AS at FROM usage_limits WHERE cleared_at IS NULL AND check_at IS NOT NULL;

-- name: SetUsageLimitCheck :execrows
UPDATE usage_limits SET check_at = sqlc.narg(check_at), version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version) AND cleared_at IS NULL;

-- name: SetUsageLimitReset :execrows
UPDATE usage_limits SET resets_at = sqlc.narg(resets_at), version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version) AND cleared_at IS NULL;

-- A gateway hold's estimate from Harbor Pilot (quota board or budget status) and its next re-check.
-- name: SetUsageLimitEstimate :execrows
UPDATE usage_limits SET resets_at = sqlc.narg(resets_at), check_at = sqlc.narg(check_at), source = sqlc.arg(source), version = version + 1,
  updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version) AND cleared_at IS NULL;

-- name: ClearUsageLimit :execrows
UPDATE usage_limits SET updated_at = sqlc.arg(now), cleared_at = sqlc.arg(now), cleared_by = sqlc.arg(cleared_by), version = version + 1
WHERE id = sqlc.arg(id) AND cleared_at IS NULL;

-- An account-level "clear now": every space's open row for the account and kind (ADR-0079 decision 7). Needs account-level
-- permission; a space operator clears only their own space's row (ClearUsageLimit).
-- name: ClearUsageLimitsByAccount :execrows
UPDATE usage_limits SET updated_at = sqlc.arg(now), cleared_at = sqlc.arg(now), cleared_by = sqlc.arg(cleared_by), version = version + 1
WHERE scope = 'account' AND account_id = sqlc.arg(account_id) AND kind = sqlc.arg(kind) AND cleared_at IS NULL;

-- name: MarkUsageLimitControlNotified :execrows
UPDATE usage_limits SET updated_at = sqlc.arg(now), notified_control_at = sqlc.arg(now), version = version + 1
WHERE id = sqlc.arg(id) AND notified_control_at IS NULL;

-- name: MarkUsageLimitHumansNotified :execrows
UPDATE usage_limits SET updated_at = sqlc.arg(now), notified_humans_at = sqlc.arg(now), version = version + 1
WHERE id = sqlc.arg(id) AND notified_humans_at IS NULL;

-- Open holds with a known reset later than the given time whose space's people were not told yet, in spaces that
-- aren't archived (ADR-0079 Q5). The space's own threshold is applied by the caller. A keyset page, furthest reset
-- first, after the cursor (resets_at, id): the caller reads every page, so holds it skips can't starve later ones.
-- name: ListUsageLimitsToTellPeople :many
SELECT u.* FROM usage_limits u JOIN spaces s ON s.id = u.space_id
WHERE u.cleared_at IS NULL AND u.notified_humans_at IS NULL AND u.resets_at > sqlc.arg(after)
  AND (u.resets_at < sqlc.arg(cursor_reset) OR (u.resets_at = sqlc.arg(cursor_reset) AND u.id < sqlc.arg(cursor_id)))
  AND s.state <> 'archived'
ORDER BY u.resets_at DESC, u.id DESC LIMIT sqlc.arg(lim);

-- name: InsertLimitWait :exec
INSERT INTO limit_waits (id, space_id, run_id, task_id, run_kind, role, node_id, instance_id, harness, session_id, launch_json, usage_limit_id,
  resume_at, release_at, attempts, switch_to_account_id, account_switch_id, cause, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- A switch's wait whose target changed (re-resolved) or that fell back to waiting (target cleared, resume_at moved).
-- name: SetLimitWaitSwitch :execrows
UPDATE limit_waits SET switch_to_account_id = sqlc.arg(switch_to_account_id), resume_at = sqlc.arg(resume_at), version = version + 1,
  updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version);

-- name: GetLimitWait :one
SELECT * FROM limit_waits WHERE id = ?;

-- name: GetLimitWaitByRun :one
SELECT * FROM limit_waits WHERE run_id = ?;

-- name: GetActiveLimitWait :one
SELECT * FROM limit_waits WHERE space_id = ? AND instance_id = ? AND state IN ('waiting', 'resuming');

-- name: ListLimitWaitsBySpace :many
SELECT * FROM limit_waits WHERE space_id = ? ORDER BY created_at DESC, id DESC LIMIT ?;

-- Waiting waits due to resume, control runs first, then account switches (ADR-0090), then the oldest (ADR-0079
-- decision 5).
-- name: ListDueLimitWaits :many
SELECT * FROM limit_waits WHERE state = 'waiting' AND resume_at <= ?
ORDER BY CASE run_kind WHEN 'control' THEN 0 ELSE 1 END, CASE WHEN switch_to_account_id <> '' THEN 0 ELSE 1 END, created_at, id LIMIT ?;

-- name: NextLimitWaitDue :one
SELECT CAST(coalesce(min(resume_at), 0) AS INTEGER) AS at FROM limit_waits WHERE state = 'waiting';

-- name: ListLimitWaitsToRelease :many
SELECT * FROM limit_waits WHERE state = 'waiting' AND release_at IS NOT NULL AND release_at <= ? ORDER BY release_at, id;

-- name: UpdateLimitWait :execrows
UPDATE limit_waits SET state = sqlc.arg(state), resume_at = sqlc.arg(resume_at), new_run_id = sqlc.arg(new_run_id),
  attempts = sqlc.arg(attempts), last_error = sqlc.arg(last_error), version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version);

-- Parking (UL-7). Live runs at a limit that SpaceMan may park when they outlive the keep-alive (control, worker and
-- diagnostic runs that are still wanted); uses ix_runs_observed_live.
-- name: ListLimitedLiveRuns :many
SELECT * FROM runs WHERE observed IN ('starting', 'running', 'stopping') AND limit_kind <> '' AND desired = 'running'
  AND kind IN ('control', 'worker', 'diagnostic')
ORDER BY id LIMIT ?;

-- The wait a run was resumed from (a re-park carries its release time and attempts forward).
-- name: GetLimitWaitResumedInto :one
SELECT * FROM limit_waits WHERE space_id = ? AND new_run_id = ? AND state = 'resumed' ORDER BY created_at DESC, id DESC LIMIT 1;

-- Waits left in resuming by a central that stopped mid-resume.
-- name: ListStuckLimitWaits :many
SELECT * FROM limit_waits WHERE state = 'resuming' AND updated_at < ? ORDER BY updated_at, id LIMIT ?;

-- name: NextLimitWaitRelease :one
SELECT CAST(coalesce(min(release_at), 0) AS INTEGER) AS at FROM limit_waits WHERE state = 'waiting' AND release_at IS NOT NULL;

-- Waits a space's control is waiting in (autostart leaves control to its wait).
-- name: GetActiveControlLimitWait :one
SELECT * FROM limit_waits WHERE space_id = ? AND run_kind = 'control' AND state IN ('waiting', 'resuming') LIMIT 1;

-- Waits that were put off for a reason that has just gone (the node said hello, the space resumed) are due now.
-- name: ListDeferredLimitWaits :many
SELECT * FROM limit_waits WHERE space_id = ? AND state = 'waiting' AND last_error = ? AND resume_at > ? ORDER BY created_at, id LIMIT ?;

-- A task's waiting waits (limit_waits(task_id) index): cancelled when the task is done with or given to another run.
-- name: ListTaskLimitWaits :many
SELECT * FROM limit_waits WHERE task_id = ? AND state = 'waiting' ORDER BY created_at, id;

-- Another live run of an instance than the one being launched (a resume re-checks it in its launch transaction).
-- name: CountOtherLiveRunsOfInstance :one
SELECT count(*) FROM runs WHERE space_id = ? AND instance_id = ? AND id <> ? AND observed IN ('pending', 'starting', 'running', 'stopping', 'unknown');

-- Waiting waits a cleared hold may let resume: parked under it, or put off because an account was held. Uses the
-- waiting-waits index (ix_limit_waits_due): only waits not yet due.
-- name: ListHoldWaitsToWake :many
SELECT * FROM limit_waits WHERE state = 'waiting' AND resume_at > sqlc.arg(now)
  AND (usage_limit_id = sqlc.arg(usage_limit_id) OR last_error = 'account_held')
ORDER BY resume_at, id LIMIT sqlc.arg(lim);

-- The latest non-limit StopFailure class a node reported for a run (protocol minor 13); a repeat writes nothing.
-- name: SetRunFailure :execrows
UPDATE runs SET failure = sqlc.arg(failure), version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND generation = sqlc.arg(generation) AND failure != sqlc.arg(failure);
