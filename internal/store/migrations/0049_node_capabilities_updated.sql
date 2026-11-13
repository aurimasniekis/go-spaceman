-- 0049_node_capabilities_updated: when the node last reported its capabilities (ADR-0077, node protocol minor 11). Hello
-- reports them on every connection; from minor 11 on the node also sends node.capabilities when its managed harness
-- cache or a channel target changes, and at least every 30 minutes. NULL: never reported. Existing nodes reported them
-- in their last hello.

-- +goose Up
ALTER TABLE nodes ADD COLUMN capabilities_updated_at INTEGER;
UPDATE nodes SET capabilities_updated_at = last_connect_at;

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
