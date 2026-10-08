package updateinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/shaul/mesh/internal/release"
)

func writeImage(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0755); err != nil { //nolint:gosec // executable test fixture
		t.Fatal(err)
	}
}

func assertExists(t *testing.T, path string, want bool) {
	t.Helper()
	_, err := os.Lstat(path)
	if got := err == nil; got != want {
		t.Fatalf("%s exists = %v, want %v (%v)", filepath.Base(path), got, want, err)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func withWorkerImageLinks(t *testing.T, needed bool) {
	t.Helper()
	prior := workerImagesNeedLinks
	workerImagesNeedLinks = needed
	t.Cleanup(func() { workerImagesNeedLinks = prior })
}

func TestStagingPrunesImagesFromFinishedTransactions(t *testing.T) {
	withWorkerImageLinks(t, false)
	f := newFixture(t)
	dir := filepath.Dir(f.engine.cfg.Executable)
	stale := []string{".mesh-update-op-27.previous", ".mesh-update-op-28.previous", ".mesh-update-op-28.candidate"}
	for _, name := range stale {
		writeImage(t, filepath.Join(dir, name), []byte("old mesh"))
	}
	unrelated := []string{".mesh-update-notes.txt", ".mesh-update-.previous", "mesh.previous", ".mesh-link-123"}
	for _, name := range unrelated {
		writeImage(t, filepath.Join(dir, name), []byte("keep"))
	}
	if err := os.Mkdir(filepath.Join(dir, ".mesh-update-dir.previous"), 0700); err != nil {
		t.Fatal(err)
	}
	status := f.stage()
	for _, name := range stale {
		assertExists(t, filepath.Join(dir, name), false)
	}
	for _, name := range append(unrelated, ".mesh-update-dir.previous") {
		assertExists(t, filepath.Join(dir, name), true)
	}
	assertExists(t, status.Previous, true)
	assertExists(t, status.Candidate, true)
}

func TestCommitPrunesEarlierImagesAndKeepsCurrentRollbackImage(t *testing.T) {
	withWorkerImageLinks(t, false)
	f := newFixture(t)
	dir := filepath.Dir(f.engine.cfg.Executable)
	f.stage()
	// Images that appear after staging, for example from a crashed older
	// installer, are cleared once the new release is healthy.
	stale := filepath.Join(dir, ".mesh-update-op-3.previous")
	writeImage(t, stale, []byte("old mesh"))
	if _, err := f.engine.Grant(context.Background(), f.request.ID, 1); err != nil {
		t.Fatal(err)
	}
	status, err := f.engine.Run(context.Background())
	if err != nil || status.Phase != Committed {
		t.Fatalf("commit = %s, %v", status.Phase, err)
	}
	assertExists(t, stale, false)
	assertExists(t, status.Previous, true)
	if err = verifyFile(status.Previous, f.request.Current.Digest); err != nil {
		t.Fatalf("current rollback image: %v", err)
	}
	f.roundTrip()
}

func TestRollbackKeepsTheRestoredImage(t *testing.T) {
	withWorkerImageLinks(t, false)
	f := newFixture(t)
	f.stage()
	f.manager.failCandidate = true
	if _, err := f.engine.Grant(context.Background(), f.request.ID, 1); err != nil {
		t.Fatal(err)
	}
	status, err := f.engine.Run(context.Background())
	if err == nil || status.Phase != RolledBack {
		t.Fatalf("rollback = %s, %v", status.Phase, err)
	}
	assertExists(t, status.Previous, true)
	if err = verifyFile(f.engine.cfg.Executable, f.request.Current.Digest); err != nil {
		t.Fatal(err)
	}
}

func TestPruneKeepsImagesThatLiveWorkersExecuteWhenLinksProveThem(t *testing.T) {
	dir := t.TempDir()
	running, idle := []byte("mesh v0.1.180"), []byte("mesh v0.1.190")
	runningPath, idlePath := filepath.Join(dir, ".mesh-update-op-a.previous"), filepath.Join(dir, ".mesh-update-op-b.previous")
	candidatePath := filepath.Join(dir, ".mesh-update-op-b.candidate")
	current := Status{Previous: filepath.Join(dir, ".mesh-update-op-c.previous"), Candidate: filepath.Join(dir, ".mesh-update-op-c.candidate")}
	reset := func() {
		writeImage(t, runningPath, running)
		writeImage(t, idlePath, idle)
		writeImage(t, candidatePath, idle)
		writeImage(t, current.Previous, idle)
	}
	worker := Worker{ID: "session-1", PID: 1, Protocol: 1, Build: &release.Build{Digest: digestBytes(running)}}

	withWorkerImageLinks(t, true)
	reset()
	if err := pruneTransactionImages(dir, current, Health{Workers: []Worker{worker}}); err != nil {
		t.Fatal(err)
	}
	assertExists(t, runningPath, true)
	assertExists(t, idlePath, false)
	assertExists(t, candidatePath, false)
	assertExists(t, current.Previous, true)

	// A worker whose executable is unknown might run any earlier image.
	reset()
	if err := pruneTransactionImages(dir, current, Health{Workers: []Worker{{ID: "legacy", PID: 2, Protocol: 1}}}); err != nil {
		t.Fatal(err)
	}
	assertExists(t, runningPath, true)
	assertExists(t, idlePath, true)
	assertExists(t, candidatePath, false)

	// Linux identifies workers through /proc, so names are not needed.
	withWorkerImageLinks(t, false)
	reset()
	if err := pruneTransactionImages(dir, current, Health{Workers: []Worker{worker}}); err != nil {
		t.Fatal(err)
	}
	assertExists(t, runningPath, false)
	assertExists(t, idlePath, false)
	assertExists(t, current.Previous, true)
}

func TestPruneLeavesSymlinkedImagesAlone(t *testing.T) {
	withWorkerImageLinks(t, false)
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "mesh")
	writeImage(t, target, []byte("mesh"))
	link := filepath.Join(dir, ".mesh-update-op-a.previous")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := pruneTransactionImages(dir, Status{}, Health{}); err != nil {
		t.Fatal(err)
	}
	assertExists(t, link, true)
	assertExists(t, target, true)
}
