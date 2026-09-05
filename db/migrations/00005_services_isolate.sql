-- +goose Up
ALTER TABLE services ADD COLUMN isolate INTEGER NOT NULL DEFAULT 0 CHECK (isolate IN (0, 1));

-- +goose Down
ALTER TABLE services DROP COLUMN isolate;
