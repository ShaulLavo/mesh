package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientArchiveFormatsAndFailuresReleaseLease(t *testing.T) {
	for _, format := range []ArchiveFormat{ArchiveGzip, ArchiveBrotli} {
		t.Run(string(format), func(t *testing.T) { testClientFormat(t, format) })
	}
}

func testClientFormat(t *testing.T, format ArchiveFormat) {
	t.Helper()
	for _, scenario := range []string{"publish", "archive digest", "binary digest", "wrong codec", "wrong name", "truncated"} {
		t.Run(scenario, func(t *testing.T) { testClientFormatScenario(t, format, scenario) })
	}
}

func testClientFormatScenario(t *testing.T, format ArchiveFormat, scenario string) {
	t.Helper()
	body := []byte("verified executable")
	archive := encodeArchive(t, format, meshTar(t, body))
	if scenario == "wrong codec" {
		archive = encodeArchive(t, ArchiveGzip, meshTar(t, body))
		format = ArchiveBrotli
	}
	if scenario == "truncated" {
		archive = archive[:len(archive)-1]
	}
	manifest := testManifestForArchive(body, archive)
	manifest.Artifacts[1].Archive = ArchiveName(manifest.Artifacts[1].Platform, format)
	if scenario == "archive digest" {
		manifest.Artifacts[1].SHA256 = testDigest([]byte("wrong archive"))
	}
	if scenario == "binary digest" {
		manifest.Artifacts[1].BinarySHA256 = testDigest([]byte("wrong executable"))
	}
	if scenario == "wrong name" {
		manifest.Artifacts[1].Archive = "prefix_" + manifest.Artifacts[1].Archive
	}
	cache := t.TempDir()
	client, requests := archiveTestOrigin(t, manifest, archive)
	path, err := client.Download(t.Context(), manifest, manifest.Artifacts[1].Platform, cache)
	assertCacheLeaseReleased(t, cache)
	if scenario != "publish" {
		if path != "" || err == nil {
			t.Fatalf("invalid archive: path %q, error %v", path, err)
		}
		assertNoArchiveTemps(t, cache)
		if scenario == "wrong name" && requests.Load() != 0 {
			t.Fatal("invalid name reached the network")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(path) //nolint:gosec // test-owned verified cache result
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatalf("cache %q: %v", actual, err)
	}
	if requests.Load() != 1 {
		t.Fatalf("archive requests %d", requests.Load())
	}
	if _, err := client.Download(t.Context(), manifest, manifest.Artifacts[1].Platform, cache); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatal("verified cache was downloaded again")
	}
	assertNoArchiveTemps(t, cache)
}

func archiveTestOrigin(t *testing.T, manifest Manifest, archive []byte) (Client, *atomic.Int32) {
	t.Helper()
	requests := new(atomic.Int32)
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/"+ManifestAssetName) {
			_, _ = w.Write(data)
			return
		}
		requests.Add(1)
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)
	return Client{BaseURL: server.URL, HTTPClient: server.Client()}, requests
}

func assertCacheLeaseReleased(t *testing.T, cache string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	unlock, err := lockCache(ctx, cache)
	if err != nil {
		t.Fatalf("cache lease leaked: %v", err)
	}
	unlock()
}

