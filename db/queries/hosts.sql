-- name: UpsertHost :one
INSERT INTO hosts (
    id,
    alias,
    mesh_identity,
    tailscale_name,
    last_seen_at
) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    alias = excluded.alias,
    mesh_identity = excluded.mesh_identity,
    tailscale_name = excluded.tailscale_name,
    last_seen_at = excluded.last_seen_at
RETURNING id, alias, mesh_identity, tailscale_name, last_seen_at;

-- name: GetHost :one
SELECT id, alias, mesh_identity, tailscale_name, last_seen_at
FROM hosts
WHERE id = ?;

-- name: ListHosts :many
SELECT id, alias, mesh_identity, tailscale_name, last_seen_at
FROM hosts
ORDER BY last_seen_at DESC, id;
