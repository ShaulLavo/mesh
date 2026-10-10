package apps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func maliciousArchive(t *testing.T, header *tar.Header, data []byte) (string, string) {
	t.Helper()
	var packed bytes.Buffer
	zipper := gzip.NewWriter(&packed)
	writer := tar.NewWriter(zipper)
	if err := writer.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipper.Close(); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "upload.tar.gz")
	if err := os.WriteFile(filename, packed.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return filename, digestBytes(packed.Bytes())
}

func TestUnpackRefusesTraversalCredentialsAndLinks(t *testing.T) {
	cases := []struct {
		name string
		kind byte
		link string
	}{{"../escape", tar.TypeReg, ""}, {"/absolute", tar.TypeReg, ""}, {"a/../../escape", tar.TypeReg, ""}, {`a\escape`, tar.TypeReg, ""}, {".env", tar.TypeReg, ""}, {"nested/.env.local", tar.TypeReg, ""}, {".git/config", tar.TypeReg, ""}, {"private.pem", tar.TypeReg, ""}, {"id_ed25519", tar.TypeReg, ""}, {"credentials.json", tar.TypeReg, ""}, {"escape", tar.TypeSymlink, "../../outside"}, {"escape", tar.TypeLink, "outside"}, {"fifo", tar.TypeFifo, ""}}
	for _, test := range cases {
		t.Run(strings.ReplaceAll(test.name, "/", "_"), func(t *testing.T) {
			root := t.TempDir()
			outside := filepath.Join(root, "outside")
			if err := os.WriteFile(outside, []byte("preserve"), 0600); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(root, "destination")
			if err := os.Mkdir(dest, 0700); err != nil {
				t.Fatal(err)
			}
			header := &tar.Header{Name: test.name, Typeflag: test.kind, Mode: 0600, Linkname: test.link}
			var data []byte
			if test.kind == tar.TypeReg {
				data = []byte("attack")
				header.Size = int64(len(data))
			}
			archive, digest := maliciousArchive(t, header, data)
			if err := unpack(archive, dest, digest, 0); err == nil {
				t.Fatalf("accepted unsafe entry %#v", header)
			}
			original, err := os.ReadFile(outside) //nolint:gosec // Test reads its own sentinel to prove archive traversal did not overwrite it.
			if err != nil || string(original) != "preserve" {
				t.Fatal("archive touched sibling data")
			}
		})
	}
}

func TestPackLeavesSourcePristineAndDropsGeneratedOrSecretInputs(t *testing.T) {
	source := t.TempDir()
	files := map[string]string{"index.html": "hello", "nested/code.js": "export default 1", ".env": "SECRET=value", ".git/config": "credential", "node_modules/dependency.js": "dependency", ".venv/config": "generated"}
	for name, data := range files {
		target := filepath.Join(source, name)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var archive bytes.Buffer
	digest, err := Pack(context.Background(), source, &archive, 0)
	if err != nil {
		t.Fatal(err)
	}
	upload := filepath.Join(t.TempDir(), "upload.tar.gz")
	if err := os.WriteFile(upload, archive.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := unpack(upload, dest, digest, 0); err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		actual, err := os.ReadFile(filepath.Join(source, name)) //nolint:gosec // Read the test-owned source to prove packing left it unchanged.
		if err != nil || string(actual) != data {
			t.Fatalf("modified source %s", name)
		}
		info, err := os.Stat(filepath.Join(source, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("changed source permissions %s", name)
		}
	}
	for _, name := range []string{"index.html", "nested/code.js"} {
		if _, err := os.Stat(filepath.Join(dest, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{".env", ".git", "node_modules", ".venv"} {
		if _, err := os.Stat(filepath.Join(dest, name)); !os.IsNotExist(err) {
			t.Fatalf("packed excluded %s: %v", name, err)
		}
	}
}

func TestPackRefusesCredentialsAndSymlinks(t *testing.T) {
	for _, name := range []string{"private.key", "credentials", "id_rsa", "private.pem"} {
		t.Run(name, func(t *testing.T) {
			source := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, name), []byte("secret"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Pack(context.Background(), source, io.Discard, 0); err == nil {
				t.Fatal("silently selected unsafe source")
			}
		})
	}
	source := t.TempDir()
	outside := filepath.Join(t.TempDir(), "external")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(source, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := Pack(context.Background(), source, io.Discard, 0); err == nil {
		t.Fatal("followed source symlink")
	}
}

func TestUnpackRejectsDigestMismatchAndDuplicateEntries(t *testing.T) {
	header := &tar.Header{Name: "index.html", Typeflag: tar.TypeReg, Mode: 0600, Size: 2}
	archive, _ := maliciousArchive(t, header, []byte("ok"))
	if err := unpack(archive, t.TempDir(), strings.Repeat("0", 64), 0); err == nil {
		t.Fatal("accepted altered upload")
	}
	var packed bytes.Buffer
	zipper := gzip.NewWriter(&packed)
	writer := tar.NewWriter(zipper)
	for range 2 {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte("ok")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipper.Close(); err != nil {
		t.Fatal(err)
	}
	archive = filepath.Join(t.TempDir(), "duplicate.tar.gz")
	if err := os.WriteFile(archive, packed.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := unpack(archive, t.TempDir(), digestBytes(packed.Bytes()), 0); err == nil {
		t.Fatal("silently overwrote duplicate archive entry")
	}
}
