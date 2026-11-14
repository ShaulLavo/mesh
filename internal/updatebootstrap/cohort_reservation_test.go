package updatebootstrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/session"
	"github.com/shaul/mesh/internal/session/execution"
	"github.com/shaul/mesh/internal/worker"
)

func TestReserveRetainedWorkerCohort(t *testing.T) {
	directory, output := os.Getenv("MESH_COHORT_RESERVE_ROOT"), os.Getenv("MESH_COHORT_RESERVATION")
	if directory == "" || output == "" {
		t.Skip("native cohort reservation exporter")
	}
	ref, err := worker.Reserve(directory, session.Generic)
	if err != nil {
		t.Fatal(err)
	}
	files, err := execution.OpenFiles(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = files.Close() }()
	for _, name := range []string{".reservation", ".launching"} {
		var saved session.Reservation
		if err := files.Read(filepath.Join(ref.ID, name), &saved); err != nil || saved != ref {
			t.Fatalf("maintained reservation marker %s differs: %v", name, err)
		}
	}
	data, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(filepath.Dir(output))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile(filepath.Base(output), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
