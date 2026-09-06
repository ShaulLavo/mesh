package serve

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestResolveRootConfinesPathsAfterDecodingAndSymlinkResolution(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "nested", "inside.txt")
	outside := filepath.Join(parent, "secret.txt")
	if err := os.WriteFile(inside, []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inside, filepath.Join(root, "inside-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside-link")); err != nil {
		t.Fatal(err)
	}

	for _, requestPath := range []string{"nested/inside.txt", "/nested/inside.txt", "/inside-link"} {
		t.Run("allows "+requestPath, func(t *testing.T) {
			got, err := ResolveRoot(root, requestPath)
			if err != nil {
				t.Fatal(err)
			}
			if got != inside {
				t.Fatalf("resolved path = %q, want %q", got, inside)
			}
		})
	}

	attacks := []string{
		"../secret.txt",
		"/../secret.txt",
		"/nested/../../secret.txt",
		"/%2e%2e/secret.txt",
		"/%2E%2E/secret.txt",
		"/%252e%252e/secret.txt",
		"/%25252e%25252e/secret.txt",
		"/%2e%2e%2fsecret.txt",
		"/%252e%252e%252fsecret.txt",
		"/nested%2f..%2f..%2fsecret.txt",
		"/outside-link",
		"/inside.txt\x00/secret",
		"/inside.txt%00/secret",
		"/inside.txt%2500/secret",
		"/..\\secret.txt",
	}
	for _, requestPath := range attacks {
		t.Run("rejects "+requestPath, func(t *testing.T) {
			resolved, err := ResolveRoot(root, requestPath)
			if err == nil {
				contents, readErr := os.ReadFile(resolved) //nolint:gosec // the test inspects only the resolver result inside its temporary fixture
				t.Fatalf("attack resolved to %q (contents %q, read error %v)", resolved, contents, readErr)
			}
		})
	}
}

func TestResolveRootReportsUnavailableRoot(t *testing.T) {
	for _, root := range []string{"", filepath.Join(t.TempDir(), "gone")} {
		_, err := ResolveRoot(root, "/")
		if !errors.Is(err, ErrRootUnavailable) {
			t.Fatalf("root %q error = %v, want ErrRootUnavailable", root, err)
		}
	}
}

func TestResolveRootBoundsRepeatedDecoding(t *testing.T) {
	encoded := "%2e%2e"
	for range maxPathDecodings {
		encoded = strings.ReplaceAll(encoded, "%", "%25")
	}
	if _, err := ResolveRoot(t.TempDir(), "/"+encoded); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("error = %v, want ErrInvalidPath", err)
	}
}

func TestResolveRootAllowsExactlyMaximumPathDecodings(t *testing.T) {
	root := t.TempDir()
	want := filepath.Join(root, "safe", "file.txt")
	if err := os.Mkdir(filepath.Dir(want), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}

	encoded := "/safe/file.txt"
	for depth := 0; depth <= maxPathDecodings+1; depth++ {
		got, err := ResolveRoot(root, encoded)
		if depth <= maxPathDecodings {
			if err != nil || got != want {
				t.Fatalf("depth %d resolved to %q, %v; want %q", depth, got, err, want)
			}
		} else if !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("depth %d error = %v, want ErrInvalidPath", depth, err)
		}
		if depth == 0 {
			encoded = "/safe%2ffile.txt"
		} else {
			encoded = strings.ReplaceAll(encoded, "%", "%25")
		}
	}
}

