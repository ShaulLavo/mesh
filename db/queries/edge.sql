-- name: GetEdgeSnapshot :one
SELECT origin_id, target_id, sequence, digest, issued_at, expires_at, last_seen_at, signature
FROM edge_snapshots
WHERE origin_id = ?;

-- name: ListEdgeSnapshots :many
SELECT origin_id, target_id, sequence, digest, issued_at, expires_at, last_seen_at, signature
FROM edge_snapshots
ORDER BY origin_id;

-- name: UpsertEdgeSnapshot :exec
INSERT INTO edge_snapshots (
    origin_id, target_id, sequence, digest, issued_at, expires_at, last_seen_at, signature
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (origin_id) DO UPDATE SET
    target_id = excluded.target_id,
    sequence = excluded.sequence,
    digest = excluded.digest,
    issued_at = excluded.issued_at,
    expires_at = excluded.expires_at,
    last_seen_at = excluded.last_seen_at,
    signature = excluded.signature;

-- name: ListEdgeRoutes :many
SELECT origin_id, public_name, service_name, wake_on_request
FROM edge_routes
ORDER BY public_name, service_name;

-- name: ListEdgeRoutesForOrigin :many
SELECT origin_id, public_name, service_name, wake_on_request
FROM edge_routes
WHERE origin_id = ?
ORDER BY public_name, service_name;

-- name: GetEdgeRoute :one
SELECT origin_id, public_name, service_name, wake_on_request
FROM edge_routes
WHERE public_name = ? AND service_name = ?;

-- name: DeleteEdgeRoutesForOrigin :exec
DELETE FROM edge_routes
WHERE origin_id = ?;

-- name: DeleteEdgeSnapshot :exec
DELETE FROM edge_snapshots
WHERE origin_id = ?;

-- name: InsertEdgeRoute :exec
INSERT INTO edge_routes (origin_id, public_name, service_name, wake_on_request)
VALUES (?, ?, ?, ?);

-- name: GetEdgeOutbox :one
SELECT target_id, sequence, digest, snapshot_json, acknowledged
FROM edge_outbox
WHERE target_id = ?;

-- name: GetEdgeOutboxSize :one
SELECT length(snapshot_json)
FROM edge_outbox
WHERE target_id = ?;

-- name: UpsertEdgeOutbox :execrows
INSERT INTO edge_outbox (target_id, sequence, digest, snapshot_json, acknowledged)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (target_id) DO UPDATE SET
    sequence = excluded.sequence,
    digest = excluded.digest,
    snapshot_json = excluded.snapshot_json,
    acknowledged = excluded.acknowledged
WHERE excluded.sequence > edge_outbox.sequence;

-- name: AcknowledgeEdgeOutbox :execrows
UPDATE edge_outbox
SET acknowledged = 1
WHERE target_id = ? AND sequence = ? AND digest = ?;
