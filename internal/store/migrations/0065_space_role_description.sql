-- 0065_space_role_description: a space role's own description (ADR-0095).
--   description  what the role is for in this space ('' : use the role template's description). Routing guidance for
--                whoever chooses a role; it is not part of a launched agent's prompt or of run manifests.
-- Numbers 0061-0064 belong to other branches of the same release; the migrator needs no contiguous numbers.

-- +goose Up
ALTER TABLE space_roles ADD COLUMN description TEXT NOT NULL DEFAULT '';

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
