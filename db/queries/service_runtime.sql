-- name: ListServiceRuntime :many
SELECT * FROM service_runtime ORDER BY name;

-- name: GetServiceRuntime :one
SELECT * FROM service_runtime WHERE name = ?;

-- name: InsertServiceRuntime :execrows
INSERT INTO service_runtime (name, generation, version, record)
VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING;

-- name: UpdateServiceRuntime :execrows
UPDATE service_runtime SET version = sqlc.arg(next_version), record = sqlc.arg(record)
WHERE name = sqlc.arg(name) AND generation = sqlc.arg(generation) AND version = sqlc.arg(expected_version);

-- name: DeleteServiceRuntime :execrows
DELETE FROM service_runtime
WHERE service_runtime.name = ? AND service_runtime.generation = ? AND service_runtime.version = ?
AND NOT EXISTS (SELECT 1 FROM services WHERE services.name = service_runtime.name);

-- name: ConfirmServiceRuntime :exec
UPDATE service_runtime_barrier SET revision = revision + 1 WHERE id = 1;
