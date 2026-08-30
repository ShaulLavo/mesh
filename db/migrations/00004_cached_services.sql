-- +goose Up
CREATE TABLE cached_services (
    host_id TEXT NOT NULL,
    private_name TEXT NOT NULL CHECK (length(private_name) <= 253),
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 512),
    kind TEXT NOT NULL CHECK (kind IN ('static', 'files', 'proxy')),
    target TEXT NOT NULL CHECK (length(target) BETWEEN 1 AND 2048),
    public_name TEXT NOT NULL CHECK (length(public_name) <= 253),
    wake_on_request INTEGER NOT NULL CHECK (wake_on_request IN (0, 1)),
    healthy INTEGER NOT NULL CHECK (healthy IN (0, 1)),
    problem TEXT NOT NULL CHECK (length(problem) <= 256),
    observed_at INTEGER NOT NULL CHECK (observed_at >= 0),
    PRIMARY KEY (host_id, name),
    FOREIGN KEY (host_id) REFERENCES hosts(id) ON DELETE CASCADE
) STRICT;

-- +goose Down
DROP TABLE cached_services;
