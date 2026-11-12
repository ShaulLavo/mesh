package updatebootstrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/updateinstall"
)

type cohortWorker struct {
	Session  string `json:"session"`
	PID      int    `json:"pid"`
	ShellPID int    `json:"shellPid"`
	Digest   string `json:"digest"`
	Protocol int    `json:"protocol"`
}

func TestRetainedWorkerCohortIdentity(t *testing.T) {
	state, record := os.Getenv("MESH_COHORT_STATE"), os.Getenv("MESH_COHORT_RECORD")
	if state == "" || record == "" {
		t.Skip("native release worker cohort control")
	}
	root, err := os.OpenRoot(filepath.Dir(record))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(filepath.Base(record))
	if err != nil {
		t.Fatal(err)
	}
	var expected struct {
		Workers  []cohortWorker `json:"workers"`
		Original *cohortWorker  `json:"original"`
	}
	if err := json.Unmarshal(data, &expected); err != nil {
		t.Fatal(err)
	}
	observed, err := Inspect(t.Context(), state)
	if err != nil {
		t.Fatal(err)
	}
	if len(expected.Workers) == 0 {
		t.Fatal("empty retained cohort cannot certify worker coverage")
	}
	if expected.Original != nil {
		expected.Workers = append(expected.Workers, *expected.Original)
	}
	for _, want := range expected.Workers {
		if !matchesCohort(want, observed.Health.Workers) {
			t.Fatalf("retained worker %s lost its exact running image, protocol or process identity", want.Session)
		}
	}
}

func matchesCohort(want cohortWorker, observed []updateinstall.Worker) bool {
	for _, got := range observed {
		if got.ID == want.Session && got.PID == want.PID && got.ShellPID == want.ShellPID && got.Protocol == want.Protocol && got.Build != nil && got.Build.Digest == want.Digest {
			return true
		}
	}
	return false
}
