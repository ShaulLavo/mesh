package edge

import (
	"context"
	"errors"
	"time"
)

var (
	ErrSnapshotNotFound = errors.New("edge: snapshot not found")
	ErrStaleSequence    = errors.New("edge: snapshot sequence is stale")
	ErrSequenceConflict = errors.New("edge: snapshot sequence has a different digest")
	ErrRouteCollision   = errors.New("edge: public route is already claimed")
	ErrWakeUnavailable  = errors.New("edge: wake-on-request is not configured")
)

// MaximumOutboxBytes bounds the persisted JSON form of one maximal snapshot.
const MaximumOutboxBytes = 512 << 10

// SnapshotVersion is the durable idempotence key for one origin.
type SnapshotVersion struct {
	Sequence uint64
	Digest   string
}

// StoredOrigin is one complete durable snapshot plus edge-observed liveness.
type StoredOrigin struct {
	Snapshot   Snapshot
	Digest     string
	LastSeenAt time.Time
}

// StateStore is the edge's transactional desired-state boundary.
type StateStore interface {
	EdgeSnapshotVersion(context.Context, string) (SnapshotVersion, error)
	ApplyEdgeSnapshot(context.Context, Snapshot, string, time.Time) error
	LoadEdgeState(context.Context) ([]StoredOrigin, error)
}

// OutboxRecord is the origin's exact durable registration attempt.
type OutboxRecord struct {
	Snapshot     Snapshot
	Digest       string
	Acknowledged bool
}

// OutboxStore prevents sequence reuse and preserves ambiguous sends across
// daemon restarts.
type OutboxStore interface {
	LoadEdgeOutbox(context.Context, string) (OutboxRecord, error)
	SaveEdgeOutbox(context.Context, OutboxRecord) error
	AcknowledgeEdgeOutbox(context.Context, string, uint64, string) error
}
