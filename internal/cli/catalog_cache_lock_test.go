package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/shaul/mesh/internal/protocol"
)

const (
	catalogCacheHelperEnv = "MESH_CATALOG_CACHE_HELPER"
	catalogCacheStateEnv  = "MESH_STATE_DIR"
)

func TestCatalogCacheConcurrentWriters(t *testing.T) {
	if os.Getenv(catalogCacheHelperEnv) != "" {
		runCatalogCacheHelper(t)
		return
	}

	stateDir := t.TempDir()
	t.Setenv(catalogCacheStateEnv, stateDir)
	seedCatalogCache(t)

	const writers = 2
	writerOpen := make([]string, writers)
	writerStart := make([]*exec.Cmd, writers)
	// Optional cache setup can lose a connection-open race. Establish healthy
	// stores before releasing both writers at the shared Save barrier.
	for index := range writers {
		writerOpen[index] = filepath.Join(stateDir, fmt.Sprintf("writer-%d-open", index))
		writerStart[index] = startCatalogCacheHelper(t, stateDir, writerOpen[index], filepath.Join(stateDir, "writers-start"))
		waitForCatalogCacheFile(t, writerOpen[index])
	}

	// A held third writer may outlive the 150 ms SQLite budget under load.
	// TestCatalogSQLiteWriterContentionWithoutCancellation covers that failure path.
	if err := os.WriteFile(filepath.Join(stateDir, "writers-start"), nil, 0o600); err != nil { //nolint:gosec // marker path is inside the test TempDir.
		t.Fatal(err)
	}
	// Reap both helpers before failing so each can flush its buffered evidence.
	var writerErr error
	for _, writer := range writerStart {
		writerErr = errors.Join(writerErr, writer.Wait())
	}
	if writerErr != nil {
		t.Fatalf("cache helper failed: %v", writerErr)
	}
	cache, err := OpenCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cache.Close() }()
	host := HostRecord{ID: "host-cache-lock", MachineName: "pc", MeshIdentity: "identity-cache-lock"}
	rows, err := cache.Load(t.Context(), host)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		seen[row.ID] = true
	}
	for index := range writers {
		want := "SESSION-writer-" + strconv.Itoa(index) + "-open"
		if !seen[want] {
			t.Fatalf("cache lost concurrent writer %s: rows=%v", want, rows)
		}
	}
}

func seedCatalogCache(t *testing.T) {
	t.Helper()
	cache, err := OpenCatalogCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cache.Close() }()
	host := HostRecord{ID: "host-cache-lock", MachineName: "pc", MeshIdentity: "identity-cache-lock"}
	if err := cache.Save(t.Context(), host, nil); err != nil {
		t.Fatal(err)
	}
}

func startCatalogCacheHelper(t *testing.T, stateDir, readyPath, startPath string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", "^TestCatalogCacheConcurrentWriters$") //nolint:gosec // test helper launches this exact test binary with fixed arguments.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		catalogCacheHelperEnv+"=writer",
		catalogCacheStateEnv+"="+stateDir,
		"MESH_CATALOG_CACHE_READY="+readyPath,
		"MESH_CATALOG_CACHE_START="+startPath,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process == nil {
			return
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func waitForCatalogCacheFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil { //nolint:gosec // marker paths are created under this test's TempDir.
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for cache helper marker %s", path)
}

func runCatalogCacheHelper(t *testing.T) {
	readyPath := os.Getenv("MESH_CATALOG_CACHE_READY")
	startPath := os.Getenv("MESH_CATALOG_CACHE_START")
	observer := newCatalogWriterObserver(readyPath)
	defer observer.flush()
	observer.record(catalogWriterEvent{kind: "open-entry"})
	cache, err := OpenCatalogCache(context.Background())
	observer.result("open-return", err)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.close(cache)
	if cache.store == nil {
		t.Fatalf("cache writer setup: %v", cache.openErr)
	}
	if err := os.WriteFile(readyPath, nil, 0o600); err != nil { //nolint:gosec // helper marker is inside the test TempDir.
		t.Fatal(err)
	}
	observer.record(catalogWriterEvent{kind: "ready-before-barrier"})
	waitForCatalogCacheFile(t, startPath)
	observer.record(catalogWriterEvent{kind: "barrier-return"})
	index := filepath.Base(readyPath)
	host := HostRecord{ID: "host-cache-lock", MachineName: "pc", MeshIdentity: "identity-cache-lock"}
	row := protocol.SessionInfo{ID: "SESSION-" + index, HostID: host.ID, Command: []string{"sh"}, Cwd: "/tmp", State: "detached", CreatedAt: time.Now().UTC()}
	ctx, cancel := context.WithTimeout(context.Background(), catalogCacheWriteTimeout)
	defer func() {
		cancel()
		observer.result("context-after-cancel", ctx.Err())
	}()
	observer.beginSave(ctx)
	err = cache.Save(ctx, host, []protocol.SessionInfo{row})
	observer.result("save-return", err)
	observer.result("context-after-save", ctx.Err())
	if err != nil {
		t.Fatalf("cache writer %s: %v", index, err)
	}
}
