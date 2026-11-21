-- +goose Up
-- see event.ChainHash. null on events from clients before the chain
ALTER TABLE events ADD COLUMN prev_hash TEXT, ADD COLUMN hash TEXT;

-- the last chained event per install, every new one has to continue from it
ALTER TABLE installs ADD COLUMN head_seq BIGINT, ADD COLUMN head_hash TEXT;

-- +goose Down
ALTER TABLE installs DROP COLUMN head_seq, DROP COLUMN head_hash;
ALTER TABLE events DROP COLUMN prev_hash, DROP COLUMN hash;
