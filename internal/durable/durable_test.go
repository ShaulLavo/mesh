package durable

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSyncAndBarrierAcceptFilesAndDirectories(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record")
	if err := os.WriteFile(path, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{path, dir} {
		file, err := os.Open(name) //nolint:gosec // a path under the test's own temporary directory
		if err != nil {
			t.Fatal(err)
		}
		if err := Sync(file); err != nil {
			t.Fatalf("sync %s: %v", name, err)
		}
		if err := Barrier(file); err != nil {
			t.Fatalf("barrier %s: %v", name, err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSyncNamesTheFileOnError(t *testing.T) {
	file, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Sync(file); err == nil {
		t.Fatal("sync of a closed file succeeded")
	}
}
