-- name: InsertEvent :one
INSERT INTO events (ts, kind, session_id, project_id, harness, source, data)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING seq;

-- the chain links to the latest event before seq, "" when there's none or it's from before the chain
-- name: PrevHash :one
SELECT CAST(COALESCE((SELECT hash FROM events WHERE seq < ? ORDER BY seq DESC LIMIT 1), '') AS TEXT);

-- name: SetHash :exec
UPDATE events SET prev_hash = ?, hash = ? WHERE seq = ?;

-- name: InsertPrompt :exec
INSERT INTO prompts (seq, text) VALUES (?, ?);

-- name: GetPrompt :one
SELECT text FROM prompts WHERE seq = ?;

-- name: EventsBetween :many
SELECT * FROM events
WHERE ts >= sqlc.arg(from_ts) AND ts < sqlc.arg(to_ts)
ORDER BY ts, seq;

-- name: CountSessionEvents :one
SELECT count(*) FROM events WHERE session_id = ? AND source = ?;

-- name: PendingEvents :many
SELECT * FROM events
WHERE uploaded_at IS NULL
ORDER BY seq
LIMIT ?;

-- name: UnsentEvents :many
SELECT * FROM events WHERE uploaded_at IS NULL ORDER BY seq;

-- name: MarkAllPending :exec
UPDATE events SET uploaded_at = NULL;

-- name: MarkUploaded :exec
UPDATE events
SET uploaded_at = sqlc.arg(uploaded_at)
WHERE seq <= sqlc.arg(up_to_seq) AND uploaded_at IS NULL;

-- name: GetMeta :one
SELECT value FROM meta WHERE key = ?;

-- name: SetMeta :exec
INSERT INTO meta (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value;

-- name: SetMetaIfMissing :exec
INSERT INTO meta (key, value) VALUES (?, ?)
ON CONFLICT (key) DO NOTHING;

-- name: GetSessionState :one
SELECT data FROM session_state WHERE session_id = ?;

-- name: PutSessionState :exec
INSERT INTO session_state (session_id, data, updated_at) VALUES (?, ?, ?)
ON CONFLICT (session_id) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at;

-- name: SyncStatus :one
SELECT
    CAST(count(*) FILTER (WHERE uploaded_at IS NULL) AS INTEGER) AS pending,
    CAST(COALESCE(max(uploaded_at), 0) AS INTEGER) AS last_upload
FROM events;

-- name: LastEventFrom :one
SELECT CAST(COALESCE(max(ts), 0) AS INTEGER) FROM events WHERE harness = ? AND source = ?;

-- name: ListProjects :many
SELECT DISTINCT project_id FROM events WHERE project_id <> '' ORDER BY project_id;

-- name: ProjectPromptSummary :one
SELECT count(*) AS total, count(p.seq) AS cached
FROM events e LEFT JOIN prompts p ON p.seq = e.seq
WHERE e.project_id = ? AND e.kind = 'prompt_sent';

-- name: LocalReviewPrompt :one
SELECT p.text, e.session_id, e.ts FROM prompts p JOIN events e ON e.seq = p.seq
WHERE e.project_id = ? AND p.seq = ? AND e.kind = 'prompt_sent';

-- name: MatchesLocalReviewAnchor :one
SELECT EXISTS(SELECT 1 FROM events WHERE seq = ? AND project_id = ? AND hash = ?);
