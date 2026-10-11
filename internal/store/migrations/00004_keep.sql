-- +goose Up
-- Keep an app running past the idle stop until this time (NULL: the idle
-- stop applies). For downloads and updates nobody is streaming.
ALTER TABLE apps ADD COLUMN keep_until timestamptz;

-- +goose Down
ALTER TABLE apps DROP COLUMN keep_until;
