-- +goose Up
DROP TABLE cli_codes;
CREATE TABLE device_login_requests (
    approval_hash BYTEA PRIMARY KEY,
    poll_hash BYTEA NOT NULL UNIQUE,
    hostname TEXT NOT NULL,
    user_id TEXT REFERENCES users(id),
    denied BOOLEAN NOT NULL DEFAULT false,
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX device_login_expiry ON device_login_requests (expires_at);

-- +goose Down
DROP TABLE device_login_requests;
CREATE TABLE cli_codes (
    code_hash BYTEA PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id),
    challenge TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);
