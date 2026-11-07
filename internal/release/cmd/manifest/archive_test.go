package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"github.com/shaul/mesh/internal/release"
	"os"
	"path/filepath"
	"testing"
)

func TestManifestRejectsGzipTerminalError(t *testing.T) {
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "mesh", Mode: 0700, Size: 1}); err != nil {
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
	archive := data.Bytes()
	archive[len(archive)-8] ^= 1
	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := os.WriteFile(path, archive, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := hashArchiveBinary(path); err == nil {
		t.Fatal("manifest accepted a corrupted gzip CRC after tar EOF")
	}
}

func TestManifestReaderStagePublishesOnlyGzip(t *testing.T) {
	directory := t.TempDir()
	writeTestArchives(t, directory)
	artifacts, err := readArtifacts(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range artifacts {
		if artifact.Archive != release.ArchiveName(artifact.Platform, release.ArchiveGzip) {
			t.Fatalf("producer cut over during reader stage: %s", artifact.Archive)
		}
	}
}
