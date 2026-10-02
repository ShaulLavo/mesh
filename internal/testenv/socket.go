package testenv

import (
	"os"
	"testing"
)

// SocketTempDir keeps Unix socket paths below their platform limit under caller scratch.
func SocketTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "s-")
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
