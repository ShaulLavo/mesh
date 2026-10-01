package cli

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/daemon"
	"github.com/shaul/mesh/internal/protocol"
	"github.com/shaul/mesh/internal/transport"
)

func compactSocketTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "c-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}

func TestSocketFixturesBindAndDetectAbsentDaemonUnderLongScratchRoot(t *testing.T) {
	callerRoot := os.TempDir()
	root, err := os.MkdirTemp(callerRoot, "cli-long-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	t.Setenv("TMPDIR", root)
	stateDir := compactSocketTempDir(t)
	socket := daemon.SocketPath(stateDir)
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
	if filepath.Dir(stateDir) != callerRoot {
		t.Fatalf("socket fixture %q is not directly under caller scratch root %q", stateDir, callerRoot)
	}
}
