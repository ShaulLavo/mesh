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
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResolvePlatformBinaryRefusesImplicitReleaseFromDevelopmentBuild(t *testing.T) {
	if releaseVersion != "" {
		t.Skip("test requires an unstamped development build")
	}

	remoteOS := Linux
	if runtime.GOOS == "darwin" {
		remoteOS = Darwin
	}
	remoteArch := AMD64
	if runtime.GOARCH == "amd64" {
		remoteArch = ARM64
	}
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.NotFound(w, nil)
	}))
	t.Cleanup(server.Close)

	_, err := resolvePlatformBinary(context.Background(), binarySelection{
		artifactDir: t.TempDir(),
		baseURL:     server.URL + "/releases",
		httpClient:  server.Client(),
	}, Platform{OS: remoteOS, Arch: remoteArch})
	assertDiagnosticCode(t, err, DiagnosticWrongArch)
	if err == nil || !strings.Contains(err.Error(), "unversioned development build") {
		t.Fatalf("development-build fallback error = %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("development-build fallback made %d release requests, want 0", got)
	}
}

func TestResolvePlatformBinaryStillUsesMatchingDevelopmentExecutable(t *testing.T) {
	var localOS OS
	switch runtime.GOOS {
	case "linux":
		localOS = Linux
	case "darwin":
		localOS = Darwin
	default:
		t.Skip("test executable is not on a supported bootstrap OS")
	}
	var localArch Arch
	switch runtime.GOARCH {
	case "amd64":
		localArch = AMD64
	case "arm64":
		localArch = ARM64
	default:
		t.Skip("test executable is not on a supported bootstrap architecture")
	}

	binary, err := resolvePlatformBinary(context.Background(), binarySelection{}, Platform{OS: localOS, Arch: localArch})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(binary.cleanup)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if binary.path != executable {
		t.Fatalf("selected binary = %q, want running executable %q", binary.path, executable)
	}
}

func TestFetchReleaseBinaryVerifiesChecksumBeforeExtraction(t *testing.T) {
	t.Parallel()

	archive := releaseArchive(t, []byte("mesh-binary"))
	assetName := "mesh_linux_arm64.tar.gz"
	checksum := sha256.Sum256(archive)
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/download/v1.2.3/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "%x  %s\n", checksum, assetName)
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
	contents, err := os.ReadFile(path) //nolint:gosec // path is the temporary binary returned by the extractor under test
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
		_, _ = fmt.Fprintf(w, "%064x  mesh_linux_amd64.tar.gz\n", 1)
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
