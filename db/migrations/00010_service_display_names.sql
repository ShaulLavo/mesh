-- +goose Up
ALTER TABLE services ADD COLUMN display_name TEXT NOT NULL DEFAULT '';
ALTER TABLE cached_services ADD COLUMN display_name TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE cached_services DROP COLUMN display_name;
ALTER TABLE services DROP COLUMN display_name;
