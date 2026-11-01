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
    display_name
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (name) DO UPDATE SET
    kind = excluded.kind,
    target = excluded.target,
    private_host = excluded.private_host,
    isolate = excluded.isolate,
    listens = excluded.listens,
    demand = excluded.demand,
    display_name = excluded.display_name,
    local_only = excluded.local_only
RETURNING name, kind, target, isolate, listens, demand, local_only, display_name, private_host;

-- name: GetService :one
SELECT name, kind, target, isolate, listens, demand, local_only, display_name, private_host
FROM services
WHERE name = ?;

-- name: ListServices :many
SELECT name, kind, target, isolate, listens, demand, local_only, display_name, private_host
FROM services
ORDER BY name;

-- name: DeleteService :execrows
DELETE FROM services
WHERE name = ?;
