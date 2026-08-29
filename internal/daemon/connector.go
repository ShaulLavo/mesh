package daemon

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"time"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/storage"
	"github.com/shaul/mesh/internal/transport"
)

type sessionLookup interface {
	Get(context.Context, storage.SessionID) (storage.Session, error)
}

type workerConnector struct {
	sessionsDir string
	catalog     sessionLookup
	dialer      net.Dialer
}

func newWorkerConnector(sessionsDir string, catalog sessionLookup) (*workerConnector, error) {
	if sessionsDir == "" {
		return nil, fmt.Errorf("daemon: empty sessions directory")
	}
	if catalog == nil {
		return nil, fmt.Errorf("daemon: nil session catalog")
	}
	return &workerConnector{
		sessionsDir: sessionsDir,
		catalog:     catalog,
		dialer:      net.Dialer{Timeout: 2 * time.Second},
	}, nil
}

func (c *workerConnector) ConnectWorker(ctx context.Context, id protocol.SessionID) (transport.Conn, error) {
	text := id.String()
	if !isCanonicalSessionID(text) {
		return nil, fmt.Errorf("daemon: invalid canonical session ID %q", text)
	}
	stored, err := c.catalog.Get(ctx, storage.SessionID(text))
	if err != nil {
		return nil, fmt.Errorf("daemon: resolve session %s: %w", text, err)
	}
	if stored.ID != storage.SessionID(text) {
		return nil, fmt.Errorf("daemon: catalog returned session %s for %s", stored.ID, text)
	}
	if stored.State != storage.StateRunning && stored.State != storage.StateDetached {
		return nil, fmt.Errorf("daemon: session %s is %s", text, stored.State)
	}

	socketPath := paths.Socket(filepath.Join(c.sessionsDir, text))
	stream, err := c.dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("daemon: connect worker %s at %s: %w", text, socketPath, err)
	}
	conn, err := transport.NewStreamConn(stream)
	if err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("daemon: adapt worker %s connection: %w", text, err)
	}
	return conn, nil
}

func isCanonicalSessionID(id string) bool {
	parsed, err := session.ParseID(id)
	return err == nil && parsed == id
}

var _ WorkerConnector = (*workerConnector)(nil)
