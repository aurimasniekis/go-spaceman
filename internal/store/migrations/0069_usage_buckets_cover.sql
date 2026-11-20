-- 0069_usage_buckets_cover: the cross-space usage series (get-usage-series, ADR-0103) sums every space's ledger rows
-- of a range. Through ix_usage_buckets_time each row cost a primary-key lookup for its counters (about 1 s for 30 days
-- of 200k rows); this index carries the counters, so the sum reads the index alone. It replaces the time index: its
-- leading columns are the same (granularity, bucket_start), so the retention roll-up and pruning use it as before.
--
-- Rollback (additive, nothing else depends on it): DROP INDEX ix_usage_buckets_time_cover; then
-- CREATE INDEX ix_usage_buckets_time ON usage_buckets (granularity, bucket_start);

-- +goose Up
CREATE INDEX ix_usage_buckets_time_cover ON usage_buckets (granularity, bucket_start, space_id, account_id, role, harness, model, input_tokens,
  output_tokens, cache_write_tokens, cache_read_tokens, reasoning_tokens, requests, cost_usd);
DROP INDEX ix_usage_buckets_time;

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
