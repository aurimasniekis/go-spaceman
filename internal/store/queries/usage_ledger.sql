-- name: GetUsageLedgerMark :one
SELECT counted_json, counted_cost FROM usage_ledger_marks WHERE space_id = ? AND mark_key = ?;

-- name: SetUsageLedgerMark :exec
INSERT INTO usage_ledger_marks (space_id, mark_key, counted_json, counted_cost, updated_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (space_id, mark_key) DO UPDATE SET counted_json = excluded.counted_json, counted_cost = excluded.counted_cost, updated_at = excluded.updated_at;

-- name: PruneUsageLedgerMarks :execrows
DELETE FROM usage_ledger_marks WHERE (space_id, mark_key) IN (
  SELECT k.space_id, k.mark_key FROM usage_ledger_marks k WHERE k.updated_at < sqlc.arg(cutoff) LIMIT sqlc.arg(lim));

-- name: AddUsageBucket :exec
INSERT INTO usage_buckets (space_id, account_id, role, harness, model, granularity, bucket_start, input_tokens, output_tokens, cache_write_tokens,
  cache_read_tokens, reasoning_tokens, requests, cost_usd)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (space_id, account_id, role, harness, model, granularity, bucket_start) DO UPDATE SET
  input_tokens = usage_buckets.input_tokens + excluded.input_tokens,
  output_tokens = usage_buckets.output_tokens + excluded.output_tokens,
  cache_write_tokens = usage_buckets.cache_write_tokens + excluded.cache_write_tokens,
  cache_read_tokens = usage_buckets.cache_read_tokens + excluded.cache_read_tokens,
  reasoning_tokens = usage_buckets.reasoning_tokens + excluded.reasoning_tokens,
  requests = usage_buckets.requests + excluded.requests,
  cost_usd = usage_buckets.cost_usd + excluded.cost_usd;

-- name: ListUsageBuckets :many
SELECT * FROM usage_buckets WHERE space_id = ? AND granularity = ? AND bucket_start >= ? AND bucket_start < ?
ORDER BY bucket_start, account_id, role, harness, model;

-- name: UpsertAccountWindow :exec
-- The newest observation wins: an observation that is not newer than the stored one changes nothing.
INSERT INTO account_windows (account_id, kind, used_pct, resets_at, observed_at, run_id, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (account_id, kind) DO UPDATE SET used_pct = excluded.used_pct, resets_at = excluded.resets_at, observed_at = excluded.observed_at,
  run_id = excluded.run_id, updated_at = excluded.updated_at
WHERE excluded.observed_at > account_windows.observed_at;

-- name: ListAccountWindows :many
SELECT * FROM account_windows WHERE account_id = ? ORDER BY kind;

-- name: UpsertAccountWindowSample :exec
-- One sample per account, kind and slot; a newer observation in the slot replaces the older one.
INSERT INTO account_window_samples (account_id, kind, slot, used_pct, resets_at, observed_at, run_id) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (account_id, kind, slot) DO UPDATE SET used_pct = excluded.used_pct, resets_at = excluded.resets_at, observed_at = excluded.observed_at,
  run_id = excluded.run_id
WHERE excluded.observed_at > account_window_samples.observed_at;

-- name: ListAccountWindowSamples :many
SELECT * FROM account_window_samples WHERE account_id = ? AND observed_at >= ? ORDER BY observed_at, kind;

-- name: PruneAccountWindowSamples :execrows
DELETE FROM account_window_samples WHERE (account_id, kind, slot) IN (
  SELECT s.account_id, s.kind, s.slot FROM account_window_samples s WHERE s.observed_at < sqlc.arg(cutoff) LIMIT sqlc.arg(lim));

-- name: OldestHourlyBucketBefore :one
SELECT bucket_start FROM usage_buckets WHERE granularity = 'hour' AND bucket_start < ? ORDER BY bucket_start LIMIT 1;

-- name: RollUpHourlyBuckets :exec
-- Adds the hourly rows of [from, to) into daily rows starting at day_start.
INSERT INTO usage_buckets (space_id, account_id, role, harness, model, granularity, bucket_start, input_tokens, output_tokens, cache_write_tokens,
  cache_read_tokens, reasoning_tokens, requests, cost_usd)
SELECT h.space_id, h.account_id, h.role, h.harness, h.model, 'day', sqlc.arg(day_start), SUM(h.input_tokens), SUM(h.output_tokens), SUM(h.cache_write_tokens),
  SUM(h.cache_read_tokens), SUM(h.reasoning_tokens), SUM(h.requests), SUM(h.cost_usd)
FROM usage_buckets h WHERE h.granularity = 'hour' AND h.bucket_start >= sqlc.arg(from_start) AND h.bucket_start < sqlc.arg(to_start)
GROUP BY h.space_id, h.account_id, h.role, h.harness, h.model
ON CONFLICT (space_id, account_id, role, harness, model, granularity, bucket_start) DO UPDATE SET
  input_tokens = usage_buckets.input_tokens + excluded.input_tokens,
  output_tokens = usage_buckets.output_tokens + excluded.output_tokens,
  cache_write_tokens = usage_buckets.cache_write_tokens + excluded.cache_write_tokens,
  cache_read_tokens = usage_buckets.cache_read_tokens + excluded.cache_read_tokens,
  reasoning_tokens = usage_buckets.reasoning_tokens + excluded.reasoning_tokens,
  requests = usage_buckets.requests + excluded.requests,
  cost_usd = usage_buckets.cost_usd + excluded.cost_usd;

-- name: DeleteHourlyBuckets :execrows
DELETE FROM usage_buckets WHERE granularity = 'hour' AND bucket_start >= ? AND bucket_start < ?;

-- name: DeleteDailyBucketsBefore :execrows
DELETE FROM usage_buckets WHERE granularity = 'day' AND bucket_start < ?;

-- name: PruneClosedUsageLimits :execrows
DELETE FROM usage_limits WHERE id IN (SELECT l.id FROM usage_limits l WHERE l.cleared_at IS NOT NULL AND l.cleared_at < sqlc.arg(cutoff) LIMIT sqlc.arg(lim));

-- name: PruneClosedLimitWaits :execrows
DELETE FROM limit_waits WHERE id IN (SELECT w.id FROM limit_waits w WHERE w.state IN ('resumed', 'released', 'cancelled', 'failed') AND w.updated_at < sqlc.arg(cutoff)
  LIMIT sqlc.arg(lim));

-- name: PruneClosedAccountSwitches :execrows
DELETE FROM account_switches WHERE id IN (SELECT a.id FROM account_switches a WHERE a.state IN ('succeeded', 'failed', 'fell_back', 'cancelled')
  AND a.updated_at < sqlc.arg(cutoff) LIMIT sqlc.arg(lim));

-- name: ListAccountUsageBuckets :many
-- An account's buckets across spaces (the API keeps only the spaces the caller may read).
SELECT * FROM usage_buckets WHERE account_id = ? AND granularity = ? AND bucket_start >= ? AND bucket_start < ?
ORDER BY bucket_start, space_id, role, harness, model;

-- name: ListAccountBindingSpaces :many
SELECT space_id FROM space_accounts WHERE account_id = ? ORDER BY space_id;

-- name: ListSpaceAccountWindows :many
-- The latest windows of the accounts bound to a space.
SELECT w.account_id, a.name AS account_name, sa.harness, w.kind, w.used_pct, w.resets_at, w.observed_at
FROM space_accounts sa
JOIN accounts a ON a.id = sa.account_id
JOIN account_windows w ON w.account_id = sa.account_id
WHERE sa.space_id = ?
ORDER BY sa.harness, sa.created_at, w.account_id, w.kind;
