package testenv

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// SocketTempDir reserves room for a full session ID and Darwin's shorter sun_path.
func SocketTempDir(t *testing.T) string {
	t.Helper()
	base, err := socketTempBase(os.TempDir(), os.Getenv("MESH_SHORT_TMP"))
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, "s-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil { //nolint:gosec // cleanup owns the fresh MkdirTemp directory under trusted test-runner scratch
			t.Error(err)
		}
	})
	return dir
}

func socketTempBase(caller, override string) (string, error) {
	base := caller
	if override != "" {
		base = override
	}
	canonical, err := filepath.EvalSymlinks(filepath.Clean(base))
	if err != nil {
		return "", fmt.Errorf("resolve socket fixture scratch %q: %w", base, err)
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", fmt.Errorf("resolve socket fixture scratch %q: %w", base, err)
	}
	if len(filepath.Join(canonical, "s-4294967295", "sessions", "00000000000000000000000000", "sock")) <= 103 {
		return canonical, nil
	}
	if override != "" {
		return "", fmt.Errorf("socket fixture scratch %q exceeds the 103-byte socket path budget; set MESH_SHORT_TMP to a shorter directory", canonical)
	}
	return socketTempBase("/tmp", "/tmp")
}
