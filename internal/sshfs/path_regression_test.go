package sshfs_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestSFTPKeepsPercentNamesLiteral(t *testing.T) {
	f := newFixture(t)
	files := map[string]string{
		"report%20final.txt": "literal percent file",
		"report final.txt":   "different space file",
		"100%.txt":           "plain percent file",
		"%2e%2e":             "literal encoded parent",
		"value%00.txt":       "literal encoded null",
		"nested%2fvalue.txt": "literal encoded separator",
	}
	for name, contents := range files {
		writeFile(t, filepath.Join(f.root, name), contents)
	}
	t.Run("listing", func(t *testing.T) {
		assertNames(t, f.client, "/files", "%2e%2e", "100%.txt", "nested", "nested%2fvalue.txt", "report final.txt", "report%20final.txt", "value%00.txt")
	})
	for name, contents := range files {
		t.Run(name, func(t *testing.T) { assertLiteralName(t, f, name, contents) })
	}
}

func assertLiteralName(t *testing.T, f fixture, name, contents string) {
	t.Helper()
	remote := "/files/" + name
	assertRead(t, f.client, remote, contents)
	for _, stat := range []func(string) (fs.FileInfo, error){f.client.Stat, f.client.Lstat} {
		info, err := stat(remote)
		if err != nil || info.Size() != int64(len(contents)) {
			t.Fatalf("metadata for %q = %v, %v; want size %d", remote, info, err, len(contents))
		}
	}
	canonical, err := f.client.RealPath(remote)
	if err != nil || canonical != remote {
		t.Fatalf("RealPath(%q) = %q, %v", remote, canonical, err)
	}
	link := name + "-link"
	if err := os.Symlink(name, filepath.Join(f.root, link)); err != nil {
		t.Fatal(err)
	}
	canonical, err = f.client.ReadLink("/files/" + link)
	if err != nil || canonical != remote {
		t.Fatalf("ReadLink(%q) = %q, %v; want %q", link, canonical, err, remote)
	}
}

func TestSFTPReadlinkResolvesParentAfterIntermediateSymlink(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(filepath.Join(f.root, "directory", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.root, "directory", "value"), "actual target")
	writeFile(t, filepath.Join(f.root, "value"), "different target")
	if err := os.Symlink("directory/nested", filepath.Join(f.root, "shortcut")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("shortcut/../value", filepath.Join(f.root, "link")); err != nil {
		t.Fatal(err)
	}
	assertRead(t, f.client, "/files/link", "actual target")
	for _, resolve := range []func(string) (string, error){f.client.RealPath, f.client.ReadLink} {
		canonical, err := resolve("/files/link")
		if err != nil || canonical != "/files/directory/value" {
			t.Fatalf("canonical target = %q, %v; want /files/directory/value", canonical, err)
		}
	}
}

func TestSFTPMetadataRejectsOutsideTargetAfterIntermediateSymlink(t *testing.T) {
	f := newFixture(t)
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(outside, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(outside, "value"), "outside target")
	writeFile(t, filepath.Join(f.root, "value"), "inside decoy")
	if err := os.Symlink(filepath.Join(outside, "nested"), filepath.Join(f.root, "shortcut")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("shortcut/../value", filepath.Join(f.root, "link")); err != nil {
		t.Fatal(err)
	}
	for name, operation := range readOperations(f.client) {
		t.Run(name, func(t *testing.T) { assertPermission(t, operation("/files/link")) })
	}
	assertNames(t, f.client, "/files", "nested", "value")
}
