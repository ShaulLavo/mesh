-- name: UpsertSession :one
INSERT INTO sessions (
    id,
    host_id,
    command,
    cwd,
    state,
    created_at,
    last_attached_at,
    exit_code,
    last_output_sequence
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (host_id, id) DO UPDATE SET
    command = excluded.command,
    cwd = excluded.cwd,
    state = excluded.state,
    created_at = excluded.created_at,
    last_attached_at = excluded.last_attached_at,
    exit_code = excluded.exit_code,
    last_output_sequence = MAX(sessions.last_output_sequence, excluded.last_output_sequence)
RETURNING id, host_id, command, cwd, state, created_at, last_attached_at, exit_code, last_output_sequence;

-- name: GetSession :one
SELECT id, host_id, command, cwd, state, created_at, last_attached_at, exit_code, last_output_sequence
FROM sessions
WHERE host_id = ? AND id = ?;

-- name: ListHostSessions :many
SELECT id, host_id, command, cwd, state, created_at, last_attached_at, exit_code, last_output_sequence
FROM sessions
WHERE host_id = ?
ORDER BY created_at DESC, id;

-- name: ListSessionsByState :many
SELECT id, host_id, command, cwd, state, created_at, last_attached_at, exit_code, last_output_sequence
FROM sessions
WHERE state = ?
ORDER BY created_at DESC, host_id, id;

-- name: SetSessionState :one
UPDATE sessions
SET state = ?, exit_code = ?
WHERE host_id = ? AND id = ?
RETURNING id, host_id, command, cwd, state, created_at, last_attached_at, exit_code, last_output_sequence;

-- name: InterruptActiveSessionsForHost :exec
UPDATE sessions
SET state = 'interrupted', exit_code = NULL
WHERE host_id = ? AND state IN ('running', 'detached');
