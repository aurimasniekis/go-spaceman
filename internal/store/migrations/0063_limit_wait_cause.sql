-- 0063_limit_wait_cause: why a limit wait exists (ADR-0098).
--   cause  '' : the run was parked at a usage limit (ADR-0079); 'agent_restart': the run ended stopped_by_restart
--          while it was wanted, and the same wait machinery resumes it (or, after the attempt cap, gives it up).

-- +goose Up
ALTER TABLE limit_waits ADD COLUMN cause TEXT NOT NULL DEFAULT '';

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
