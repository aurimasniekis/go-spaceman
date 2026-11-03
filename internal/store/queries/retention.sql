-- Retention (ADR-0045): the daily pass, its candidates and the audit log's pruned prefix.

-- name: InsertRetentionPass :exec
INSERT INTO retention_passes (id, trigger, requested_by, state, started_at) VALUES (?, ?, ?, 'running', ?);

-- name: FinishRetentionPass :exec
UPDATE retention_passes SET state = ?, finished_at = ?, deleted_json = ?, error = ? WHERE id = ?;

-- name: InterruptRetentionPasses :execrows
UPDATE retention_passes SET state = 'interrupted', finished_at = ? WHERE state = 'running';

-- name: LastRetentionPass :one
SELECT * FROM retention_passes ORDER BY started_at DESC LIMIT 1;

-- name: ListRetentionPasses :many
SELECT * FROM retention_passes ORDER BY started_at DESC LIMIT ?;

-- name: ListRetentionSpaces :many
SELECT id, slug FROM spaces ORDER BY id;

-- Ended runs of a space whose central recording is older than the cutoff and not yet deleted.
-- name: RetainableRecordings :many
SELECT id FROM runs
WHERE space_id = sqlc.arg(space_id) AND ended_at IS NOT NULL AND ended_at < sqlc.arg(cutoff) AND recording_deleted_at IS NULL
ORDER BY ended_at LIMIT sqlc.arg(lim);

-- name: MarkRecordingDeleted :execrows
UPDATE runs SET recording_deleted_at = ? WHERE id = ? AND recording_deleted_at IS NULL;

-- name: RetainableTranscripts :many
SELECT run_id FROM run_transcripts
WHERE space_id = sqlc.arg(space_id) AND state = 'stored' AND updated_at < sqlc.arg(cutoff)
ORDER BY updated_at LIMIT sqlc.arg(lim);

-- Finished human actions older than the cutoff that nothing open refers to: no open task they block or belong to, no
-- open follow-up, and no response notification that has not been acknowledged.
-- name: RetainableHumanActions :many
SELECT h.id, h.version FROM human_actions h
WHERE h.space_id = sqlc.arg(space_id) AND (sqlc.narg(only_id) IS NULL OR h.id = sqlc.narg(only_id)) AND h.status IN ('resolved', 'cancelled', 'expired') AND h.updated_at < sqlc.arg(cutoff)
  AND NOT EXISTS (SELECT 1 FROM task_blockers b JOIN tasks t ON t.id = b.task_id
    WHERE b.kind = 'human_action' AND b.ref_id = h.id AND t.status NOT IN ('done', 'failed', 'cancelled'))
  AND NOT EXISTS (SELECT 1 FROM tasks t WHERE h.task_id <> '' AND t.id = h.task_id AND t.status NOT IN ('done', 'failed', 'cancelled'))
  AND NOT EXISTS (SELECT 1 FROM human_actions f WHERE f.follow_up_of = h.id AND f.status IN ('open', 'in_progress'))
  AND NOT EXISTS (SELECT 1 FROM human_notifications n WHERE n.action_id = h.id AND n.state <> 'acknowledged')
  AND NOT EXISTS (SELECT 1 FROM inbox_items i WHERE i.linked_kind = 'human_action' AND i.linked_id = h.id
    AND i.status IN ('new', 'in_progress', 'deferred'))
ORDER BY h.updated_at LIMIT sqlc.arg(lim);

-- name: DeleteFinishedHumanAction :execrows
DELETE FROM human_actions WHERE id = ? AND version = ? AND status IN ('resolved', 'cancelled', 'expired');

-- Attachments older than the cutoff that are used only by finished items: every referencing inbox item is done or
-- rejected, unchanged since the cutoff, and neither linked to nor the origin of an open task or human action. An
-- attachment with an owner of any other kind is kept. Unreferenced uploads are left to the hourly collection.
-- name: RetainableAttachments :many
SELECT a.id, a.size FROM attachments a
WHERE a.space_id = sqlc.arg(space_id) AND (sqlc.narg(only_id) IS NULL OR a.id = sqlc.narg(only_id)) AND a.created_at < sqlc.arg(cutoff)
  AND EXISTS (SELECT 1 FROM attachment_refs r WHERE r.attachment_id = a.id)
  AND NOT EXISTS (
    SELECT 1 FROM attachment_refs r WHERE r.attachment_id = a.id AND (
      r.owner_kind <> 'inbox_item'
      OR NOT EXISTS (SELECT 1 FROM inbox_items i WHERE i.id = r.owner_id)
      OR EXISTS (
        SELECT 1 FROM inbox_items i WHERE i.id = r.owner_id AND (
          i.status NOT IN ('done', 'rejected') OR i.updated_at >= sqlc.arg(cutoff)
          OR (i.linked_kind = 'task' AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = i.linked_id AND t.status NOT IN ('done', 'failed', 'cancelled')))
          OR (i.linked_kind = 'human_action' AND EXISTS (SELECT 1 FROM human_actions h WHERE h.id = i.linked_id AND h.status IN ('open', 'in_progress')))
          OR EXISTS (SELECT 1 FROM tasks t WHERE t.origin_id = i.id AND t.status NOT IN ('done', 'failed', 'cancelled'))
          OR EXISTS (SELECT 1 FROM human_actions h WHERE h.inbox_item_id = i.id AND h.status IN ('open', 'in_progress'))))))
ORDER BY a.created_at LIMIT sqlc.arg(lim);

-- name: DeleteAttachment :execrows
DELETE FROM attachments WHERE id = ?;

-- The newest audit event older than the cutoff, within the next batch after the pruned prefix.
-- name: AuditPruneBoundary :one
SELECT seq, hash, at FROM audit_events WHERE at < sqlc.arg(cutoff) AND seq <= sqlc.arg(max_seq) ORDER BY seq DESC LIMIT 1;

-- name: DeleteAuditThrough :execrows
DELETE FROM audit_events WHERE seq <= ?;

-- name: GetAuditPrune :one
SELECT * FROM audit_prune WHERE id = 1;

-- name: UpsertAuditPrune :exec
INSERT INTO audit_prune (id, through_seq, through_hash, through_at, pruned, updated_at) VALUES (1, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET through_seq = excluded.through_seq, through_hash = excluded.through_hash, through_at = excluded.through_at,
  pruned = audit_prune.pruned + excluded.pruned, updated_at = excluded.updated_at;

-- name: OldestAuditSeq :one
SELECT CAST(coalesce(min(seq), 0) AS INTEGER) AS oldest FROM audit_events;

-- name: ListAuditForward :many
SELECT * FROM audit_events
WHERE seq > sqlc.arg(after_seq)
  AND (sqlc.narg(action) IS NULL OR action = sqlc.narg(action))
  AND (sqlc.narg(target_id) IS NULL OR target_id = sqlc.narg(target_id))
  AND (sqlc.narg(actor_id) IS NULL OR actor_id = sqlc.narg(actor_id))
ORDER BY seq LIMIT sqlc.arg(lim);
