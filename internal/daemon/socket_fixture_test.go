package daemon

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/paths"
)

func compactSocketTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "d-")
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

func TestSocketFixturesBindUnderLongScratchRoot(t *testing.T) {
	root := compactSocketTempDir(t)
	if filepath.Dir(root) != os.TempDir() {
		t.Fatalf("socket fixture %q is not directly under caller scratch root %q", root, os.TempDir())
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
