-- +goose Up
CREATE TABLE service_runtime (
    name TEXT PRIMARY KEY CHECK (length(name) BETWEEN 1 AND 512),
    generation TEXT NOT NULL CHECK (length(generation) = 32),
    version INTEGER NOT NULL CHECK (version > 0),
    record TEXT NOT NULL CHECK (json_valid(record))
) STRICT;

CREATE TABLE service_runtime_barrier (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    revision INTEGER NOT NULL
) STRICT;
INSERT INTO service_runtime_barrier VALUES (1, 0);

-- +goose Down
DROP TABLE service_runtime_barrier;
DROP TABLE service_runtime;
