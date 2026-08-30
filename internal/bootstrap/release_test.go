package bootstrap

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFetchReleaseBinaryVerifiesChecksumBeforeExtraction(t *testing.T) {
	t.Parallel()

	archive := releaseArchive(t, []byte("mesh-binary"))
	assetName := "mesh_linux_arm64.tar.gz"
	checksum := sha256.Sum256(archive)
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/download/v1.2.3/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%x  %s\n", checksum, assetName)
	})
	mux.HandleFunc("/releases/download/v1.2.3/"+assetName, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	})
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)

	path, cleanup, err := fetchReleaseBinary(context.Background(), Platform{OS: Linux, Arch: ARM64}, releaseOptions{
		baseURL:    server.URL + "/releases",
		version:    "v1.2.3",
		httpClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("fetchReleaseBinary() error = %v", err)
	}
	defer cleanup()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "mesh-binary" {
		t.Fatalf("fetched binary = %q", contents)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("fetched binary mode = %v, want 0700", info.Mode().Perm())
	}
}

func TestFetchReleaseBinaryRejectsChecksumMismatch(t *testing.T) {
	t.Parallel()

	archive := releaseArchive(t, []byte("mesh-binary"))
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest/download/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%064x  mesh_linux_amd64.tar.gz\n", 1)
	})
	mux.HandleFunc("/releases/latest/download/mesh_linux_amd64.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	})
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)

	_, _, err := fetchReleaseBinary(context.Background(), Platform{OS: Linux, Arch: AMD64}, releaseOptions{
		baseURL:    server.URL + "/releases",
		version:    "latest",
		httpClient: server.Client(),
	})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("checksum")) {
		t.Fatalf("fetchReleaseBinary() error = %v", err)
	}
}

func releaseArchive(t *testing.T, binary []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: filepath.Join("mesh", "mesh"), Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
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
