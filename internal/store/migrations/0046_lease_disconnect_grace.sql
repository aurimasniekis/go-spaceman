-- 0046_lease_disconnect_grace: the disconnect grace a space's lease was last granted with (ADR-0074). A node's lease
-- guard stops a space's runs once the lease's expiry plus the grace from its last grant pass without central, so the
-- stale-run sweeper waits for the larger of this grace and the current one before it treats a disconnected node's runs as
-- stopped. 0: granted before this column existed (the current grace applies).

-- +goose Up
ALTER TABLE execution_leases ADD COLUMN grace_us INTEGER NOT NULL DEFAULT 0;

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
