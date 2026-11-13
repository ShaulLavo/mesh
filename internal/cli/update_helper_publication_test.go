package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/release"
	"github.com/shaul/mesh/internal/update"
	"github.com/shaul/mesh/internal/updateinstall"
)

func helperPublicationFixture(t *testing.T, phase updateinstall.Phase) (string, updateinstall.Status, *update.Store) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("PATH", root)
	store, err := update.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	current := release.Build{Version: "v0.1.9001", Digest: strings.Repeat("a", 64), Platform: release.CurrentPlatform()}
	manifest := release.Manifest{Schema: 1, Version: "v0.1.9003", Commit: strings.Repeat("d", 40),
		Compatibility: release.Compatibility{StateReadMin: 16, StateReadMax: 16, StateWrite: 16, WorkerMin: 1, WorkerMax: 1, WorkerWrite: 1, JournalVersion: 1}}
	for _, platform := range []release.Platform{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}, {OS: "darwin", Arch: "arm64"}} {
		manifest.Artifacts = append(manifest.Artifacts, release.Artifact{Platform: platform, Archive: "mesh_" + platform.OS + "_" + platform.Arch + ".tar.gz",
			SHA256: strings.Repeat("f", 64), BinarySHA256: strings.Repeat("b", 64)})
	}
	request := updateinstall.Request{ID: strings.Repeat("e", 32), Generation: 3, TargetID: "fixture-host", Current: current, Manifest: manifest}
	settings := updateinstall.Settings{StateDir: root, Executable: filepath.Join(root, "absent-image"), CacheDir: filepath.Join(root, "cache"),
		Service: updateinstall.ServiceSpec{Kind: "systemd", Name: "mesh.service"}}
	worker := updateinstall.Worker{ID: "synthetic-worker", PID: 101, ShellPID: 102}
	status := updateinstall.Status{Schema: 1, Phase: phase, Request: request, Settings: settings,
		Original:  updateinstall.Health{HostID: request.TargetID, Build: current, Workers: []updateinstall.Worker{worker}},
		Verified:  &updateinstall.Health{HostID: request.TargetID, Build: release.Build{Digest: manifest.Artifacts[0].BinarySHA256}, Workers: []updateinstall.Worker{worker}},
		UpdatedAt: time.Date(2026, 11, 13, 12, 10, 42, 139400051, time.UTC)}
	owned := update.OwnedRequest{Request: request.ID, Generation: request.Generation, PolicyRevision: 1, Epoch: 1,
		Installation: request, Settings: settings, Metadata: strings.Repeat("c", 64),
		Edge: update.Edge{From: current.Digest, To: manifest.Artifacts[0].BinarySHA256, Platform: current.Platform}}
	if phase == updateinstall.RolledBack {
		status.Verified.Build = current
		status.Error = "synthetic unhealthy candidate"
	}
	writeHelperPublicationJSON(t, root, "update/installation.json", status)
	writeHelperPublicationJSON(t, root, "updates/host-update.json", update.HostUpdate{Schema: 1, Phase: update.HostInstalling, Owned: &owned})
	return root, status, store
}

func writeHelperPublicationJSON(t *testing.T, root, name string, value any) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateHelperPublishesOwnedCommitBeforePromotion(t *testing.T) {
	root, journal, store := helperPublicationFixture(t, updateinstall.Committed)
	// The absent image stops helper promotion before any executable or manager runs.
	err := updateHelperStep(t.Context(), root)
	if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "upgrade committed update helper") {
		t.Fatalf("helper did not reach the isolated promotion failure: %v", err)
	}
	assertHelperPublishedCommit(t, store, journal)
	err = updateHelperStep(t.Context(), root)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repeated promotion failure = %v", err)
	}
	assertHelperPublishedCommit(t, store, journal)
}

func assertHelperPublishedCommit(t *testing.T, store *update.Store, journal updateinstall.Status) {
	t.Helper()
	state, err := store.HostState(t.Context())
	if err != nil || state.Owned != nil || state.LastRequest != journal.Request.ID || state.LastPhase != updateinstall.Committed {
		t.Fatalf("helper left the committed installation unpublished: %+v, %v", state, err)
	}
	entries, more, err := store.HostLedger(t.Context(), 0, 64)
	if err != nil || more || len(entries) != 1 {
		t.Fatalf("helper ledger = %+v, more %v, %v", entries, more, err)
	}
	entry := entries[0]
	if entry.Request != journal.Request.ID || entry.Generation != journal.Request.Generation || entry.Metadata != strings.Repeat("c", 64) || entry.Origin != "host" || entry.Workers != 1 || entry.WorkersPreserved != 1 || !entry.CommittedAt.Equal(journal.UpdatedAt) {
		t.Fatalf("helper changed the actual owned commit: %+v", entry)
	}
}

