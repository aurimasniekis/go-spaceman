-- 0026_relocations: explicit, audited space relocation (SM-INV-003, SM-SPACE-005…008, ADR-0037). A relocation is a resumable
-- state machine owned by central. spaces.desired_node_id holds the destination while it runs (SM-SPACE-006); spaces.node_id
-- and the execution lease move to the destination only in the finalizing transaction, with a new lease generation that
-- fences the source. Reports and manifests are JSON produced by central from node reports; they never contain file
-- contents, secrets or terminal output.

-- +goose Up
CREATE TABLE relocations (
  id                TEXT PRIMARY KEY,
  space_id          TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  source_node_id    TEXT NOT NULL REFERENCES nodes (id),
  dest_node_id      TEXT NOT NULL REFERENCES nodes (id),
  -- clean: the source is online and checked; source_unavailable: central's copies only, with an acknowledgement;
  -- precheck: a dry run that inspects the source and changes nothing.
  mode              TEXT NOT NULL CHECK (mode IN ('clean', 'source_unavailable', 'precheck')),
  state             TEXT NOT NULL CHECK (state IN ('requested', 'draining', 'checking', 'blocked', 'exporting', 'preparing_destination',
                                                   'finalizing', 'done', 'checked', 'failed', 'cancelled')),
  -- The step a blocked relocation resumes at when retried: check, export or prepare.
  blocked_step      TEXT NOT NULL DEFAULT '' CHECK (blocked_step IN ('', 'check', 'export', 'prepare')),
  dirty_policy      TEXT NOT NULL DEFAULT 'refuse' CHECK (dirty_policy IN ('refuse', 'export')),
  stop_runs         INTEGER NOT NULL DEFAULT 0,
  recovery          TEXT NOT NULL DEFAULT 'retain' CHECK (recovery IN ('retain', 'delete')),
  retain_days       INTEGER NOT NULL DEFAULT 0,
  -- source_unavailable only: the operator's acknowledgement of possibly lost local state, verbatim.
  acknowledgement   TEXT NOT NULL DEFAULT '',
  -- Operator-listed repositories for source_unavailable (central keeps no repository configuration).
  repos_json        TEXT NOT NULL DEFAULT '[]',
  report_json       TEXT NOT NULL DEFAULT '',
  issues_json       TEXT NOT NULL DEFAULT '[]',
  loss_json         TEXT NOT NULL DEFAULT '',
  export_json       TEXT NOT NULL DEFAULT '',
  prepare_json      TEXT NOT NULL DEFAULT '',
  -- Commands of the step in flight (node_commands.id); a report is accepted only for the current one.
  step_command_id   TEXT NOT NULL DEFAULT '',
  release_command_id TEXT NOT NULL DEFAULT '',
  generation_before INTEGER NOT NULL DEFAULT 0,
  generation_after  INTEGER NOT NULL DEFAULT 0,
  paused_space      INTEGER NOT NULL DEFAULT 0,
  error             TEXT NOT NULL DEFAULT '',
  source_state      TEXT NOT NULL DEFAULT '' CHECK (source_state IN ('', 'pending', 'retained', 'deleted', 'unreachable')),
  retain_until      INTEGER,
  version           INTEGER NOT NULL DEFAULT 1,
  created_at        INTEGER NOT NULL,
  created_by        TEXT NOT NULL,
  updated_at        INTEGER NOT NULL,
  finished_at       INTEGER
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_relocations_space ON relocations (space_id, created_at);
-- One relocation in progress per space (precheck dry runs included, so a dry run never races a real one).
CREATE UNIQUE INDEX ux_relocations_active ON relocations (space_id)
  WHERE state NOT IN ('done', 'checked', 'failed', 'cancelled');
CREATE INDEX ix_relocations_retain ON relocations (retain_until) WHERE source_state = 'retained' AND retain_until IS NOT NULL;

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
