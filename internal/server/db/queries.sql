-- api_key is only used when the user is new
-- name: UpsertUser :one
INSERT INTO users (id, slack_id, name, api_key) VALUES ($1, $2, $3, $4)
ON CONFLICT (id) DO UPDATE SET slack_id = excluded.slack_id, name = excluded.name
RETURNING *;

-- name: UserByAPIKey :one
SELECT * FROM users WHERE api_key = $1;

-- name: SetAPIKey :exec
UPDATE users SET api_key = $2 WHERE id = $1;

-- creates the install on first sight. no row back means it belongs to someone else
-- name: ClaimInstall :one
INSERT INTO installs (id, user_id) VALUES ($1, $2)
ON CONFLICT (id) DO UPDATE SET last_seen_at = now()
WHERE installs.user_id = excluded.user_id
RETURNING id;

-- retries are safe: a seq already stored is skipped
-- name: InsertEvent :execrows
INSERT INTO events (install_id, seq, ts, kind, session_id, project_id, harness, source, data, prev_hash, hash)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (install_id, seq) DO NOTHING;

-- locked until the batch commits, so two syncs can't both extend the chain
-- name: InstallHead :one
SELECT head_seq, head_hash FROM installs WHERE id = $1 FOR UPDATE;

-- name: SetInstallHead :exec
UPDATE installs SET head_seq = $2, head_hash = $3 WHERE id = $1;

-- name: StoredEventHash :one
SELECT hash FROM events WHERE install_id = $1 AND seq = $2;

-- name: UserEventsBetween :many
SELECT e.* FROM events e
JOIN installs i ON i.id = e.install_id
WHERE i.user_id = $1 AND e.ts >= sqlc.arg(from_ts) AND e.ts < sqlc.arg(to_ts)
ORDER BY e.ts, e.seq;

-- name: UpsertProject :exec
INSERT INTO projects (install_id, project_id, name) VALUES ($1, $2, $3)
ON CONFLICT (install_id, project_id) DO UPDATE SET name = excluded.name, updated_at = now();

-- name: UserProjects :many
SELECT p.* FROM projects p
JOIN installs i ON i.id = p.install_id
WHERE i.user_id = $1;

-- name: CreateSession :exec
INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3);

-- name: UserBySession :one
SELECT u.* FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = $1 AND s.expires_at > now();

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token_hash = $1;

-- name: SessionModels :many
SELECT DISTINCT ON (e.session_id) e.session_id, (e.data->>'model')::text AS model
FROM events e
JOIN installs i ON i.id = e.install_id
WHERE i.user_id = $1 AND e.kind = 'session_start' AND e.session_id = ANY(sqlc.arg(sessions)::text[])
  AND e.data->>'model' <> ''
ORDER BY e.session_id, e.ts DESC;

-- name: VerificationCheckpoint :one
SELECT last_seq, received_at FROM verification_checkpoints
WHERE user_id = $1 AND install_id = $2 AND project_id = $3;

-- name: AdvanceVerification :one
INSERT INTO verification_checkpoints (user_id, install_id, project_id, last_seq)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id, install_id, project_id) DO UPDATE SET
    received_at = CASE WHEN excluded.last_seq > verification_checkpoints.last_seq
                       THEN now() ELSE verification_checkpoints.received_at END,
    last_seq = greatest(excluded.last_seq, verification_checkpoints.last_seq)
RETURNING last_seq, received_at;