func assertNoArchiveTemps(t *testing.T, cache string) {
	t.Helper()
	err := filepath.WalkDir(cache, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(entry.Name(), ".mesh-") || strings.HasPrefix(entry.Name(), ".archive-") {
			t.Errorf("temporary cache file remains: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Observe actual extraction/publication calls to cancel deterministically, without
// production hooks, polling goroutines, or a filesystem race against rename.
type archivePhaseContext struct {
	context.Context
	cancel context.CancelFunc
	phase  string
	root   string
	size   int64
}

func (c archivePhaseContext) Err() error {
	if archivePhaseReached(c.phase, c.root, c.size) {
		c.cancel()
	}
	if c.Context.Err() != nil {
		return context.Canceled
	}
	return nil
}

func archivePhaseReached(phase, root string, size int64) bool {
	files, _ := filepath.Glob(filepath.Join(root, ".mesh-*"))
	if len(files) != 1 {
		return false
	}
	info, err := os.Stat(files[0])
	if err != nil {
		return false
	}
	switch phase {
	case "buffered output":
		return info.Size() > 0 && info.Size() < size && archiveOnStack("archiveContextWriter.Write")
	case "final write":
		return info.Size() == size && archiveOnStack("archiveContextWriter.Write")
	case "trailer":
		return archiveOnStack("validateArchiveTail")
	case "publication":
		return info.Mode().Perm() == 0700 && !archiveOnStack("extractBinary")
	}
	return false
}

func archiveOnStack(function string) bool {
	var pcs [32]uintptr
	count := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:count])
	for {
		frame, more := frames.Next()
		if strings.HasSuffix(frame.Function, "."+function) {
			return true
		}
		if !more {
			return false
		}
	}
}

func TestClientCancellationCleansCacheAndReleasesLease(t *testing.T) {
	for _, format := range []ArchiveFormat{ArchiveGzip, ArchiveBrotli} {
		t.Run(string(format), func(t *testing.T) { testClientCancellationFormat(t, format) })
	}
}

func testClientCancellationFormat(t *testing.T, format ArchiveFormat) {
	t.Helper()
	for _, phase := range []string{"before decode", "buffered output", "final write", "trailer", "publication"} {
		t.Run(phase, func(t *testing.T) { testClientCancellationPhase(t, format, phase) })
	}
}

func testClientCancellationPhase(t *testing.T, format ArchiveFormat, phase string) {
	t.Helper()
	body := bytes.Repeat([]byte("x"), 3*archiveChunk)
	archive := encodeArchive(t, format, meshTar(t, body))
	manifest := testManifestForArchive(body, archive)
	manifest.Artifacts[1].Archive = ArchiveName(manifest.Artifacts[1].Platform, format)
	cache := t.TempDir()
	root, err := cacheRoot(cache, manifest, manifest.Artifacts[1].Platform)
	if err != nil {
		t.Fatal(err)
	}
	client, _ := archiveTestOrigin(t, manifest, archive)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observed := archivePhaseContext{Context: ctx, cancel: cancel, phase: phase, root: root, size: int64(len(body))}
	if phase == "before decode" {
		cancel()
	}
	path, err := client.Download(observed, manifest, manifest.Artifacts[1].Platform, cache)
	if !errors.Is(err, context.Canceled) || path != "" {
		t.Fatalf("%s: path %q, error %v", phase, path, err)
	}
	assertCacheLeaseReleased(t, cache)
	assertNoArchiveTemps(t, cache)
	if _, err := os.Stat(filepath.Join(root, "mesh")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled executable published: %v", err)
	}
	// The same real cache remains usable once cancellation is removed.
	if _, err := client.Download(t.Context(), manifest, manifest.Artifacts[1].Platform, cache); err != nil {
		t.Fatal(err)
	}
	assertCacheLeaseReleased(t, cache)
}

func TestClientPublicationFailureRemovesTemporaryAndReleasesLease(t *testing.T) {
	body := []byte("executable")
	archive := encodeArchive(t, ArchiveBrotli, meshTar(t, body))
	manifest := testManifestForArchive(body, archive)
	platform := manifest.Artifacts[1].Platform
	manifest.Artifacts[1].Archive = ArchiveName(platform, ArchiveBrotli)
	cache := t.TempDir()
	root, err := cacheRoot(cache, manifest, platform)
	if err != nil {
		t.Fatal(err)
	}
	// A pre-existing directory makes the actual atomic publication fail.
	destination := filepath.Join(root, "mesh")
	if err := os.MkdirAll(destination, 0700); err != nil {
		t.Fatal(err)
	}
	client, _ := archiveTestOrigin(t, manifest, archive)
	if _, err := client.Download(t.Context(), manifest, platform, cache); err == nil || !strings.Contains(err.Error(), "publish cached executable") {
		t.Fatalf("rename failure: %v", err)
	}
	assertCacheLeaseReleased(t, cache)
	assertNoArchiveTemps(t, cache)
	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Download(t.Context(), manifest, platform, cache); err != nil {
		t.Fatal(err)
	}
}

func TestClientRecoversPersistedGzipManifestWithoutDigestChange(t *testing.T) {
	body := []byte("persisted gzip executable")
	archive := encodeArchive(t, ArchiveGzip, meshTar(t, body))
	manifest := testManifestForArchive(body, archive)
	before := manifest.Digest()
	root := t.TempDir()
	record := filepath.Join(root, "retained-manifest.json")
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(record, data, 0600); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(record) //nolint:gosec // test-owned persisted manifest
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := decodeManifest(contents)
	if err != nil || recovered.Digest() != before {
		t.Fatalf("persisted digest changed: %v", err)
	}
	client, _ := archiveTestOrigin(t, recovered, archive)
	cache := filepath.Join(root, "cache")
	path, err := client.Download(t.Context(), recovered, recovered.Artifacts[1].Platform, cache)
	if err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(cache, before, "linux_amd64", "mesh")
	if path != expected {
		t.Fatalf("recovery changed content-addressed cache key: %q", path)
	}
	after, err := os.ReadFile(record) //nolint:gosec // test-owned persisted manifest
	if err != nil || !bytes.Equal(after, data) {
		t.Fatalf("recovery rewrote retained record: %v", err)
	}
	assertCacheLeaseReleased(t, cache)
}
