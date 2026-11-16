-- 0056_run_failure: the latest non-limit StopFailure class a node reported for a run (RunReport.failure, protocol minor
-- 13). An enum value from proto.FailureClasses, '' for none; no CHECK on purpose (central maps an unlisted value to ''
-- before storing, and a class added later needs no table rebuild).

-- +goose Up
ALTER TABLE runs ADD COLUMN failure TEXT NOT NULL DEFAULT '';

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
