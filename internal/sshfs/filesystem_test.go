package sshfs

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/shaul/mesh/internal/serve"
)

func TestNestedMountIntermediateOverridesMissingFilesAndUnsafeLinks(t *testing.T) {
	for _, kind := range []string{"absent", "file", "directory", "outside-link", "inside-link"} {
		t.Run(kind, func(t *testing.T) {
			testNestedMountIntermediate(t, kind)
		})
	}
}

func testNestedMountIntermediate(t *testing.T, kind string) {
	t.Helper()
	ancestor := t.TempDir()
	nested := t.TempDir()
	writeFilesystemFixture(t, filepath.Join(nested, "mounted.txt"), "nested service")
	makeMountIntermediate(t, ancestor, kind)
	filesystem := newFilesystemRegistry(t, []serve.Service{
		{Name: "site", Kind: serve.Static, Target: ancestor},
		{Name: "site/docs/api", Kind: serve.Files, Target: nested},
	})
	rootNames := []string{"docs"}
	if kind == "inside-link" {
		rootNames = []string{".inside", "docs"}
	}
	assertFilesystemNames(t, filesystem, "/site", rootNames...)
	wanted := []string{"api"}
	if kind == "directory" || kind == "inside-link" {
		wanted = append(wanted, "kept.txt")
	}
	assertFilesystemNames(t, filesystem, "/site/docs", wanted...)
	assertFilesystemNames(t, filesystem, "/site/docs/api", "mounted.txt")
	for _, follow := range []bool{false, true} {
		info, err := filesystem.stat("/site/docs", follow)
		if err != nil || !info.IsDir() || info.Name() != "docs" {
			t.Fatalf("stat follow=%v: info %v, error %v", follow, info, err)
		}
	}
	canonical, err := filesystem.RealPath("/site/docs")
	if err != nil || canonical != "/site/docs" {
		t.Fatalf("canonical intermediate = %q, error %v", canonical, err)
	}
	if _, err := filesystem.Readlink("/site/docs"); !errors.Is(err, permissionDenied) {
		t.Fatalf("virtual intermediate readlink = %v", err)
	}
	assertFilesystemContents(t, filesystem, "/site/docs/api/mounted.txt", "nested service")
	if kind != "outside-link" {
		return
	}
	if _, _, err := filesystem.open("/site/docs/secret.txt"); !errors.Is(err, permissionDenied) {
		t.Fatalf("opened hidden outside entry: %v", err)
	}
}

func makeMountIntermediate(t *testing.T, ancestor, kind string) {
	t.Helper()
	docs := filepath.Join(ancestor, "docs")
	switch kind {
	case "absent":
		return
	case "file":
		writeFilesystemFixture(t, docs, "hidden file")
	case "directory":
		makeFilesystemDirectory(t, docs)
		writeFilesystemFixture(t, filepath.Join(docs, "kept.txt"), "kept")
	case "outside-link":
		outside := t.TempDir()
		writeFilesystemFixture(t, filepath.Join(outside, "secret.txt"), "outside")
		makeFilesystemLink(t, outside, docs)
	case "inside-link":
		inside := filepath.Join(ancestor, ".inside")
		makeFilesystemDirectory(t, inside)
		writeFilesystemFixture(t, filepath.Join(inside, "kept.txt"), "kept")
		makeFilesystemLink(t, inside, docs)
	}
}

func TestNestedMountDoesNotHideMissingDeclaredRoot(t *testing.T) {
	ancestor := t.TempDir()
	nested := t.TempDir()
	filesystem := newFilesystemRegistry(t, []serve.Service{
		{Name: "site", Kind: serve.Static, Target: ancestor},
		{Name: "site/docs/api", Kind: serve.Files, Target: nested},
	})
	if err := os.Remove(ancestor); err != nil {
		t.Fatal(err)
	}
	if _, err := filesystem.stat("/site", true); err == nil {
		t.Fatal("missing declared root still has stat metadata")
	}
	if _, err := filesystem.readDir("/site"); err == nil {
		t.Fatal("missing declared root still has a directory listing")
	}
	assertFilesystemNames(t, filesystem, "/site/docs", "api")
}

