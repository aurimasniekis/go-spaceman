-- 0028_core_subscriptions: plain NATS Core event subscriptions (SM-EVT-011, ADR-0042).
--
-- A subscription receives events either through its own JetStream durable consumer (durable: acknowledged after the
-- inbox commit, redelivered, replayable) or through a plain NATS Core subscription (core) for sources that publish
-- without JetStream. Core subscriptions are non-durable: nothing is acknowledged or redelivered, there is no cursor to
-- replay or reset, and messages published while SpaceMan is not connected are lost. The mode is fixed at creation.

-- +goose Up
ALTER TABLE event_subscriptions ADD COLUMN delivery TEXT NOT NULL DEFAULT 'durable' CHECK (delivery IN ('durable', 'core'));

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
