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
	ReconcileHost(context.Context, storage.Host, []storage.Session) error
	ListHostSessions(context.Context, storage.HostID) ([]storage.Session, error)
	GetSession(context.Context, storage.HostID, storage.SessionID) (storage.Session, error)
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
}

// WorkerConnector resolves and opens one worker without exposing filesystem
// layout to the relay.
type WorkerConnector interface {
	ConnectWorker(context.Context, protocol.SessionID) (transport.Conn, error)
}
