package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/shaul/mesh/internal/edge"
	dbsqlc "github.com/shaul/mesh/internal/storage/sqlc"
)

// EdgeSnapshotVersion returns the durable idempotence key for one origin.
func (s *Store) EdgeSnapshotVersion(ctx context.Context, originID string) (edge.SnapshotVersion, error) {
	row, err := s.queries.GetEdgeSnapshot(ctx, originID)
	if errors.Is(err, sql.ErrNoRows) {
		return edge.SnapshotVersion{}, edge.ErrSnapshotNotFound
	}
	if err != nil {
		return edge.SnapshotVersion{}, fmt.Errorf("storage: get edge snapshot version: %w", err)
	}
	return edge.SnapshotVersion{Sequence: uint64(row.Sequence), Digest: row.Digest}, nil
}

// ApplyEdgeSnapshot atomically replaces one origin's complete claims. It
// rechecks sequence and collisions inside the write transaction.
func (s *Store) ApplyEdgeSnapshot(ctx context.Context, snapshot edge.Snapshot, digest string, receivedAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("storage: begin edge snapshot transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // commit decides the outcome
	queries := s.queries.WithTx(tx)

	current, err := queries.GetEdgeSnapshot(ctx, snapshot.OriginID)
	switch {
	case err == nil && uint64(current.Sequence) > snapshot.Sequence:
		return edge.ErrStaleSequence
	case err == nil && uint64(current.Sequence) == snapshot.Sequence && current.Digest != digest:
		return edge.ErrSequenceConflict
	case err == nil && uint64(current.Sequence) == snapshot.Sequence:
		return nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("storage: inspect edge snapshot: %w", err)
	}

	for _, route := range snapshot.Routes {
		owner, routeErr := queries.GetEdgeRoute(ctx, dbsqlc.GetEdgeRouteParams{PublicName: route.PublicName, ServiceName: route.ServiceName})
		if routeErr == nil && owner.OriginID != snapshot.OriginID {
			return edge.ErrRouteCollision
		}
		if routeErr != nil && !errors.Is(routeErr, sql.ErrNoRows) {
			return fmt.Errorf("storage: inspect edge route collision: %w", routeErr)
		}
	}
	if err := queries.UpsertEdgeSnapshot(ctx, dbsqlc.UpsertEdgeSnapshotParams{
		OriginID: snapshot.OriginID, TargetID: snapshot.TargetID, Sequence: int64(snapshot.Sequence), Digest: digest,
		IssuedAt: snapshot.IssuedAt.UTC().UnixNano(), ExpiresAt: snapshot.ExpiresAt.UTC().UnixNano(),
		LastSeenAt: receivedAt.UTC().UnixNano(), Signature: append([]byte(nil), snapshot.Signature...),
	}); err != nil {
		return fmt.Errorf("storage: upsert edge snapshot: %w", err)
	}
	if err := queries.DeleteEdgeRoutesForOrigin(ctx, snapshot.OriginID); err != nil {
		return fmt.Errorf("storage: replace edge routes: %w", err)
	}
	for _, route := range snapshot.Routes {
		if err := queries.InsertEdgeRoute(ctx, dbsqlc.InsertEdgeRouteParams{
			OriginID: snapshot.OriginID, PublicName: route.PublicName, ServiceName: route.ServiceName, WakeOnRequest: boolInt64(route.WakeOnRequest),
		}); err != nil {
			return fmt.Errorf("storage: insert edge route: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit edge snapshot: %w", err)
	}
	return nil
}

// LoadEdgeState restores all durable claims. Restored origins intentionally
// have no endpoint and remain offline until a newer authenticated heartbeat.
func (s *Store) LoadEdgeState(ctx context.Context) ([]edge.StoredOrigin, error) {
	snapshots, err := s.queries.ListEdgeSnapshots(ctx)
	if err != nil {
		return nil, fmt.Errorf("storage: list edge snapshots: %w", err)
	}
	routes, err := s.queries.ListEdgeRoutes(ctx)
	if err != nil {
		return nil, fmt.Errorf("storage: list edge routes: %w", err)
	}
	byOrigin := make(map[string][]edge.Route, len(snapshots))
	for _, row := range routes {
		wake, err := sqliteBool("wake_on_request", row.WakeOnRequest)
		if err != nil {
			return nil, fmt.Errorf("storage: decode edge route: %w", err)
		}
		byOrigin[row.OriginID] = append(byOrigin[row.OriginID], edge.Route{
			PublicName: row.PublicName, ServiceName: row.ServiceName, WakeOnRequest: wake,
		})
	}
	result := make([]edge.StoredOrigin, 0, len(snapshots))
	for _, row := range snapshots {
		result = append(result, edge.StoredOrigin{
			Snapshot: edge.Snapshot{
				TargetID: row.TargetID, OriginID: row.OriginID, Sequence: uint64(row.Sequence),
				IssuedAt: time.Unix(0, row.IssuedAt).UTC(), ExpiresAt: time.Unix(0, row.ExpiresAt).UTC(),
				Routes: byOrigin[row.OriginID], Signature: append([]byte(nil), row.Signature...),
			},
			Digest: row.Digest, LastSeenAt: time.Unix(0, row.LastSeenAt).UTC(),
		})
	}
	return result, nil
}

// LoadEdgeOutbox returns the last exact outbound snapshot for targetID.
func (s *Store) LoadEdgeOutbox(ctx context.Context, targetID string) (edge.OutboxRecord, error) {
	size, err := s.queries.GetEdgeOutboxSize(ctx, targetID)
	if errors.Is(err, sql.ErrNoRows) {
		return edge.OutboxRecord{}, edge.ErrSnapshotNotFound
	}
	if err != nil {
		return edge.OutboxRecord{}, fmt.Errorf("storage: inspect edge outbox size: %w", err)
	}
	if size == nil || *size <= 0 || *size > edge.MaximumOutboxBytes {
		return edge.OutboxRecord{}, fmt.Errorf("storage: edge outbox size is outside 1..%d", edge.MaximumOutboxBytes)
	}
	row, err := s.queries.GetEdgeOutbox(ctx, targetID)
	if errors.Is(err, sql.ErrNoRows) {
		return edge.OutboxRecord{}, edge.ErrSnapshotNotFound
	}
	if err != nil {
		return edge.OutboxRecord{}, fmt.Errorf("storage: load edge outbox: %w", err)
	}
	if len(row.SnapshotJson) == 0 || len(row.SnapshotJson) > edge.MaximumOutboxBytes {
		return edge.OutboxRecord{}, fmt.Errorf("storage: edge outbox changed to size %d after validation", len(row.SnapshotJson))
	}
	var snapshot edge.Snapshot
	if err := json.Unmarshal(row.SnapshotJson, &snapshot); err != nil {
		return edge.OutboxRecord{}, fmt.Errorf("storage: decode edge outbox: %w", err)
	}
	digest, err := edge.VerifySnapshot(snapshot, targetID, snapshot.OriginID)
	if err != nil {
		return edge.OutboxRecord{}, fmt.Errorf("storage: verify edge outbox: %w", err)
	}
	if row.TargetID != targetID || row.Sequence <= 0 || uint64(row.Sequence) != snapshot.Sequence || row.Digest != digest {
		return edge.OutboxRecord{}, errors.New("storage: edge outbox metadata does not match its signed snapshot")
	}
	acknowledged, err := sqliteBool("acknowledged", row.Acknowledged)
	if err != nil {
		return edge.OutboxRecord{}, fmt.Errorf("storage: decode edge outbox: %w", err)
	}
	return edge.OutboxRecord{Snapshot: snapshot, Digest: row.Digest, Acknowledged: acknowledged}, nil
}

// SaveEdgeOutbox durably consumes a new sequence before it can be sent.
func (s *Store) SaveEdgeOutbox(ctx context.Context, record edge.OutboxRecord) error {
	if record.Snapshot.Sequence == 0 || record.Snapshot.Sequence > math.MaxInt64 || record.Acknowledged {
		return errors.New("storage: invalid new edge outbox record")
	}
	digest, err := edge.VerifySnapshot(record.Snapshot, record.Snapshot.TargetID, record.Snapshot.OriginID)
	if err != nil || digest != record.Digest {
		return errors.New("storage: new edge outbox record is not a verified signed snapshot")
	}
	encoded, err := json.Marshal(record.Snapshot)
	if err != nil {
		return fmt.Errorf("storage: encode edge outbox: %w", err)
	}
	if len(encoded) > edge.MaximumOutboxBytes {
		return fmt.Errorf("storage: encoded edge outbox exceeds %d bytes", edge.MaximumOutboxBytes)
	}
	updated, err := s.queries.UpsertEdgeOutbox(ctx, dbsqlc.UpsertEdgeOutboxParams{
		TargetID: record.Snapshot.TargetID, Sequence: int64(record.Snapshot.Sequence), Digest: record.Digest,
		SnapshotJson: encoded, Acknowledged: 0,
	})
	if err != nil {
		return fmt.Errorf("storage: save edge outbox: %w", err)
	}
	if updated != 1 {
		return errors.New("storage: edge outbox sequence did not increase")
	}
	return nil
}

// AcknowledgeEdgeOutbox marks only the exact sequence and digest acknowledged.
func (s *Store) AcknowledgeEdgeOutbox(ctx context.Context, targetID string, sequence uint64, digest string) error {
	updated, err := s.queries.AcknowledgeEdgeOutbox(ctx, dbsqlc.AcknowledgeEdgeOutboxParams{TargetID: targetID, Sequence: int64(sequence), Digest: digest})
	if err != nil {
		return fmt.Errorf("storage: acknowledge edge outbox: %w", err)
	}
	if updated != 1 {
		return errors.New("storage: edge acknowledgement does not match pending snapshot")
	}
	return nil
}
