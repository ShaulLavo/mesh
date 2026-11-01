-- name: UpsertService :one
INSERT INTO services (
    name,
    kind,
    target,
    private_host,
    isolate,
    listens,
    demand,
    local_only,
    display_name,
    trusted_origins
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (name) DO UPDATE SET
    kind = excluded.kind,
    target = excluded.target,
    private_host = excluded.private_host,
    isolate = excluded.isolate,
    listens = excluded.listens,
    demand = excluded.demand,
    display_name = excluded.display_name,
    local_only = excluded.local_only,
    trusted_origins = excluded.trusted_origins
RETURNING name, kind, target, isolate, listens, demand, local_only, display_name, private_host, trusted_origins;

-- name: GetService :one
SELECT name, kind, target, isolate, listens, demand, local_only, display_name, private_host, trusted_origins
FROM services
WHERE name = ?;

-- name: ListServices :many
SELECT name, kind, target, isolate, listens, demand, local_only, display_name, private_host, trusted_origins
FROM services
ORDER BY name;

-- name: DeleteService :execrows
DELETE FROM services
WHERE name = ?;
