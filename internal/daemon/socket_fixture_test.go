package daemon

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/paths"
	"github.com/shaul/mesh/internal/testenv"
)

func compactSocketTempDir(t *testing.T) string {
	t.Helper()
	return testenv.SocketTempDir(t)
}

func TestSocketFixturesBindUnderLongScratchRoot(t *testing.T) {
	root := compactSocketTempDir(t)
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || canonical != root {
		t.Fatalf("socket fixture %q is not canonical: %q, %v", root, canonical, err)
	}
	sessionDir := filepath.Join(root, "sessions", "7K3D")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, socket := range []string{SocketPath(root), paths.Socket(sessionDir)} {
		if len(socket) > 103 {
			t.Fatalf("socket path has %d bytes, exceeds the portable 103-byte limit: %q", len(socket), socket)
		}
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
