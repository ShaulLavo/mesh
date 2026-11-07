-- name: DeleteHostSessionFacts :exec
DELETE FROM cached_session_facts WHERE host_id = ?;

-- name: InsertSessionFacts :exec
INSERT INTO cached_session_facts (host_id, session_id, hibernated, recovery_pending, label, detached_at)
VALUES (?, ?, ?, ?, ?, ?);

-- name: ListObservedHostSessions :many
SELECT s.id, s.host_id, s.command, s.cwd, s.state, s.created_at,
       s.last_attached_at, s.exit_code, s.last_output_sequence,
       f.session_id, f.hibernated, f.recovery_pending, f.label, f.detached_at
FROM sessions AS s
LEFT JOIN cached_session_facts AS f ON f.host_id = s.host_id AND f.session_id = s.id
WHERE s.host_id = ?
ORDER BY s.created_at DESC, s.id;
