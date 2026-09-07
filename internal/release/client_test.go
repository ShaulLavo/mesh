package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestClientResolvesLatestOnceAndDownloadsExactVersion(t *testing.T) {
	binary := []byte("mesh-test-binary")
	archive := testArchive(t, binary)
	manifest := testManifestForArchive(binary, archive)
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var manifestRequests atomic.Int32
	var archiveRequests atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest/download/"+ManifestAssetName, func(writer http.ResponseWriter, _ *http.Request) {
		manifestRequests.Add(1)
		_, _ = writer.Write(manifestJSON)
	})
	mux.HandleFunc("/releases/download/v0.2.0/mesh_linux_amd64.tar.gz", func(writer http.ResponseWriter, _ *http.Request) {
		archiveRequests.Add(1)
		_, _ = writer.Write(archive)
	})
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	client := Client{BaseURL: server.URL + "/releases", HTTPClient: server.Client()}

	resolved, err := client.Manifest(context.Background(), "latest")
	if err != nil {
		t.Fatalf("Manifest() error = %v", err)
	}
	path, err := client.Download(context.Background(), resolved, Platform{OS: "linux", Arch: "amd64"}, t.TempDir())
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	contents, err := os.ReadFile(path) //nolint:gosec // path is returned by Download under a test directory
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(contents, binary) {
		t.Fatalf("downloaded binary = %q, want %q", contents, binary)
	}
	if manifestRequests.Load() != 1 || archiveRequests.Load() != 1 {
		t.Fatalf("requests = manifest %d archive %d, want 1 each", manifestRequests.Load(), archiveRequests.Load())
	}
	second, err := client.Download(context.Background(), resolved, Platform{OS: "linux", Arch: "amd64"}, filepath.Dir(filepath.Dir(filepath.Dir(path))))
	if err != nil {
		t.Fatalf("cached Download() error = %v", err)
	}
	if second != path || archiveRequests.Load() != 1 {
		t.Fatalf("cached download = %q, requests %d; want %q and 1", second, archiveRequests.Load(), path)
	}
}

func TestClientRejectsArchiveAndBinaryDigestMismatches(t *testing.T) {
	binary := []byte("mesh-test-binary")
	archive := testArchive(t, binary)
	manifest := testManifestForArchive(binary, archive)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(archive)
	}))
	t.Cleanup(server.Close)
	client := Client{BaseURL: server.URL, HTTPClient: server.Client()}

	manifest.Artifacts[1].SHA256 = testDigest([]byte("wrong archive"))
	if _, err := client.Download(context.Background(), manifest, Platform{OS: "linux", Arch: "amd64"}, t.TempDir()); err == nil {
		t.Fatal("Download() accepted wrong archive digest")
	}
	manifest = testManifestForArchive(binary, archive)
	manifest.Artifacts[1].BinarySHA256 = testDigest([]byte("wrong binary"))
	if _, err := client.Download(context.Background(), manifest, Platform{OS: "linux", Arch: "amd64"}, t.TempDir()); err == nil {
		t.Fatal("Download() accepted wrong binary digest")
	}
}

func TestClientManifestRejectsUnknownFieldsAndVersionSubstitution(t *testing.T) {
	manifest := testManifest()
	contents, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	contents = append(contents[:len(contents)-1], []byte(`,"surprise":true}`)...)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(contents)
	}))
	t.Cleanup(server.Close)
	client := Client{BaseURL: server.URL, HTTPClient: server.Client()}
	if _, err := client.Manifest(context.Background(), "v0.1.0"); err == nil {
		t.Fatal("Manifest() accepted an unknown field")
	}

	clean, _ := json.Marshal(manifest)
	server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { _, _ = writer.Write(clean) })
	if _, err := client.Manifest(context.Background(), "v0.1.0"); err == nil {
		t.Fatal("Manifest() accepted a substituted version")
	}
}

func testManifestForArchive(binary, archive []byte) Manifest {
	manifest := testManifest()
	for index := range manifest.Artifacts {
		if manifest.Artifacts[index].Platform == (Platform{OS: "linux", Arch: "amd64"}) {
			manifest.Artifacts[index].SHA256 = testDigest(archive)
			manifest.Artifacts[index].BinarySHA256 = testDigest(binary)
		}
	}
	return manifest
}

func testDigest(contents []byte) string {
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}

func testArchive(t *testing.T, binary []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "mesh", Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(tarWriter, bytes.NewReader(binary)); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