func TestUpdateHelperPublishesOwnedRollbackBeforeReturning(t *testing.T) {
	root, journal, store := helperPublicationFixture(t, updateinstall.RolledBack)
	if err := updateHelperStep(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	state, err := store.HostState(t.Context())
	if err != nil || state.Owned != nil || state.LastPhase != updateinstall.RolledBack {
		t.Fatalf("helper left rollback unpublished: %+v, %v", state, err)
	}
	files, err := filepath.Glob(filepath.Join(root, "updates", "host-failed-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("helper lost failed-canary evidence: %v, %v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var failed update.FailedEdge
	if err := json.Unmarshal(data, &failed); err != nil || failed.Version != journal.Request.Manifest.Version || failed.Problem != journal.Error {
		t.Fatalf("helper changed failed-canary evidence: %+v, %v", failed, err)
	}
}

func TestUpdateHelperPublicationKeepsInvalidOwnedResult(t *testing.T) {
	for _, boundary := range []string{"request", "generation", "settings", "host", "image", "worker"} {
		t.Run(boundary, func(t *testing.T) { checkHelperPublicationBoundary(t, boundary) })
	}
}

func checkHelperPublicationBoundary(t *testing.T, boundary string) {
	t.Helper()
	root, journal, store := helperPublicationFixture(t, updateinstall.Committed)
	switch boundary {
	case "request":
		journal.Request.ID = strings.Repeat("f", 32)
	case "generation":
		journal.Request.Generation++
	case "settings":
		journal.Settings.Executable = filepath.Join(root, "another-image")
	case "host":
		journal.Verified.HostID = "another-host"
	case "image":
		journal.Verified.Build.Digest = strings.Repeat("f", 64)
	case "worker":
		journal.Verified.Workers = nil
	default:
		t.Fatal("unknown publication boundary")
	}
	writeHelperPublicationJSON(t, root, "update/installation.json", journal)
	if err := updateHelperStep(t.Context(), root); err == nil || strings.Contains(err.Error(), "upgrade committed update helper") {
		t.Fatalf("invalid %s passed publication: %v", boundary, err)
	}
	state, err := store.HostState(t.Context())
	if err != nil || state.Owned == nil || state.LastRequest != "" {
		t.Fatalf("invalid %s cleared ownership: %+v, %v", boundary, state, err)
	}
	entries, _, err := store.HostLedger(t.Context(), 0, 64)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid %s published a ledger entry: %+v, %v", boundary, entries, err)
	}
}

func TestUpdateHelperPublicationWaitsForTerminalJournal(t *testing.T) {
	root, _, store := helperPublicationFixture(t, updateinstall.Validating)
	if err := update.PublishHostResult(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	state, err := store.HostState(t.Context())
	if err != nil || state.Owned == nil || state.LastRequest != "" {
		t.Fatalf("validating installation was published: %+v, %v", state, err)
	}
	entries, _, err := store.HostLedger(t.Context(), 0, 64)
	if err != nil || len(entries) != 0 {
		t.Fatalf("validating ledger = %+v, %v", entries, err)
	}
}

func TestUpdateHelperPublicationConcurrentReplayIsIdempotent(t *testing.T) {
	root, journal, store := helperPublicationFixture(t, updateinstall.Committed)
	var workers sync.WaitGroup
	results := make(chan error, 8)
	for range cap(results) {
		workers.Go(func() { results <- update.PublishHostResult(t.Context(), root) })
	}
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertHelperPublishedCommit(t, store, journal)
}

func TestUpdateHelperPublicationPreservesAdoptionConfirmation(t *testing.T) {
	root, journal, store := helperPublicationFixture(t, updateinstall.Committed)
	pin := strings.Repeat("c", 64)
	details, err := json.Marshal(map[string]any{"schema": 1, "id": journal.Request.ID, "pin": pin, "request": journal.Request, "settings": journal.Settings})
	if err != nil {
		t.Fatal(err)
	}
	writeHelperPublicationJSON(t, root, "updates/host-adoption.json", update.HostAdoption{Schema: 1, Operation: journal.Request.ID, Pin: pin, Details: details})
	if err := update.PublishHostResult(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	state, err := store.HostState(t.Context())
	if err != nil || state.Owned == nil || state.LastRequest != "" {
		t.Fatalf("helper bypassed adoption confirmation: %+v, %v", state, err)
	}
	entries, _, err := store.HostLedger(t.Context(), 0, 64)
	if err != nil || len(entries) != 0 {
		t.Fatalf("unconfirmed adoption ledger = %+v, %v", entries, err)
	}
}

func TestUpdateHelperPublicationFailureKeepsOwnershipForRetry(t *testing.T) {
	root, journal, store := helperPublicationFixture(t, updateinstall.Committed)
	blocked := filepath.Join(root, "updates", "host-ledger.json")
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := updateHelperStep(t.Context(), root); err == nil || strings.Contains(err.Error(), "upgrade committed update helper") {
		t.Fatalf("ledger failure was hidden by helper promotion: %v", err)
	}
	state, err := store.HostState(t.Context())
	if err != nil || state.Owned == nil || state.LastRequest != "" {
		t.Fatalf("failed ledger publication cleared ownership: %+v, %v", state, err)
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := update.PublishHostResult(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	assertHelperPublishedCommit(t, store, journal)
}
