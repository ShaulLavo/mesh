package bootstrap

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
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	meshrelease "github.com/shaul/mesh/internal/release"
)

func TestResolvePlatformBinaryRefusesImplicitReleaseFromDevelopmentBuild(t *testing.T) {
	if meshrelease.Version != "" {
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
	manifest := bootstrapTestManifest([]byte("mesh-binary"), archive, meshrelease.Platform{OS: "linux", Arch: "arm64"})
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/download/v1.2.3/"+meshrelease.ManifestAssetName, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(manifestJSON)
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
	manifest := bootstrapTestManifest([]byte("mesh-binary"), archive, meshrelease.Platform{OS: "linux", Arch: "amd64"})
	manifest.Artifacts[1].SHA256 = strings.Repeat("0", 64)
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest/download/"+meshrelease.ManifestAssetName, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(manifestJSON)
	})
	mux.HandleFunc("/releases/download/v1.2.3/mesh_linux_amd64.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	})
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)

	_, _, err = fetchReleaseBinary(context.Background(), Platform{OS: Linux, Arch: AMD64}, releaseOptions{
		baseURL:    server.URL + "/releases",
		version:    "latest",
		httpClient: server.Client(),
	})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("SHA-256")) {
		t.Fatalf("fetchReleaseBinary() error = %v", err)
	}
}

func releaseArchive(t *testing.T, binary []byte) []byte {
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

func bootstrapTestManifest(binary, archive []byte, selected meshrelease.Platform) meshrelease.Manifest {
	platforms := []meshrelease.Platform{{OS: "darwin", Arch: "arm64"}, {OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}}
	artifacts := make([]meshrelease.Artifact, len(platforms))
	for index, platform := range platforms {
		archiveDigest := strings.Repeat(string(rune('1'+index)), 64)
		binaryDigest := strings.Repeat(string(rune('4'+index)), 64)
		if platform == selected {
			archiveDigest = digestHex(archive)
			binaryDigest = digestHex(binary)
		}
		artifacts[index] = meshrelease.Artifact{
			Platform: platform, Archive: "mesh_" + platform.OS + "_" + platform.Arch + ".tar.gz",
			SHA256: archiveDigest, BinarySHA256: binaryDigest,
		}
	}
	return meshrelease.Manifest{
		Schema: meshrelease.ManifestSchema, Version: "v1.2.3", Commit: strings.Repeat("a", 40), Artifacts: artifacts,
		Compatibility: meshrelease.Compatibility{
			StateReadMin: 7, StateReadMax: 7, StateWrite: 7,
			WorkerMin: 1, WorkerMax: 1, WorkerWrite: 1, JournalVersion: 1,
		},
	}
}

func digestHex(contents []byte) string {
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}
