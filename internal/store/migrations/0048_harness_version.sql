-- 0048_harness_version: harness versions (ADR-0077, node protocol minor 11). A harness profile can pin the harness
-- version its runs get: '' follows the node's channel, else latest, stable or an exact X.Y.Z (checked by the catalog).
-- A run records what it actually launched, from its node's run reports: the version, what was requested (the profile's
-- pin, else the node's channel) and the fallback reason when another version than the request's target ran. '' means
-- not reported (an older node, a run that never started, or a harness the node doesn't manage).

-- +goose Up
ALTER TABLE harness_profiles ADD COLUMN harness_version TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN harness_version TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN harness_requested TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN harness_fallback TEXT NOT NULL DEFAULT '';

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
