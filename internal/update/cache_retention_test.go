package update

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/updateinstall"
)

func TestCacheRetentionKeepsUnfinishedRunsAndInstallation(t *testing.T) {
	state, cache := t.TempDir(), t.TempDir()
	store, err := OpenStore(state)
	if err != nil {
		t.Fatal(err)
	}
	fleet := testFleet(t, 1)
	complete, err := store.Start(fleet.Members[0].ID, fleet, testManifest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Change(complete.ID, func(r *Run) error { r.Targets[0].State = Updated; return nil }); err != nil {
		t.Fatal(err)
	}
	manifest := testManifest()
	manifest.Commit = strings.Repeat("f", 40)
	pending, err := store.Start(fleet.Members[0].ID, fleet, manifest)
	if err != nil {
		t.Fatal(err)
	}
	installed := testManifest()
	installed.Commit = strings.Repeat("1", 40)
	status := updateinstall.Status{Schema: 1, Phase: updateinstall.Committed, Request: updateinstall.Request{Manifest: installed}}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(state, "update"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "update", "installation.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, digest := range []string{complete.ReleaseDigest, pending.ReleaseDigest, installed.Digest()} {
		if err := os.Mkdir(filepath.Join(cache, digest), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := PruneCache(t.Context(), state, cache); err != nil {
		t.Fatal(err)
	}
	for digest, want := range map[string]bool{complete.ReleaseDigest: false, pending.ReleaseDigest: true, installed.Digest(): true} {
		_, err := os.Stat(filepath.Join(cache, digest))
		if (err == nil) != want {
			t.Fatalf("manifest %s retained = %v, want %v", digest, err == nil, want)
		}
	}
}
