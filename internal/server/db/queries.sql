-- name: UpsertUser :one
INSERT INTO users (id, slack_id, name) VALUES ($1, $2, $3)
ON CONFLICT (id) DO UPDATE SET slack_id = excluded.slack_id, name = excluded.name
RETURNING *;

-- name: UserByDeviceToken :one
SELECT u.* FROM users u JOIN device_credentials d ON d.user_id = u.id
WHERE d.token_hash = $1;

-- name: CreateDeviceCredential :exec
INSERT INTO device_credentials (id, user_id, token_hash, hostname) VALUES ($1, $2, $3, $4);

-- name: DeviceCredentials :many
SELECT id, created_at, hostname FROM device_credentials WHERE user_id = $1 ORDER BY created_at DESC, id;

-- name: RevokeDeviceCredential :exec
DELETE FROM device_credentials WHERE id = $1 AND user_id = $2;

-- name: DeleteExpiredDeviceLogins :exec
DELETE FROM device_login_requests WHERE expires_at <= now();

-- name: CreateDeviceLogin :exec
INSERT INTO device_login_requests (approval_hash, poll_hash, hostname, expires_at)
VALUES ($1, $2, $3, now() + interval '5 minutes');

-- name: PendingDeviceLogin :one
SELECT hostname FROM device_login_requests WHERE approval_hash = $1
AND user_id IS NULL AND NOT denied AND expires_at > now();

-- name: DecideDeviceLogin :execrows
UPDATE device_login_requests SET user_id = $2, denied = $3
WHERE approval_hash = $1 AND user_id IS NULL AND NOT denied AND expires_at > now();

-- name: PollDeviceLogin :one
SELECT user_id, denied, hostname FROM device_login_requests WHERE poll_hash = $1 AND expires_at > now() FOR UPDATE;

-- name: ConsumeDeviceLogin :exec
DELETE FROM device_login_requests WHERE poll_hash = $1;

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

-- name: AdminStats :one
SELECT
    (SELECT count(*) FROM users) AS accounts,
    (SELECT count(*) FROM users WHERE created_at >= now() - interval '30 days') AS new_accounts_30d,
    activity.active_7d,
    activity.active_30d,
    (SELECT count(DISTINCT (user_id, project_id)) FROM verification_analysis
     WHERE analyzed_at >= now() - interval '30 days') AS verified_projects_30d
FROM (
    SELECT count(DISTINCT i.user_id) FILTER (WHERE e.received_at >= now() - interval '7 days') AS active_7d,
           count(DISTINCT i.user_id) AS active_30d
    FROM events e JOIN installs i ON i.id = e.install_id
    WHERE e.received_at >= now() - interval '30 days'
) activity;

-- name: AnalysisCheckpoint :one
SELECT last_seq FROM verification_analysis WHERE user_id = $1 AND install_id = $2 AND project_id = $3;

-- name: LockAccountAnalysis :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(scope)::text, 0));

-- name: AdvanceAnalysis :exec
INSERT INTO verification_analysis (user_id, install_id, project_id, last_seq, unavailable)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (user_id, install_id, project_id) DO UPDATE
SET last_seq = excluded.last_seq, analyzed_at = now(), unavailable = verification_analysis.unavailable + excluded.unavailable;

-- name: ReviewPromptEvents :many
SELECT e.seq, e.ts, e.session_id, e.data, e.hash FROM events e JOIN installs i ON e.install_id = i.id
WHERE i.user_id = $1 AND e.install_id = $2 AND e.project_id = $3 AND e.kind = 'prompt_sent'
AND e.seq = ANY(sqlc.arg(sequences)::bigint[]);

-- name: ReviewHourCounts :many
SELECT event_hour, count(*) AS used FROM verification_review_signals
WHERE user_id = $1 AND project_id = $2 AND event_hour = ANY(sqlc.arg(hours)::timestamptz[])
GROUP BY event_hour;

-- name: SaveReviewSignal :exec
INSERT INTO verification_review_signals (user_id, install_id, project_id, seq, event_hour, label, model, metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: CurrentReviewPlan :one
SELECT through_seq, sequences FROM verification_review_plans
WHERE user_id = $1 AND install_id = $2 AND project_id = $3;

-- name: CreateReviewPlan :exec
INSERT INTO verification_review_plans (user_id, install_id, project_id, through_seq, sequences)
VALUES ($1, $2, $3, $4, $5);

-- name: DropReviewPlan :exec
DELETE FROM verification_review_plans WHERE user_id = $1 AND install_id = $2 AND project_id = $3;

-- name: ReviewCandidates :many
SELECT e.seq, e.ts FROM events e JOIN installs i ON e.install_id = i.id
WHERE i.user_id = $1 AND e.install_id = $2 AND e.project_id = $3 AND e.kind = 'prompt_sent'
AND e.seq > sqlc.arg(after_seq) AND e.hash IS NOT NULL
ORDER BY e.seq LIMIT 100;

-- name: ReviewToolActivity :one
SELECT count(*) FILTER (WHERE e.data->>'tool' = 'shell') AS shells,
       count(*) FILTER (WHERE e.data->>'tool' = 'edit') AS edits
FROM events e WHERE e.install_id = $1 AND e.project_id = $2 AND e.session_id = $3
AND e.kind = 'tool_call' AND e.ts >= sqlc.arg(hour_start) AND e.ts < sqlc.arg(hour_end);

-- name: ReviewQuotaEvents :many
SELECT e.install_id, e.seq, e.ts, e.kind, e.session_id, e.project_id, e.data,
       e.seq <= coalesce(a.last_seq, 0) AS already_reviewed
FROM events e JOIN installs i ON i.id = e.install_id
LEFT JOIN verification_analysis a
  ON a.user_id = i.user_id AND a.install_id = e.install_id AND a.project_id = e.project_id
WHERE i.user_id = sqlc.arg(user_id)
  AND e.ts < sqlc.arg(until_ts)
  AND (e.seq <= coalesce(a.last_seq, 0) OR
       (e.install_id = sqlc.arg(install_id) AND e.project_id = sqlc.arg(project_id)
        AND e.seq <= sqlc.arg(through_seq)))
ORDER BY e.ts, e.install_id, e.seq;

-- name: ReviewInstalls :many
SELECT DISTINCT ON (e.install_id) e.install_id, e.seq, e.hash
FROM events e JOIN installs i ON i.id = e.install_id
WHERE i.user_id = $1 AND e.project_id = $2 AND e.kind = 'prompt_sent' AND e.hash IS NOT NULL
ORDER BY e.install_id, e.seq DESC;

-- name: ReviewProjects :many
SELECT a.user_id, u.name, a.project_id, max(a.analyzed_at) AS analyzed_at,
       sum(a.unavailable)::bigint AS unavailable
FROM verification_analysis a JOIN users u ON u.id = a.user_id
GROUP BY a.user_id, u.name, a.project_id
ORDER BY max(a.analyzed_at) DESC, a.user_id, a.project_id;

-- name: ReviewProjectScopes :many
SELECT install_id, last_seq, unavailable FROM verification_analysis
WHERE user_id = $1 AND project_id = $2 ORDER BY install_id;

-- name: ProjectReviewSignals :many
SELECT r.install_id, r.seq, r.label, r.model, r.metadata
FROM verification_review_signals r JOIN events e ON e.install_id = r.install_id AND e.seq = r.seq
WHERE r.user_id = $1 AND r.project_id = $2
  AND e.ts >= sqlc.arg(from_ts) AND e.ts < sqlc.arg(to_ts);
