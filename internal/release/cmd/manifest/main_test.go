package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaul/mesh/internal/release"
)

func TestGenerateBuildsValidatedManifestFromFinalArchives(t *testing.T) {
	root := t.TempDir()
	dist := filepath.Join(root, "dist")
	if err := os.Mkdir(dist, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestArchives(t, dist)
	compatibility := release.Compatibility{
		StateReadMin: 7, StateReadMax: 7, StateWrite: 7,
		WorkerMin: 1, WorkerMax: 1, WorkerWrite: 1, JournalVersion: 1,
	}
	compatibilityPath := filepath.Join(root, "compatibility.json")
	writeJSON(t, compatibilityPath, compatibility)
	output := filepath.Join(root, "mesh-release.json")
	if err := generate(options{
		dist: dist, version: "v0.2.0", commit: strings.Repeat("a", 40),
		compatibility: compatibilityPath, output: output,
	}); err != nil {
		t.Fatalf("generate() error = %v", err)
	}
	var manifest release.Manifest
	readJSON(t, output, &manifest)
	if err := manifest.Validate(); err != nil {
		t.Fatalf("generated manifest is invalid: %v", err)
	}
	if len(manifest.Artifacts) != 3 || manifest.Artifacts[1].BinarySHA256 != digest([]byte("linux/amd64")) {
		t.Fatalf("generated artifacts = %+v", manifest.Artifacts)
	}
}

func TestGenerateRequiresContentAddressedPassingTransitionProof(t *testing.T) {
	root := t.TempDir()
	dist := filepath.Join(root, "dist")
	proofs := filepath.Join(root, "proofs")
	if err := os.MkdirAll(dist, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(proofs, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestArchives(t, dist)
	from := strings.Repeat("b", 64)
	to := digest([]byte("linux/amd64"))
	proof := receipt{
		Schema: 1, Platform: release.Platform{OS: "linux", Arch: "amd64"},
		FromDigest: from, ToDigest: to,
		StateReadMin: 5, StateReadMax: 7, StateWrite: 7,
		WorkerMin: 1, WorkerMax: 1, WorkerWrite: 1, JournalVersion: 1,
		RetainedOpenedCandidateState: true, SessionsPreserved: true, RecoveryRecordsPreserved: true,
	}
	proofContents, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	proofDigest := digest(proofContents)
	proofPath := filepath.Join(proofs, proofDigest+".json")
	if err := os.WriteFile(proofPath, proofContents, 0o600); err != nil {
		t.Fatal(err)
	}
	compatibility := release.Compatibility{
		StateReadMin: 5, StateReadMax: 7, StateWrite: 7,
		WorkerMin: 1, WorkerMax: 1, WorkerWrite: 1, JournalVersion: 1,
		Transitions: []release.Transition{{
			FromDigest: from, ToDigest: to, Platform: proof.Platform, Proof: proofDigest,
		}},
	}
	compatibilityPath := filepath.Join(root, "compatibility.json")
	writeJSON(t, compatibilityPath, compatibility)
	config := options{
		dist: dist, version: "v0.2.0", commit: strings.Repeat("a", 40),
		compatibility: compatibilityPath, proofs: proofs, output: filepath.Join(root, "manifest.json"),
	}
	if err := generate(config); err != nil {
		t.Fatalf("generate() error = %v", err)
	}
	proof.SessionsPreserved = false
	writeJSON(t, proofPath, proof)
	if err := generate(config); err == nil {
		t.Fatal("generate() accepted a changed transition proof")
	}
}

func writeTestArchives(t *testing.T, directory string) {
	t.Helper()
	for _, platform := range []release.Platform{{OS: "darwin", Arch: "arm64"}, {OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}} {
		name := "mesh_" + platform.OS + "_" + platform.Arch + ".tar.gz"
		if err := os.WriteFile(filepath.Join(directory, name), archive(t, []byte(platform.String())), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func archive(t *testing.T, binary []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "mesh", Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(binary); err != nil {
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

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	contents, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readJSON(t *testing.T, path string, value any) {
	t.Helper()
	contents, err := os.ReadFile(path) //nolint:gosec // test path is under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(contents, value); err != nil {
		t.Fatal(err)
	}
}

func digest(contents []byte) string {
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:])
}
