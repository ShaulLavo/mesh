package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateDirMakesConfiguredPathAbsolute(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	want := t.TempDir()
	relative, err := filepath.Rel(cwd, want)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESH_STATE_DIR", relative)

	got, err := StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("StateDir() = %q, want absolute path", got)
	}
	if got != filepath.Clean(want) {
		t.Fatalf("StateDir() = %q, want %q", got, filepath.Clean(want))
	}
}
