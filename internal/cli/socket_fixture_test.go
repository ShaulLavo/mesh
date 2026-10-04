package cli

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/daemon"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/testenv"
	"github.com/shaul/mesh/internal/transport"
)

func compactSocketTempDir(t *testing.T) string {
	t.Helper()
	return testenv.SocketTempDir(t)
}

func TestSocketFixturesBindAndDetectAbsentDaemonUnderLongScratchRoot(t *testing.T) {
	stateDir := compactSocketTempDir(t)
	socket := daemon.SocketPath(stateDir)
	if len(socket) > 103 {
		t.Fatalf("socket path has %d bytes, exceeds the portable 103-byte limit: %q", len(socket), socket)
	}
	if _, err := ListViaDaemon(context.Background(), socket); !errors.Is(err, ErrDaemonUnavailable) {
		t.Fatalf("absent daemon probe = %v, want ErrDaemonUnavailable", err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	fixtureSocket, done := startDaemonCreateServer(t, func(conn transport.Conn, request protocol.Control) error {
		return writeDaemonControl(conn, protocol.Control{Type: protocol.TypeListed, RequestID: request.RequestID})
	})
	if _, err := ListViaDaemon(context.Background(), fixtureSocket); err != nil {
		t.Fatal(err)
	}
	awaitDaemonServer(t, done)
	canonical, err := filepath.EvalSymlinks(stateDir)
	if err != nil || canonical != stateDir {
		t.Fatalf("socket fixture %q is not canonical: %q, %v", stateDir, canonical, err)
	}
}
