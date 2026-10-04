package paths

import (
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/testenv"
)

func TestSocketPathNativeBoundary(t *testing.T) {
	root := testenv.SocketTempDir(t)
	remaining := socketPathLimit() - len(root) - 1
	if remaining < 1 {
		t.Fatal("use a shorter TMPDIR for native socket boundary test")
	}
	socket := filepath.Join(root, strings.Repeat("s", remaining))
	if err := ValidateSocketPath(socket); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	_ = listener.Close()
	if err := ValidateSocketPath(socket + "s"); err == nil {
		t.Fatal("accepted socket path beyond native limit")
	}
	if listener, err := net.Listen("unix", socket+"s"); err == nil {
		_ = listener.Close()
		t.Fatal("kernel accepted socket path beyond native limit")
	}
	if err := ValidateSocketPath(strings.Repeat("é", socketPathLimit()/2+1)); err == nil {
		t.Fatal("counted characters instead of filesystem bytes")
	}
}
