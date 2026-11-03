-- 0034_incidents_permissions: the incident policy's trusted triggers, worker handling, capacity and resumption
-- (SM-QUEUE-009), and human-action type permissions per space (SM-HUI-008). ADR-0050.
--
-- queue_policies gains the incident fields. The defaults keep what spaces had before: operators and control are the
-- trusted triggers, other incident requests become proposals, a preempted task's workers keep running (finish), no
-- extra top-level slot for incidents, and preempted work is resumed by control or an operator (manual).
-- tasks gain the incident that preempted them (cleared when they resume), the trigger that raised an incident, and the
-- source of an incident proposal that is waiting for an operator. task_preemptions records what happened to each
-- preempted task and its worker runs, and how it resumed.

-- +goose Up
ALTER TABLE queue_policies ADD COLUMN incident_triggers_json TEXT NOT NULL DEFAULT '["operator","control"]';
ALTER TABLE queue_policies ADD COLUMN incident_untrusted TEXT NOT NULL DEFAULT 'propose' CHECK (incident_untrusted IN ('propose', 'refuse'));
ALTER TABLE queue_policies ADD COLUMN incident_workers TEXT NOT NULL DEFAULT 'finish' CHECK (incident_workers IN ('finish', 'pause', 'interrupt'));
ALTER TABLE queue_policies ADD COLUMN incident_capacity INTEGER NOT NULL DEFAULT 0 CHECK (incident_capacity BETWEEN 0 AND 20);
ALTER TABLE queue_policies ADD COLUMN incident_resume TEXT NOT NULL DEFAULT 'manual' CHECK (incident_resume IN ('manual', 'auto'));

ALTER TABLE tasks ADD COLUMN preempted_by TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN incident_trigger TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN incident_proposed_by TEXT NOT NULL DEFAULT '';

CREATE TABLE task_preemptions (
  id          TEXT PRIMARY KEY,
  space_id    TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  incident_id TEXT NOT NULL,
  task_id     TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
  -- The worker run this row is about; empty for the task's own row.
  run_id      TEXT NOT NULL DEFAULT '',
  -- What the policy did: the task returned to ready (task rows), its worker finished, was paused or was interrupted.
  action      TEXT NOT NULL CHECK (action IN ('preempted', 'finish', 'pause', 'interrupt')),
  created_at  INTEGER NOT NULL,
  resumed_at  INTEGER,
  resumed_by  TEXT NOT NULL DEFAULT '',
  outcome     TEXT NOT NULL DEFAULT ''
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_task_preemptions_task ON task_preemptions (task_id, resumed_at);
CREATE INDEX ix_task_preemptions_incident ON task_preemptions (incident_id, resumed_at);

-- Who may see, create and respond to a human-action type in a space (SM-HUI-008). action_type is a built-in type
-- (question, approval, todo, message) or a custom catalog type's ID. No row: viewers see, operators create and
-- respond, any agent may ask. Roles are minimums over the space's built-in roles; the role permissions
-- (human_action:read/respond, approval:decide) still apply on top.
CREATE TABLE space_human_type_permissions (
  space_id         TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  action_type      TEXT NOT NULL,
  view_role        TEXT NOT NULL DEFAULT 'viewer' CHECK (view_role IN ('viewer', 'operator', 'admin')),
  create_role      TEXT NOT NULL DEFAULT 'operator' CHECK (create_role IN ('operator', 'admin')),
  respond_role     TEXT NOT NULL DEFAULT 'operator' CHECK (respond_role IN ('operator', 'admin')),
  agents           TEXT NOT NULL DEFAULT 'any' CHECK (agents IN ('any', 'roles', 'none')),
  agent_roles_json TEXT NOT NULL DEFAULT '[]',
  version          INTEGER NOT NULL DEFAULT 1,
  updated_at       INTEGER NOT NULL,
  updated_by       TEXT NOT NULL,
  PRIMARY KEY (space_id, action_type)
) STRICT, WITHOUT ROWID;

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