func TestOpenRootedPathRejectsRetargetedDirectory(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	served := filepath.Join(root, "served")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(served, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(served, "value.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "value.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveRootPath(root, "/served/value.txt")
	if err != nil {
		t.Fatal(err)
	}
	rootHandle, _, err := openAnchoredRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer rootHandle.Close() //nolint:errcheck // test cleanup
	if err := os.Rename(served, filepath.Join(root, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, served); err != nil {
		t.Fatal(err)
	}
	file, _, err := openRootedPath(rootHandle, resolved.relative, resolved.path)
	if err == nil {
		defer file.Close() //nolint:errcheck // test cleanup
		contents, readErr := io.ReadAll(file)
		t.Fatalf("retargeted path opened %q, read error %v", contents, readErr)
	}
}

func TestOpenRootEntryKeepsAbsoluteInRootSymlinkSupport(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	file, info, err := OpenRootEntry(root, "/link.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close() //nolint:errcheck // test cleanup
	contents, err := io.ReadAll(file)
	if err != nil || info.Name() != "target.txt" || string(contents) != "inside" {
		t.Fatalf("opened link as %q with %q, %v", info.Name(), contents, err)
	}
}

func TestOpenRootEntryDoesNotBlockOnSpecialFile(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		file, _, err := OpenRootEntry(root, "/pipe")
		if file != nil {
			_ = file.Close()
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("special file was accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("special-file open blocked")
	}
}

func TestLiteralRootPathLstatPreservesEntryMetadata(t *testing.T) {
	root := rootMetadataFixture(t)
	cases := []struct {
		path string
		name string
		mode fs.FileMode
	}{
		{path: "/", name: ".", mode: fs.ModeDir},
		{path: "/nested", name: "nested", mode: fs.ModeDir},
		{path: "/nested/value.txt", name: "value.txt"},
		{path: "/relative-link", name: "relative-link", mode: fs.ModeSymlink},
		{path: "/absolute-link", name: "absolute-link", mode: fs.ModeSymlink},
		{path: "/directory-link/value.txt", name: "value.txt"},
		{path: "/directory-link/parent-link", name: "parent-link", mode: fs.ModeSymlink},
	}
	for _, test := range cases {
		info, err := LiteralRootPath(test.path).Lstat(root)
		if err != nil {
			t.Fatalf("Lstat(%q): %v", test.path, err)
		}
		if info.Name() != test.name || info.Mode().Type() != test.mode {
			t.Fatalf("Lstat(%q) = %q, %s; want %q, %s", test.path, info.Name(), info.Mode().Type(), test.name, test.mode)
		}
	}
}

func TestLiteralRootPathReadlinkReturnsConfinedCanonicalTargets(t *testing.T) {
	root := rootMetadataFixture(t)
	cases := map[string]string{
		"/relative-link":              "nested/value.txt",
		"/absolute-link":              "nested/value.txt",
		"/directory-link":             "nested",
		"/nested/parent-link":         "nested/value.txt",
		"/directory-link/parent-link": "nested/value.txt",
		"/root-link":                  ".",
	}
	for requestPath, want := range cases {
		got, err := LiteralRootPath(requestPath).Readlink(root)
		if err != nil || got != want {
			t.Fatalf("Readlink(%q) = %q, %v; want %q", requestPath, got, err, want)
		}
	}
	if _, err := LiteralRootPath("/nested/value.txt").Readlink(root); err == nil {
		t.Fatal("Readlink accepted a regular file")
	}
}

func TestLiteralRootPathResolveRelativeDoesNotExposeHostPaths(t *testing.T) {
	root := rootMetadataFixture(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"/":                 ".",
		"/nested":           "nested",
		"/nested/value.txt": "nested/value.txt",
		"/absolute-link":    "nested/value.txt",
	}
	for requestPath, want := range cases {
		got, err := LiteralRootPath(requestPath).ResolveRelative(alias)
		if err != nil || got != want {
			t.Fatalf("ResolveRelative(%q) = %q, %v; want %q", requestPath, got, err, want)
		}
	}
}

func TestRootMetadataOperationsRejectTraversalAndOutsideLinks(t *testing.T) {
	root := rootMetadataFixture(t)
	operations := map[string]func(string) error{
		"Lstat": func(requestPath string) error {
			_, err := LiteralRootPath(requestPath).Lstat(root)
			return err
		},
		"Readlink": func(requestPath string) error {
			_, err := LiteralRootPath(requestPath).Readlink(root)
			return err
		},
		"Realpath": func(requestPath string) error {
			_, err := LiteralRootPath(requestPath).ResolveRelative(root)
			return err
		},
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			checkRootMetadataTraversal(t, operation)
		})
	}
}

func checkRootMetadataTraversal(t *testing.T, operation func(string) error) {
	t.Helper()
	attacks := []string{
		"../secret.txt", "/../secret.txt", "/nested/../../secret.txt",
		"/%2e%2e/secret.txt", "/%2E%2E/secret.txt", "/%252e%252e/secret.txt",
		"/%25252e%25252e/secret.txt", "/%2e%2e%2fsecret.txt", "/%252e%252e%252fsecret.txt",
		"/nested%2f..%2f..%2fsecret.txt", "/outside-link", "/outside-directory/secret.txt",
		"/outside-directory/link", "/nested/value.txt\x00/secret", "/nested/value.txt%00/secret",
		"/nested/value.txt%2500/secret", "/..\\secret.txt", "/etc/passwd", "/dangling-link",
	}
	for _, requestPath := range attacks {
		if err := operation(requestPath); err == nil {
			t.Errorf("accepted %q", requestPath)
		}
	}
	if err := operation("/outside-link"); !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("outside-link error = %v, want ErrOutsideRoot", err)
	}
}

func TestRootMetadataOperationsReportDeletedRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	_, lstatErr := LiteralRootPath("/").Lstat(root)
	_, readlinkErr := LiteralRootPath("/link").Readlink(root)
	_, realpathErr := LiteralRootPath("/").ResolveRelative(root)
	for _, err := range []error{lstatErr, readlinkErr, realpathErr} {
		if !errors.Is(err, ErrRootUnavailable) || !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("deleted-root error = %v, want ErrRootUnavailable and fs.ErrNotExist", err)
		}
	}
}

