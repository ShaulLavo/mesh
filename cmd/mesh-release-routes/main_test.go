package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/update"
)

func TestMaintainerPlansRecordedEvidenceAndResumesLocalCursor(t *testing.T) {
	fixture := "../../internal/update/testdata/discovery"
	args := []string{"-input", filepath.Join(fixture, "source.json"), "-documents", fixture, "-cursor", filepath.Join(t.TempDir(), "cursor.json")}
	if err := run(t.Context(), args, &bytes.Buffer{}); err == nil {
		t.Fatal("local evidence acknowledgment was optional")
	}
	args = append(args, "-local-evidence")
	for pass := 0; pass < 2; pass++ {
		var output bytes.Buffer
		if err := run(t.Context(), args, &output); err != nil {
			t.Fatal(err)
		}
		var step update.DiscoveryStep
		if err := json.Unmarshal(output.Bytes(), &step); err != nil {
			t.Fatal(err)
		}
		if step.Next == nil || step.Next.Manifest.Version != "v0.1.306" || step.Next.Transition.Proof != "6dcd6a3569d1e678456cb7b5918efc531dd93e698a38b31c941812f147a8c6a6" {
			t.Fatal("maintainer did not plan recorded exact edge")
		}
	}
}
