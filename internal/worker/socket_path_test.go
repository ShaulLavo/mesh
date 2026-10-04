package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/testenv"
)

func TestRunRejectsLongSocketPathBeforeStartingCommand(t *testing.T) {
	root := testenv.SocketTempDir(t)
	dir := filepath.Join(root, strings.Repeat("s", 110))
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "started")
	_, err := Run(Config{ID: "7K3D", Dir: dir, Command: []string{"sh", "-c", "touch \"$1\"", "fixture", marker}})
	if err == nil || !strings.Contains(err.Error(), "MESH_STATE_DIR") {
		t.Fatalf("worker error = %v, want shorter state path guidance", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("command started before path validation: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("worker artifacts created before path validation: %v, %v", entries, err)
	}
}
