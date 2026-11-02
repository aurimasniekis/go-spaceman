-- 0020_usage: normalized run usage and stored native transcripts (SM-REC-005, ADR-0028). Usage keeps only what the
-- harness recorded: unknown stays unknown (NULL, an "unavailable" source), never zero; a cost estimate keeps who made it
-- and what it assumes. Transcripts are kept apart from terminal recordings.

-- +goose Up
CREATE TABLE run_usage (
  run_id           TEXT PRIMARY KEY REFERENCES runs (id) ON DELETE CASCADE,
  space_id         TEXT NOT NULL,
  source           TEXT NOT NULL,
  models_json      TEXT NOT NULL DEFAULT '[]',
  estimate_json    TEXT NOT NULL DEFAULT '',
  transcript_bytes INTEGER,
  final            INTEGER NOT NULL DEFAULT 0 CHECK (final IN (0, 1)),
  note             TEXT NOT NULL DEFAULT '',
  updated_at       INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE INDEX run_usage_space ON run_usage (space_id);

CREATE TABLE run_transcripts (
  run_id     TEXT PRIMARY KEY REFERENCES runs (id) ON DELETE CASCADE,
  space_id   TEXT NOT NULL,
  size       INTEGER NOT NULL,
  received   INTEGER NOT NULL DEFAULT 0,
  sha256     TEXT NOT NULL DEFAULT '',
  truncated  INTEGER NOT NULL DEFAULT 0 CHECK (truncated IN (0, 1)),
  state      TEXT NOT NULL CHECK (state IN ('receiving', 'stored', 'failed', 'deleted')),
  error      TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

-- +goose Down
DROP TABLE run_transcripts;
DROP TABLE run_usage;
