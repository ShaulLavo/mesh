-- +goose Up
CREATE TABLE app_state (
    key TEXT PRIMARY KEY CHECK (length(key) BETWEEN 1 AND 128),
    data BLOB NOT NULL CHECK (length(data) BETWEEN 1 AND 16777216)
) STRICT;

CREATE TABLE app_names (
    public_name TEXT PRIMARY KEY CHECK (length(public_name) BETWEEN 1 AND 253),
    owner_id TEXT NOT NULL CHECK (length(owner_id) = 43),
    active INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1))
) STRICT;

CREATE INDEX app_names_by_owner ON app_names(owner_id, active);

-- +goose Down
DROP INDEX app_names_by_owner;
DROP TABLE app_names;
DROP TABLE app_state;