func TestCanonicalLinksCannotChangeMountedService(t *testing.T) {
	ancestor := t.TempDir()
	nested := t.TempDir()
	makeFilesystemDirectory(t, filepath.Join(ancestor, "docs"))
	writeFilesystemFixture(t, filepath.Join(ancestor, "docs", "file.txt"), "ancestor content")
	writeFilesystemFixture(t, filepath.Join(ancestor, "ordinary.txt"), "ordinary content")
	writeFilesystemFixture(t, filepath.Join(nested, "file.txt"), "nested content")
	makeFilesystemLink(t, "docs/file.txt", filepath.Join(ancestor, "alias"))
	makeFilesystemLink(t, "ordinary.txt", filepath.Join(ancestor, "ordinary-link"))
	filesystem := newFilesystemRegistry(t, []serve.Service{
		{Name: "site", Kind: serve.Static, Target: ancestor},
		{Name: "site/docs", Kind: serve.Files, Target: nested},
	})
	assertFilesystemContents(t, filesystem, "/site/alias", "ancestor content")
	assertFilesystemContents(t, filesystem, "/site/docs/file.txt", "nested content")
	for _, canonical := range []func(string) (string, error){filesystem.RealPath, filesystem.Readlink} {
		if name, err := canonical("/site/alias"); name != "" || !errors.Is(err, permissionDenied) {
			t.Fatalf("overlaid canonical target = %q, error %v", name, err)
		}
		if name, err := canonical("/site/ordinary-link"); name != "/site/ordinary.txt" || err != nil {
			t.Fatalf("ordinary canonical target = %q, error %v", name, err)
		}
	}
}

func TestCanonicalLinksCannotChangeIntoVirtualIntermediate(t *testing.T) {
	ancestor := t.TempDir()
	makeFilesystemDirectory(t, filepath.Join(ancestor, "docs"))
	makeFilesystemLink(t, "docs", filepath.Join(ancestor, "alias"))
	filesystem := newFilesystemRegistry(t, []serve.Service{
		{Name: "site", Kind: serve.Static, Target: ancestor},
		{Name: "site/docs/api", Kind: serve.Files, Target: t.TempDir()},
	})
	for _, canonical := range []func(string) (string, error){filesystem.RealPath, filesystem.Readlink} {
		if name, err := canonical("/site/alias"); name != "" || !errors.Is(err, permissionDenied) {
			t.Fatalf("virtual intermediate canonical target = %q, error %v", name, err)
		}
	}
}

func newFilesystemRegistry(t *testing.T, services []serve.Service) *Filesystem {
	t.Helper()
	registry, err := serve.NewRegistry(services)
	if err != nil {
		t.Fatal(err)
	}
	return New(registry)
}

func assertFilesystemNames(t *testing.T, filesystem *Filesystem, name string, want ...string) {
	t.Helper()
	entries, err := filesystem.readDir(name)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name()
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("entries under %s = %v, want %v", name, names, want)
	}
}

func assertFilesystemContents(t *testing.T, filesystem *Filesystem, name, want string) {
	t.Helper()
	file, _, err := filesystem.open(name)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(file)
	if err := errors.Join(err, file.Close()); err != nil {
		t.Fatal(err)
	}
	if string(contents) != want {
		t.Fatalf("contents of %s = %q, want %q", name, contents, want)
	}
}

func writeFilesystemFixture(t *testing.T, name, contents string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func makeFilesystemDirectory(t *testing.T, name string) {
	t.Helper()
	if err := os.Mkdir(name, 0o700); err != nil {
		t.Fatal(err)
	}
}

func makeFilesystemLink(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		t.Fatal(err)
	}
}
