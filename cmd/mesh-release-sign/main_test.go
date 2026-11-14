package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFailedSigningNeverWritesEnvelope(t *testing.T) {
	root := t.TempDir()
	bundle, subject, output := filepath.Join(root, "bundle"), filepath.Join(root, "subject"), filepath.Join(root, "output")
	for path, contents := range map[string]string{bundle: `{"mediaType":"unsupported"}`, subject: "artifact", output: "prior verified output"} {
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := run([]string{"wrap", bundle, output, subject}); err == nil {
		t.Fatal("accepted unsupported bundle")
	}
	contents, err := os.ReadFile(output) //nolint:gosec // The test reads its own temporary output.
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "prior verified output" {
		t.Fatal("failed signing replaced output")
	}
	if err := os.Remove(output); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"wrap", bundle, output, subject}); err == nil {
		t.Fatal("accepted unsupported bundle")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("failed signing created output: %v", err)
	}
}

func TestReleaseSignerRejectsMissingFilesAndArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"wrap"}, {"verify"}, {"metadata"}, {"verify-metadata"}, {"refresh"}, {"metadata", "missing"}, {"verify", "missing", "missing"}} {
		if err := run(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestVerifyMetadataManifestByteBounds(t *testing.T) {
	for _, size := range []int{(64 << 10) + 1, 512 << 10, (512 << 10) + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) { checkManifestByteBound(t, size) })
	}
}

func checkManifestByteBound(t *testing.T, size int) {
	t.Helper()
	t.Setenv("MESH_RELEASE_TRUST_CACHE", "")
	repository, err := os.OpenRoot("../..")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repository.Close() }()
	manifest, err := repository.ReadFile("internal/update/testdata/discovery/v0.1.306/mesh-release.json")
	if err != nil {
		t.Fatal(err)
	}
	manifest = append(manifest, []byte(strings.Repeat(" ", size-len(manifest)))...)
	directory := t.TempDir()
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for name, data := range map[string][]byte{"metadata": []byte("{}"), "envelope": []byte("{}"), "manifest": manifest, "page": []byte("{}")} {
		if err := root.WriteFile(name, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	err = verifyMetadata([]string{filepath.Join(directory, "metadata"), filepath.Join(directory, "envelope"), filepath.Join(directory, "manifest"), filepath.Join(directory, "page")})
	if size <= 512<<10 && (err == nil || !strings.Contains(err.Error(), "verify release metadata")) {
		t.Fatalf("valid-sized manifest did not reach signature verification: %v", err)
	}
	if size > 512<<10 && (err == nil || !strings.Contains(err.Error(), "exceeds 524288")) {
		t.Fatalf("oversized manifest was accepted or decoded: %v", err)
	}
}
