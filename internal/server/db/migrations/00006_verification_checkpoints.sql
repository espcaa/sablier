-- +goose Up
CREATE TABLE IF NOT EXISTS verification_checkpoints (
    user_id TEXT NOT NULL REFERENCES users(id),
    install_id UUID NOT NULL,
    project_id TEXT NOT NULL,
    last_seq BIGINT NOT NULL CHECK (last_seq > 0),
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, install_id, project_id)
);

-- +goose Down
DROP TABLE verification_checkpoints;
