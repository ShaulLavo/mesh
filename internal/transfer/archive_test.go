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

func TestReadonlyDirectoryAndExistingDirectoryParent(t *testing.T) {
	source := t.TempDir()
	readonly := filepath.Join(source, "readonly")
	if err := os.Mkdir(readonly, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(readonly, "index0.txt"), []byte("readonly contents"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readonly, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readonly, 0700) })
	parent := t.TempDir()
	destination := filepath.Join(parent, "destination")
	if err := os.Mkdir(destination, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(parent, 0700)
		_ = os.Chmod(filepath.Join(destination, filepath.Base(source), "readonly"), 0700)
	})
	info := receivePacked(t, source, destination)
	target := filepath.Join(destination, info.Name)
	if data, err := os.ReadFile(filepath.Join(target, "readonly", "index0.txt")); err != nil || string(data) != "readonly contents" {
		t.Fatalf("readonly copy = %q, %v", data, err)
	}
	if err := os.WriteFile(filepath.Join(target, "preserved"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(target, "readonly"), 0700); err != nil {
		t.Fatal(err)
	}
	receivePacked(t, source, destination)
	if _, err := os.Stat(filepath.Join(target, "preserved")); err != nil {
		t.Fatal(err)
	}
	assertNoTransferTemporaries(t, destination)
}

func TestPackReceiveLargeBinary(t *testing.T) {
	root := t.TempDir()
	source, err := os.Create(filepath.Join(root, "binary"))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	buffer := make([]byte, 64*1024)
	state := uint32(1)
	for range 1040 {
		for index := range buffer {
			state ^= state << 13
			state ^= state >> 17
			state ^= state << 5
			buffer[index] = byte(state)
		}
		if _, err := io.MultiWriter(source, hash).Write(buffer); err != nil {
			t.Fatal(err)
		}
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	archive, err := os.CreateTemp(root, "large-archive-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archive.Close() }()
	info, err := Pack(context.Background(), source.Name(), archive)
	if err != nil {
		t.Fatal(err)
	}
	archiveInfo, err := archive.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if archiveInfo.Size() <= 64*1024*1024 {
		t.Fatalf("large binary compressed to %d bytes; fixture must exercise archives above 64 MiB", archiveInfo.Size())
	}
	destination := filepath.Join(root, "received")
	if err := Receive(context.Background(), archive, info, destination); err != nil {
		t.Fatal(err)
	}
	result, err := os.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = result.Close() }()
	resultHash := sha256.New()
	if _, err := io.Copy(resultHash, result); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hash.Sum(nil), resultHash.Sum(nil)) {
		t.Fatal("large binary bytes changed")
	}
	assertNoTransferTemporaries(t, root)
}

type cancelAfterChecks struct {
	context.Context
	remaining int
}

func (c *cancelAfterChecks) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

func TestPackAndReceiveCancellationLeavesNoDestination(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.WriteFile(source, bytes.Repeat([]byte("content"), 1024*1024), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := &cancelAfterChecks{Context: context.Background(), remaining: 5}
	if _, err := Pack(ctx, source, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("Pack cancellation = %v", err)
	}
	archive, info := packedFile(t, source)
	defer func() { _ = archive.Close() }()
	ctx = &cancelAfterChecks{Context: context.Background(), remaining: 8}
	destination := filepath.Join(root, "destination")
	if err := Receive(ctx, archive, info, destination); !errors.Is(err, context.Canceled) {
		t.Fatalf("Receive cancellation = %v", err)
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancellation published destination: %v", err)
	}
	assertNoTransferTemporaries(t, root)
}

func TestReceiveRejectsMalformedArchivesBeforeChangingDestination(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", `a\escape`, "nested/../escape", "ordinary"} {
		t.Run(strings.ReplaceAll(name, "/", "_"), func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "target")
			if err := os.Mkdir(target, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(target, "preserved"), []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			kind := byte(tar.TypeReg)
			if name == "ordinary" {
				kind = tar.TypeSymlink
			}
			archive, info := rawArchive(t, []archiveTestEntry{{name: ".", kind: tar.TypeDir}, {name: "okay", kind: tar.TypeReg, data: "valid"}, {name: name, kind: kind, data: "invalid"}})
			defer func() { _ = archive.Close() }()
			if err := Receive(context.Background(), archive, info, root); err == nil {
				t.Fatal("accepted malformed archive")
			}
			if _, err := os.Stat(filepath.Join(target, "okay")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("published partial tree: %v", err)
			}
			assertNoTransferTemporaries(t, root)
		})
	}
}

func TestReceiveRejectsGzipChecksumBeforePublishing(t *testing.T) {
	archive, info := rawArchive(t, []archiveTestEntry{{name: ".", kind: tar.TypeReg, data: "payload"}})
	defer func() { _ = archive.Close() }()
	data, err := os.ReadFile(archive.Name())
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-8] ^= 0xff
	if err := archive.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(data); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	info.Directory = false
	info.Digest = hex.EncodeToString(digest[:])
	root := t.TempDir()
	if err := Receive(context.Background(), archive, info, filepath.Join(root, "target")); err == nil {
		t.Fatal("accepted damaged gzip checksum")
	}
	assertNoTransferTemporaries(t, root)
	if _, err := os.Stat(filepath.Join(root, "target")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published damaged archive: %v", err)
	}
}

type archiveTestEntry struct {
	name string
	kind byte
	data string
}

func rawArchive(t *testing.T, entries []archiveTestEntry) (*os.File, Info) {
	t.Helper()
	var data bytes.Buffer
	compressed := gzip.NewWriter(&data)
	writer := tar.NewWriter(compressed)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Typeflag: entry.kind, Mode: 0750}
		if entry.kind == tar.TypeReg {
			header.Size = int64(len(entry.data))
		}
		if entry.kind == tar.TypeSymlink {
			header.Linkname = "../outside"
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if entry.kind == tar.TypeReg {
			if _, err := writer.Write([]byte(entry.data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(t.TempDir(), "archive-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data.Bytes()); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data.Bytes())
	return file, Info{Name: "target", Directory: true, Digest: hex.EncodeToString(digest[:])}
}

func assertNoTransferTemporaries(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".mesh-copy-") {
			t.Fatalf("transfer temporary remains: %s", entry.Name())
		}
	}
}

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
}
