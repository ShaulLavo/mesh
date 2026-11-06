-- +goose Up
ALTER TABLE cached_services ADD COLUMN local_only INTEGER NOT NULL DEFAULT 0 CHECK (local_only IN (0, 1));
ALTER TABLE cached_services ADD COLUMN listens TEXT NOT NULL DEFAULT '';

-- Existing advisory rows discarded the scope evidence at observation time.
DELETE FROM cached_services;

-- +goose Down
DELETE FROM cached_services;
ALTER TABLE cached_services DROP COLUMN listens;
ALTER TABLE cached_services DROP COLUMN local_only;
