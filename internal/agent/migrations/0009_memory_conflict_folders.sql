-- 0009_memory_conflict_folders: the conflicts/<name>/ folders of the most recent conflicts the node preserved for a role
-- collection (newline-separated, oldest first, one per increment of conflicts), reported to central so it ties each
-- conflict to its folder exactly (ADR-0092).

-- +goose Up
ALTER TABLE memory_sync ADD COLUMN conflict_folders TEXT NOT NULL DEFAULT '';

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
