-- +goose Up
-- Missing facts mean unknown, never false. These are client observations only.
CREATE TABLE cached_session_facts (
    host_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    hibernated INTEGER NOT NULL CHECK (hibernated IN (0, 1)),
    recovery_pending INTEGER NOT NULL CHECK (recovery_pending IN (0, 1)),
    label TEXT NOT NULL CHECK (length(CAST(label AS BLOB)) <= 4096),
    detached_at INTEGER CHECK (detached_at IS NULL OR detached_at >= 0),
    PRIMARY KEY (host_id, session_id),
    FOREIGN KEY (host_id, session_id) REFERENCES sessions(host_id, id) ON DELETE CASCADE
) STRICT;

-- Any later shared-row write invalidates its complete observation.
-- +goose StatementBegin
CREATE TRIGGER sessions_forget_cached_facts AFTER UPDATE ON sessions
BEGIN
    DELETE FROM cached_session_facts WHERE host_id = OLD.host_id AND session_id = OLD.id;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER sessions_forget_cached_facts;
DROP TABLE cached_session_facts;
