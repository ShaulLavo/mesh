//go:build mesh_installed_fixture

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"

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
	if os.Args[1] == "update-helper" {
		holdInstalledHelper(path)
	}
}

func holdInstalledHelper(path string) {
	var fault struct {
		Hold []string `json:"holdHelper"`
	}
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &fault) != nil {
		os.Exit(71)
	}
	prefix := os.Getenv("MESH_INSTALLED_START_RECORD")
	if prefix == "" {
		os.Exit(73)
	}
	marker := prefix + ".helper." + release.Version + ".json"
	value, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "version": release.Version})
	if err := os.WriteFile(marker, value, 0600); err != nil {
		os.Exit(73)
	}
	for slices.Contains(fault.Hold, release.Version) {
		time.Sleep(10 * time.Millisecond)
		data, err = os.ReadFile(path)
		fault.Hold = nil
		if err != nil || json.Unmarshal(data, &fault) != nil {
			os.Exit(71)
		}
	}
}

func recordInstalledStart() {
	path := os.Getenv("MESH_INSTALLED_START_RECORD")
	if path == "" {
		return
	}
	policy, err := os.ReadFile(filepath.Join(os.Getenv("MESH_STATE_DIR"), "updates", "host-policy.json"))
	entry := map[string]any{"version": release.Version, "pid": os.Getpid(), "policy": json.RawMessage("null")}
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
