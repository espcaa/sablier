-- +goose Up
CREATE TABLE IF NOT EXISTS verification_analysis (
    user_id TEXT NOT NULL,
    install_id UUID NOT NULL,
    project_id TEXT NOT NULL,
    last_seq BIGINT NOT NULL,
    unavailable BIGINT NOT NULL DEFAULT 0,
    analyzed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, install_id, project_id)
);

CREATE TABLE IF NOT EXISTS verification_review_signals (
    user_id TEXT NOT NULL,
    install_id UUID NOT NULL,
    project_id TEXT NOT NULL,
    seq BIGINT NOT NULL,
    event_hour TIMESTAMPTZ NOT NULL,
    label TEXT NOT NULL,
    model TEXT NOT NULL,
    metadata JSONB NOT NULL,
    PRIMARY KEY (user_id, install_id, project_id, seq),
    FOREIGN KEY (user_id, install_id, project_id) REFERENCES verification_analysis
);
CREATE INDEX IF NOT EXISTS verification_review_signals_project_hour ON verification_review_signals (user_id, project_id, event_hour);

CREATE TABLE IF NOT EXISTS verification_review_plans (
    user_id TEXT NOT NULL,
    install_id UUID NOT NULL,
    project_id TEXT NOT NULL,
    through_seq BIGINT NOT NULL,
    sequences BIGINT[] NOT NULL,
    PRIMARY KEY (user_id, install_id, project_id)
);

-- upgrade databases that ran an earlier local version
ALTER TABLE verification_analysis ADD COLUMN IF NOT EXISTS unavailable BIGINT NOT NULL DEFAULT 0;
ALTER TABLE verification_review_signals ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE verification_review_signals ALTER COLUMN metadata DROP DEFAULT;

-- +goose Down
DROP TABLE verification_review_plans;
DROP TABLE verification_review_signals;
DROP TABLE verification_analysis;
