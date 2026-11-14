package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
)

func TestRollbackContextUsesTheRealCacheManifestDecoder(t *testing.T) {
	root, err := os.OpenRoot("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile("internal/update/testdata/discovery/v0.1.306/mesh-release.json")
	if err != nil {
		t.Fatal(err)
	}
	var candidate release.Manifest
	if err := json.Unmarshal(data, &candidate); err != nil {
		t.Fatal(err)
	}
	checkRollbackContextDecoder(t, release.Manifest{Compatibility: candidate.Compatibility}, false)
	checkRollbackContextDecoder(t, candidate, true)
}

func checkRollbackContextDecoder(t *testing.T, manifest release.Manifest, valid bool) {
	t.Helper()
	state, cache := t.TempDir(), t.TempDir()
	root, err := os.OpenRoot(state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := root.Mkdir("update", 0o700); err != nil {
		t.Fatal(err)
	}
	journal := updateinstall.Status{Schema: 1, Phase: updateinstall.RollingBack, Request: updateinstall.Request{Manifest: manifest}}
	data, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("update/installation.json", data, 0o600); err != nil {
		t.Fatal(err)
	}
	err = update.PruneCache(t.Context(), state, cache)
	if valid && err != nil {
		t.Fatalf("complete actual candidate was refused by cache reader: %v", err)
	}
	if !valid && (err == nil || !strings.Contains(err.Error(), "manifest schema 0")) {
		t.Fatalf("incomplete producer manifest did not reproduce cache refusal: %v", err)
	}
	manifestPath := filepath.Join(state, "manifest.json")
	contents, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("manifest.json", contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyContextManifest(manifestPath); (err == nil) != valid {
		t.Fatalf("producer decoder agreement: valid=%v error=%v", valid, err)
	}
}
