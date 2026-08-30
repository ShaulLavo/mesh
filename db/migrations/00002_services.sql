-- +goose Up
CREATE TABLE services (
    name TEXT PRIMARY KEY CHECK (length(name) > 0),
    kind TEXT NOT NULL CHECK (kind IN ('static', 'files', 'proxy')),
    target TEXT NOT NULL CHECK (length(target) > 0),
    public_name TEXT NOT NULL,
    wake_on_request INTEGER NOT NULL CHECK (wake_on_request IN (0, 1))
) STRICT;

-- +goose Down
DROP TABLE services;
