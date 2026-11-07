package bootstrap

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalArchiveRequiresExactLogicalMesh(t *testing.T) {
	for _, name := range []string{"dir/mesh", "directory-entry"} {
		t.Run(name, func(t *testing.T) { testStrictLocalArchive(t, name) })
	}
}

func testStrictLocalArchive(t *testing.T, name string) {
	t.Helper()
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	tw := tar.NewWriter(gz)
	if name == "directory-entry" {
		if err := tw.WriteHeader(&tar.Header{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0700}); err != nil {
			t.Fatal(err)
		}
		name = "mesh"
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0700, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := os.WriteFile(archive, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	path, cleanup, err := extractReleaseArchive(t.Context(), archive)
	t.Cleanup(cleanup)
	if err == nil {
		t.Fatalf("accepted unsafe local archive at %q", path)
	}
}

func TestLocalArchiveValidatesTrailerAndCancellation(t *testing.T) {
	binary := []byte("local executable")
	valid := releaseArchive(t, binary)
	bad := bytes.Clone(valid)
	bad[len(bad)-8] ^= 1
	for _, scenario := range []string{"valid", "bad checksum", "bad trailer", "canceled"} {
		t.Run(scenario, func(t *testing.T) { testLocalArchiveValidation(t, scenario, valid, bad) })
	}
}

func testLocalArchiveValidation(t *testing.T, scenario string, valid, bad []byte) {
	t.Helper()
	root := t.TempDir()
	// Own the bootstrap temporary files too, so rejected inputs cannot leak them.
	t.Setenv("TMPDIR", root)
	archive := valid
	if scenario == "bad trailer" {
		archive = bad
	}
	asset := "mesh_linux_arm64.tar.gz"
	path := filepath.Join(root, asset)
	if err := os.WriteFile(path, archive, 0600); err != nil {
		t.Fatal(err)
	}
	checksum := sha256.Sum256(archive)
	if scenario == "bad checksum" {
		checksum = sha256.Sum256([]byte("wrong archive"))
	}
	checksums := filepath.Join(root, "checksums.txt")
	if err := os.WriteFile(checksums, fmt.Appendf(nil, "%x  %s\n", checksum, asset), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if scenario == "canceled" {
		cancel()
	}
	binary, cleanup, err := extractLocalReleaseBinary(ctx, path, checksums, asset)
	t.Cleanup(cleanup)
	if scenario == "valid" {
		if err != nil {
			t.Fatal(err)
		}
		cleanup()
	} else if err == nil || binary != "" {
		t.Fatalf("invalid local archive: %q %v", binary, err)
	}
	if scenario == "canceled" && !errors.Is(err, context.Canceled) {
		t.Fatalf("local cancellation: %v", err)
	}
	remains, err := filepath.Glob(filepath.Join(root, "mesh-bootstrap-binary-*"))
	if err != nil || len(remains) != 0 {
		t.Fatalf("local extraction leaked files: %v %v", remains, err)
	}
}
