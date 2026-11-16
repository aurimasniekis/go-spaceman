-- 0060_memory_conflict_folder: which conflicts/<name>/ folder of the collection a conflict was preserved in.
--   folder  the folder name under conflicts/ in the collection ('' : not known, a conflict recorded before this
--           migration or by central). When an accepted checkpoint no longer contains the folder, the conflict is
--           resolved by the system (ADR-0092).

-- +goose Up
ALTER TABLE memory_conflicts ADD COLUMN folder TEXT NOT NULL DEFAULT '';

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
