-- 0027_xspace: cross-space requests (SM-XSPACE-001…003, ADR-0038).
--
-- Each space configures, per peer space, what it accepts from that peer (inbound: discovery, questions, delegation,
-- knowledge, what its replies may disclose, quotas) and what its own control may send to that peer (outbound). Both are
-- denied unless a row says otherwise, and a request needs the destination's inbound and the source's outbound grant.
--
-- A request is created by the source space's control and lands in the destination's inbox (questions, delegation) or
-- is routed to one of the destination's knowledge bases (knowledge). The destination's control accepts, declines and
-- replies; it owns any local work. The stored reply is what the destination's disclosure level allowed, never more.

-- +goose Up
CREATE TABLE xspace_settings (
  space_id              TEXT PRIMARY KEY REFERENCES spaces (id) ON DELETE CASCADE,
  -- Shown to peers this space lets discover it (agent-visible, so it is written for agents).
  profile               TEXT NOT NULL DEFAULT '',
  -- Requests deeper than this (a request caused by a request caused by …) are refused at this destination.
  max_depth             INTEGER NOT NULL DEFAULT 3 CHECK (max_depth BETWEEN 1 AND 8),
  -- Requests this space's control may send per hour, to every peer together.
  max_outgoing_per_hour INTEGER NOT NULL DEFAULT 60 CHECK (max_outgoing_per_hour BETWEEN 1 AND 1000),
  version               INTEGER NOT NULL DEFAULT 1,
  updated_at            INTEGER NOT NULL,
  updated_by            TEXT NOT NULL
) STRICT, WITHOUT ROWID;

CREATE TABLE xspace_peers (
  space_id         TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  peer_space_id    TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  -- Inbound: what space_id accepts from peer_space_id.
  in_discover      INTEGER NOT NULL DEFAULT 0 CHECK (in_discover IN (0, 1)),
  in_questions     INTEGER NOT NULL DEFAULT 0 CHECK (in_questions IN (0, 1)),
  in_delegation    INTEGER NOT NULL DEFAULT 0 CHECK (in_delegation IN (0, 1)),
  in_knowledge     INTEGER NOT NULL DEFAULT 0 CHECK (in_knowledge IN (0, 1)),
  -- KB slugs the peer may ask; empty means every enabled KB of the space.
  in_kbs_json      TEXT NOT NULL DEFAULT '[]',
  -- What replies to the peer may contain: none (the outcome only), summary (bounded text) or detailed (text and evidence).
  disclosure       TEXT NOT NULL DEFAULT 'none' CHECK (disclosure IN ('none', 'summary', 'detailed')),
  max_per_hour     INTEGER NOT NULL DEFAULT 20 CHECK (max_per_hour BETWEEN 1 AND 1000),
  max_pending      INTEGER NOT NULL DEFAULT 5 CHECK (max_pending BETWEEN 1 AND 100),
  -- Outbound: what space_id's control may send to peer_space_id.
  out_questions    INTEGER NOT NULL DEFAULT 0 CHECK (out_questions IN (0, 1)),
  out_delegation   INTEGER NOT NULL DEFAULT 0 CHECK (out_delegation IN (0, 1)),
  out_knowledge    INTEGER NOT NULL DEFAULT 0 CHECK (out_knowledge IN (0, 1)),
  version          INTEGER NOT NULL DEFAULT 1,
  created_at       INTEGER NOT NULL,
  created_by       TEXT NOT NULL,
  updated_at       INTEGER NOT NULL,
  updated_by       TEXT NOT NULL,
  PRIMARY KEY (space_id, peer_space_id),
  CHECK (space_id <> peer_space_id)
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_xspace_peers_peer ON xspace_peers (peer_space_id);

CREATE TABLE xspace_requests (
  id                 TEXT PRIMARY KEY,
  source_space_id    TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  dest_space_id      TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  kind               TEXT NOT NULL CHECK (kind IN ('question', 'delegation', 'knowledge')),
  title              TEXT NOT NULL,
  body               TEXT NOT NULL,
  kb_slug            TEXT NOT NULL DEFAULT '',
  kb_kind            TEXT NOT NULL DEFAULT '',
  requester_run      TEXT NOT NULL,
  requester_role     TEXT NOT NULL,
  -- The chain: the root request's ID, the request this one was caused by, the spaces visited (JSON list) and the depth.
  correlation_id     TEXT NOT NULL,
  caused_by          TEXT NOT NULL DEFAULT '',
  chain_json         TEXT NOT NULL,
  depth              INTEGER NOT NULL CHECK (depth >= 1),
  state              TEXT NOT NULL CHECK (state IN ('pending', 'accepted', 'completed', 'failed', 'declined', 'cancelled', 'expired')),
  deadline_at        INTEGER NOT NULL,
  -- Destination-local references (never shown to the source).
  inbox_item_id      TEXT NOT NULL DEFAULT '',
  kb_request_id      TEXT NOT NULL DEFAULT '',
  local_task_id      TEXT NOT NULL DEFAULT '',
  -- The reply as disclosed to the source (already reduced to the disclosure level in force when it was given).
  disclosure         TEXT NOT NULL DEFAULT '',
  reply              TEXT NOT NULL DEFAULT '',
  reply_truncated    INTEGER NOT NULL DEFAULT 0,
  reply_basis        TEXT NOT NULL DEFAULT '',
  reply_evidence_json TEXT NOT NULL DEFAULT '[]',
  reason             TEXT NOT NULL DEFAULT '',
  decided_by         TEXT NOT NULL DEFAULT '',
  decided_at         INTEGER,
  closed_at          INTEGER,
  dedup_key          TEXT NOT NULL DEFAULT '',
  created_at         INTEGER NOT NULL,
  updated_at         INTEGER NOT NULL,
  version            INTEGER NOT NULL DEFAULT 1
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_xspace_requests_source ON xspace_requests (source_space_id, created_at);
CREATE INDEX ix_xspace_requests_dest ON xspace_requests (dest_space_id, created_at);
CREATE INDEX ix_xspace_requests_pair ON xspace_requests (source_space_id, dest_space_id, created_at);
CREATE INDEX ix_xspace_requests_open ON xspace_requests (deadline_at) WHERE state IN ('pending', 'accepted');
CREATE UNIQUE INDEX ux_xspace_requests_dedup ON xspace_requests (source_space_id, requester_run, dedup_key) WHERE dedup_key <> '';

-- A KB request asked on behalf of a cross-space request reports to it instead of to a local requester.
ALTER TABLE kb_requests ADD COLUMN xspace_request_id TEXT NOT NULL DEFAULT '';

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
