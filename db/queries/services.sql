-- name: UpsertService :one
INSERT INTO services (
    name,
    kind,
    target,
    public_name,
    wake_on_request,
    isolate,
    listens,
    demand,
    local_only,
    display_name
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (name) DO UPDATE SET
    kind = excluded.kind,
    target = excluded.target,
    public_name = excluded.public_name,
    wake_on_request = excluded.wake_on_request,
    isolate = excluded.isolate,
    listens = excluded.listens,
    demand = excluded.demand,
    display_name = excluded.display_name,
    local_only = excluded.local_only
RETURNING name, kind, target, public_name, wake_on_request, isolate, listens, demand, local_only, display_name;

-- name: GetService :one
SELECT name, kind, target, public_name, wake_on_request, isolate, listens, demand, local_only, display_name
FROM services
WHERE name = ?;

-- name: ListServices :many
SELECT name, kind, target, public_name, wake_on_request, isolate, listens, demand, local_only, display_name
FROM services
ORDER BY name;

-- name: DeleteService :execrows
DELETE FROM services
WHERE name = ?;
