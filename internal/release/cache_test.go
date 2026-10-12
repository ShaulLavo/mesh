package release

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPruneCacheReleasesHistoricalDownloadsAndPreservesPinnedImages(t *testing.T) {
	binary := []byte("mesh retention fixture")
	archive := testArchive(t, binary)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) }))
	t.Cleanup(server.Close)
	client := Client{BaseURL: server.URL, HTTPClient: server.Client()}
	cache := t.TempDir()
	var paths []string
	var pinned Manifest
	for i := range 12 {
		manifest := testManifestForArchive(binary, archive)
		manifest.Version = fmt.Sprintf("v0.2.%d", i)
		path, err := client.Download(t.Context(), manifest, Platform{OS: "linux", Arch: "amd64"}, cache)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
		pinned = manifest
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(cache, strings.Repeat("f", 64))); err != nil {
		t.Fatal(err)
	}
	if err := PruneCache(t.Context(), cache, map[string]bool{pinned.Digest(): true}); err != nil {
		t.Fatal(err)
	}
	for i, path := range paths {
		_, err := os.Stat(path)
		if (err == nil) != (i == len(paths)-1) {
			t.Fatalf("download %d retained unexpectedly: %v", i, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(cache, strings.Repeat("f", 64))); err != nil {
		t.Fatalf("unowned symlink removed: %v", err)
	}
}

func TestPruneCannotRemoveAnExecutableBeingConsumed(t *testing.T) {
	binary := []byte("mesh leased fixture")
	archive := testArchive(t, binary)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) }))
	t.Cleanup(server.Close)
	client := Client{BaseURL: server.URL, HTTPClient: server.Client()}
	cache := t.TempDir()
	manifest := testManifestForArchive(binary, archive)
	err := client.WithExecutable(t.Context(), manifest, Platform{OS: "linux", Arch: "amd64"}, cache, func(path string) error {
		ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
		defer cancel()
		if err := PruneCache(ctx, cache, nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("prune bypassed held executable lease: %v", err)
		}
		_, err := os.ReadFile(path) //nolint:gosec // leased test download
		if err != nil {
			return fmt.Errorf("read leased fixture: %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
