-- name: UpsertRunUsage :exec
INSERT INTO run_usage (run_id, space_id, source, models_json, estimate_json, transcript_bytes, final, note, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (run_id) DO UPDATE SET source = excluded.source, models_json = excluded.models_json, estimate_json = excluded.estimate_json,
  transcript_bytes = excluded.transcript_bytes, final = excluded.final, note = excluded.note, updated_at = excluded.updated_at
-- A harness's own figures replace usage Harbor Pilot filled in (even when final); "unavailable" never replaces it.
WHERE (run_usage.final = 0 AND NOT (run_usage.source = 'harbor-pilot' AND excluded.source = 'unavailable'))
   OR (run_usage.source = 'harbor-pilot' AND excluded.source NOT IN ('unavailable', 'harbor-pilot'));

-- name: GetRunUsage :one
SELECT * FROM run_usage WHERE run_id = ?;

-- name: ListSpaceUsage :many
SELECT u.*, r.role, r.kind, r.harness, r.created_at AS run_created_at FROM run_usage u JOIN runs r ON r.id = u.run_id
WHERE u.space_id = ? ORDER BY r.created_at DESC LIMIT ?;

-- name: CountRunsWithoutUsage :one
SELECT COUNT(*) FROM runs r WHERE r.space_id = ? AND NOT EXISTS (SELECT 1 FROM run_usage u WHERE u.run_id = r.id);

-- name: GetRunTranscript :one
SELECT * FROM run_transcripts WHERE run_id = ?;

-- name: InsertRunTranscript :exec
INSERT INTO run_transcripts (run_id, space_id, size, sha256, truncated, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, 'receiving', ?, ?)
ON CONFLICT (run_id) DO NOTHING;

-- name: SetRunTranscriptProgress :exec
UPDATE run_transcripts SET received = ?, state = ?, error = ?, updated_at = ? WHERE run_id = ?;
