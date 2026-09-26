-- +goose Up
ALTER TABLE services ADD COLUMN listens TEXT NOT NULL DEFAULT '';
ALTER TABLE services ADD COLUMN demand TEXT NOT NULL DEFAULT '';
ALTER TABLE services ADD COLUMN local_only INTEGER NOT NULL DEFAULT 0 CHECK (local_only IN (0, 1));

-- +goose Down
ALTER TABLE services DROP COLUMN local_only;
ALTER TABLE services DROP COLUMN demand;
ALTER TABLE services DROP COLUMN listens;
