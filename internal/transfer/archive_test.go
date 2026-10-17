package transfer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func packedFile(t *testing.T, source string) (*os.File, Info) {
	t.Helper()
	var data bytes.Buffer
	info, err := Pack(context.Background(), source, &data)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := os.CreateTemp(t.TempDir(), "archive-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(data.Bytes()); err != nil {
		t.Fatal(err)
	}
	info.Digest = info.Digest
	return archive, info
}

func receivePacked(t *testing.T, source, destination string) Info {
	t.Helper()
	archive, info := packedFile(t, source)
	defer func() { _ = archive.Close() }()
	if err := Receive(context.Background(), archive, info, destination); err != nil {
		t.Fatal(err)
	}
	return info
}

func TestPackReceiveDirectoryPreservesFilesModesAndMerge(t *testing.T) {
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "empty"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".git", "config"), []byte("git"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "payload"), []byte("payload"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "run"), []byte("run"), 0751); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	info := receivePacked(t, source, destination)
	target := filepath.Join(destination, info.Name)
	for _, name := range []string{"empty", ".git/config", "payload"} {
		if _, err := os.Stat(filepath.Join(target, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	runInfo, err := os.Stat(filepath.Join(target, "run"))
	if err != nil || runInfo.Mode().Perm() != 0751 {
		t.Fatalf("run mode = %v, %v", runInfo.Mode().Perm(), err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	receivePacked(t, source, destination)
	if _, err := os.Stat(filepath.Join(target, "keep")); err != nil {
		t.Fatalf("merge removed existing file: %v", err)
	}
}

func TestReceiveFileUsesCpDestinationRulesAndAtomicOverwrite(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "value.txt")
	if err := os.WriteFile(source, []byte("new"), 0750); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "destination")
	if err := os.Mkdir(destination, 0700); err != nil {
		t.Fatal(err)
	}
	receivePacked(t, source, destination)
	if data, err := os.ReadFile(filepath.Join(destination, "value.txt")); err != nil || string(data) != "new" {
		t.Fatalf("destination file = %q, %v", data, err)
	}
	if err := os.WriteFile(filepath.Join(destination, "value.txt"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	receivePacked(t, source, destination)
	if data, err := os.ReadFile(filepath.Join(destination, "value.txt")); err != nil || string(data) != "new" {
		t.Fatalf("overwritten file = %q, %v", data, err)
	}
	exact := filepath.Join(root, "renamed.txt")
	receivePacked(t, source, exact)
	if _, err := os.Stat(exact); err != nil {
		t.Fatal(err)
	}
}

func TestReceiveRejectsDigestTraversalAndDestinationSymlinks(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "file"), []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	archive, info := packedFile(t, source)
	defer func() { _ = archive.Close() }()
	info.Digest = strings.Repeat("0", sha256.Size*2)
	destination := filepath.Join(t.TempDir(), "destination")
	if err := Receive(context.Background(), archive, info, destination); err == nil {
		t.Fatal("accepted digest mismatch")
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created destination after digest failure: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	info = receivePacked(t, source, t.TempDir())
	archive, info = packedFile(t, source)
	defer func() { _ = archive.Close() }()
	if err := Receive(context.Background(), archive, info, link); err == nil {
		t.Fatal("followed destination symlink")
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "outside" {
		t.Fatalf("symlink target changed: %q, %v", data, err)
	}
}

func TestPackRejectsSymlinkAndSpecialSource(t *testing.T) {
	source := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := Pack(context.Background(), source, io.Discard); err == nil {
		t.Fatal("followed source symlink")
	}
}

func TestTemporaryIsUnlinked(t *testing.T) {
	destination := t.TempDir()
	file, err := Temporary(destination)
	if err != nil {
		t.Fatal(err)
	}
	name := file.Name()
	if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file still named: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveDigestIsCompressedBytes(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "file"), []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	var packed bytes.Buffer
	info, err := Pack(context.Background(), source, &packed)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(packed.Bytes())
	if info.Digest != hex.EncodeToString(hash[:]) {
		t.Fatalf("digest = %s, want compressed bytes digest", info.Digest)
	}
	gzipReader, err := gzip.NewReader(bytes.NewReader(packed.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, gzipReader)
	if err != nil {
		t.Fatal(err)
	}
	_ = gzipReader.Close()
	tarReader := tar.NewReader(bytes.NewReader(nil))
	_ = tarReader
}
