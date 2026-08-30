package serve

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
				contents, readErr := os.ReadFile(resolved)
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
