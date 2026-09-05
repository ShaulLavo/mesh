-- name: UpsertService :one
INSERT INTO services (
    name,
    kind,
    target,
    public_name,
    wake_on_request,
    isolate
) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (name) DO UPDATE SET
    kind = excluded.kind,
    target = excluded.target,
    public_name = excluded.public_name,
    wake_on_request = excluded.wake_on_request,
    isolate = excluded.isolate
RETURNING name, kind, target, public_name, wake_on_request, isolate;

-- name: GetService :one
SELECT name, kind, target, public_name, wake_on_request, isolate
FROM services
WHERE name = ?;

-- name: ListServices :many
SELECT name, kind, target, public_name, wake_on_request, isolate
FROM services
ORDER BY name;

-- name: DeleteService :execrows
DELETE FROM services
WHERE name = ?;