func TestRootMetadataRejectsRetargetedParent(t *testing.T) {
	root := rootMetadataFixture(t)
	rootHandle, rootPath, relative, err := openLiteralRootEntryParent(root, "/nested/parent-link")
	if err != nil {
		t.Fatal(err)
	}
	defer rootHandle.Close() //nolint:errcheck // test cleanup
	if err := os.Rename(filepath.Join(root, "nested"), filepath.Join(root, "original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(filepath.Dir(root), "outside"), filepath.Join(root, "nested")); err != nil {
		t.Fatal(err)
	}
	if info, err := rootHandle.Lstat(relative); err == nil {
		t.Fatalf("Lstat accepted retargeted parent: %v", info)
	}
	if target, err := readRootLink(rootHandle, rootPath, relative, "/nested/parent-link"); err == nil {
		t.Fatalf("Readlink accepted retargeted parent: %q", target)
	}
}

func TestReadRootLinkRejectsReplacedRoot(t *testing.T) {
	root := rootMetadataFixture(t)
	rootHandle, rootPath, relative, err := openLiteralRootEntryParent(root, "/relative-link")
	if err != nil {
		t.Fatal(err)
	}
	defer rootHandle.Close() //nolint:errcheck // test cleanup
	if err := os.Rename(root, root+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(filepath.Dir(root), "outside"), root); err != nil {
		t.Fatal(err)
	}
	if target, err := readRootLink(rootHandle, rootPath, relative, "/relative-link"); err == nil {
		t.Fatalf("Readlink accepted replaced root: %q", target)
	}
}

func TestRootMetadataResolvesParentAfterIntermediateSymlink(t *testing.T) {
	root := rootMetadataFixture(t)
	if err := os.Mkdir(filepath.Join(root, "nested", "deeper"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "value.txt"), []byte("decoy"), 0o600); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{
		"shortcut":          "nested/deeper",
		"relative-dot-link": "shortcut/../value.txt",
		"absolute-dot-link": root + "/shortcut/../value.txt",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, requestPath := range []string{"/relative-dot-link", "/absolute-dot-link"} {
		assertRootLinkTarget(t, root, requestPath, "nested/value.txt")
	}
	assertHTTPRootEntry(t, root, "/%2572elative-dot-link", "nested/value.txt")
}

func assertRootLinkTarget(t *testing.T, root, requestPath, want string) {
	t.Helper()
	entry := LiteralRootPath(requestPath)
	for _, resolve := range []func(string) (string, error){entry.Readlink, entry.ResolveRelative} {
		got, err := resolve(root)
		if err != nil || got != want {
			t.Fatalf("target for %q = %q, %v; want %q", requestPath, got, err, want)
		}
	}
	info, err := entry.Lstat(root)
	if err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("Lstat(%q) = %v, %v; want symlink", requestPath, info, err)
	}
	assertHTTPRootEntry(t, root, requestPath, want)
}

func assertHTTPRootEntry(t *testing.T, root, requestPath, want string) {
	t.Helper()
	resolved, err := ResolveRoot(root, requestPath)
	if err != nil || resolved != filepath.Join(root, want) {
		t.Fatalf("ResolveRoot(%q) = %q, %v; want %q", requestPath, resolved, err, filepath.Join(root, want))
	}
	file, _, err := OpenRootEntry(root, requestPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close() //nolint:errcheck // test cleanup
	contents, err := io.ReadAll(file)
	if err != nil || string(contents) != filepath.Join(root, want) {
		t.Fatalf("OpenRootEntry(%q) contents = %q, %v", requestPath, contents, err)
	}
}

func TestRootMetadataRejectsOutsideTargetAfterIntermediateSymlink(t *testing.T) {
	root := rootMetadataFixture(t)
	outside := filepath.Join(filepath.Dir(root), "outside")
	if err := os.Mkdir(filepath.Join(outside, "deeper"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("decoy"), 0o600); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{
		"shortcut":          filepath.Join(outside, "deeper"),
		"relative-dot-link": "shortcut/../secret.txt",
		"absolute-dot-link": root + "/shortcut/../secret.txt",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, requestPath := range []string{"/relative-dot-link", "/absolute-dot-link"} {
		_, readErr := LiteralRootPath(requestPath).Readlink(root)
		_, statErr := LiteralRootPath(requestPath).Lstat(root)
		if !errors.Is(readErr, ErrOutsideRoot) || !errors.Is(statErr, ErrOutsideRoot) {
			t.Errorf("outside target %q: Readlink = %v, Lstat = %v; want ErrOutsideRoot", requestPath, readErr, statErr)
		}
	}
}

func rootMetadataFixture(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	outside := filepath.Join(parent, "outside")
	for _, directory := range []string{filepath.Join(root, "nested"), outside} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, filename := range []string{filepath.Join(root, "nested", "value.txt"), filepath.Join(outside, "secret.txt")} {
		if err := os.WriteFile(filename, []byte(filename), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	links := map[string]string{
		filepath.Join(root, "relative-link"):         "nested/value.txt",
		filepath.Join(root, "absolute-link"):         filepath.Join(root, "nested", "value.txt"),
		filepath.Join(root, "directory-link"):        filepath.Join(root, "nested"),
		filepath.Join(root, "root-link"):             ".",
		filepath.Join(root, "nested", "parent-link"): "../relative-link",
		filepath.Join(root, "outside-link"):          filepath.Join(outside, "secret.txt"),
		filepath.Join(root, "outside-directory"):     outside,
		filepath.Join(root, "dangling-link"):         "missing.txt",
		filepath.Join(outside, "link"):               "secret.txt",
		filepath.Join(outside, "parent-link"):        "secret.txt",
	}
	for filename, target := range links {
		if err := os.Symlink(target, filename); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
