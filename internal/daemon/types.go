// Package daemon coordinates session workers without owning their processes or PTYs.
package daemon

import (
	"context"
	"time"

	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
)

// CatalogStore is the durable boundary used while rediscovering local workers.
type CatalogStore interface {
	ApplyHostChanges(context.Context, storage.HostID, storage.HostChanges) error
	ListHostSessions(context.Context, storage.HostID) ([]storage.Session, error)
	GetSession(context.Context, storage.HostID, storage.SessionID) (storage.Session, error)
	// RetireSessions deletes finished sessions the catalog has pruned from
	// disk. Retirement is always an explicit set: a session merely absent from
	// an observation is interrupted, not retired.
	RetireSessions(context.Context, storage.HostID, []storage.SessionID) (int64, error)
}

// WorkerProbe reports whether a worker socket accepts a connection.
type WorkerProbe interface {
	Probe(context.Context, string) error
}

// CatalogConfig contains the external boundaries needed for reconciliation.
type CatalogConfig struct {
	SessionsDir string
	Host        storage.Host
	Store       CatalogStore
	Probe       WorkerProbe
	BootID      func() string
	Now         func() time.Time
	// OnReconcile observes successful passes, including unchanged passes.
	// The caller observes failures through Reconcile's returned error.
	OnReconcile func([]storage.Session)
	// OnChange runs synchronously after a committed session delta, under the
	// catalog gate. It must not block or call back into the catalog.
	OnChange func(SessionDiff)
	// OnObservation runs under the gate for every completed pass, using monotonic time at its consumer.
	OnObservation func(error)
}

// WorkerConnector resolves and opens one worker without exposing filesystem
// layout to the relay.
type WorkerConnector interface {
	ConnectWorker(context.Context, protocol.SessionID) (transport.Conn, error)
}
