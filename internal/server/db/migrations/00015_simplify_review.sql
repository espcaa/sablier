-- +goose Up
-- failed prompts are now stored as signals labeled unavailable
DROP TABLE IF EXISTS verification_checkpoints;
ALTER TABLE verification_analysis DROP COLUMN unavailable;
ALTER TABLE verification_review_signals DROP COLUMN metadata;

-- +goose Down
ALTER TABLE verification_review_signals ADD COLUMN metadata JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE verification_analysis ADD COLUMN unavailable BIGINT NOT NULL DEFAULT 0;
