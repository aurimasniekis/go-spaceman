-- 0004_transcripts: uploads of ended runs' native transcripts to central (SM-REC-005). Next is the offset central has
-- stored; an interrupted upload resumes from it.

-- +goose Up
CREATE TABLE transcript_uploads (
  run_id     TEXT PRIMARY KEY,
  path       TEXT NOT NULL,
  size       INTEGER NOT NULL,
  sha256     TEXT NOT NULL,
  truncated  INTEGER NOT NULL DEFAULT 0,
  next       INTEGER NOT NULL DEFAULT 0,
  done       INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

-- +goose Down
DROP TABLE transcript_uploads;
