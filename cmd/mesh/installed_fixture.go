//go:build mesh_installed_fixture

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	if os.Args[1] == "daemon" {
		recordInstalledStart()
	}
	if os.Args[1] == "daemon" && slices.Contains(fault.Daemon, release.Version) {
		os.Exit(70)
	}
	if os.Args[1] == "update-helper" && slices.Contains(fault.Helper, release.Version) {
		os.Exit(70)
	}
}

func recordInstalledStart() {
	path := os.Getenv("MESH_INSTALLED_START_RECORD")
	if path == "" {
		return
	}
	policy, err := os.ReadFile(filepath.Join(os.Getenv("MESH_STATE_DIR"), "updates", "host-policy.json"))
	entry := map[string]any{"version": release.Version, "policy": json.RawMessage("null")}
	if err == nil {
		entry["policy"] = json.RawMessage(policy)
	}
	data, err := json.Marshal(entry)
	if err != nil {
		os.Exit(73)
	}
	if err := os.WriteFile(path+"."+release.Version+".json", data, 0600); err != nil {
		os.Exit(73)
	}
}
