-- +goose Up
ALTER TABLE services ADD COLUMN trusted_origins TEXT NOT NULL DEFAULT '';
ALTER TABLE cached_services ADD COLUMN trusted_origins TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE cached_services DROP COLUMN trusted_origins;
ALTER TABLE services DROP COLUMN trusted_origins;
