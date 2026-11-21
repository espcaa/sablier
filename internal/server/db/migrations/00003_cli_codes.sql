-- +goose Up
CREATE TABLE cli_codes (
    code_hash  BYTEA       PRIMARY KEY,
    user_id    TEXT        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    challenge  TEXT        NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);

-- +goose Down
DROP TABLE cli_codes;
