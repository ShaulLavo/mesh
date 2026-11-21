//go:build mesh_installed_fixture

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/transporttrace"
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
	if os.Args[1] == "daemon" {
		configureTransportLifecycle()
	}
	if os.Args[1] == "update-helper" {
		holdInstalledHelper(path)
	}
}

func configureTransportLifecycle() {
	directory := os.Getenv("MESH_TRANSPORT_LIFECYCLE_DIR")
	if directory == "" {
		return
	}
	build := release.Current()
	transporttrace.Configure(transporttrace.New(directory, transporttrace.Identity{
		PID: os.Getpid(), Version: build.Version, Commit: build.Commit, Digest: build.Digest,
	}))
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
	if err := writeInstalledMarker(marker, value); err != nil {
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
	if err := writeInstalledMarker(path+"."+release.Version+".json", data); err != nil {
		os.Exit(73)
	}
}

func writeInstalledMarker(path string, value []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".installed-start-*")
	if err != nil {
		return fmt.Errorf("create installed startup marker: %w", err)
	}
	defer func() { _ = os.Remove(file.Name()) }()
	_, writeErr := file.Write(value)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return fmt.Errorf("write installed startup marker: %w", err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("replace installed startup marker: %w", err)
	}
	return nil
}
