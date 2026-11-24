-- +goose Up
ALTER TABLE device_credentials ADD COLUMN hostname TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE device_credentials DROP COLUMN hostname;
