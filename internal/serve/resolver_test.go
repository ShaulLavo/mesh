package serve

import (
	"errors"
	"io"
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
