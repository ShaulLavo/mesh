-- +goose Up
ALTER TABLE cached_services ADD COLUMN isolate INTEGER NOT NULL DEFAULT 0 CHECK (isolate IN (0, 1));

-- +goose Down
ALTER TABLE cached_services DROP COLUMN isolate;
