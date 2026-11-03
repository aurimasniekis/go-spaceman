-- 0024_human_followup: what happens after a human action is asked (SM-HUM-009, SM-HUM-011, ADR-0032).
--
-- Policies per action type, overridable per space: reminders, escalation, rate limits, and how a response reaches a
-- requester whose run has ended. A row without a space is the installation default for that type; 'all' covers every
-- type. The most specific row applies as a whole (space and type, space and 'all', default and type, default and 'all'),
-- and built-in defaults apply when there is none.
--
-- Actions carry their next reminder and escalation times, so one timer for the earliest deadline drives expiry,
-- reminders and escalation (no polling). Notifications carry where they are routed and why: the requester's run, the
-- next run of the requester's agent instance (held until a deadline), the space's control, or the operators when no
-- agent can take it, with the original run, the previous target and the reason.

-- +goose Up
CREATE TABLE human_policies (
  id                     TEXT PRIMARY KEY,
  space_id               TEXT REFERENCES spaces (id) ON DELETE CASCADE,
  action_type            TEXT NOT NULL CHECK (action_type IN ('all', 'question', 'approval', 'todo', 'message', 'custom')),
  remind_after_minutes   INTEGER NOT NULL DEFAULT 0,
  remind_every_minutes   INTEGER NOT NULL DEFAULT 0,
  max_reminders          INTEGER NOT NULL DEFAULT 1,
  escalate_after_minutes INTEGER NOT NULL DEFAULT 0,
  escalate_priority      TEXT NOT NULL DEFAULT '' CHECK (escalate_priority IN ('', 'high', 'urgent')),
  escalate_assignee      TEXT NOT NULL DEFAULT '',
  requester_ended        TEXT NOT NULL DEFAULT 'reroute' CHECK (requester_ended IN ('reroute', 'next_launch')),
  hold_minutes           INTEGER NOT NULL DEFAULT 60,
  successor_role         TEXT NOT NULL DEFAULT '',
  wake_idle              INTEGER NOT NULL DEFAULT 1,
  rate_per_run_hour      INTEGER NOT NULL DEFAULT 30,
  rate_per_space_hour    INTEGER NOT NULL DEFAULT 300,
  created_at             INTEGER NOT NULL,
  created_by             TEXT NOT NULL,
  updated_at             INTEGER NOT NULL,
  updated_by             TEXT NOT NULL,
  version                INTEGER NOT NULL DEFAULT 1
) STRICT, WITHOUT ROWID;

CREATE UNIQUE INDEX ux_human_policies_default ON human_policies (action_type) WHERE space_id IS NULL;
CREATE UNIQUE INDEX ux_human_policies_space ON human_policies (space_id, action_type) WHERE space_id IS NOT NULL;

ALTER TABLE human_actions ADD COLUMN reminder_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE human_actions ADD COLUMN reminded_at INTEGER;
ALTER TABLE human_actions ADD COLUMN next_reminder_at INTEGER;
ALTER TABLE human_actions ADD COLUMN escalate_at INTEGER;
ALTER TABLE human_actions ADD COLUMN escalated_at INTEGER;
ALTER TABLE human_actions ADD COLUMN escalated_from TEXT NOT NULL DEFAULT '';

CREATE INDEX ix_human_reminder ON human_actions (next_reminder_at) WHERE next_reminder_at IS NOT NULL AND status IN ('open', 'in_progress');
CREATE INDEX ix_human_escalate ON human_actions (escalate_at) WHERE escalate_at IS NOT NULL AND status IN ('open', 'in_progress');
CREATE INDEX ix_human_requester_created ON human_actions (requester_run, created_at) WHERE requester_run <> '';
CREATE INDEX ix_human_space_created ON human_actions (space_id, created_at);

ALTER TABLE human_notifications ADD COLUMN target_kind TEXT NOT NULL DEFAULT 'run' CHECK (target_kind IN ('run', 'instance', 'control', 'operators'));
ALTER TABLE human_notifications ADD COLUMN target_instance TEXT NOT NULL DEFAULT '';
ALTER TABLE human_notifications ADD COLUMN hold_until INTEGER;
ALTER TABLE human_notifications ADD COLUMN wake INTEGER NOT NULL DEFAULT 1;
ALTER TABLE human_notifications ADD COLUMN original_run TEXT NOT NULL DEFAULT '';
ALTER TABLE human_notifications ADD COLUMN routed_from TEXT NOT NULL DEFAULT '';
ALTER TABLE human_notifications ADD COLUMN route_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE human_notifications ADD COLUMN routed_at INTEGER;
ALTER TABLE human_notifications ADD COLUMN route_count INTEGER NOT NULL DEFAULT 0;

-- Existing notifications were addressed to the requester's run.
UPDATE human_notifications SET original_run = target_run;

CREATE INDEX ix_human_notifications_open ON human_notifications (state, target_kind, target_run);
CREATE INDEX ix_human_notifications_hold ON human_notifications (hold_until) WHERE hold_until IS NOT NULL AND state = 'pending';

CREATE INDEX approvals_valid_until ON approvals (valid_until) WHERE state = 'approved' AND uses_left > 0;

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
