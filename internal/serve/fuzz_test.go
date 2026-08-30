package serve

import (
	"os"
	"path/filepath"
	"testing"
)

func FuzzResolveRootConfinement(f *testing.F) {
	parent := f.TempDir()
	root := filepath.Join(parent, "root")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(filepath.Join(root, "safe"), 0o700); err != nil {
		f.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o700); err != nil {
		f.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "safe", "file.txt"), []byte("safe"), 0o600); err != nil {
		f.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		f.Fatal(err)
	}
	links := [][2]string{
		{"safe/file.txt", filepath.Join(root, "inside-link")},
		{"safe", filepath.Join(root, "inside-dir-link")},
		{filepath.Join(outside, "secret.txt"), filepath.Join(root, "outside-link")},
		{outside, filepath.Join(root, "outside-dir-link")},
		{"loop-b", filepath.Join(root, "loop-a")},
		{"loop-a", filepath.Join(root, "loop-b")},
		{root, filepath.Join(parent, "root-alias")},
	}
	for _, link := range links {
		if err := os.Symlink(link[0], link[1]); err != nil {
			f.Skipf("symlinks unavailable: %v", err)
		}
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		f.Fatal(err)
	}
	rootAlias := filepath.Join(parent, "root-alias")

	for _, seed := range []string{
		"", "/", ".", "///./", "/safe/file.txt", "/safe%2ffile.txt", "/inside-link", "/inside-dir-link/file.txt",
		"../outside/secret.txt", "/%2e%2e/outside/secret.txt", "/%252e%252e/outside/secret.txt",
		"/safe%2f..%2f..%2foutside/secret.txt", "/outside-link", "/outside-dir-link/secret.txt", "/loop-a",
		"/safe/file.txt\x00", "/safe/file.txt%00", "/safe/file.txt%2500", "/safe\\file.txt", "/safe%5cfile.txt",
		"/safe%255cfile.txt", "%", "%2", "%zz", string([]byte{'/', 0xff}),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, requestPath string) {
		resolved, err := ResolveRoot(root, requestPath)
		aliasResolved, aliasErr := ResolveRoot(rootAlias, requestPath)
		if (err == nil) != (aliasErr == nil) {
			t.Fatalf("root and symlink alias disagree: root error %v, alias error %v", err, aliasErr)
		}
		if err != nil {
			return
		}
		if resolved != aliasResolved {
			t.Fatalf("root resolved to %q, alias resolved to %q", resolved, aliasResolved)
		}
		canonicalResolved, err := filepath.EvalSymlinks(resolved)
		if err != nil {
			t.Fatalf("successful path does not exist: %v", err)
		}
		if canonicalResolved != resolved {
			t.Fatalf("successful path %q is not canonical; resolves to %q", resolved, canonicalResolved)
		}
		file, openedInfo, err := OpenRootEntry(root, requestPath)
		if err != nil {
			t.Fatalf("successful path could not be opened through its root: %v", err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("close rooted entry: %v", err)
		}
		resolvedInfo, err := os.Stat(resolved)
		if err != nil {
			t.Fatalf("stat successful path: %v", err)
		}
		if !os.SameFile(openedInfo, resolvedInfo) {
			t.Fatalf("rooted open and resolved path identify different entries")
		}
		relative, err := filepath.Rel(canonicalRoot, resolved)
		if err != nil || filepath.IsAbs(relative) || relative == ".." || len(relative) >= 3 && relative[:3] == ".."+string(filepath.Separator) {
			t.Fatalf("successful path %q escapes canonical root %q (relative %q, error %v)", resolved, canonicalRoot, relative, err)
		}
	})
}
