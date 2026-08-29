-- +goose Up
CREATE TABLE hosts (
    id TEXT PRIMARY KEY CHECK (length(id) > 0),
    alias TEXT UNIQUE CHECK (alias IS NULL OR length(alias) > 0),
    mesh_identity TEXT NOT NULL UNIQUE CHECK (length(mesh_identity) > 0),
    tailscale_name TEXT CHECK (tailscale_name IS NULL OR length(tailscale_name) > 0),
    last_seen_at INTEGER NOT NULL CHECK (last_seen_at >= 0)
) STRICT;

CREATE TABLE sessions (
    id TEXT NOT NULL CHECK (length(id) > 0),
    host_id TEXT NOT NULL,
    command TEXT NOT NULL CHECK (json_valid(command) AND json_type(command) = 'array' AND json_array_length(command) > 0),
    cwd TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('running', 'detached', 'exited', 'interrupted')),
    created_at INTEGER NOT NULL CHECK (created_at >= 0),
    last_attached_at INTEGER CHECK (last_attached_at IS NULL OR last_attached_at >= 0),
    exit_code INTEGER,
    last_output_sequence INTEGER NOT NULL DEFAULT 0 CHECK (last_output_sequence >= 0),
    PRIMARY KEY (host_id, id),
    FOREIGN KEY (host_id) REFERENCES hosts(id) ON DELETE CASCADE,
    CHECK (
        (state = 'exited' AND exit_code IS NOT NULL)
        OR (state <> 'exited' AND exit_code IS NULL)
    )
) STRICT;

CREATE INDEX sessions_by_state
    ON sessions (state, created_at DESC);

CREATE INDEX sessions_by_host
    ON sessions (host_id, created_at DESC);

-- +goose Down
DROP TABLE sessions;
DROP TABLE hosts;
