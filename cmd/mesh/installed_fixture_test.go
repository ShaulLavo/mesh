//go:build mesh_installed_fixture

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/release"
)

func TestInstalledStartMarkerReplacesFile(t *testing.T) {
	directory := t.TempDir()
	prefix := filepath.Join(directory, "start")
	t.Setenv("MESH_INSTALLED_START_RECORD", prefix)
	t.Setenv("MESH_STATE_DIR", directory)

	recordInstalledStart()
	path := prefix + "." + release.Version + ".json"
	previous, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = previous.Close() })
	oldInfo, err := previous.Stat()
	if err != nil {
		t.Fatal(err)
	}

	recordInstalledStart()
	newInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(oldInfo, newInfo) {
		t.Fatal("startup marker was rewritten in place")
	}
}
