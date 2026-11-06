package cli

import (
	"context"
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
	for _, writer := range writerStart {
		waitForCatalogCacheProcess(t, writer)
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

func waitForCatalogCacheProcess(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("cache helper failed: %v", err)
	}
}

func runCatalogCacheHelper(t *testing.T) {
	readyPath := os.Getenv("MESH_CATALOG_CACHE_READY")
	startPath := os.Getenv("MESH_CATALOG_CACHE_START")
	cache, err := OpenCatalogCache(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cache.Close() }()
	if cache.store == nil {
		t.Fatalf("cache writer setup: %v", cache.openErr)
	}
	if err := os.WriteFile(readyPath, nil, 0o600); err != nil { //nolint:gosec // helper marker is inside the test TempDir.
		t.Fatal(err)
	}
	waitForCatalogCacheFile(t, startPath)
	index := filepath.Base(readyPath)
	host := HostRecord{ID: "host-cache-lock", MachineName: "pc", MeshIdentity: "identity-cache-lock"}
	row := protocol.SessionInfo{ID: "SESSION-" + index, HostID: host.ID, Command: []string{"sh"}, Cwd: "/tmp", State: "detached", CreatedAt: time.Now().UTC()}
	ctx, cancel := context.WithTimeout(context.Background(), catalogCacheWriteTimeout)
	defer cancel()
	if err := cache.Save(ctx, host, []protocol.SessionInfo{row}); err != nil {
		t.Fatalf("cache writer %s: %v", index, err)
	}
}
