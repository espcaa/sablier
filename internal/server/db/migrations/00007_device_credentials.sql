-- +goose Up
ALTER TABLE users DROP COLUMN api_key;

CREATE TABLE device_credentials (
    id UUID PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id),
    token_hash BYTEA NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE cli_codes (
    code_hash BYTEA PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id),
    challenge TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);

-- +goose Down
DROP TABLE cli_codes;
DROP TABLE device_credentials;
ALTER TABLE users ADD COLUMN api_key TEXT UNIQUE;
