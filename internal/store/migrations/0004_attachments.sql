-- 0004_attachments: uploaded files (SM-ATT-*, ADR-0013). Content lives in the artifact store, addressed by sha256;
-- rows are per space (quota, authorization, purge) and are referenced by the items that use them.

-- +goose Up
CREATE TABLE attachments (
  id          TEXT PRIMARY KEY,
  space_id    TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  sha256      TEXT NOT NULL,
  size        INTEGER NOT NULL CHECK (size >= 0),
  mime        TEXT NOT NULL,
  name        TEXT NOT NULL,
  uploaded_by TEXT NOT NULL,
  created_at  INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_attachments_space ON attachments (space_id, created_at);
CREATE INDEX ix_attachments_sha ON attachments (sha256);

-- Which items use an attachment. An attachment without references is removed after a grace period (garbage
-- collection); its content is removed once no attachment row points to it.
CREATE TABLE attachment_refs (
  attachment_id TEXT NOT NULL REFERENCES attachments (id) ON DELETE CASCADE,
  owner_kind    TEXT NOT NULL,
  owner_id      TEXT NOT NULL,
  created_at    INTEGER NOT NULL,
  PRIMARY KEY (attachment_id, owner_kind, owner_id)
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_attachment_refs_owner ON attachment_refs (owner_kind, owner_id);

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
