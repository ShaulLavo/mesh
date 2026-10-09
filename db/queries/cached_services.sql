-- name: DeleteCachedServicesForHost :exec
DELETE FROM cached_services
WHERE host_id = ?;

-- name: UpsertCachedService :exec
INSERT INTO cached_services (
    host_id,
    private_name,
    name,
    kind,
    target,
    private_host,
    healthy,
    problem,
    observed_at,
    isolate,
    display_name
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (host_id, name) DO UPDATE SET
	private_name = excluded.private_name,
    kind = excluded.kind,
    target = excluded.target,
    private_host = excluded.private_host,
    healthy = excluded.healthy,
    problem = excluded.problem,
    observed_at = excluded.observed_at,
    display_name = excluded.display_name,
    isolate = excluded.isolate;

-- name: ListCachedServicesForHost :many
SELECT host_id, private_name, name, kind, target, healthy, problem, observed_at, isolate, display_name, private_host
FROM cached_services
WHERE host_id = ?
ORDER BY name;

-- name: ListCachedServices :many
SELECT host_id, private_name, name, kind, target, healthy, problem, observed_at, isolate, display_name, private_host
FROM cached_services
ORDER BY host_id, name
LIMIT 8193;

-- name: CountCachedServices :one
SELECT count(*)
FROM cached_services;
