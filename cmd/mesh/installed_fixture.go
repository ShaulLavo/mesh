//go:build mesh_installed_fixture

package main

import (
	"encoding/json"
	"os"
	"slices"

	"github.com/shaul/mesh/internal/release"
)

// Unpublished installed fixtures can fail a real native service start while
// leaving the signed executable, service definition and helper checks intact.
func init() {
	path := os.Getenv("MESH_INSTALLED_FAULT")
	if path == "" || len(os.Args) < 2 || slices.Contains(os.Args, "--check-journal") {
		return
	}
	var fault struct {
		Daemon []string `json:"daemon"`
		Helper []string `json:"helper"`
	}
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &fault) != nil {
		os.Exit(71)
	}
	if os.Args[1] == "daemon" && slices.Contains(fault.Daemon, release.Version) {
		os.Exit(70)
	}
	if os.Args[1] == "update-helper" && slices.Contains(fault.Helper, release.Version) {
		os.Exit(70)
	}
}
