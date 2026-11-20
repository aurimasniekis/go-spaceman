-- The cross-space usage series (get-usage-series, ADR-0103): the ledger summed in SQL per (bucket, group), so a range
-- across every space never loads its bucket rows into memory. Each list filter is a JSON array of strings, or NULL for
-- no filter; an empty array matches nothing. (JSON keeps one bound parameter per list: sqlc's slice expansion
-- renumbers anonymous parameters and breaks the numbered ones that follow a slice of more than one value.) bucket_us is 1 for an hourly series (hour rows are the buckets) and a
-- day in microseconds for a daily one (hour rows not yet rolled up fall into their UTC day next to the day rows).

-- name: SumUsageSeries :many
SELECT CAST(b.bucket_start - (b.bucket_start % sqlc.arg(bucket_us)) AS INTEGER) AS bucket,
  CAST(CASE sqlc.arg(group_by) WHEN 'space' THEN b.space_id WHEN 'account' THEN b.account_id WHEN 'model' THEN b.model WHEN 'role' THEN b.role
    ELSE b.harness END AS TEXT) AS grp,
  CAST(SUM(b.input_tokens) AS INTEGER) AS input_tokens, CAST(SUM(b.output_tokens) AS INTEGER) AS output_tokens,
  CAST(SUM(b.cache_write_tokens) AS INTEGER) AS cache_write_tokens, CAST(SUM(b.cache_read_tokens) AS INTEGER) AS cache_read_tokens,
  CAST(SUM(b.reasoning_tokens) AS INTEGER) AS reasoning_tokens, CAST(SUM(b.requests) AS INTEGER) AS requests, CAST(SUM(b.cost_usd) AS REAL) AS cost_usd
FROM usage_buckets b
WHERE b.granularity IN (SELECT value FROM json_each(sqlc.arg(grans))) AND b.bucket_start >= sqlc.arg(from_us) AND b.bucket_start < sqlc.arg(to_us)
  AND (sqlc.narg(spaces) IS NULL OR b.space_id IN (SELECT value FROM json_each(sqlc.narg(spaces))))
  AND (sqlc.narg(accounts) IS NULL OR b.account_id IN (SELECT value FROM json_each(sqlc.narg(accounts))))
  AND (sqlc.narg(models) IS NULL OR b.model IN (SELECT value FROM json_each(sqlc.narg(models))))
  AND (sqlc.narg(roles) IS NULL OR b.role IN (SELECT value FROM json_each(sqlc.narg(roles))))
  AND (sqlc.narg(harnesses) IS NULL OR b.harness IN (SELECT value FROM json_each(sqlc.narg(harnesses))))
GROUP BY bucket, grp
HAVING bucket > CAST(sqlc.arg(after_bucket) AS INTEGER) OR (bucket = CAST(sqlc.arg(after_bucket) AS INTEGER) AND grp > CAST(sqlc.arg(after_group) AS TEXT))
ORDER BY bucket, grp
LIMIT sqlc.arg(lim);

-- name: SumUsageTotals :one
SELECT CAST(COALESCE(SUM(b.input_tokens), 0) AS INTEGER) AS input_tokens, CAST(COALESCE(SUM(b.output_tokens), 0) AS INTEGER) AS output_tokens,
  CAST(COALESCE(SUM(b.cache_write_tokens), 0) AS INTEGER) AS cache_write_tokens, CAST(COALESCE(SUM(b.cache_read_tokens), 0) AS INTEGER) AS cache_read_tokens,
  CAST(COALESCE(SUM(b.reasoning_tokens), 0) AS INTEGER) AS reasoning_tokens, CAST(COALESCE(SUM(b.requests), 0) AS INTEGER) AS requests,
  CAST(COALESCE(SUM(b.cost_usd), 0) AS REAL) AS cost_usd
FROM usage_buckets b
WHERE b.granularity IN (SELECT value FROM json_each(sqlc.arg(grans))) AND b.bucket_start >= sqlc.arg(from_us) AND b.bucket_start < sqlc.arg(to_us)
  AND (sqlc.narg(spaces) IS NULL OR b.space_id IN (SELECT value FROM json_each(sqlc.narg(spaces))))
  AND (sqlc.narg(accounts) IS NULL OR b.account_id IN (SELECT value FROM json_each(sqlc.narg(accounts))))
  AND (sqlc.narg(models) IS NULL OR b.model IN (SELECT value FROM json_each(sqlc.narg(models))))
  AND (sqlc.narg(roles) IS NULL OR b.role IN (SELECT value FROM json_each(sqlc.narg(roles))))
  AND (sqlc.narg(harnesses) IS NULL OR b.harness IN (SELECT value FROM json_each(sqlc.narg(harnesses))));

-- name: CountUnledgeredRuns :many
-- Runs active in the range whose usage the ledger does not hold: every run of a harness other than Claude Code (Codex
-- keeps no usage SpaceMan can read; Harbor Pilot read-back goes to run_usage only), and a Claude Code run whose usage
-- came from another source. A run is active when it was created before the end and ended (or was last updated) after
-- the start.
SELECT r.harness, CAST(COUNT(*) AS INTEGER) AS runs, CAST(COALESCE(SUM(CASE WHEN u.source = 'harbor-pilot' THEN 1 ELSE 0 END), 0) AS INTEGER) AS harbor_pilot_runs
FROM runs r LEFT JOIN run_usage u ON u.run_id = r.id
WHERE r.created_at < sqlc.arg(to_us) AND COALESCE(r.ended_at, r.updated_at) >= CAST(sqlc.arg(from_us) AS INTEGER)
  AND (r.harness <> 'claude-code' OR (u.source IS NOT NULL AND u.source <> 'claude-transcript'))
  AND (sqlc.narg(spaces) IS NULL OR r.space_id IN (SELECT value FROM json_each(sqlc.narg(spaces))))
  AND (sqlc.narg(accounts) IS NULL OR (CASE WHEN u.run_id IS NOT NULL THEN u.account_id WHEN json_valid(r.launch_json)
    THEN COALESCE(json_extract(r.launch_json, '$.account_ref'), '') ELSE '' END) IN (SELECT value FROM json_each(sqlc.narg(accounts))))
  AND (sqlc.narg(roles) IS NULL OR r.role IN (SELECT value FROM json_each(sqlc.narg(roles))))
  AND (sqlc.narg(harnesses) IS NULL OR r.harness IN (SELECT value FROM json_each(sqlc.narg(harnesses))))
GROUP BY r.harness
ORDER BY r.harness;

-- name: ListSpaceNamesByID :many
SELECT id, name FROM spaces WHERE id IN (SELECT value FROM json_each(sqlc.arg(ids))) ORDER BY id;

-- name: ListBoundAccountNames :many
-- Names of the given accounts that are bound to at least one of the given spaces (or to any space when spaces is NULL),
-- directly or through a role pool: the rule of ListAllAccountBindings (the account-windows view).
SELECT a.id, a.name FROM accounts a
WHERE a.id IN (SELECT value FROM json_each(sqlc.arg(ids))) AND (
  EXISTS (SELECT 1 FROM space_accounts sa WHERE sa.account_id = a.id
    AND (sqlc.narg(spaces) IS NULL OR sa.space_id IN (SELECT value FROM json_each(sqlc.narg(spaces)))))
  OR EXISTS (SELECT 1 FROM space_role_accounts ra WHERE ra.account_id = a.id
    AND (sqlc.narg(spaces) IS NULL OR ra.space_id IN (SELECT value FROM json_each(sqlc.narg(spaces))))))
ORDER BY a.id;
