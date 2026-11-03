-- 0005_relocations: outcomes of relocation steps (ADR-0037), re-sent to central after a reconnect until they age out, and
-- the folder name of every space the node has held, so ended runs' files are found after the space's lease is gone.

-- +goose Up
CREATE TABLE relocation_results (
  command_id    TEXT PRIMARY KEY,
  relocation_id TEXT NOT NULL,
  kind          TEXT NOT NULL,
  report_json   TEXT NOT NULL,
  created_at    INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE TABLE space_workdirs (
  space_id   TEXT PRIMARY KEY,
  workdir    TEXT NOT NULL,
  updated_at INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

-- +goose Down
DROP TABLE space_workdirs;
DROP TABLE relocation_results;
