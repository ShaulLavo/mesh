-- +goose Up
CREATE TABLE edge_snapshots (
    origin_id TEXT PRIMARY KEY CHECK (length(origin_id) = 43),
    target_id TEXT NOT NULL CHECK (length(target_id) = 43),
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    digest TEXT NOT NULL CHECK (length(digest) = 64),
    issued_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL CHECK (expires_at > issued_at),
    last_seen_at INTEGER NOT NULL,
    signature BLOB NOT NULL CHECK (length(signature) = 64)
) STRICT;

CREATE TABLE edge_routes (
    origin_id TEXT NOT NULL,
    public_name TEXT NOT NULL CHECK (length(public_name) > 0),
    service_name TEXT NOT NULL CHECK (length(service_name) > 0),
    wake_on_request INTEGER NOT NULL CHECK (wake_on_request IN (0, 1)),
    PRIMARY KEY (public_name, service_name),
    FOREIGN KEY (origin_id) REFERENCES edge_snapshots(origin_id) ON DELETE CASCADE
) STRICT;

CREATE INDEX edge_routes_by_origin
    ON edge_routes (origin_id, public_name, service_name);

CREATE TABLE edge_outbox (
    target_id TEXT PRIMARY KEY CHECK (length(target_id) = 43),
    sequence INTEGER NOT NULL CHECK (sequence > 0),
    digest TEXT NOT NULL CHECK (length(digest) = 64),
    snapshot_json BLOB NOT NULL CHECK (length(snapshot_json) BETWEEN 1 AND 524288),
    acknowledged INTEGER NOT NULL CHECK (acknowledged IN (0, 1))
) STRICT;

-- +goose Down
DROP TABLE edge_outbox;
DROP INDEX edge_routes_by_origin;
DROP TABLE edge_routes;
DROP TABLE edge_snapshots;
