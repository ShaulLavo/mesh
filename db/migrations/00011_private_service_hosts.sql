-- +goose Up
ALTER TABLE services ADD COLUMN private_host TEXT NOT NULL DEFAULT '';
ALTER TABLE cached_services ADD COLUMN private_host TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE cached_services DROP COLUMN private_host;
ALTER TABLE services DROP COLUMN private_host;
